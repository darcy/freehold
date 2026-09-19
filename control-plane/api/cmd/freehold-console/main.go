// Command freehold-console is the Go port of the Rust control-plane binary:
// the CP's loopback admin/ops web console (NIP-98 operator auth when an admin
// whitelist is seeded) plus the CP CLI verbs the operator toolchain drives
// (serve/provision/grant/adopt/add-secret/identity). It replaces the Rust
// control-plane console crate at parity — same routes, same security guards
// (loopback-only until authn, the DNS-rebinding Origin guard, HttpOnly+
// SameSite=Strict session cookies, single-use portal tokens).
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"freehold/contract/crypto"
	"freehold/control-plane/api/console"
	"freehold/control-plane/api/cpbuild"
	"freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "freehold-console <serve|provision|grant|adopt|add-secret|revoke|identity> …")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "provision":
		err = cmdProvision(os.Args[2:])
	case "grant":
		err = cmdGrant(os.Args[2:])
	case "adopt":
		err = cmdAdopt(os.Args[2:])
	case "add-secret":
		err = cmdAddSecret(os.Args[2:])
	case "revoke":
		err = cmdRevoke(os.Args[2:])
	case "identity":
		err = cmdIdentity(os.Args[2:])
	case "services":
		err = cmdServices(os.Args[2:])
	case "dns":
		err = cmdDNS(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (serve|provision|grant|adopt|add-secret|revoke|identity|services|dns)\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "CP state dir (state.json + console identity)")
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP bind address")
	adminPubkeys := fs.String("admin-pubkeys", "", "comma-separated operator pubkeys (64-hex) — seeds NIP-98 console auth; empty = loopback-only posture")
	relayURL := fs.String("relay-url", "", "relay scope (recorded in state.json)")
	relayPubkey := fs.String("relay-pubkey", "", "relay signing pubkey (roster trust anchor)")
	relayHost := fs.String("relay-host", "", "relay community host")
	publicOrigin := fs.String("public-origin", "", "the console's fronted public origin (DNS-rebinding guard C3.5)")
	agentToolsURL := fs.String("agent-tools-url", "", "agent-tools MCP server URL (served on /api/world)")
	agentToolsPubkey := fs.String("agent-tools-pubkey", "", "agent-tools server pubkey (served on /api/world)")
	// The default is the sibling dir the build populates: the toolset's durable
	// home (registry.json + facts.json), which /api/world serves as the world's
	// single status source.
	agentToolsStateDir := fs.String("agent-tools-state-dir", "/srv/data/cp/agent-tools", "agent-tools durable state dir (authoritative agent registry + world facts)")
	// The CP build executor (world-build): the co-located runner + the world
	// coordinates the console needs to bring the world up CP-side. Absent =>
	// the console serves status/ops only (no /api/world-build).
	runnerAddr := fs.String("runner-addr", "", "co-located runner MCP addr (world_build drives it)")
	runnerPK := fs.String("runner-pubkey", "", "co-located runner pubkey (deploy exec audience)")
	runnerTarget := fs.String("runner-target", "", "co-located runner target (reachable box)")
	relayIP := fs.String("relay-ip", "", "relay LXC LAN IP (world coords)")
	relayWS := fs.String("relay-ws", "", "relay wss:// origin (agent pods)")
	cpHost := fs.String("cp-host", "", "control plane public host")
	cpIP := fs.String("cp-ip", "", "cp LXC LAN IP")
	proxyIP := fs.String("proxy-ip", "", "proxy/k3s node static IP")
	litellmIP := fs.String("litellm-ip", "", "litellm gateway node IP")
	cpLxc := fs.Uint("cp-lxc", 0, "cp LXC vmid (the resolver lives here)")
	relayLxc := fs.Uint("relay-lxc", 0, "relay LXC vmid")
	k3sVmid := fs.Uint("k3s-vmid", 0, "k3s LXC vmid")
	planePool := fs.String("plane-pool", "", "durable-plane backend pool (VG or zpool)")
	planeKind := fs.String("plane-kind", "", "recorded backend kind (zfs|lvmth)")
	thinPool := fs.String("thin-pool", "", "the freehold-CREATED thin pool")
	var sizeGB, poolSizeGB uint64
	fs.Uint64Var(&sizeGB, "size-gb", 10, "per-tenant LV size GB")
	fs.Uint64Var(&poolSizeGB, "pool-size-gb", 40, "thin-pool size GB")
	rootfsGB := fs.Uint("rootfs-gb", 16, "LXC rootfs size GB")
	memoryMB := fs.Uint("memory-mb", 2048, "LXC memory MB")
	storageName := fs.String("storage", "local-lvm", "PVE LXC storage")
	relayGW := fs.String("relay-gw", "192.168.30.1", "gateway for the k3s STATIC guest IP")
	bridge := fs.String("bridge", "vmbr0", "PVE LXC bridge")
	relayCompose := fs.String("relay-compose", "/srv/data/relay/deploy/compose", "relay compose dir")
	cpaName := fs.String("cpa-name", "", "CPA display name")
	ownerPub := fs.String("owner-pubkey", "", "operator pubkey (agent identity secret owner)")
	litellmBase := fs.String("litellm-base", "", "litellm gateway base URL")
	selfURL := fs.String("self-url", "", "console reachable HTTP base URL")
	// The box hands the full world config to the console as one JSON blob
	// (cpbuild.Coords) — the authoritative source for the CP build executor;
	// the individual --* flags above are the manual/spelled-out equivalent.
	worldConfig := fs.String("world-config", "", "cpbuild.Coords JSON (the world config handed to the console at deploy)")
	fs.Parse(args)

	if *stateDir == "" {
		return fmt.Errorf("serve needs --state-dir")
	}
	store, err := state.Open(*stateDir)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}

	// Admin whitelist: seeded on first serve, persisted in state.json.
	var admins []string
	if *adminPubkeys != "" {
		for _, pk := range strings.Split(*adminPubkeys, ",") {
			pk = strings.TrimSpace(pk)
			if pk == "" {
				continue
			}
			if !isHex64(pk) {
				return fmt.Errorf("--admin-pubkeys entries must be 64-hex Nostr pubkeys (got %q)", pk)
			}
			admins = append(admins, pk)
		}
		if len(admins) > 0 {
			store.SetAdmins(admins)
			if err := store.Save(); err != nil {
				return fmt.Errorf("persist admins: %w", err)
			}
		}
	} else {
		admins = store.Admins()
	}

	// Relay / agent-tools scope (recorded, served on /api/world).
	if *relayURL != "" {
		if err := store.SetRelayURL(strPtr(*relayURL)); err != nil {
			return err
		}
	}
	if *relayHost != "" {
		if err := store.SetRelayHost(strPtr(*relayHost)); err != nil {
			return err
		}
	}
	if *relayPubkey != "" {
		if !isHex64(*relayPubkey) {
			return fmt.Errorf("--relay-pubkey must be a 64-hex Nostr pubkey (got %q)", *relayPubkey)
		}
		if err := store.SetRelayPubkey(strPtr(*relayPubkey)); err != nil {
			return err
		}
	}
	if *agentToolsURL != "" {
		if err := store.SetAgentToolsURL(strPtr(*agentToolsURL)); err != nil {
			return err
		}
	}
	if *agentToolsPubkey != "" {
		if !isHex64(*agentToolsPubkey) {
			return fmt.Errorf("--agent-tools-pubkey must be a 64-hex Nostr pubkey (got %q)", *agentToolsPubkey)
		}
		if err := store.SetAgentToolsPubkey(strPtr(*agentToolsPubkey)); err != nil {
			return err
		}
	}

	// The console identity (minted on the box at first serve, 0600 — a keypair
	// is NEVER shipped; deploy-cp reads the pubkey back to relay-member it).
	secret, consolePK, err := console.EnsureConsoleIdentity(*stateDir)
	if err != nil {
		return err
	}

	// Bind guard: unauthenticated console stays loopback-only (C3).
	var auth *console.Auth
	if len(admins) > 0 {
		auth = console.NewAuth(admins)
		log.Printf("console auth enabled (NIP-98, %d operators) — non-loopback bind allowed", len(admins))
	} else {
		if err := console.ValidateLoopbackBind(*addr); err != nil {
			return err
		}
	}

	var pubOrigin *string
	if *publicOrigin != "" {
		pubOrigin = strPtr(*publicOrigin)
	}
	// The CP build executor: the console drives the shared cpbuild engine
	// through the co-located runner, signed as ITS OWN identity (now granted on
	// the runner at deploy). The world coords ride here so a thin box can
	// trigger /api/world-build without a box-one or agent-tools dependency.
	var builder *cpbuild.Spec
	if *worldConfig != "" {
		raw, derr := base64.StdEncoding.DecodeString(*worldConfig)
		if derr != nil {
			raw = []byte(*worldConfig) // a plain JSON string is also accepted
		}
		var c cpbuild.Coords
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("world-config not valid cpbuild coords JSON: %w", err)
		}
		builder = cpbuild.NewSpec(c, secret, consolePK)
		// The Spec drives identity / migrations / world-secrets / the CP guest
		// dirs, all relative to the CONSOLE's own state dir — the world-config
		// producer (rebuild) doesn't know the console's run-time dir, so the
		// serve always wins with its authoritative *stateDir.
		builder.StateDir = *stateDir
	} else if *runnerAddr != "" {
		builder = &cpbuild.Spec{
			StateDir:       *stateDir,
			RelayURL:       *relayURL,
			RelayAuthURL:   *relayURL,
			RelayWS:        *relayWS,
			RelayHost:      *relayHost,
			RelayIP:        *relayIP,
			CpHost:         *cpHost,
			CpIP:           *cpIP,
			CpLxc:          uint32(*cpLxc),
			ProxyIP:        *proxyIP,
			LitellmIP:      *litellmIP,
			PlanePool:      *planePool,
			PlaneKind:      *planeKind,
			ThinPool:       *thinPool,
			SizeGB:         sizeGB,
			PoolSizeGB:     poolSizeGB,
			RootfsGB:       uint32(*rootfsGB),
			MemoryMB:       uint32(*memoryMB),
			StorageName:    *storageName,
			RelayGW:        *relayGW,
			Bridge:         *bridge,
			RelayLxc:       uint32(*relayLxc),
			RelayCompose:   *relayCompose,
			K3sVmid:        uint32(*k3sVmid),
			RunnerAddr:     *runnerAddr,
			RunnerPK:       *runnerPK,
			RunnerTarget:   *runnerTarget,
			CpaName:        *cpaName,
			OwnerPub:       *ownerPub,
			LitellmBaseURL: *litellmBase,
			SelfURL:        strings.TrimSuffix(*selfURL, "/"),
			Sec:            secret,
			Audience:       consolePK,
		}
	}
	// Mint/reconcile agent identities into the agent-tools durable state dir
	// (the same root the agent-tools server uses), never the console's own dir —
	// otherwise a CP-side reconcile would mint a fresh keypair and orphan every
	// grant on the surviving identity.
	if builder != nil {
		builder.AgentIdentityDir = *agentToolsStateDir
	}
	srv := &console.Server{
		Store: store, ConsoleSecret: secret, ConsolePubkey: consolePK,
		Auth: auth, PublicOrigin: pubOrigin, RelayHost: *relayHost,
		StateDir: *stateDir, AgentToolsDir: *agentToolsStateDir,
		Builder: builder,
	}
	log.Printf("freehold-console (Go) serving on %s (console agent %s)", *addr, consolePK)
	return http.ListenAndServe(*addr, srv)
}

