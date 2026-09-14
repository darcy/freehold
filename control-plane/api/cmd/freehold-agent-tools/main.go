// Command freehold-agent-tools is the CP's dedicated agent-management MCP
// server. It runs on the control plane as its own privileged process and
// exposes create_agent / grant_agent / manage_agent to agents — a DISTINCT
// semantic surface from the runner's generic exec funnel. Each tool handler
// calls internal/agent/tools.go in-process (direct, not a proxy, not an HTTP
// API hop), and callers authenticate with the same shared signed-header scheme
// the runner uses (core/src/auth.rs): granted pubkey + BIP-340 over
// `audience|ts|raw_body`, 60s window.
//
// The WHITELIST is the server's own relay roster, not a static list: the
// server is a private NIP-29 channel (9007), grants are channel membership
// (9000 put-user / 9001 remove-user), and the live whitelist is the relay's
// signed 39002 roster, read fresh for every call and fail-closed on relay
// outage — the exact model a runner's grants follow. Seeded at bootstrap with
// the operator/build identity; revocation is a roster change, no restart.
//
// Subcommands:
//
//	identity --state-dir DIR      mint (or reuse) the server's identity, print its pubkey
//	serve    ...                  MCP JSON-RPC over HTTP on the CP
//	mcp      ...                  stdio MCP facade the CPA pod's harness spawns,
//	                               forwarding to serve with the agent's signed auth
//	registry <verb> ...           verify/import-console-managed registry ops the
//	                               versioned migration scripts drive
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/term"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/relay"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/api/cpbuild"
	"freehold/control-plane/api/cpstate"
	"freehold/control-plane/cli/flows"
)

const relayFreeholdChannel = "00000000-0000-4000-8000-00000000f0ef"

// bridgeByDefault reports whether a bare invocation (NO subcommand) should run
// as the mcp stdio Bridge: the harness drives it over a stdin PIPE with empty
// args, whereas an operator sitting at a real terminal meant to use a
// subcommand and should see usage instead. Any explicit subcommand always wins.
func bridgeByDefault(argc int, stdinTTY bool) bool { return argc == 0 && !stdinTTY }

