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
	"freehold/orchestrator/internal/crypto"
	"freehold/orchestrator/internal/delegate"
	"freehold/orchestrator/internal/flows"
	"freehold/orchestrator/internal/relay"
	"freehold/orchestrator/prompts"
)

const relayFreeholdChannel = "00000000-0000-4000-8000-00000000f0ef"

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		// buzz-agent spawns the MCP server via BUZZ_ACP_MCP_COMMAND with NO
		// subcommand and empty args (build_mcp_servers sets args=[]). When
		// stdin is a pipe (a harness driving us as the stdio bridge), run the
		// `mcp` bridge; only a real terminal means the operator forgot a
		// subcommand and should see usage instead.
		if !term.IsTerminal(os.Stdin.Fd()) {
			cmdMCP(nil)
			return
		}
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

func (s *deploySpec) run(cmd string, timeoutS uint64) error {
	mc, err := s.client()
	if err != nil {
		return err
	}
	out, err := mc.Exec(s.runnerTarget, cmd, []string{s.runnerTarget}, timeoutS)
	if err != nil {
		return err
	}
	if !out.TimedOut && out.ExitCode != nil && *out.ExitCode == 0 {
		return nil
	}
	ec := -1
	if out.ExitCode != nil {
		ec = *out.ExitCode
	}
	return fmt.Errorf("runner exec failed (timed=%v exit=%d): %s %s", out.TimedOut, ec, strings.TrimSpace(out.Stdout), strings.TrimSpace(out.Stderr))
}

// buildCreateAgentFn returns the create-agent deploy: mint a durable identity
// on the CP, add it as a relay member, seat it in #freehold, apply its pod
// through the co-located runner, and hand the minted pubkey to Tools.CreateAgent
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