func cmdRevoke(args []string) error {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	relayURL := fs.String("relay-url", "", "relay to cut the runner off on")
	stateDir := fs.String("state-dir", "", "CP state dir")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	if name == "" || *stateDir == "" {
		return fmt.Errorf("revoke <name> --state-dir")
	}
	store, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	rec, err := provisioner.RevokeRunner(store, name)
	if err != nil {
		return err
	}
	if *relayURL != "" {
		if err := provisioner.RevokeRunnerChannel(store, *relayURL, name, *stateDir); err != nil {
			return err
		}
		fmt.Printf("revoked %s on the relay (%s)\n", name, *relayURL)
	}
	fmt.Printf("revoked runner %s (was %s)\n", name, rec.NostrPubkey)
	return nil
}

func cmdIdentity(args []string) error {
	fs := flag.NewFlagSet("identity", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "CP state dir")
	enc := fs.Bool("enc-pubkey", false, "print the console identity's X25519 ENCRYPTION pubkey (the DNS-handoff + cert seal audience)")
	fs.Parse(args)
	if *stateDir == "" {
		return fmt.Errorf("identity needs --state-dir")
	}
	if *enc {
		encPub, err := console.ConsoleEncPubkey(*stateDir)
		if err != nil {
			return err
		}
		fmt.Println(encPub)
		return nil
	}
	_, pk, err := console.EnsureConsoleIdentity(*stateDir)
	if err != nil {
		return err
	}
	fmt.Println(pk)
	return nil
}

