// Command freehold-console is the Go port of the Rust control-plane binary:
// the CP's loopback admin/ops web console (NIP-98 operator auth when an admin
// whitelist is seeded) plus the CP CLI verbs the operator toolchain drives
// (serve/provision/grant/adopt/add-secret/identity). It replaces the Rust
// control-plane console crate at parity — same routes, same security guards
// (loopback-only until authn, the DNS-rebinding Origin guard, HttpOnly+
// SameSite=Strict session cookies, single-use portal tokens).
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/state"
	"freehold/control-plane/api/console"
	"freehold/control-plane/secret-management"
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
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (serve|provision|grant|adopt|add-secret|revoke|identity|services)\n", os.Args[1])
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
	srv := &console.Server{
		Store: store, ConsoleSecret: secret, ConsolePubkey: consolePK,
		Auth: auth, PublicOrigin: pubOrigin, RelayHost: *relayHost,
		StateDir: *stateDir, AgentToolsDir: *agentToolsStateDir,
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
	fs.Parse(args)
	if *stateDir == "" {
		return fmt.Errorf("identity needs --state-dir")
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