func main() {
	log.SetFlags(0)
	if bridgeByDefault(len(os.Args)-1, term.IsTerminal(os.Stdin.Fd())) {
		cmdMCP(nil)
		return
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "identity":
		cmdIdentity(os.Args[2:])
	case "seed":
		cmdSeed(os.Args[2:])
	case "mcp":
		cmdMCP(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "registry":
		cmdRegistry(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `freehold-agent-tools — the CP's agent-management MCP server

usage:
  freehold-agent-tools identity --state-dir DIR          mint (or reuse) this server's identity, print its pubkey
  freehold-agent-tools seed --state-dir DIR --relay-url URL [--granted PK1,PK2]
                                                          create the server's private channel + member the granted
                                                          bootstrap identities into its roster (one-time bootstrap seed)
  freehold-agent-tools serve FLAGS                       serve create/grant/manage-agent over MCP (signed-header auth)
  freehold-agent-tools registry verify --registry PATH   migration 001: registry is a loadable store
  freehold-agent-tools registry import-console --registry PATH --console-state DIR
                                                          migration 002: fold console agents into the registry additively

serve FLAGS:
  --state-dir       DIR          durable identity + created-agent identity dirs (/srv/data/cp on the CP)
  --addr            HOST:PORT    HTTP MCP bind address (the CP's LAN address, e.g. 192.168.30.214:8089)
  --secret          HEX          this server's own Nostr secret (audience + roster-query + deploy identity)
  --relay-url       URL          the community relay HTTP origin
  --relay-ws        URL          the wss:// origin agent pods dial (defaults to relay-url https->wss)
  --relay-pubkey    HEX          the relay's signing pubkey (the 39002 roster trust anchor)
  --relay-lxc       VMID         the relay LXC's vmid (relay membership runs through the runner's pct exec)
  --relay-compose   DIR          relay compose dir inside the relay LXC (buzz-admin runs there)
  --k3s-vmid        VMID         the k3s LXC's vmid (agent pods apply here)
  --relay-host      HOST         relay's public host (Caddy edge front)
  --relay-ip        IP           relay LXC LAN IP (Caddy upstream), CIDR ok
  --cp-host         HOST         control plane's public host (Caddy edge front)
  --cp-ip           IP           cp LXC LAN IP (Caddy upstream), CIDR ok
  --cp-lxc          VMID         the cp LXC's vmid (the dnsmasq resolver lives here)
  --proxy-ip        IP           proxy/k3s node static IP (CIDR ok) the public hosts resolve to
  --litellm-ip      IP           litellm gateway node IP (k3s node), CIDR ok
  --plane-pool      POOL         durable-plane backend pool (VG or zpool), e.g. pve
  --plane-kind      KIND         recorded backend kind (zfs|lvmth); detect when empty
  --thin-pool       POOL         the freehold-CREATED thin pool (LVM-thin backend)
  --size-gb         GB           per-tenant LV size GB (LVM-thin)
  --pool-size-gb    GB           thin-pool size GB when carved
  --rootfs-gb       GB           LXC rootfs size GB (relay/k3s boots)
  --memory-mb       MB           LXC memory MB (relay/k3s boots)
  --storage         NAME         PVE LXC storage (relay/k3s boots)
  --relay-gw        IP           gateway for the k3s STATIC guest IP
  --bridge          NAME         PVE LXC bridge (relay/k3s boots)
  --runner-addr     ADDR         the CP's co-located runner MCP addr (127.0.0.1:8787) deploys run through
  --runner-pubkey   HEX          the runner's pubkey (audience for the deploy exec)
  --runner-target   TGT          the runner target whose door reaches the box (e.g. proxmox-box)
  --console-url     URL          the CP console base URL (create-agent registry)
  --cpa-name        NAME         the CPA display name (a create of this name deploys as the CPA)
  --owner-pubkey    HEX          the agent identity secret's owner (operator pubkey)
  --litellm-base    URL          litellm gateway base URL for agent pods (CPA uses it to reason)
`)
}

func cmdIdentity(args []string) {
	fs := flag.NewFlagSet("identity", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "durable state dir")
	encPubkey := fs.Bool("enc-pubkey", false, "print the ENCRYPTION pubkey (X25519 of the enc secret) instead of the nostr pubkey")
	fs.Parse(args)
	if *stateDir == "" {
		log.Fatal("identity needs --state-dir")
	}
	id, _, err := serverIdentity(*stateDir)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	if *encPubkey {
		secret, err := hex.DecodeString(id.EncSecretHex)
		if err != nil {
			log.Fatalf("identity enc secret: %v", err)
		}
		pk, err := crypto.X25519PublicKey(secret)
		if err != nil {
			log.Fatalf("identity enc pubkey: %v", err)
		}
		fmt.Println(hex.EncodeToString(pk))
		return
	}
	pk, err := id.NostrPubkeyHex()
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	fmt.Println(pk)
}

// serverIdentity loads the durable identity in dir, minting it on first use.
func serverIdentity(dir string) (*flows.Identity, string, error) {
	if _, err := agent.EnsureIdentity(dir); err != nil {
		return nil, "", err
	}
	id, err := flows.LoadIdentity(dir)
	if err != nil {
		return nil, "", err
	}
	pk, err := id.NostrPubkeyHex()
	if err != nil {
		return nil, "", err
	}
	return id, pk, nil
}

// cmdSeed creates this server's private channel (9007) and members the granted
// bootstrap identities into it — the one-time seed that establishes the CP-side
// roster as the durable source of truth (structurally the same as how
// --operator-pubkey seeds the console admin whitelist). The server is the
// channel owner, so it may member/revoke members itself going forward.
func cmdSeed(args []string) {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "durable state dir")
	relayURL := fs.String("relay-url", "", "relay HTTP origin (dial URL; LAN http://<domain>:3000 pre-Caddy)")
	relayAuthURL := fs.String("relay-auth-url", "", "relay CANONICAL URL for NIP-98 signing (public https://<domain>); defaults to relay-url")
	granted := fs.String("granted", "", "comma-separated bootstrap grant pubkeys to member")
	revoke := fs.String("revoke", "", "comma-separated pubkeys to REMOVE from the roster (idempotent cleanup)")
	name := fs.String("name", "agent-tools", "channel/identity display name")
	fs.Parse(args)
	if *stateDir == "" || *relayURL == "" {
		log.Fatal("seed needs --state-dir --relay-url")
	}
	id, pk, err := serverIdentity(*stateDir)
	if err != nil {
		log.Fatalf("seed identity: %v", err)
	}
	sec, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil {
		log.Fatal(err)
	}
	authURL := *relayAuthURL
	if authURL == "" {
		authURL = *relayURL
	}
	if err := relay.CreateRunnerChannelAuth(*relayURL, authURL, sec, pk, *name); err != nil {
		log.Fatalf("seed channel: %v", err)
	}
	for _, g := range strings.Split(*granted, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if err := relay.PutUserAuth(*relayURL, authURL, sec, pk, g); err != nil {
			log.Fatalf("seed member %s: %v", g, err)
		}
	}
	for _, g := range strings.Split(*revoke, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if err := relay.RemoveUserAuth(*relayURL, authURL, sec, pk, g); err != nil {
			log.Fatalf("seed revoke %s: %v", g, err)
		}
	}
	fmt.Println(pk)
}

// cmdRegistry exposes the versioned-migration registry ops the migration
// scripts drive: `verify` (the registry is a loadable store) and
// `import-console` (fold the console state.json agents in additively). The
// script passes paths via argv (they are non-secret durable-plane paths); this
// keeps the Omarchy `<epoch>.sh` migration content deterministic and Go-tested.
func cmdRegistry(args []string) {
	if len(args) < 1 {
		log.Fatal("registry needs a verb: verify|import-console")
	}
	switch args[0] {
	case "verify":
		fs := flag.NewFlagSet("registry verify", flag.ExitOnError)
		path := fs.String("registry", "", "path to registry.json")
		fs.Parse(args[1:])
		if *path == "" {
			log.Fatal("registry verify needs --registry")
		}
		if err := agenttools.CheckRegistry(*path); err != nil {
			log.Fatalf("registry verify: %v", err)
		}
		fmt.Println("registry verified")
	case "import-console":
		fs := flag.NewFlagSet("registry import-console", flag.ExitOnError)
		path := fs.String("registry", "", "path to registry.json (authoritative)")
		console := fs.String("console-state", "", "console state dir (its state.json agents)")
		fs.Parse(args[1:])
		if *path == "" || *console == "" {
			log.Fatal("registry import-console needs --registry --console-state")
		}
		reg, err := agenttools.OpenRegistry(*path)
		if err != nil {
			log.Fatalf("registry open: %v", err)
		}
		if err := agenttools.ImportConsoleAgents(reg, *console); err != nil {
			log.Fatalf("registry import-console: %v", err)
		}
		fmt.Println("registry import-console converged")
	default:
		log.Fatalf("registry: unknown verb %q (verify|import-console)", args[0])
	}
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "durable state dir")
	consoleStateDir := fs.String("console-state-dir", "/srv/data/cp/control-plane", "the CONSOLE's durable state dir (its state.json + console identity — the roster owner this server fronts)")
	addr := fs.String("addr", "127.0.0.1:8089", "HTTP MCP bind address")
	secret := fs.String("secret", "", "this server's Nostr secret hex (audience)")
	relayURL := fs.String("relay-url", "", "relay HTTP origin (dial URL; LAN http://<domain>:3000 pre-Caddy)")
	relayAuthURL := fs.String("relay-auth-url", "", "relay CANONICAL URL for NIP-98 signing (public https://<domain>); defaults to relay-url")
	relayWS := fs.String("relay-ws", "", "relay wss:// origin for agent pods")
	relayPK := fs.String("relay-pubkey", "", "relay signing pubkey (roster trust anchor)")
	var relayLxc, k3sVmid uint
	fs.UintVar(&relayLxc, "relay-lxc", 0, "relay LXC vmid")
	relayCompose := fs.String("relay-compose", "/srv/data/relay/deploy/compose", "relay compose dir")
	fs.UintVar(&k3sVmid, "k3s-vmid", 0, "k3s LXC vmid")
	relayHost := fs.String("relay-host", "", "relay's public host (Caddy edge front)")
	relayIP := fs.String("relay-ip", "", "relay LXC LAN IP (Caddy upstream), CIDR ok")
	cpHost := fs.String("cp-host", "", "control plane's public host (Caddy edge front)")
	cpIP := fs.String("cp-ip", "", "cp LXC LAN IP (Caddy upstream), CIDR ok")
	var cpLxc uint
	fs.UintVar(&cpLxc, "cp-lxc", 0, "cp LXC vmid (the dnsmasq resolver lives here)")
	proxyIP := fs.String("proxy-ip", "", "proxy/k3s node static IP (CIDR ok) the public hosts resolve to")
	litellmIP := fs.String("litellm-ip", "", "litellm gateway node IP (k3s node), CIDR ok")
	planePool := fs.String("plane-pool", "", "durable-plane backend pool (VG or zpool), e.g. pve")
	planeKind := fs.String("plane-kind", "", "recorded backend kind (zfs|lvmth); detect when empty")
	thinPool := fs.String("thin-pool", "", "the freehold-CREATED thin pool (LVM-thin backend)")
	var sizeGB, poolSizeGB uint
	fs.UintVar(&sizeGB, "size-gb", 10, "per-tenant LV size GB (LVM-thin)")
	fs.UintVar(&poolSizeGB, "pool-size-gb", 40, "thin-pool size GB when carved")
	rootfsGB := fs.Uint("rootfs-gb", 16, "LXC rootfs size GB (relay/k3s boots)")
	memoryMB := fs.Uint("memory-mb", 2048, "LXC memory MB (relay/k3s boots)")
	storageName := fs.String("storage", "local-lvm", "PVE LXC storage (relay/k3s boots)")
	relayGW := fs.String("relay-gw", "192.168.30.1", "gateway for the k3s STATIC guest IP")
	bridge := fs.String("bridge", "vmbr0", "PVE LXC bridge (relay/k3s boots)")
	runnerAddr := fs.String("runner-addr", "127.0.0.1:8787", "CP co-located runner MCP addr")
	runnerPK := fs.String("runner-pubkey", "", "runner pubkey (deploy exec audience)")
	runnerTarget := fs.String("runner-target", "", "runner target that reaches the box")
	cpaName := fs.String("cpa-name", agent.DefaultCPAName, "CPA display name")
	ownerPub := fs.String("owner-pubkey", "", "operator pubkey (agent identity secret owner)")
	litellmBase := fs.String("litellm-base", "", "litellm gateway base URL for agent pods")
	selfURL := fs.String("self-url", "", "this server's reachable HTTP base URL (e.g. http://<cpIP>:8089) the CPA pod bootstraps its mcp bridge from")
	fs.Parse(args)

	if *stateDir == "" || *relayURL == "" {
		log.Fatal("serve needs --state-dir --relay-url")
	}
	if *runnerPK == "" || *runnerTarget == "" {
		log.Fatal("serve needs --runner-pubkey --runner-target (the CP's co-located runner the deploy runs through)")
	}
	if *ownerPub == "" {
		log.Fatal("serve needs --owner-pubkey (the operator identity that owns agent identity secrets)")
	}
	if *selfURL == "" {
		log.Fatal("serve needs --self-url (the reachable URL the CPA pod bootstraps its mcp bridge from)")
	}
	// The relay/k3s vmids are OPTIONAL: a fresh world (coords cleared at
	// teardown) has none recorded yet — world_build discovers + boots them and
	// the create-agent relay membership resolves the relay vmid by hostname.
	if relayLxc == 0 && k3sVmid == 0 {
		log.Print("serve: no relay/k3s vmids recorded — world_build will discover them (fresh world)")
	}
	// The secret is this server's durable identity (minted by `identity`/
	// `seed` in --state-dir); load it when not passed explicitly so the launch
	// argv never carries the nsec.
	if *secret == "" {
		id, _, err := serverIdentity(*stateDir)
		if err != nil {
			log.Fatalf("serve identity unreadable at %s: %v", *stateDir, err)
		}
		*secret = id.NostrSecretHex
	}
	secBytes, err := hex.DecodeString(*secret)
	if err != nil || len(secBytes) != 32 {
		log.Fatalf("--secret must be 32-byte hex: %v", err)
	}
	audience, err := crypto.PubkeyFromSecret(secBytes)
	if err != nil {
		log.Fatal(err)
	}
	relayWSURL := *relayWS
	if relayWSURL == "" {
		relayWSURL = strings.Replace(*relayURL, "https://", "wss://", 1)
	}
	litellmBaseURL := strings.TrimSpace(*litellmBase)
	if litellmBaseURL != "" {
		litellmBaseURL = strings.TrimSuffix(litellmBaseURL, "/") + "/v1"
	}

	spec := &cpbuild.Spec{
		StateDir:       *stateDir,
		RelayURL:       *relayURL,
		RelayAuthURL:   *relayAuthURL,
		RelayWS:        relayWSURL,
		RelayPK:        *relayPK,
		RelayHost:      *relayHost,
		RelayIP:        strings.TrimSpace(config.StripCIDR(*relayIP)),
		CpHost:         *cpHost,
		CpIP:           strings.TrimSpace(config.StripCIDR(*cpIP)),
		CpLxc:          uint32(cpLxc),
		ProxyIP:        strings.TrimSpace(config.StripCIDR(*proxyIP)),
		LitellmIP:      strings.TrimSpace(config.StripCIDR(*litellmIP)),
		PlanePool:      *planePool,
		PlaneKind:      *planeKind,
		ThinPool:       *thinPool,
		SizeGB:         uint64(sizeGB),
		PoolSizeGB:     uint64(poolSizeGB),
		RootfsGB:       uint32(*rootfsGB),
		MemoryMB:       uint32(*memoryMB),
		StorageName:    *storageName,
		RelayGW:        *relayGW,
		Bridge:         *bridge,
		RelayLxc:       uint32(relayLxc),
		RelayCompose:   *relayCompose,
		K3sVmid:        uint32(k3sVmid),
		RunnerAddr:     *runnerAddr,
		RunnerPK:       *runnerPK,
		RunnerTarget:   *runnerTarget,
		CpaName:        *cpaName,
		OwnerPub:       *ownerPub,
		LitellmBaseURL: litellmBaseURL,
		Sec:            secBytes,
		Audience:       audience,
		SelfURL:        strings.TrimSuffix(*selfURL, "/"),
	}

	// Roster-fresh whitelist, fail-closed: a relay read error yields empty
	// grants => VerifyRequest denies every request. The roster is the relay's
	// signed 39002 for this server's channel (self-consistent; relayPubkey is
	// an optional additional author pin).
	grants := func() ([]string, error) {
		// The recorded relay_pubkey is the relay's NIP-11 key, not the GROUP
		// key that signs 39002 rosters on buzz (they differ) — QueryRoster pins
		// to it when it matches the roster author and falls back to
		// self-consistent verification when it doesn't (the relay is the sole
		// 39002 publisher, so a present-and-valid roster IS the relay's).
		m, err := agenttools.QueryRosterAuth(*relayURL, *relayAuthURL, *relayPK, audience, secBytes)
		if err != nil {
			log.Printf("freehold-agent-tools: roster unreadable — failing closed: %v", err)
			return nil, nil
		}
		return m, nil
	}

	reg, err := agenttools.OpenRegistry(filepath.Join(*stateDir, "registry.json"))
	if err != nil {
		log.Fatalf("open registry: %v", err)
	}
	// The console-owner credential for roster writes (grant_agent): the
	// console's OWN identity, read from ITS state dir (0600 durable plane),
	// used in-process to sign put-user publishes. Missing = grants refuse
	// loudly (fail-closed), never silently succeed.
	consoleSecret, cerr := cpstate.ConsoleSecret(*consoleStateDir)
	if cerr != nil {
		log.Printf("serve: console-owner credential unreadable at %s — grant_agent will fail closed: %v", *consoleStateDir, cerr)
	}
	reg.RelayURL = *relayURL
	reg.ConsoleSecret = consoleSecret
	reg.ConsoleStateDir = *consoleStateDir

	facts, err := agenttools.OpenFacts(filepath.Join(*stateDir, "facts.json"))
	if err != nil {
		log.Fatalf("open world facts: %v", err)
	}

	tools := &agent.Tools{Console: reg, Create: cpbuild.BuildCreateAgentFn(spec)}
	tools.Migrate = cpbuild.BuildMigrator(spec, *consoleStateDir)
	tools.World = cpbuild.BuildWorldApply(spec)
	tools.Exec = cpbuild.BuildWorldExec(spec)
	tools.Status = cpbuild.BuildWorldStatus(spec, reg, *consoleStateDir, facts)
	doorAuth, doorRevoke := cpbuild.BuildWorldDoor(spec)
	tools.DoorAuthorize = doorAuth
	tools.DoorRevoke = doorRevoke
	srv := &agenttools.Server{
		Facts:    facts,
		Audience: audience,
		Grants:   grants,
		Tools:    tools,
		IsAgent: func(caller string) bool {
			agents, err := reg.Agents()
			if err != nil {
				// Fail CLOSED on a registry-read error: if we cannot prove the
				// caller is an operator (not in the registry), treat it as an
				// agent and deny the operator-scoped tools.
				return true
			}
			for _, a := range agents {
				if a.Pubkey == caller {
					return true
				}
			}
			return false
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", srv.ServeHTTP)
	// Serve this binary so the CPA pod can fetch the `mcp` stdio bridge at
	// bootstrap (the image doesn't ship freehold-agent-tools).
	mux.HandleFunc("/freehold-agent-tools-binary", func(w http.ResponseWriter, r *http.Request) {
		self, err := os.Executable()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.ServeFile(w, r, self)
	})
	log.Printf("freehold-agent-tools serving on %s (audience %s, self-url %s)", *addr, audience, *selfURL)
	s := &http.Server{Addr: *addr, Handler: mux}
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	_ = context.Background
}