// cmdServices registers the world-services health registry (k3s/litellm/caddy
// coords) directly into state.json — the inside-the-CP channel the build's
// worldServices step calls, mirroring how `dns add` writes DNS records. The Go
// console then probes them co-located and serves them on /api/world.
func cmdServices(args []string) error {
	fs := flag.NewFlagSet("services", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "CP state dir")
	kind := fs.String("kind", "", "service kind (k3s|litellm|caddy)")
	url := fs.String("url", "", "probe target URL")
	reachHost := fs.String("reach-host", "", "optional reachability host (caddy edge)")
	clear := fs.Bool("clear", false, "drop the whole registry (teardown/bookkeeping)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stateDir == "" {
		return fmt.Errorf("services needs --state-dir")
	}
	store, err := state.Open(*stateDir)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	if *clear {
		for name := range store.Snapshot().Services {
			store.RemoveService(name)
		}
		if err := store.Save(); err != nil {
			return fmt.Errorf("clear services: %w", err)
		}
		fmt.Println("services registry cleared")
		return nil
	}
	if *kind == "" || *url == "" {
		return fmt.Errorf("services set needs --kind and --url")
	}
	if *kind != "k3s" && *kind != "litellm" && *kind != "caddy" {
		return fmt.Errorf("unknown service kind %q (k3s|litellm|caddy)", *kind)
	}
	store.InsertService(*kind, state.WorldService{
		Kind: *kind, URL: *url, ReachHost: *reachHost, CreatedAt: uint64(time.Now().Unix()),
	})
	if err := store.Save(); err != nil {
		return fmt.Errorf("save services: %w", err)
	}
	fmt.Printf("service %s -> %s registered\n", *kind, *url)
	return nil
}

