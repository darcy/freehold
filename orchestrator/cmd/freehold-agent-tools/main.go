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
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/term"

	"freehold/orchestrator/internal/agent"
	"freehold/orchestrator/internal/agenttools"
	"freehold/orchestrator/internal/client"
	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/crypto"
	"freehold/orchestrator/internal/delegate"
	"freehold/orchestrator/internal/deploy"
	"freehold/orchestrator/internal/flows"
	"freehold/orchestrator/internal/migrations"
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

// ---- litellm on the CP (the two-leg box stageLitellm, CP-side) -------------
//
// Leg 1 (kube workloads, no operator secret): the master key + postgres pw are
// CP-generated (first-run-wins, the k8s Secrets are canonical across rebuilds;
// the master is read back for lockstep on reuse). The operator's provider key
// is NOT here — it rides the co-located runner package.
// Leg 2 (admin call, runner-decrypted): the model-registration curl runs
// THROUGH the co-located runner with the litellm master + provider key
// requested BY NAME — the runner decrypts, injects env, redacts output.

// litellmMasterKey resolves the gateway's ACTUAL admin master key: the
// canonical copy is the first-run-wins k8s `litellm-keys` Secret, so on a
// reuse run we read it back (a re-minted master would diverge from the running
// gateway and every admin call would 401). Returns "" only when the Secret
// doesn't exist yet (a fresh run mints it).
func (s *deploySpec) litellmMasterKey() string {
	out, err := s.runOut(fmt.Sprintf("pct exec %d -- /usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get secret litellm-keys -n litellm -o jsonpath='{.data.master-key}' 2>/dev/null | base64 -d", s.k3sVmid), 30)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// providerKeyFromStore opens the operator's litellm provider key from the CP's
// durable sealed store (<stateDir>/world-secrets/litellm-provider.json, sealed
// to the agent-tools identity — written by the box build's hand-off). Returns
// "" when no copy has been handed off yet.
func (s *deploySpec) providerKeyFromStore() (string, error) {
	path := filepath.Join(s.stateDir, "world-secrets", "litellm-provider.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var st struct {
		Sealed string `json:"sealed"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", err
	}
	blob, err := hex.DecodeString(st.Sealed)
	if err != nil {
		return "", err
	}
	id, err := flows.LoadIdentity(s.stateDir)
	if err != nil {
		return "", err
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return "", err
	}
	plain, err := crypto.Open(secret, []byte("litellm-provider"), blob)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// runnerHasSecret reports whether the co-located runner package inside the cp
// LXC already carries a secret by name (a reuse skip — it must not be clobbered
// with an empty re-seal).
func (s *deploySpec) runnerHasSecret(name string) bool {
	_, stateDir := s.cpGuestDirs()
	runnerDir := filepath.Join(stateDir, "runner", s.runnerTarget)
	out, err := s.runOut(fmt.Sprintf("pct exec %d -- sh -c \"grep -q '\\\"%s\\\"' %s/secrets.json 2>/dev/null && echo HAVE || echo NONE\"", s.cpLxc, name, runnerDir), 30)
	if err != nil {
		return false
	}
	return strings.Contains(out, "HAVE")
}

// addRunnerSecretFile seals a plaintext value into the co-located runner
// package WITHOUT the value crossing argv/audit (the operator's provider key
// AND the CP-generated master/postgres — no credential is ever a shell literal
// in the audited command): write a temp file on the CP, sftp-upload to the
// box, pct push into the guest, add-secret from a shell-read env var, then
// clean up both temps.
func (s *deploySpec) addRunnerSecretFile(name string, plain []byte) error {
	binDir, stateDir := s.cpGuestDirs()
	mc, err := s.client()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "fh-sec-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	if err := os.WriteFile(tmpPath, plain, 0o600); err != nil {
		return err
	}
	if _, err := mc.Upload(s.runnerTarget, tmpPath, "/tmp/fh-sec", 60); err != nil {
		return err
	}
	inner := fmt.Sprintf("V=$(cat /tmp/fh-sec); export V; %s/control-plane add-secret %s %s --state-dir %s --secret-env V; rm -f /tmp/fh-sec",
		binDir, s.runnerTarget, name, stateDir)
	cmd := fmt.Sprintf("pct push %d /tmp/fh-sec /tmp/fh-sec && pct exec %d -- sh -c '%s' && rm -f /tmp/fh-sec", s.cpLxc, s.cpLxc, inner)
	return s.run(cmd, 90)
}

// restartCoLocatedRunner restarts the CP's co-located runner and waits for it
// to be active: it loads its package (the freshly-sealed litellm secrets) at
// boot only, so a register exec after add-secret needs the restart.
func (s *deploySpec) restartCoLocatedRunner() error {
	cmd := fmt.Sprintf("pct exec %d -- sh -c 'systemctl restart freehold-runner 2>/dev/null; for i in $(seq 1 15); do systemctl is-active freehold-runner >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1'", s.cpLxc)
	return s.run(cmd, 60)
}

// worldLiteLLM brings the litellm gateway up CP-side (the box stageLitellm
// pair, driven through the co-located runner): seal the master + postgres pw +
// provider key into the co-located runner package (all file-transit, never
// argv/audit), restart it so the package reloads, then apply the postgres +
// gateway kube workloads, register the model, and seed the CPA pod's litellm
// key — each exec requests its secrets BY NAME so the runner injects (and
// redacts) the values. Fails loudly when no provider key is on the CP yet (the
// box build's hand-off ships it).
func (s *deploySpec) worldLiteLLM() error {
	gwURL := "http://" + s.litellmIP + ":31400"
	masterKey := s.litellmMasterKey()
	if masterKey == "" {
		masterKey = stages.GenSecretHex()
	}
	postgresPw := stages.GenSecretHex()
	providerKey, err := s.providerKeyFromStore()
	if err != nil {
		return fmt.Errorf("litellm provider key: %w", err)
	}
	hasProvider := providerKey != "" || s.runnerHasSecret("provider-key")
	if !hasProvider {
		return fmt.Errorf("litellm needs the provider key on the CP and none is present: no sealed copy at %s and none in the co-located runner — run `freehold build` to hand it off (or seal it via control-plane add-secret %s provider-key)",
			filepath.Join(s.stateDir, "world-secrets", "litellm-provider.json"), s.runnerTarget)
	}
	// Seal the co-located runner's package: master + postgres every run (the
	// canonical values the k8s Secrets were seeded with), provider key once
	// (only when handed off this run). All file-transit.
	if err := s.addRunnerSecretFile("litellm", []byte(masterKey)); err != nil {
		return fmt.Errorf("seal litellm master: %w", err)
	}
	if err := s.addRunnerSecretFile("postgres-pw", []byte(postgresPw)); err != nil {
		return fmt.Errorf("seal litellm postgres pw: %w", err)
	}
	if providerKey != "" {
		if err := s.addRunnerSecretFile("provider-key", []byte(providerKey)); err != nil {
			return fmt.Errorf("seal litellm provider key: %w", err)
		}
	}
	if err := s.restartCoLocatedRunner(); err != nil {
		return fmt.Errorf("co-located runner restart: %w", err)
	}
	// Leg 1: kube workloads — the k8s Secrets read the injected env by name.
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
		// 1. k3s durable local-path re-assert (idempotent).
		if spec.k3sVmid != 0 {
			cmd := fmt.Sprintf("pct exec %d -- bash -c '%s'", spec.k3sVmid, strings.TrimSpace(stages.K3sLocalPathDurableScript))
			if err := spec.run(cmd, 180); err != nil {
				return "", fmt.Errorf("world-build k3s local-path: %w", err)
			}
			report = append(report, "k3s durable local-path re-asserted")
		}
		// 2. Relay compose stack reconverge (up-if-not-running, idempotent).
		// set -o pipefail so the pipeline's exit is docker compose's (not tail's),
		// else a failing bring-up would still echo RELAY_COMPOSE_OK and
		// spec.run would report success falsely.
		if spec.relayLxc != 0 && spec.relayCompose != "" {
			cmd := fmt.Sprintf("pct exec %d -- bash -c 'set -o pipefail; cd %s && docker compose up -d --no-recreate 2>&1 | tail -3 && echo RELAY_COMPOSE_OK'",
				spec.relayLxc, spec.relayCompose)
			if err := spec.run(cmd, 240); err != nil {
				return "", fmt.Errorf("world-build relay compose: %w", err)
			}
			report = append(report, "relay compose reconverged")
		}
		// 3. The CP-owned resolver: register the split-horizon names (bare
		// guests + the dotted public hosts via the proxy) and point every guest
		// at the CP as its nameserver, then verify the resolver actually ANSWERS
		// (dnsmasq served the records, not merely tcp/53 open). No secrets.
		if spec.cpLxc != 0 && spec.cpIP != "" {
			if err := spec.worldDNS(); err != nil {
				return "", err
			}
			report = append(report, "dns register/point applied")
		}
		// 4. The litellm gateway (kube workloads + model registration through the
		// co-located runner) — CP-owned; the operator's provider key rides the
		// CP (hand-off) or the co-located runner package, never argv.
		if spec.k3sVmid != 0 && spec.litellmIP != "" {
			if err := spec.worldLiteLLM(); err != nil {
				return "", fmt.Errorf("world-build litellm: %w", err)
			}
			report = append(report, "litellm gateway live")
		}
		// 5. Caddy TLS edge re-apply (the relay + CP vhost fronts) when the edge
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
