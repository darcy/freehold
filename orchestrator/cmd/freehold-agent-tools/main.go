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
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	"freehold/orchestrator/internal/agent"
	"freehold/orchestrator/internal/agenttools"
	"freehold/orchestrator/internal/bootstrap"
	"freehold/orchestrator/internal/cert"
	"freehold/orchestrator/internal/client"
	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/crypto"
	"freehold/orchestrator/internal/delegate"
	"freehold/orchestrator/internal/deploy"
	"freehold/orchestrator/internal/drive"
	"freehold/orchestrator/internal/flows"
	"freehold/orchestrator/internal/migrations"
	"freehold/orchestrator/internal/planebase"
	"freehold/orchestrator/internal/relay"
	"freehold/orchestrator/internal/stages"
	"freehold/orchestrator/prompts"
)

const relayFreeholdChannel = "00000000-0000-4000-8000-00000000f0ef"

// bridgeByDefault reports whether a bare invocation (NO subcommand) should run
// as the mcp stdio bridge: the harness drives it over a stdin PIPE with empty
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
	fs.Parse(args)
	if *stateDir == "" {
		log.Fatal("identity needs --state-dir")
	}
	_, pk, err := serverIdentity(*stateDir)
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
	relayURL := fs.String("relay-url", "", "relay HTTP origin")
	granted := fs.String("granted", "", "comma-separated bootstrap grant pubkeys to member")
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
	if err := relay.CreateRunnerChannel(*relayURL, sec, pk, *name); err != nil {
		log.Fatalf("seed channel: %v", err)
	}
	for _, g := range strings.Split(*granted, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if err := relay.PutUser(*relayURL, sec, pk, g); err != nil {
			log.Fatalf("seed member %s: %v", g, err)
		}
	}
	fmt.Println(pk)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "durable state dir")
	addr := fs.String("addr", "127.0.0.1:8089", "HTTP MCP bind address")
	secret := fs.String("secret", "", "this server's Nostr secret hex (audience)")
	relayURL := fs.String("relay-url", "", "relay HTTP origin")
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
	if relayLxc == 0 || k3sVmid == 0 {
		log.Fatal("serve needs --relay-lxc and --k3s-vmid")
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

	spec := &deploySpec{
		stateDir:       *stateDir,
		relayURL:       *relayURL,
		relayWS:        relayWSURL,
		relayPK:        *relayPK,
		relayHost:      *relayHost,
		relayIP:        strings.TrimSpace(config.StripCIDR(*relayIP)),
		cpHost:         *cpHost,
		cpIP:           strings.TrimSpace(config.StripCIDR(*cpIP)),
		cpLxc:          uint32(cpLxc),
		proxyIP:        strings.TrimSpace(config.StripCIDR(*proxyIP)),
		litellmIP:      strings.TrimSpace(config.StripCIDR(*litellmIP)),
		planePool:      *planePool,
		planeKind:      *planeKind,
		thinPool:       *thinPool,
		sizeGB:         uint64(sizeGB),
		poolSizeGB:     uint64(poolSizeGB),
		rootfsGB:       uint32(*rootfsGB),
		memoryMB:       uint32(*memoryMB),
		storageName:    *storageName,
		relayGW:        *relayGW,
		bridge:         *bridge,
		relayLxc:       uint32(relayLxc),
		relayCompose:   *relayCompose,
		k3sVmid:        uint32(k3sVmid),
		runnerAddr:     *runnerAddr,
		runnerPK:       *runnerPK,
		runnerTarget:   *runnerTarget,
		cpaName:        *cpaName,
		ownerPub:       *ownerPub,
		litellmBaseURL: litellmBaseURL,
		sec:            secBytes,
		audience:       audience,
		selfURL:        strings.TrimSuffix(*selfURL, "/"),
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
		m, err := agenttools.QueryRoster(*relayURL, *relayPK, audience, secBytes)
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

	tools := &agent.Tools{Console: reg, Create: buildCreateAgentFn(spec)}
	tools.Migrate = buildMigrator(spec)
	tools.World = buildWorldApply(spec)
	srv := &agenttools.Server{Audience: audience, Grants: grants, Tools: tools}

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

// deploySpec is the server's deploy backend wiring (the CP's co-located
// runner + the recorded world coordinates the deploy scripts need).
type deploySpec struct {
	stateDir       string
	relayURL       string
	relayWS        string
	relayPK        string
	relayHost      string
	relayIP        string
	cpHost         string
	cpIP           string
	cpLxc          uint32
	proxyIP        string
	litellmIP      string
	planePool      string
	planeKind      string
	thinPool       string
	sizeGB         uint64
	poolSizeGB     uint64
	rootfsGB       uint32
	memoryMB       uint32
	storageName    string
	relayGW        string
	bridge         string
	relayLxc       uint32
	relayCompose   string
	k3sVmid        uint32
	runnerAddr     string
	runnerPK       string
	runnerTarget   string
	cpaName        string
	ownerPub       string
	litellmBaseURL string
	sec            []byte
	audience       string
	selfURL        string
}

func (s *deploySpec) client() (*client.McpClient, error) {
	auth := &client.AgentAuth{}
	copy(auth.Secret[:], s.sec)
	auth.Pubkey = s.audience
	return client.New(client.ConnectURL(s.runnerAddr), auth, s.runnerPK)
}

// execOut runs cmd through the co-located runner (target = the box) and
// returns its stdout. Secrets requested: the runner requires the SSH target's
// OWN credential among the requested secrets (it does not default to it then),
// so the target name is always first; extraSecrets add requested secrets by
// name (the runner injects each as an env var + redacts it).
func (s *deploySpec) execOut(cmd string, timeoutS uint64, extraSecrets ...string) (string, error) {
	mc, err := s.client()
	if err != nil {
		return "", err
	}
	secrets := append([]string{s.runnerTarget}, extraSecrets...)
	out, err := mc.Exec(s.runnerTarget, cmd, secrets, timeoutS)
	if err != nil {
		return "", err
	}
	if out.TimedOut || out.ExitCode == nil || *out.ExitCode != 0 {
		ec := -1
		if out.ExitCode != nil {
			ec = *out.ExitCode
		}
		return "", fmt.Errorf("runner exec failed (timed=%v exit=%d): %s %s", out.TimedOut, ec, strings.TrimSpace(out.Stdout), strings.TrimSpace(out.Stderr))
	}
	return out.Stdout, nil
}

func (s *deploySpec) runOut(cmd string, timeoutS uint64) (string, error) {
	return s.execOut(cmd, timeoutS)
}

func (s *deploySpec) run(cmd string, timeoutS uint64) error {
	_, err := s.execOut(cmd, timeoutS)
	return err
}

// runSecrets runs cmd through the co-located runner requesting extra secret
// names by name (e.g. the litellm master + provider key the register curl
// reads from $LITELLM / $PROVIDER_KEY).
func (s *deploySpec) runSecrets(cmd string, timeoutS uint64, extra ...string) error {
	_, err := s.execOut(cmd, timeoutS, extra...)
	return err
}

// cpGuestDirs derives the deployed control-plane's bin + state dirs inside the
// cp LXC from the agent-tools state dir (<root>/agent-tools -> <root>/bin +
// <root>/control-plane, the cpGuestDirs layout deploy-cp uses).
func (s *deploySpec) cpGuestDirs() (binDir, stateDir string) {
	root := filepath.Dir(s.stateDir)
	return filepath.Join(root, "bin"), filepath.Join(root, "control-plane")
}

// guestSearchBase reads the `search` line from the CP LXC's resolv.conf (PVE
// writes the same search domain to every guest it manages). Empty when absent.
func (s *deploySpec) guestSearchBase() string {
	out, err := s.runOut(fmt.Sprintf("pct exec %d -- sh -c \"grep '^search' /etc/resolv.conf | head -1 | cut -d' ' -f2-\"", s.cpLxc), 30)
	if err != nil {
		return ""
	}
	base := strings.TrimSpace(out)
	if base == "" || strings.ContainsAny(base, " \"'`$;(){}") || !strings.Contains(base, ".") {
		return ""
	}
	return base
}

// guestNameserver returns the CP LXC's dnsmasq UPSTREAM (the router), tried in
// order: the PVE-owned net0 gw= (static guests only), then the default route,
// then the resolv.conf nameserver that isn't the resolver's own IP.
func (s *deploySpec) guestNameserver() string {
	if out, err := s.runOut(fmt.Sprintf("pct config %d", s.cpLxc), 30); err == nil {
		if gw := stages.ParsePctGateway(out); gw != "" {
			return gw
		}
	}
	if out, err := s.runOut(fmt.Sprintf("pct exec %d -- sh -c \"ip route show default | head -1 | cut -d' ' -f3\"", s.cpLxc), 30); err == nil {
		if ns := strings.TrimSpace(out); ns != "" && strings.ContainsAny(ns, "0123456789") {
			return ns
		}
	}
	if out, err := s.runOut(fmt.Sprintf("pct exec %d -- sh -c \"grep '^nameserver' /etc/resolv.conf | cut -d' ' -f2\"", s.cpLxc), 30); err == nil {
		for _, l := range strings.Split(out, "\n") {
			ns := strings.TrimSpace(l)
			if ns != "" && ns != s.cpIP && strings.ContainsAny(ns, "0123456789") {
				return ns
			}
		}
	}
	return ""
}

// worldDNS registers the CP resolver's explicit records and points every guest
// at it (the stageDnsRegister + stageDnsPoint pair, CP-side): upsert the
// split-horizon names via `control-plane dns add` inside the CP, pct-set the
// guests' nameserver, rewrite their resolv.conf now (pct regenerates it only at
// the next boot), and prove the resolver ANSWERS a record from its own loopback.
func (s *deploySpec) worldDNS() error {
	binDir, stateDir := s.cpGuestDirs()
	searchBase := s.guestSearchBase()
	for _, r := range stages.DnsRecords(s.relayHost, s.relayIP, s.cpHost, s.cpIP, s.proxyIP, s.litellmIP) {
		if err := s.run(stages.DnsAddCmd(s.cpLxc, binDir, stateDir, r.Name, r.IP, r.Source, searchBase), 120); err != nil {
			return fmt.Errorf("world-build dns register %s: %w", r.Name, err)
		}
	}
	router := s.guestNameserver()
	for _, role := range []struct {
		name string
		vmid uint32
	}{
		{"relay", s.relayLxc}, {"cp", s.cpLxc}, {"k3s", s.k3sVmid},
	} {
		if role.vmid == 0 {
			continue
		}
		r := ""
		if role.name == "cp" {
			r = router
		}
		pctSet, resolvConf := stages.DnsPointCmd(role.vmid, s.cpIP, r, searchBase)
		if err := s.run(pctSet, 60); err != nil {
			return fmt.Errorf("world-build dns point %s: %w", role.name, err)
		}
		if err := s.run(resolvConf, 60); err != nil {
			return fmt.Errorf("world-build dns point %s resolv.conf: %w", role.name, err)
		}
	}
	for _, q := range []struct{ name, want string }{
		{"relay", s.relayIP}, {"litellm", s.litellmIP},
	} {
		if q.want == "" {
			continue
		}
		if err := s.run(stages.DnsVerifyCmd(s.cpLxc, q.name, q.want), 30); err != nil {
			return fmt.Errorf("world-build dns verify %s: %w", q.name, err)
		}
	}
	return nil
}

// worldStorage re-ensures the durable volume plane CP-side (the box's
// stagePlacement + stageStorage ensure half): resolve the backend kind
// (recorded --plane-kind, else detect like the box's parseKind), then ensure
// each tenant's dataset/LV onto the recorded pool and chown it guest-writable
// — idempotent, no box-side secret (the storage ops run on the PVE host
// through the co-located runner, exactly as the box drives them). Returns the
// ensured mounts per tenant (the born-at-create mounts the LXC boots bake).
func (s *deploySpec) worldStorage() (map[planebase.Tenant][]planebase.MountSpec, error) {
	mc, err := s.client()
	if err != nil {
		return nil, err
	}
	kind := planebase.BackendKind(s.planeKind)
	if kind == "" {
		action, err := bootstrap.ResolveProxmox(mc, s.runnerTarget, false, nil)
		if err != nil {
			return nil, fmt.Errorf("storage resolve: %w", err)
		}
		if action.Kind != "Reuse" {
			return nil, fmt.Errorf("no storage backend to ensure onto: %s", action.Message)
		}
		if *action.Detected == planebase.ExistingZfs {
			kind = planebase.KindZfs
		} else {
			kind = planebase.KindLvmThin
		}
	}
	mounts := map[planebase.Tenant][]planebase.MountSpec{}
	for _, tenant := range []planebase.Tenant{planebase.TenantRelay, planebase.TenantCp, planebase.TenantK3sVolumes} {
		var ms []planebase.MountSpec
		switch kind {
		case planebase.KindZfs:
			ms, err = drive.ResolveTenantMounts(mc, s.runnerTarget, s.planePool, s.relayHost, tenant)
		case planebase.KindLvmThin:
			ms, err = drive.ResolveLvmMounts(mc, s.runnerTarget, s.planePool, s.relayHost, tenant, s.sizeGB, s.poolSizeGB, s.thinPool)
		default:
			return nil, fmt.Errorf("unknown storage backend kind %q (zfs|lvmth)", kind)
		}
		if err != nil {
			return nil, fmt.Errorf("storage ensure %s: %w", tenant, err)
		}
		mounts[tenant] = ms
	}
	return mounts, nil
}

// bootLxc boots (or reuses) a role's LXC via the shared bootstrap driver
// through the co-located runner, baking the durable-plane mounts at create.
// role's static address (k3s = the proxy IP; relay/cp are DHCP behind the
// proxy) rides the spec.
func (s *deploySpec) bootLxc(role string, vmid uint32, mounts []planebase.MountSpec) error {
	hostname, err := bootstrap.DomainLXCName(s.relayHost, role)
	if err != nil {
		return err
	}
	spec := &bootstrap.ProxmoxLxcSpec{
		Hostname: hostname,
		Storage:  s.storageName,
		RootfsGB: s.rootfsGB,
		MemoryMB: s.memoryMB,
		Bridge:   s.bridge,
		Mounts:   mounts,
	}
	if vmid != 0 {
		spec.VMID = &vmid
	}
	if role == "k3s" && s.proxyIP != "" {
		ip := s.proxyIP
		gw := s.relayGW
		spec.NetIP = &ip
		spec.NetGW = &gw
	}
	mc, err := s.client()
	if err != nil {
		return err
	}
	if _, err := bootstrap.BootstrapProxmoxLxc(mc, s.runnerTarget, spec); err != nil {
		return fmt.Errorf("boot %s LXC: %w", role, err)
	}
	return nil
}

// refreshGuestIPs re-reads the relay/cp/k3s guests' CURRENT IPv4 after a boot
// (a DHCP re-lease can change an address the recorded coords no longer match),
// updating the fields the DNS/caddy/litellm/cert steps consume. Best-effort.
func (s *deploySpec) refreshGuestIPs() {
	type role struct {
		vmid uint32
		ip   *string
	}
	for _, r := range []role{
		{s.relayLxc, &s.relayIP},
		{s.cpLxc, &s.cpIP},
		{s.k3sVmid, &s.proxyIP},
	} {
		if r.vmid == 0 {
			continue
		}
		out, err := s.runOut(fmt.Sprintf("pct exec %d -- ip -4 -o addr show eth0", r.vmid), 30)
		if err != nil {
			continue
		}
		for _, t := range strings.Fields(out) {
			if strings.Contains(t, "/") && t != "127.0.0.1/8" {
				// ip -4 -o addr reports CIDR; the downstream consumers
				// (DNS records, Caddy/litellm upstreams) expect a bare IP.
				*r.ip = config.StripCIDR(t)
				break
			}
		}
	}
}

// worldBootRelay boots the relay LXC (if missing) + deploys the Buzz stack
// into it via the shared deploy driver (idempotent compose bring-up).
func (s *deploySpec) worldBootRelay(mounts []planebase.MountSpec) error {
	if err := s.bootLxc("relay", s.relayLxc, mounts); err != nil {
		return err
	}
	deployDir := ""
	for _, m := range mounts {
		if m.GuestPath != "/var/lib/docker" {
			deployDir = m.GuestPath
			break
		}
	}
	if deployDir == "" {
		deployDir = "/srv/data/relay"
	}
	mc, err := s.client()
	if err != nil {
		return err
	}
	host := s.relayHost
	relayURL := "https://" + host
	if _, err := deploy.DeployRelay(mc, s.runnerTarget, &deploy.RelayDeploySpec{
		RelayName:      "relay",
		DeployDir:      deployDir,
		HTTPPort:       3000,
		BuzzRef:        deploy.DefaultBufRef,
		LXc:            &s.relayLxc,
		OwnerPubkey:    s.ownerPub,
		RelayURL:       relayURL,
		OperatorPubkey: s.ownerPub,
		Domain:         &host,
	}); err != nil {
		return fmt.Errorf("deploy relay: %w", err)
	}
	return nil
}

// worldBootK3s boots the k3s LXC (if missing), installs k3s inside it (the
// shared in-guest install script), and re-asserts the durable local-path.
func (s *deploySpec) worldBootK3s(mounts []planebase.MountSpec) error {
	if err := s.bootLxc("k3s", s.k3sVmid, mounts); err != nil {
		return err
	}
	cmd := fmt.Sprintf("pct exec %d -- bash -c '%s'", s.k3sVmid, strings.TrimSpace(stages.K3sInstallScript))
	if err := s.run(cmd, 900); err != nil {
		return fmt.Errorf("k3s install: %w", err)
	}
	cmd = fmt.Sprintf("pct exec %d -- bash -c '%s'", s.k3sVmid, strings.TrimSpace(stages.K3sLocalPathDurableScript))
	if err := s.run(cmd, 180); err != nil {
		return fmt.Errorf("k3s local-path: %w", err)
	}
	return nil
}

// ---- the edge cert on the CP (the box's F3 start/await pair, CP-side) ------
//
// Durable-reuse gate FIRST (read-only on the node, no LE order when a valid
// cert survives the durable mirror at /srv/data/k8s-volumes/caddy-edge/<slot>):
// a teardown+rebuild comes back on the same cert with no challenge and no
// rate-limit exposure — the PVC is seeded from the mirror. Only when no valid
// cert exists does issuance run: lego IN-PROCESS (the DNS-01 provider cred is
// sealed to the agent-tools identity under <stateDir>/world-secrets, opened in
// memory, resumable), the private key is sealed into the co-located runner
// package (which the box's own issuance already ships), and the install runs
// through the runner with the key requested BY NAME.

// durableFullchain reads a slot's fullchain from the durable-plane mirror
// (/srv/data/k8s-volumes/caddy-edge/<slot>) — a plain node file, readable
// BEFORE the Caddy edge exists (the cold-rebuild recovery gate).
func (s *deploySpec) durableFullchain(k3sVmid uint32, slot string) ([]byte, error) {
	out, err := s.runOut(fmt.Sprintf("pct exec %d -- bash -c 'test -s %s/fullchain.pem 2>/dev/null && base64 -w0 < %s/fullchain.pem'",
		k3sVmid, stages.CaddyEdgeDurableDir(slot), stages.CaddyEdgeDurableDir(slot)), 60)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("no durable mirror for %s", slot)
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(out))
}

// durableKeyPresent reports whether the durable mirror also holds a non-empty
// key.pem (a fullchain alone is not enough to serve TLS).
func (s *deploySpec) durableKeyPresent(k3sVmid uint32, slot string) bool {
	return s.run(fmt.Sprintf("pct exec %d -- bash -c 'test -s %s/key.pem'", k3sVmid, stages.CaddyEdgeDurableDir(slot)), 60) == nil
}

// seedCaddyCertFromDurable seeds a slot's Caddy PVC backing dir from the
// durable-plane mirror and restarts the edge. No private key transits a
// command or the audit — both files come from the node's own durable volume.
func (s *deploySpec) seedCaddyCertFromDurable(k3sVmid uint32, slot string) error {
	cmd := fmt.Sprintf(`set -e
pct exec %d -- bash -c '
set -e
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
PV=$($K get pvc caddy-data -n caddy -o jsonpath={.spec.volumeName})
PDIR=$($K get pv $PV -o jsonpath={.spec.local.path})
DIR=$PDIR/tls/%s
SRC=%s
mkdir -p "$DIR"
cp "$SRC/fullchain.pem" "$DIR/fullchain.pem"
cp "$SRC/key.pem" "$DIR/key.pem"
chmod 600 "$DIR/key.pem"
$K -n caddy rollout restart deploy/caddy >/dev/null 2>&1 || true
echo DURABLE_SEED_OK
'`, k3sVmid, slot, stages.CaddyEdgeDurableDir(slot))
	return s.run(cmd, 120)
}

// dnsCredFromStore opens the operator's DNS-01 provider credential for a slot
// from the CP's durable sealed store (<stateDir>/world-secrets/dns-<slot>.json)
// via the established cert.LoadCreds record (sealed to the agent-tools
// identity — written by the box build's hand-off). Errors loudly when no copy
// has been handed off yet (the ISSUE path needs it; the durable-reuse path
// does not).
func (s *deploySpec) dnsCredFromStore(slot string) (string, map[string]string, error) {
	path := filepath.Join(s.stateDir, "world-secrets", "dns-"+slot+".json")
	if !cert.CredExists(path) {
		return "", nil, fmt.Errorf("no DNS provider credential on the CP at %s — run `freehold build` to hand it off (or the durable-reuse path serves an existing cert)", path)
	}
	id, err := flows.LoadIdentity(s.stateDir)
	if err != nil {
		return "", nil, err
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return "", nil, err
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	return cert.LoadCreds(path, open, secret)
}

// issueCert runs the resumable DNS-01 issuance IN-PROCESS for one slot's host
// (lego via internal/cert; the sealed DNS cred opened in memory), so a re-run
// after a timeout RESUMES the same order instead of re-challenging.
func (s *deploySpec) issueCert(slot, host, provider string, env map[string]string) (*cert.Issued, error) {
	id, err := flows.LoadIdentity(s.stateDir)
	if err != nil {
		return nil, err
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return nil, err
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return nil, err
	}
	dp, err := cert.NewDNSProvider(provider, env)
	if err != nil {
		return nil, err
	}
	statePath := filepath.Join(s.stateDir, "world-secrets", "cert-pending-"+slot+".json")
	resume := &cert.Resume{
		Domain:   host,
		Provider: dp,
		Seal:     func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) },
		Open:     func(secret, aad, blob []byte) ([]byte, error) { return crypto.Open(secret, aad, blob) },
		SealPub:  pub,
		OpenSec:  secret,
		Path:     statePath,
	}
	po, ok, err := resume.TryLoad()
	if err != nil {
		return nil, err
	}
	if !ok {
		po, err = resume.Begin()
		if err != nil {
			return nil, err
		}
	}
	issued, err := resume.Resolve(po)
	if err != nil {
		return nil, err
	}
	return issued, nil
}