// cmdDNS implements `dns add <name> <ip> <source>` inside the CP — the direct
// state-write + resolver-reload the world-build DNS step calls (the Go console's
// `control-plane dns add` equivalent). Writes the record into state.json, renders
// the dnsmasq addn-hosts + conf, and reloads dnsmasq in-process.
func cmdDNS(args []string) error {
	fs := flag.NewFlagSet("dns", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "CP state dir")
	domain := fs.String("domain", "", "search base (adds <name>.<domain>)")
	apex := fs.String("apex", "", "resolver wildcard apex (all subdomains of apex -> --ip)")
	apexIP := fs.String("ip", "", "resolver wildcard target IP (the proxy/Caddy edge)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("dns <add NAME IP SOURCE|apex --apex BASE --ip PROXY> [--state-dir DIR]")
	}
	store, err := state.Open(*stateDir)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	switch rest[0] {
	case "apex":
		if *apex == "" || *apexIP == "" {
			return fmt.Errorf("dns apex needs --apex BASE --ip PROXY")
		}
		if err := store.SetResolverWildcard(&state.DnsWildcard{
			Apex: *apex, IP: *apexIP, Source: "world-build dns apex", CreatedAt: uint64(time.Now().Unix()),
		}); err != nil {
			return fmt.Errorf("set resolver wildcard: %w", err)
		}
		return syncDNS(store, *stateDir, *domain)
	case "add":
		if len(rest) < 4 {
			return fmt.Errorf("dns add <name> <ip> <source> [--domain <base>] [--state-dir DIR]")
		}
		name, ip, source := rest[1], rest[2], rest[3]
		if _, err := console.Upsert(store, name, ip, source); err != nil {
			return err
		}
		if err := syncDNS(store, *stateDir, *domain); err != nil {
			return err
		}
		fmt.Printf("dns %s -> %s\n", name, ip)
		return nil
	default:
		return fmt.Errorf("dns: unknown verb %q (add|apex)", rest[0])
	}
}

// syncDNS writes the dnsmasq addn-hosts + conf (with the resolver wildcard when
// set) and reloads it in-process.
func syncDNS(store *state.StateStore, stateDir, domain string) error {
	snap := store.Snapshot()
	rd := domain
	if rd == "" && snap.ResolverDomain != nil {
		rd = *snap.ResolverDomain
	}
	var apex, wid *string
	if snap.ResolverWildcard != nil {
		apex = &snap.ResolverWildcard.Apex
		wid = &snap.ResolverWildcard.IP
	}
	write := func(path, body string) error { return os.WriteFile(path, []byte(body), 0o644) }
	reload := func() error { return console.ReloadDnsmasq(stateDir) }
	return console.SyncResolver(stateDir, snap.DNS, &rd, apex, wid, write, reload)
}

func cmdProvision(args []string) error {
	fs := flag.NewFlagSet("provision", flag.ExitOnError)
	kind := fs.String("kind", "", "connector kind")
	address := fs.String("address", "", "target address")
	secretEnv := fs.String("secret-env", "", "read the credential from this env var")
	var grants multiFlag
	fs.Var(&grants, "grant", "agent pubkey granted to call the runner (repeatable)")
	runnerDir := fs.String("runner-dir", "", "where the runner package lands")
	risk := fs.String("risk", "", "runner risk class override")
	relayURL := fs.String("relay-url", "", "relay to sync the runner channel to")
	stateDir := fs.String("state-dir", "", "CP state dir")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	if name == "" || *stateDir == "" {
		return fmt.Errorf("provision <name> --kind --address --state-dir")
	}
	store, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	if *runnerDir == "" {
		*runnerDir = "./.freehold/runner/" + name
	}
	// SSH runners: GENERATE the runner's keypair (nobody pastes an existing
	// key); the operator installs the PUBLIC half. Other kinds: the operator
	// pastes the API credential once (via --secret-env, never argv).
	var secret []byte
	var generatedPub string
	switch {
	case *secretEnv != "":
		v, ok := os.LookupEnv(*secretEnv)
		if !ok {
			return fmt.Errorf("--secret-env %s is not set", *secretEnv)
		}
		secret = []byte(v)
	case *kind == "ssh":
		priv, pub, err := crypto.GenerateSSHKeypair(name)
		if err != nil {
			return err
		}
		secret = priv
		generatedPub = pub
	default:
		return fmt.Errorf("provision of kind %q needs --secret-env (headless) — paste the credential via the console/CLI", *kind)
	}
	res, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: name, Kind: *kind, Address: *address, Secret: secret, RunnerDir: *runnerDir,
		Grants: grants, RiskLevel: optStr(*risk),
	})
	if err != nil {
		return err
	}
	fmt.Printf("provisioned runner %s (active)\n", res.Name)
	fmt.Printf("  nostr pubkey:      %s\n", res.NostrPubkey)
	fmt.Printf("  encryption pubkey: %s\n", res.EncPubkey)
	fmt.Printf("  package:           %s\n", res.PackageDir)
	if generatedPub != "" {
		fmt.Printf("  PUBLIC KEY — add this line to %s's ~/.ssh/authorized_keys:\n  %s\n", *address, generatedPub)
		fmt.Println("  (the private half is the sealed credential — it never leaves this box)")
	}
	if *relayURL != "" {
		if err := provisioner.SyncRunnerChannel(store, *relayURL, name, *stateDir); err != nil {
			return err
		}
		fmt.Printf("synced runner channel on the relay (%s)\n", *relayURL)
	}
	return nil
}