// installCaddyCertFile writes a slot's FRESH issued fullchain + key into the
// caddy-data PVC /data/tls/<slot> AND the durable mirror, then rolls caddy.
// The key is file-transited (sftp upload + pct push) — NEVER the runner's
// sealed cert-key-<slot>, which would be a STALE key mismatching the fresh
// fullchain (and the serving co-located runner cannot be restarted mid-call to
// reload a fresh seal). No credential crosses argv/audit.
func (s *deploySpec) installCaddyCertFile(k3sVmid uint32, slot string, fullchain, key []byte) error {
	mc, err := s.client()
	if err != nil {
		return err
	}
	fcTmp, err := os.CreateTemp("", "fh-fc-*")
	if err != nil {
		return err
	}
	keyTmp, err := os.CreateTemp("", "fh-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(fcTmp.Name())
	defer os.Remove(keyTmp.Name())
	if err := fcTmp.Close(); err != nil {
		return err
	}
	if err := keyTmp.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(fcTmp.Name(), fullchain, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(keyTmp.Name(), key, 0o600); err != nil {
		return err
	}
	if _, err := mc.Upload(s.runnerTarget, fcTmp.Name(), "/tmp/fh-fc-"+slot+".pem", 60); err != nil {
		return err
	}
	if _, err := mc.Upload(s.runnerTarget, keyTmp.Name(), "/tmp/fh-key-"+slot+".pem", 60); err != nil {
		return err
	}
	cmd := fmt.Sprintf(`set -e
pct push %d /tmp/fh-fc-%s.pem /tmp/fc-%s.pem
pct push %d /tmp/fh-key-%s.pem /tmp/key-%s.pem
pct exec %d -- sh -c '
set -e
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
PV=$($K get pvc caddy-data -n caddy -o jsonpath={.spec.volumeName})
PDIR=$($K get pv $PV -o jsonpath={.spec.local.path})
DIR=$PDIR/tls/%s
DUR=%s
mkdir -p "$DIR" "$DUR"
cp /tmp/fc-%s.pem "$DIR/fullchain.pem"
cp /tmp/key-%s.pem "$DIR/key.pem"
chmod 600 "$DIR/key.pem"
cp /tmp/fc-%s.pem "$DUR/fullchain.pem"
cp /tmp/key-%s.pem "$DUR/key.pem"
chmod 600 "$DUR/key.pem"
$K -n caddy rollout restart deploy/caddy >/dev/null 2>&1 || true
rm -f /tmp/fc-%s.pem /tmp/key-%s.pem
'
rm -f /tmp/fh-fc-%s.pem /tmp/fh-key-%s.pem
`,
		k3sVmid, slot, slot, k3sVmid, slot, slot, k3sVmid, // 1-7
		slot, stages.CaddyEdgeDurableDir(slot), // 8-9 (DIR tls/slot, DUR)
		slot, slot, slot, slot, slot, slot, slot, slot) // 10-17
	return s.run(cmd, 180)
}

// worldCert resolves each edge slot's cert CP-side: the durable-reuse gate
// (valid mirror => seed the PVC, no LE order) else an in-process resumable
// DNS-01 issue, then install the FRESH pair through the co-located runner with
// the key file-transited (never a stale runner-package key).
func (s *deploySpec) worldCert() error {
	for _, sl := range []struct{ slot, host string }{
		{"relay", s.relayHost}, {"cp", s.cpHost},
	} {
		if sl.host == "" {
			continue
		}
		// Durable-reuse gate: a valid cert on the durable mirror (>= 30d left)
		// means no LE order, no challenge, no rate-limit — seed the PVC from it.
		if fc, err := s.durableFullchain(s.k3sVmid, sl.slot); err == nil && s.durableKeyPresent(s.k3sVmid, sl.slot) {
			if _, ok := cert.ReuseIfValidBytes(fc, time.Now(), 30*24*time.Hour); ok {
				if err := s.seedCaddyCertFromDurable(s.k3sVmid, sl.slot); err != nil {
					return fmt.Errorf("cert %s durable seed: %w", sl.slot, err)
				}
				continue
			}
		}
		// Issue path: the sealed DNS cred must be on the CP (the box build's
		// hand-off ships it). The fresh fullchain+key pair is installed by
		// file-transit — no restart of the serving co-located runner.
		provider, env, err := s.dnsCredFromStore(sl.slot)
		if err != nil {
			return fmt.Errorf("cert %s: %w", sl.slot, err)
		}
		issued, err := s.issueCert(sl.slot, sl.host, provider, env)
		if err != nil {
			return fmt.Errorf("cert %s issue: %w", sl.slot, err)
		}
		if err := s.installCaddyCertFile(s.k3sVmid, sl.slot, issued.Fullchain, issued.Key); err != nil {
			return fmt.Errorf("cert %s install: %w", sl.slot, err)
		}
	}
	return nil
}

// ---- litellm on the CP (the two-leg box stageLitellm, CP-side) -------------
//
// Leg 1 (kube workloads, no operator secret): the master key + postgres pw are
// CP-generated (first-run-wins, the k8s Secrets are canonical across rebuilds;
// the master is read back for lockstep on reuse). The operator's provider key
// is NOT here — it rides the co-located runner package.
// Leg 2 (admin call, runner-decrypted): the model-registration curl runs
// THROUGH the co-located runner with the litellm master + provider key
// requested BY NAME — the runner decrypts, injects env, redacts output.

// runnerHasSecret reports whether the co-located runner package inside the cp
// LXC already carries a secret by name (the box build's stageLitellm seals the
// litellm master/postgres/provider into the proxmox-box package, and deploy-cp
// re-ships it into the guest — the runner loads it at boot).
func (s *deploySpec) runnerHasSecret(name string) bool {
	_, stateDir := s.cpGuestDirs()
	runnerDir := filepath.Join(stateDir, "runner", s.runnerTarget)
	out, err := s.runOut(fmt.Sprintf("pct exec %d -- sh -c \"grep -q '\\\"%s\\\"' %s/secrets.json 2>/dev/null && echo HAVE || echo NONE\"", s.cpLxc, name, runnerDir), 30)
	if err != nil {
		return false
	}
	return strings.Contains(out, "HAVE")
}

// worldLiteLLM brings the litellm gateway up CP-side (the box stageLitellm
// pair, driven through the co-located runner): apply the postgres + gateway
// kube workloads, register the model, and seed the CPA pod's litellm key —
// each exec requests its secrets BY NAME so the runner injects (and redacts)
// the values and the audited cmd carries only the $REF. The co-located runner
// package carries the litellm secrets (the box build's stageLitellm seals them
// into the proxmox-box package, which deploy-cp re-ships into the guest; the
// runner loads its package at boot — it is NEVER restarted mid-call, which
// would kill the very process serving this world_build). Fails loudly when a
// required secret is absent (no prior box build).
func (s *deploySpec) worldLiteLLM() error {
	gwURL := "http://" + s.litellmIP + ":31400"
	// The runner requires the target's own credential AND every requested name
	// to exist in its package — gate on presence so the failure is actionable.
	for _, name := range []string{"litellm", "postgres-pw", "provider-key"} {
		if !s.runnerHasSecret(name) {
			return fmt.Errorf("litellm needs secret %q in the co-located runner package and none is present — run `freehold build` to seed it (the box stageLitellm seals it, deploy-cp re-ships it)", name)
		}
	}
	// Leg 1: kube workloads — the k8s Secrets read the injected env by name
	// (first-run-wins; the `||` guard preserves the canonical values).
	if err := s.runSecrets(stages.LitellmManifestScript(s.k3sVmid), 420, "litellm", "postgres-pw"); err != nil {
		return fmt.Errorf("litellm kube apply: %w", err)
	}
	// Leg 2: register the model through the co-located runner (its own
	// ciphertext: master + provider-key) against the gateway's real NodePort.
	if err := s.runSecrets(stages.LitellmRegisterScript(gwURL, agent.CpaLiteLLMModel), 120, "litellm", "provider-key"); err != nil {
		return fmt.Errorf("litellm model registration: %w", err)
	}
	// The CPA's litellm key is the gateway master (the scoped-key follow-up
	// stands); seed the pod's Secret first-run-wins from the injected env.
	if err := s.runSecrets(agent.AgentLiteLLMKeyScript(s.k3sVmid, s.cpaName), 60, "litellm"); err != nil {
		return fmt.Errorf("seed CPA litellm key: %w", err)
	}
	return nil
}

// buildWorldApply returns the CP's world-build/reconcile driver: it runs the
// shared stage commands (internal/stages) through the co-located runner, so the
// box can "login + trigger" the CP to (re)assert the world. Each step is
// idempotent. Slice 2: re-assert the k3s durable local-path AND bring the relay
// compose stack up if not running — the two stateful bring-up stages the CP can
// own without any operator-side secret (k3s plans no secret; relay reconverge is
// plain compose in the durable deploy dir). caddy/dns/litellm/cert extend it
// toward the full CP-driven build.
func buildWorldApply(spec *deploySpec) agent.WorldApply {
	return func() (string, error) {
		var report []string
		// 1. The durable volume plane: re-ensure each tenant's dataset/LV onto
		// the recorded pool (idempotent, guest-writable) and capture the
		// born-at-create mounts. Runs FIRST — the LXC boots bake the mounts.
		var mounts map[planebase.Tenant][]planebase.MountSpec
		if spec.planePool != "" && spec.relayHost != "" {
			var err error
			mounts, err = spec.worldStorage()
			if err != nil {
				return "", fmt.Errorf("world-build storage: %w", err)
			}
			report = append(report, "durable plane ensured")
		}
		// 2. The relay LXC: boot if missing (baking the durable mounts at
		// create) + deploy the Buzz stack (idempotent compose bring-up).
		if spec.relayHost != "" {
			if err := spec.worldBootRelay(mounts[planebase.TenantRelay]); err != nil {
				return "", fmt.Errorf("world-build relay: %w", err)
			}
			report = append(report, "relay booted + stack deployed")
		}
		// 3. The k3s substrate: boot the LXC if missing, install k3s inside it,
		// and re-assert the durable local-path.
		if spec.k3sVmid != 0 {
			if err := spec.worldBootK3s(mounts[planebase.TenantK3sVolumes]); err != nil {
				return "", fmt.Errorf("world-build k3s: %w", err)
			}
			report = append(report, "k3s substrate ready")
		}
		// 3.5. Re-read the guests' CURRENT IPs (a DHCP re-lease changes an
		// address the recorded coords no longer match) — the DNS/caddy/litellm
		// steps below consume them. Best-effort.
		spec.refreshGuestIPs()
		// 4. The CP-owned resolver: register the split-horizon names (bare
		// guests + the dotted public hosts via the proxy) and point every guest
		// at the CP as its nameserver, then verify the resolver actually ANSWERS
		// (dnsmasq served the records, not merely tcp/53 open). No secrets.
		if spec.cpLxc != 0 && spec.cpIP != "" {
			if err := spec.worldDNS(); err != nil {
				return "", err
			}
			report = append(report, "dns register/point applied")
		}
		// 5. The litellm gateway (kube workloads + model registration through the
		// co-located runner) — CP-owned; the operator's provider key rides the
		// CP (hand-off) or the co-located runner package, never argv.
		if spec.k3sVmid != 0 && spec.litellmIP != "" {
			if err := spec.worldLiteLLM(); err != nil {
				return "", fmt.Errorf("world-build litellm: %w", err)
			}
			report = append(report, "litellm gateway live")
		}
		// 6. Caddy TLS edge re-apply (the relay + CP vhost fronts) when the edge
		// coords are recorded. No secrets (the Caddyfile is plain); cert
		// issuance/install remain a separate stage.
		if spec.k3sVmid != 0 && spec.relayHost != "" && spec.cpHost != "" && spec.relayIP != "" {
			relayUpstream := fmt.Sprintf("%s:3000", spec.relayIP)
			cpUpstream := ""
			if spec.cpIP != "" {
				cpUpstream = fmt.Sprintf("%s:8080", spec.cpIP)
			}
			caddyfile := deploy.RenderCaddyfile(spec.relayHost, relayUpstream, spec.cpHost, cpUpstream)
			// CaddyManifestScript is written to run ON THE PVE HOST (it wraps
			// pct push/pct exec itself), so spec.run executes it raw — never
			// wrapped in an outer pct exec (that would run the script inside
			// the guest, where pct doesn't exist).
			if err := spec.run(stages.CaddyManifestScript(spec.k3sVmid, deploy.CaddyManifest(caddyfile)), 180); err != nil {
				return "", fmt.Errorf("world-build caddy edge: %w", err)
			}
			report = append(report, "caddy TLS edge re-applied")
		}
		// 7. The edge certs: durable-reuse gate (no LE order when the durable
		// mirror has a valid cert) else an in-process resumable DNS-01 issue,
		// then install into the Caddy PVC through the co-located runner.
		if spec.k3sVmid != 0 && (spec.relayHost != "" || spec.cpHost != "") {
			if err := spec.worldCert(); err != nil {
				return "", fmt.Errorf("world-build cert: %w", err)
			}
			report = append(report, "cert issued/installed (or already present)")
		}
		if len(report) == 0 {
			return "", fmt.Errorf("world-build: no world coords recorded (k3s vmid / relay lxc)")
		}
		return strings.Join(report, "\n"), nil
	}
}

// buildCreateAgentFn returns the create-agent deploy: mint a durable identity
// on the CP, add it as a relay member, seat it in #freehold, apply its pod
// through the co-located runner, and hand the minted pubkey to Tools.CreateAgent
// buildMigrator wires the CP's verify-gated migration runner (Step 7): a
// durable ledger at <stateDir>/migrations.json (backed up with the CP plane),
// running pending idempotent migrations gated on their postcondition (🟢/🔴).
// First registered migration: the agent-tools durable registry must be a valid,
// loadable store. The CPA-prompt/config migrations land with the live-world
// build/upgrade, reusing this same runner.
func buildMigrator(spec *deploySpec) agent.Migrator {
	return func() ([]migrations.Result, error) {
		st, err := migrations.Open(filepath.Join(spec.stateDir, "migrations.json"))
		if err != nil {
			return nil, err
		}
		registryPath := filepath.Join(spec.stateDir, "registry.json")
		all := []migrations.Migration{{
			Name: "001-agent-tools-registry-loadable",
			Apply: func() error {
				_, err := agenttools.OpenRegistry(registryPath)
				return err
			},
			Verify: func() error {
				_, err := agenttools.OpenRegistry(registryPath)
				return err
			},
		}}
		return st.Run(all)
	}
}

// (which registers the registry row). Branches to the CPA manifest/prompt when
// the name is the CPA's, so stageCpa's dogfooded create_agent produces the CPA.
func buildCreateAgentFn(spec *deploySpec) agent.CreateAgentFn {
	return func(name, purpose string) (string, error) {
		if name == "" {
			return "", fmt.Errorf("create-agent needs a non-empty name")
		}
		// A DIFFERENT name that sanitizes to the CPA's pod would delete+reapply
		// the CPA's pod; the CPA's own name is allowed (stageCpa dogfoods
		// creating it). CIDR-less, pod-name collision guard only for others.
		if name != spec.cpaName && agent.PodName(name) == agent.PodName(spec.cpaName) {
			return "", fmt.Errorf("create-agent %q: the sanitized pod name collides with the control plane agent", name)
		}
		dir := filepath.Join(spec.stateDir, "agents", sanitizeDir(name))
		if _, err := agent.EnsureIdentity(dir); err != nil {
			return "", fmt.Errorf("mint %s identity: %w", name, err)
		}
		id, err := flows.LoadIdentity(dir)
		if err != nil {
			return "", fmt.Errorf("%s identity unreadable: %w", name, err)
		}
		pub, err := id.NostrPubkeyHex()
		if err != nil {
			return "", err
		}

		// Relay membership (relay-administered; the CP cannot self-add — runs
		// buzz-admin through the co-located runner into the relay LXC).
		cmdLine := fmt.Sprintf("cd %s && docker compose exec -T relay buzz-admin add-member --pubkey %s", spec.relayCompose, pub)
		full := fmt.Sprintf("pct exec %d -- sh -c '%s'", spec.relayLxc, cmdLine)
		if err := spec.run(full, 120); err != nil {
			return "", fmt.Errorf("add relay member %s: %w", pub, err)
		}

		// Profile + #freehold channel + join, signed by the agent.
		nSec, err := hex.DecodeString(id.NostrSecretHex)
		if err != nil {
			return "", err
		}
		if err := relay.PublishProfile(spec.relayURL, nSec, name, "freehold agent"); err != nil {
			return "", fmt.Errorf("publish %s profile: %w", name, err)
		}
		if err := delegate.EnsureChannel(spec.relayURL, nSec, relayFreeholdChannel, "#freehold"); err != nil {
			return "", fmt.Errorf("ensure #freehold channel: %w", err)
		}
		if err := relay.JoinChannel(spec.relayURL, nSec, relayFreeholdChannel); err != nil {
			return "", fmt.Errorf("join #freehold channel: %w", err)
		}

		if err := spec.run(agent.AgentIdentityScript(spec.k3sVmid, id.NostrSecretHex, spec.ownerPub, name), 120); err != nil {
			return "", fmt.Errorf("%s identity secret: %w", name, err)
		}
		var manifest string
		if name == spec.cpaName {
			manifest = agent.CPAManifestScript(spec.k3sVmid, spec.relayWS, prompts.CPASystemPrompt, name, spec.litellmBaseURL, "", spec.selfURL, spec.audience)
		} else {
			manifest = agent.AgentManifestScript(spec.k3sVmid, spec.relayWS, prompts.AgentSystemPrompt(name, purpose), spec.litellmBaseURL, agent.CpaLiteLLMModel, name, agent.KeySecretFor(spec.cpaName), spec.selfURL, spec.audience)
		}
		if err := spec.run(manifest, 420); err != nil {
			return "", fmt.Errorf("%s pod apply: %w", name, err)
		}
		// The CPA is the server's first-class caller: member it into this
		// server's own roster (the channel owner is this server, signing the
		// put-user with its secret) so its harness (via the mcp stdio bridge)
		// is authorized to call create/grant/manage — the same audited path the
		// build dogfoods. Idempotent on re-deploy.
		if name == spec.cpaName {
			if err := relay.PutUser(spec.relayURL, spec.sec, spec.audience, pub); err != nil {
				return "", fmt.Errorf("member CPA into the agent-tools roster: %w", err)
			}
		}
		return pub, nil
	}
}

// sanitizeDir turns an agent name into a filesystem-safe identity dir name.
func sanitizeDir(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