func cmdGrant(args []string) error {
	fs := flag.NewFlagSet("grant", flag.ExitOnError)
	pubkey := fs.String("pubkey", "", "agent pubkey; omitted = the state dir's own console/ops identity")
	relayURL := fs.String("relay-url", "", "relay to publish the grant to")
	stateDir := fs.String("state-dir", "", "CP state dir")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	if name == "" || *stateDir == "" {
		return fmt.Errorf("grant <name> --state-dir")
	}
	store, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	pk := *pubkey
	if pk == "" {
		// The state dir's own identity (the common first-run grant needs no
		// argument) — mirrors resolve_grant_pubkey defaulting to agent-ops.
		_, pub, err := console.EnsureConsoleIdentity(*stateDir)
		if err != nil {
			return err
		}
		pk = pub
	}
	grants, err := provisioner.GrantAgent(store, name, pk)
	if err != nil {
		return err
	}
	if *relayURL != "" {
		if err := provisioner.PutUserMembership(store, *relayURL, name, pk, *stateDir); err != nil {
			return err
		}
	}
	fmt.Printf("runner %s grants: %d\n", name, len(grants))
	return nil
}

func cmdAdopt(args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ExitOnError)
	kind := fs.String("kind", "", "connector kind")
	address := fs.String("address", "", "target address")
	packageDir := fs.String("package-dir", "", "the runner's existing package dir")
	mcpAddr := fs.String("mcp-addr", "", "the runner's MCP listen address")
	relayURL := fs.String("relay-url", "", "relay to sync the runner channel to")
	stateDir := fs.String("state-dir", "", "CP state dir")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	if name == "" || *stateDir == "" || *packageDir == "" {
		return fmt.Errorf("adopt <name> --kind --address --package-dir --state-dir")
	}
	store, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	if _, err := provisioner.AdoptRunner(store, name, *kind, *address, *packageDir, optStr(*mcpAddr), nil); err != nil {
		return err
	}
	if *relayURL != "" {
		if err := provisioner.SyncRunnerChannel(store, *relayURL, name, *stateDir); err != nil {
			return err
		}
	}
	fmt.Printf("adopted runner %s (active)\n", name)
	return nil
}

func cmdAddSecret(args []string) error {
	fs := flag.NewFlagSet("add-secret", flag.ExitOnError)
	secretEnv := fs.String("secret-env", "", "read the value from this env var")
	stateDir := fs.String("state-dir", "", "CP state dir")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	runner := ""
	if len(pos) > 0 {
		runner = pos[0]
	}
	name := ""
	if len(pos) > 1 {
		name = pos[1]
	}
	if runner == "" || name == "" || *stateDir == "" {
		return fmt.Errorf("add-secret <runner> <name> --state-dir")
	}
	if *secretEnv == "" {
		return fmt.Errorf("add-secret needs --secret-env (headless)")
	}
	v, ok := os.LookupEnv(*secretEnv)
	if !ok {
		return fmt.Errorf("--secret-env %s is not set", *secretEnv)
	}
	store, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	if _, err := provisioner.AddSecret(store, runner, name, []byte(v)); err != nil {
		return err
	}
	fmt.Printf("added extra secret %s to runner %s (sealed to the runner's key)\n", name, runner)
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// parseFlags parses a flag set whose flags may appear AFTER positional args
// (the box's call shape is `provision <name> --kind ...`). Go's flag package
// stops at the first non-flag, so reorder: extract `--flag [value]` tokens
// (all our flags take a value) into the flag list, bare words into positionals.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
		} else {
			pos = append(pos, a)
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	return pos, nil
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func strPtr(s string) *string { return &s }
