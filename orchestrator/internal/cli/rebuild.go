// The world-rebuild pipeline: bring the whole appliance up end to end in
// one shot — the Rust installer's stage set (installer/src/{main,lib}.rs)
// ported to Go. Shells the REAL sibling binaries exactly like the Rust
// installer did: `control-plane` + `runner` (still Rust) and THIS binary
// (os.Executable(), the teardown-engine pattern) for the orchestrator
// stages (exec/storage/bootstrap/deploy-*).
//
// Pipeline order (dialoguer main.rs + the TUI configure pipeline):
//
//	ensure_bins -> provision -> [door gate on a fresh key] -> grant ->
//	serve -> verify door -> WRITE INITIAL CONFIG -> storage (resolve +
//	ensure x3, record the plane) -> bootstrap relay -> record_lxc relay ->
//	bootstrap cp -> record_lxc cp -> [k3s stage] -> deploy-relay ->
//	deploy-cp -> NIP-11 relay pubkey (best-effort) -> final merge save.
//
// The config write happens AFTER the door verify and BEFORE storage:
// stage_storage bails without a config to record the durable-plane mapping
// into (the Rust dialoguer front-end wrote only at the end; the Rust TUI
// bootstrap wrote after verify — rebuild follows the TUI discipline).
package cli

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"freehold/orchestrator/internal/agent"
	"freehold/orchestrator/internal/cert"
	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/crypto"
	"freehold/orchestrator/internal/deploy"
	"freehold/orchestrator/internal/dnsman"
	"freehold/orchestrator/internal/drive"
	"freehold/orchestrator/internal/flows"
	"freehold/orchestrator/internal/state"
	"freehold/orchestrator/internal/wire"
	"freehold/orchestrator/prompts"
)

// dnsCredCmd stores the Caddy edge's DNS provider credential (provider + env)
// for a slot ahead of any install, so a headless /--yes or TUI rebuild reuses
// it without prompting. Interactive (no --provider): provider from lego's full
// registry + the provider's own env fields. Headless (--provider, optional
// --env), or left empty for auto-detecting providers (e.g. route53 via
// ~/.aws). Always pre-verified (throwaway TXT) then sealed to the ops identity.
// Idempotent — a stored copy is reported and left untouched (unless --force).
var dnsCredCmd = &cobra.Command{
	Use:   "dns-cred",
	Short: "Store (and pre-verify) a DNS provider credential for a cert slot — one-time seeding a rebuild reuses",
	RunE: func(cmd *cobra.Command, args []string) error {
		domain, _ := cmd.Flags().GetString("domain")
		cfgPath, _ := cmd.Flags().GetString("config")
		slot, _ := cmd.Flags().GetString("slot")
		provider, _ := cmd.Flags().GetString("provider")
		envCSV, _ := cmd.Flags().GetString("env")
		if domain == "" {
			if cfg, err := config.Load(cfgPath); err == nil && cfg != nil {
				if slot == "cp" && cfg.CPHost() != "" {
					domain = cfg.CPHost()
				} else {
					domain = cfg.RelayHost()
				}
			}
		}
		if domain == "" {
			return fmt.Errorf("dns-cred needs a domain (--domain or a config on record) so the pre-verify can target its zone")
		}
		e := &rebuildEngine{
			f:   rebuildFlags{relayDomain: domain, cpDomain: domain},
			out: os.Stdout,
			in:  os.Stdin,
		}
		env := map[string]string{}
		if envCSV != "" {
			for _, kv := range strings.Split(envCSV, ",") {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || strings.TrimSpace(k) == "" {
					return fmt.Errorf("--env expects KEY=VAL,... — bad entry %q", kv)
				}
				env[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		if provider != "" {
			if !cert.IsProvider(provider) {
				return fmt.Errorf("unknown DNS provider %q", provider)
			}
			if err := cert.Verify(domain, provider, env); err != nil {
				return fmt.Errorf("pre-verify failed for %s: %w", provider, err)
			}
			_, _, pub, err := e.certIdent()
			if err != nil {
				return err
			}
			seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
			if err := cert.SaveCreds(e.certCredPath(slot), provider, env, seal, pub, "cert-dns-"+slot); err != nil {
				return err
			}
			fmt.Fprintf(e.out, "  ✓ %s DNS provider credential saved (%s) — rebuilds will reuse it\n", slot, provider)
			fmt.Fprintf(e.out, "  stored sealed at %s\n", e.certCredPath(slot))
			return nil
		}
		// Interactive: provider picker + env collection.
		if slot == "cp" {
			e.f.relayDomain = domain
			provider, _, err := e.promptDNSCred(slot, domain, "relay")
			if err != nil {
				return err
			}
			fmt.Fprintf(e.out, "  ✓ %s DNS provider credential saved (%s) — rebuilds will reuse it\n", slot, provider)
			return nil
		}
		provider, _, err := e.promptDNSCred("relay", domain, "")
		if err != nil {
			return err
		}
		fmt.Fprintf(e.out, "  ✓ relay DNS provider credential saved (%s) — rebuilds will reuse it\n", provider)
		fmt.Fprintf(e.out, "  stored sealed at %s\n", e.certCredPath("relay"))
		return nil
	},
}

func init() {
	dnsCredCmd.Flags().String("domain", "", "Host the pre-verify targets (default: relay host from the recorded config, or the cp host with --slot cp)")
	dnsCredCmd.Flags().String("slot", "relay", "Credential slot: relay | cp")
	dnsCredCmd.Flags().String("provider", "", "DNS provider name (lego registry) — omit for the interactive picker")
	dnsCredCmd.Flags().String("env", "", "Provider env as KEY=VAL,KEY=VAL (omit/empty for auto-detecting providers like route53)")
	dnsCredCmd.Flags().String("config", defaultConfigPath(), "Config path to read the host from")
}

var buildCmd = &cobra.Command{Use: "build",
	Short: "Bring the whole world up end to end (fresh bootstrap OR rebuild — the same reconciling pipeline): door, runner, durable plane, relay + CP + k3s LXCs, deploys",
	RunE: func(cmd *cobra.Command, args []string) error {
		f := rebuildFlags{}
		f.addr, _ = cmd.Flags().GetString("addr")
		f.target, _ = cmd.Flags().GetString("target")
		f.host, _ = cmd.Flags().GetString("host")
		f.domain, _ = cmd.Flags().GetString("domain")
		f.relayDomain, _ = cmd.Flags().GetString("relay-domain")
		f.cpDomain, _ = cmd.Flags().GetString("cp-domain")
		f.operatorPubkey, _ = cmd.Flags().GetString("operator-pubkey")
		f.operatorIdentity, _ = cmd.Flags().GetString("operator-identity")
		f.agentName, _ = cmd.Flags().GetString("agent-name")
		f.sizeGB, _ = cmd.Flags().GetUint64("size-gb")
		f.poolSizeGB, _ = cmd.Flags().GetUint64("pool-size-gb")
		f.thinPool, _ = cmd.Flags().GetString("thin-pool")
		f.noK3s, _ = cmd.Flags().GetBool("no-k3s")
		f.noLitellm, _ = cmd.Flags().GetBool("no-litellm")
		f.litellmProviderKey = os.Getenv("FREEHOLD_LITELLM_PROVIDER_KEY")
		if v, _ := cmd.Flags().GetString("litellm-provider-key"); v != "" {
			f.litellmProviderKey = v
		}
		f.rootfsGB, _ = cmd.Flags().GetUint32("rootfs-gb")
		f.memoryMB, _ = cmd.Flags().GetUint32("memory-mb")
		f.relayGw, _ = cmd.Flags().GetString("relay-gw")
		f.proxyIP, _ = cmd.Flags().GetString("proxy-ip")
		f.configPath, _ = cmd.Flags().GetString("config")
		f.confirmStorage, _ = cmd.Flags().GetBool("confirm-storage")
		f.yes, _ = cmd.Flags().GetBool("yes")
		f.resetDNS, _ = cmd.Flags().GetBool("reset-dns")
		f.manageDNS, _ = cmd.Flags().GetBool("manage-dns")
		// Smooth rebuild: pull any omitted value from the stored config so a
		// rebuild is not forced to re-enter the operator key, relay/CP hosts,
		// thin-pool, etc.
		if err := applyConfigDefaults(&f, f.configPath); err != nil {
			return err
		}
		// Forget any stored DNS provider credentials so the build re-asks for
		// them (e.g. the stored one is stale/wrong from prior testing).
		if f.resetDNS {
			clearStoredDNSCreds()
			fmt.Fprintln(cmd.OutOrStdout(), "  (cleared stored DNS provider credentials — the build will ask for them again)")
		}
		// The ONE static IP (the proxy/Caddy node) must be CIDR — pct create's
		// net0=ip= wants host/prefix; relay/cp/k3s LXCs are DHCP behind it.
		if f.proxyIP != "" && !strings.Contains(f.proxyIP, "/") {
			return fmt.Errorf("--proxy-ip must be CIDR (host/prefix) — got %q", f.proxyIP)
		}

		eng, err := newRebuildEngine(f)
		if err != nil {
			return err
		}
		return eng.run()
	},
}

// applyConfigDefaults fills any omitted rebuild flag from the stored config, so
// `freehold build` is smooth: it reuses the recorded operator key, relay/CP
// hosts, thin-pool, agent name, and proxy IP instead of forcing re-entry. An
// explicit flag always wins; the config only fills blanks.
func applyConfigDefaults(f *rebuildFlags, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	if f.operatorPubkey == "" {
		f.operatorPubkey = cfg.OperatorPubkey
	}
	if f.relayDomain == "" {
		f.relayDomain = cfg.RelayHost()
	}
	if f.cpDomain == "" {
		f.cpDomain = cfg.CPHost()
	}
	if f.thinPool == "" && cfg.Plane.ThinPool != nil {
		f.thinPool = *cfg.Plane.ThinPool
	}
	if f.agentName == "" && cfg.CPAName != "" {
		f.agentName = cfg.CPAName
	}
	if f.proxyIP == "" && cfg.Proxy.Ip != nil {
		f.proxyIP = *cfg.Proxy.Ip
	}
	return nil
}

func init() {
	buildCmd.Flags().String("addr", "127.0.0.1:8787", "Runner MCP address (loopback)")
	buildCmd.Flags().String("target", "proxmox-box", "Runner name (the package + grant + target name)")
	buildCmd.Flags().String("host", "root@192.168.30.224", "Proxmox host address the runner SSH's into")
	buildCmd.Flags().String("domain", "", "DEPRECATED - use --relay-domain. Kept for old scripts.")
	buildCmd.Flags().String("relay-domain", "", "The RELAY's own public host (its Buzz origin) — REQUIRED, never derived")
	buildCmd.Flags().String("cp-domain", "", "The CONTROL PLANE's public host — REQUIRED, never derived")
	buildCmd.Flags().String("operator-pubkey", "", "Operator Nostr pubkey (64-hex) — console admin + relay owner (REQUIRED)")
	buildCmd.Flags().String("operator-identity", "", "Operator identity dir to record in the config (optional)")
	buildCmd.Flags().String("agent-name", "freehold", "The CPA's display name in Buzz (the agent the operator names at install; default 'freehold')")
	buildCmd.Flags().Uint64("size-gb", drive.TenantLVSizeGB, "Per-tenant thin LV size in GiB (LVM-thin backend)")
	buildCmd.Flags().Uint64("pool-size-gb", drive.FreshPoolSizeGB, "Thin-pool size in GiB when a NEW pool is carved")
	buildCmd.Flags().String("thin-pool", "", "Plane placement: the thin pool the tenant LVs land in — the name of an EXISTING pool to reuse, or a NEW name to carve (then carved at --pool-size-gb). Absent => interactive prompt, or reuse-detected/carve-default under --yes")
	buildCmd.Flags().Bool("no-k3s", false, "Opt-out: do NOT boot/install the k3s substrate LXC (defaults to the full world — relay/cp/k3s/litellm/CPA; stages reconcile idempotently and skip what is already present)")
	buildCmd.Flags().Uint32("rootfs-gb", 16, "LXC rootfs size in GB")
	buildCmd.Flags().Uint32("memory-mb", 2048, "LXC memory in MB")
	buildCmd.Flags().String("relay-gw", "192.168.30.1", "Gateway for the proxy's STATIC guest IP (unused with DHCP)")
	buildCmd.Flags().Bool("no-litellm", false, "Opt-out: do NOT deploy the litellm gateway (kube workloads + runner + model registration). Defaults on with k3s (the CPA needs it to reason); requires k3s")
	buildCmd.Flags().String("litellm-provider-key", "", "Fireworks/upstream provider API key for litellm's model (supplied at FIRST provision only, then sealed in the runner and reused; read from --litellm-provider-key or FREEHOLD_LITELLM_PROVIDER_KEY)")
	buildCmd.Flags().String("proxy-ip", "", "STATIC proxy (Caddy/k3s node) IP (CIDR, e.g. 192.168.30.7/24) — the ONE static address; relay/CP hosts resolve to it. Absent => DHCP")
	buildCmd.Flags().String("config", defaultConfigPath(), "Config path (default: ~/.config/freehold/config.toml)")
	buildCmd.Flags().Bool("confirm-storage", false, "Operator consent to CREATE a storage backend when none is detected")
	buildCmd.Flags().Bool("yes", false, "Non-interactive: bail (actionably) where the interactive pipeline would prompt")
	buildCmd.Flags().Bool("reset-dns", false, "Forget any stored DNS provider credentials so the build prompts for them again")
	buildCmd.Flags().Bool("manage-dns", false, "Opt-in: freehold MANAGEs the world's DNS — creates/updates relay/cp <domain> A records -> the proxy IP on your provider (currently Cloudflare only), using the same credential the Let's Encrypt cert will reuse. Interactive runs ask when omitted; --yes requires this flag")
}

// rebuildFlags is the command's collected answers.
type rebuildFlags struct {
	addr               string
	target             string
	host               string
	domain             string
	relayDomain        string
	cpDomain           string
	proxyIP            string
	operatorPubkey     string
	operatorIdentity   string
	agentName          string
	sizeGB             uint64
	poolSizeGB         uint64
	thinPool           string
	noK3s              bool
	noLitellm          bool
	litellmProviderKey string
	rootfsGB           uint32
	memoryMB           uint32
	relayGw            string
	configPath         string
	confirmStorage     bool
	resetDNS           bool
	manageDNS          bool
	yes                bool
}

// rebuildBins are the resolved sibling binary paths. Go has no
// CARGO_MANIFEST_DIR: everything is resolved relative to THIS executable's
// dir (the layout ships all bins together in target/debug/, release pairs
// in target/release/).
type rebuildBins struct {
	Self         string // this binary — the orchestrator (teardown-engine pattern)
	ControlPlane string // target/debug/control-plane (Rust)
	Runner       string // target/debug/runner (Rust)
	ReleaseCP    string // target/release/control-plane (deploy-cp --binary)
	ReleaseRun   string // target/release/runner (deploy-cp --runner-binary)
}

// resolveRebuildBins checks the binaries the pipeline actually execs and
// returns their paths, or the exact build one-liner when any is missing.
func resolveRebuildBins() (rebuildBins, error) {
	self, err := os.Executable()
	if err != nil {
		return rebuildBins{}, fmt.Errorf("cannot resolve own binary path: %w", err)
	}
	selfDir := filepath.Dir(self)
	releaseDir := filepath.Join(selfDir, "..", "release")
	b := rebuildBins{
		Self:         self,
		ControlPlane: filepath.Join(selfDir, "control-plane"),
		Runner:       filepath.Join(selfDir, "runner"),
		ReleaseCP:    filepath.Join(releaseDir, "control-plane"),
		ReleaseRun:   filepath.Join(releaseDir, "runner"),
	}
	var missing []string
	for _, p := range []struct{ path, label string }{
		{b.ControlPlane, "control-plane"},
		{b.Runner, "runner"},
		{b.ReleaseCP, "../release/control-plane"},
		{b.ReleaseRun, "../release/runner"},
	} {
		if _, err := os.Stat(p.path); err != nil {
			missing = append(missing, filepath.Join(filepath.Base(selfDir), p.label))
		}
	}
	if len(missing) > 0 {
		return b, fmt.Errorf(
			"sibling binaries missing: %s\n  build them once, then re-run:\n    cargo build --bin control-plane --bin runner && cargo build --release --bin control-plane --bin runner",
			strings.Join(missing, ", "))
	}
	return b, nil
}

// rebuildEngine runs the pipeline; every side effect goes through an
// injectable seam so the pure discipline (record/merge/parse) is testable
// hermetically (mirrors Rust record_lxc_with).
type rebuildEngine struct {
	f    rebuildFlags
	bins rebuildBins

	out io.Writer
	in  io.Reader
	// stdin is the ONE buffered reader over in. prompt() must not build a
	// fresh bufio.Reader per call: a fresh one reads ahead past the first
	// newline into its own buffer, so a back-to-back prompt (the carve
	// size after the pool name) would see an already-drained in and EOF.
	stdin *bufio.Reader

	// seams
	runBin   func(bin string, args []string) (bool, string)
	runEnv   func(bin string, env []string, args []string) (bool, string)
	runSh    func(script string) (string, error)
	portOpen func(addr string) bool
	curlGet  func(url string) (string, bool)

	// certRuns holds the background DNS-01 issuances started early in the build
	// (per distinct LE cert-domain), awaited after the Caddy edge is up.
	certRuns map[string]*certRun
}

func newRebuildEngine(f rebuildFlags) (*rebuildEngine, error) {
	if f.operatorPubkey == "" {
		return nil, fmt.Errorf("rebuild needs --operator-pubkey (64-hex or npub1…)")
	}
	// Normalize to 64-hex up front (Rust installer::main.rs runs
	// parse_pubkey_input before the pipeline): deploy-relay/deploy-cp
	// reject anything that isn't 64-hex, and the config records hex.
	pk, err := crypto.ParsePubkeyInput(f.operatorPubkey)
	if err != nil {
		return nil, err
	}
	f.operatorPubkey = pk
	bins, err := resolveRebuildBins()
	if err != nil {
		return nil, err
	}
	e := &rebuildEngine{
		f:        f,
		bins:     bins,
		out:      os.Stdout,
		in:       os.Stdin,
		runBin:   runBinDefault,
		runEnv:   runEnvDefault,
		runSh:    runShDefault,
		portOpen: portOpenDefault,
		curlGet:  curlGetDefault,
	}
	return e, nil
}

// runBinDefault runs a sibling binary capturing stdout+stderr (Rust run()).
func runBinDefault(bin string, args []string) (bool, string) {
	out, err := exec.Command(bin, args...).CombinedOutput()
	return err == nil, string(out)
}

// runEnvDefault runs a sibling binary with an EXTRA env var prefix (the
// headless secret supply: values ride env, never argv — provision/add-secret
// read them via --secret-env).
func runEnvDefault(bin string, env []string, args []string) (bool, string) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(env, os.Environ()...)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func runShDefault(script string) (string, error) {
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	return string(out), err
}

func portOpenDefault(addr string) bool {
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		host, port = addr, "8787"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// curlGetDefault mirrors Rust relay_pubkey_nip11's curl call.
func curlGetDefault(url string) (string, bool) {
	out, err := exec.Command("curl", "-sk", "--max-time", "8",
		"-H", "Accept: application/nostr+json", url).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// ---- freehold home paths (mirror installer::lib.rs) ----------------------

func rbStateDir() string   { return filepath.Join(freeholdHome(), "control-plane") }
func rbOpsDir() string     { return filepath.Join(rbStateDir(), "agent-ops") }
func rbRunnerPkgs() string { return filepath.Join(freeholdHome(), "runner") }
func rbServeLog() string   { return filepath.Join(freeholdHome(), "installer", "serve.log") }

// ---- the pipeline ---------------------------------------------------------

func (e *rebuildEngine) run() error {
	fmt.Fprintf(e.out, "rebuilding world %s (runner %s @ %s)\n", e.f.relayDomain, e.f.target, e.f.addr)

	// 1. the ops agent identity (minted on demand; its pubkey is the grant).
	ensureAgentIdentity(rbOpsDir())
	agentPK, err := loadRPubkey(rbOpsDir())
	if err != nil {
		return fmt.Errorf("ops agent identity unreadable at %s: %w", rbOpsDir(), err)
	}

	// 2. provision the door (fresh ssh key => door gate).
	doorKey, err := e.stageProvision(agentPK)
	if err != nil {
		return err
	}
	if doorKey != "" {
		if err := e.doorGate(doorKey); err != nil {
			return err
		}
	}

	// 3. grant the ops agent (belt + suspenders for a reused package).
	if err := e.stageGrant(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ ops agent granted on %s\n", e.f.target)

	// 4. the runner serves in the background (always fresh for the CURRENT
	// package — a stale listener holds old identities in memory).
	pid, err := e.stageServe()
	if err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ runner serving on %s (pid %s)\n", e.f.addr, pid)

	// 5. verify the door through the runner.
	if err := e.stageVerify(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ the door works — %s is reachable\n", e.f.host)

	// 5.5. The relay + control-plane hosts are ASKED (never derived; there is
	// no world domain). Flags (or a headless --yes run) honor the values;
	// otherwise each is prompted up front. Must run BEFORE any config build so
	// RelayURL/CPURL are correct from the initial write.
	if !e.f.yes {
		if err := e.promptDomains(); err != nil {
			return err
		}
	}
	if e.f.relayDomain == "" || e.f.cpDomain == "" {
		return fmt.Errorf("rebuild needs --relay-domain and --cp-domain (or an interactive run)")
	}

	// 5.6 The ONE static proxy IP (the Caddy/k3s node). relay/CP have no static
	// addresses — everything sits behind the proxy, which is the only way into
	// them. Asked when not supplied (--yes requires the flag or a stored value).
	if e.f.proxyIP == "" && !e.f.yes {
		ans, err := e.prompt("proxy static IP (CIDR, e.g. 192.168.30.8/24) — REQUIRED, the one address relay/CP resolve to")
		if err != nil {
			return err
		}
		if a := strings.TrimSpace(ans); a != "" {
			e.f.proxyIP = a
		}
	}
	if e.f.proxyIP == "" {
		return fmt.Errorf("rebuild needs --proxy-ip (the single static proxy/Caddy address)")
	}
	if !strings.Contains(e.f.proxyIP, "/") {
		return fmt.Errorf("--proxy-ip must be CIDR (host/prefix) — got %q", e.f.proxyIP)
	}

	// 6. write the INITIAL config so stage_storage has somewhere to record
	// the durable-plane mapping (merge preserves a surviving config's facts).
	if err := e.writeInitialConfig(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ wrote config %s\n", e.f.configPath)

	// 6.4. Freehold-managed DNS (opt-in --manage-dns). Asked BEFORE the LE
	// DNS credential collection: if accepted, freehold creates/updates the
	// world's A records (relay.<d> + cp.<d> -> the proxy static IP) on the
	// operator's provider (currently Cloudflare only), using the SAME sealed
	// credential the Let's Encrypt wildcard will reuse just below.
	if e.worldHasEdge() {
		manage := e.f.manageDNS
		if !manage && !e.f.yes {
			ans, err := e.prompt("manage the domain's DNS? freehold can point relay/cp A records at the proxy on Cloudflare. (y/n)")
			if err != nil {
				return err
			}
			manage = strings.EqualFold(strings.TrimSpace(ans), "y")
		}
		if manage {
			if err := e.manageDomainDNS(); err != nil {
				return err
			}
		}
	}

	// 6.6. DNS provider credentials for the edge's per-host certs. Asked UP
	// FRONT (like the other prompts) so a later stage failure can't strand a
	// long install without its DNS-01 tokens: sealed copies are reused
	// silently; otherwise each slot is prompted + pre-verified now, and
	// stageCert at the end reads them back. No-op under --yes unless a stored
	// copy already exists (then stageCert fails loudly at the end).
	if e.worldHasEdge() {
		if _, _, err := e.promptDNSCred("relay", e.f.relayDomain, ""); err != nil {
			return fmt.Errorf("relay DNS provider credential: %w", err)
		}
		if _, _, err := e.promptDNSCred("cp", e.f.cpDomain, "relay"); err != nil {
			return fmt.Errorf("control-plane DNS provider credential: %w", err)
		}
	}

	// 6.7. Kick off the edge's cert issuance EARLY. The DNS-01 challenge TXT is
	// pre-placed NOW so a slow DNS provider (e.g. a freshly-activated Cloudflare
	// zone propagating _acme-challenge TXT over minutes) gets the whole build to
	// serve it, instead of being created for the first time at the END and
	// timing out lego's short propagation window. Runs in the background; the
	// Caddy stage awaits + installs the result (30s poll) once the edge is up.
	if e.worldHasEdge() {
		if err := e.startCertIssuance(); err != nil {
			return fmt.Errorf("start cert issuance: %w", err)
		}
	}

	// 7a. plane placement: where do the tenant LVs live — reuse the VG's
	// detected thin pool, or carve a dedicated new one?
	placement, err := e.stagePlacement()
	if err != nil {
		return err
	}

	// 7b. the durable volume plane: ensure each tenant onto the chosen
	// pool, record, and keep PVE's local-lvm storage pointed at it.
	if err := e.stageStorage(placement); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ durable volume plane ready")

	// 8-9. boot the relay LXC, record its coordinates.
	fmt.Fprintln(e.out, "  · booting the relay LXC (create → docker → compose; can take minutes)…")
	if err := e.stageBootstrap("relay"); err != nil {
		return err
	}
	if _, err := e.stageRecordLxc("relay"); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ relay LXC booted + recorded")

	// 10-11. boot the cp LXC, record.
	fmt.Fprintln(e.out, "  · booting the cp LXC (create → docker → compose; can take minutes)…")
	if err := e.stageBootstrap("cp"); err != nil {
		return err
	}
	if _, err := e.stageRecordLxc("cp"); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ cp LXC booted + recorded")

	// 12. the k3s substrate (boot-if-missing + in-guest install + record).
	if !e.f.noK3s {
		fmt.Fprintln(e.out, "  · installing the k3s substrate (download + in-guest install; several minutes)…")
		if err := e.stageK3s(); err != nil {
			return err
		}
		fmt.Fprintln(e.out, "  ✓ k3s substrate ready")
	}

	// 13-14. deploy the relay + the control plane.
	fmt.Fprintln(e.out, "  · deploying the relay stack (compose pull + start)…")
	if err := e.stageDeployRelay(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ relay live at https://%s\n", e.f.relayDomain)
	fmt.Fprintln(e.out, "  · deploying the control plane (release binaries into the cp LXC)…")
	if err := e.stageDeployCp(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ control plane live at https://%s\n", e.f.cpDomain)

	// 14.5. C0: the litellm gateway — provision the litellm runner (master +
	// provider + postgres secrets sealed to it), apply the kube workloads,
	// register the model, and record the coords for the Services row. Part of
	// the desired world whenever k3s is on (the CPA needs it to reason); only
	// an explicit --no-litellm opts it out.
	if !e.f.noK3s && !e.f.noLitellm {
		fmt.Fprintln(e.out, "  · applying the litellm kube workloads (postgres + gateway; rollout up to 5m)…")
		if err := e.stageLitellm(); err != nil {
			return err
		}
		fmt.Fprintln(e.out, "  ✓ litellm gateway live")
	}

	// 14.6. C0: register the CP resolver's explicit records (relay/cp/k3s +
	// litellm) INSIDE the deployed CP, then point every guest at it. This runs
	// BEFORE the Caddy edge and the CPA so a cold world's first CPA boot never
	// races the very DNS wiring it dials: by the time the pod comes up, the
	// split-horizon <domain> -> relay record exists and the k3s guest's
	// nameserver is already pointed at the CP resolver (hostNetwork pod).
	fmt.Fprintln(e.out, "  · writing the resolver records (relay/cp/k3s/litellm)…")
	if err := e.stageDnsRegister(); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  · pointing every guest at the resolver + verifying it answers…")
	if err := e.stageDnsPoint(); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ internal DNS resolver live (CP-owned)")

	// 14.65. C0: the core Caddy TLS fronting proxy — freehold's own edge. It
	// fronts the relay over the wildcard cert (F3 issues it; F5 points the CPA
	// at wss://relay.<domain>). Rides the k3s node (hostNetwork), so it needs
	// the substrate; part of the desired world whenever k3s is on.
	if !e.f.noK3s {
		fmt.Fprintln(e.out, "  · applying the Caddy TLS fronting proxy (hostNetwork; relay vhost)…")
		if err := e.stageCaddy(); err != nil {
			return err
		}
		fmt.Fprintln(e.out, "  ✓ Caddy TLS edge live")
		fmt.Fprintln(e.out, "  · awaiting the edge certs (pre-placed challenges; polling every 30s)…")
		if err := e.awaitCertIssuance(); err != nil {
			return err
		}
		fmt.Fprintln(e.out, "  ✓ edge certs in place")
	}

	// 14.7. C0: the CPA — the system's reasoning touchpoint — deployed as a
	// k3s Pod running the buzz-sprig harness (Chunk 4 Phase A: A2/A3/A5).
	// It needs both the k3s substrate and the litellm gateway to reason, so
	// opting either out skips the CPA too (a brainless pod is a dead pod).
	// The DNS stages above (14.6) run first so the relay address it dials is
	// already LAN-resolvable through the node on first boot.
	if !e.f.noK3s && !e.f.noLitellm {
		fmt.Fprintln(e.out, "  · deploying the CPA (buzz-sprig pod into k3s; rollout up to 5m)…")
		if err := e.stageCpa(); err != nil {
			return err
		}
		fmt.Fprintln(e.out, "  ✓ CPA live in Buzz ("+e.f.agentName+")")
	}

	// 15. the relay's signing key via NIP-11 (best-effort trust anchor).
	if rpk, ok := e.relayPubkeyNip11(); ok {
		fmt.Fprintf(e.out, "  ✓ relay signing key: %s\n", rpk)
	} else {
		fmt.Fprintln(e.out, "  (relay signing key unreadable via NIP-11 — read it from the relay's data dir when you need --relay-pubkey)")
	}

	// 16. final MERGE save (never from-answers alone — the mid-pipeline
	// writes live on disk).
	if err := e.finalSave(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ wrote config %s\n", e.f.configPath)

	fmt.Fprintf(e.out, `
  ╭─────────────────────────────────────────────────────────╮
  │                    Freehold is up                      │
  ╰─────────────────────────────────────────────────────────╯

  relay:          https://%s
  control plane:  https://%s
  runner:         serving on %s
  operator pk:    %s
`, e.f.relayDomain, e.f.cpDomain, e.f.addr, e.f.operatorPubkey)
	return nil
}

// stageProvision runs control-plane provision; returns the fresh ssh public
// key line when a NEW door key was generated ("" on reuse).
func (e *rebuildEngine) stageProvision(agentPK string) (string, error) {
	runnerDir := filepath.Join(rbRunnerPkgs(), e.f.target)
	ok, out := e.runBin(e.bins.ControlPlane, []string{
		"provision", e.f.target,
		"--kind", "ssh",
		"--address", e.f.host,
		"--state-dir", rbStateDir(),
		"--runner-dir", runnerDir,
		"--grant", agentPK,
	})
	if ok {
		return extractSSHKey(out), nil
	}
	if isProvisionReuse(out) {
		// reuse is only safe when the PACKAGE is actually there — a leftover
		// state record with a deleted package cascades on every later stage.
		if _, err := os.Stat(filepath.Join(runnerDir, "identity.json")); err != nil {
			return "", fmt.Errorf(
				"a runner %q record exists but its package at %s is gone — wipe the world for a clean re-bootstrap:\n  rm -rf ~/.freehold\n(or revoke the record: control-plane revoke %s --state-dir %s)",
				e.f.target, runnerDir, e.f.target, rbStateDir())
		}
		return "", nil
	}
	return "", fmt.Errorf("provision failed:\n%s", out)
}

// extractSSHKey finds the fresh door key line in provision's output.
func extractSSHKey(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimLeft(l, " \t"), "ssh-ed25519") {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

func isProvisionReuse(out string) bool {
	return strings.Contains(out, "already exists") ||
		strings.Contains(out, "RunnerExists") ||
		strings.Contains(out, "PackageDirInUse") ||
		strings.Contains(out, "already holds a runner")
}

// doorGate handles a freshly minted door key: interactive mode prompts until
// the operator installed it; --yes bails actionably (a headless pipeline
// can't install the key for the operator).
func (e *rebuildEngine) doorGate(key string) error {
	instr := fmt.Sprintf("echo '%s' >> /root/.ssh/authorized_keys", key)
	if e.f.yes {
		return fmt.Errorf(
			"the door needs a NEW ssh key before rebuild can continue — install it on %s, then re-run rebuild:\n\n    %s\n\n  (on the host: mkdir -p /root/.ssh && %s)",
			e.f.host, key, instr)
	}
	for {
		e.printDoorKey(key)
		answer, err := e.prompt("Press ENTER when it's in place, or 'r' to show it again, 'q' to quit")
		if err != nil {
			return fmt.Errorf("aborted by the operator (door not installed)")
		}
		if strings.TrimSpace(answer) == "" {
			return nil
		}
	}
}

// printDoorKey is the install block shared by doorGate (fresh key) and the
// verify stage's re-surface (recovered key).
func (e *rebuildEngine) printDoorKey(key string) {
	instr := fmt.Sprintf("echo '%s' >> /root/.ssh/authorized_keys", key)
	fmt.Fprintf(e.out, `
  ─────────────────────────────────────────────────────────
  Finish the door: add this line to %s's ~/.ssh/authorized_keys:

    %s

  (on the host: mkdir -p /root/.ssh && %s)
  ─────────────────────────────────────────────────────────
`, e.f.host, key, instr)
}

// doorKeyNotInstalled is the REUSE path's door gate: provision skipped the
// gate (the package already exists), but the ssh AUTH failure proves the key
// was never installed. Recover the public line from the package and bail
// actionably exactly like doorGate's --yes — the operator must see the key
// again or they are stuck.
func (e *rebuildEngine) doorKeyNotInstalled() error {
	key := e.recoverDoorKey()
	if key == "" {
		return fmt.Errorf(
			"the door check failed: ssh authentication was refused and the door key could\nnot be recovered from the runner package at %s — wipe the world and start clean:\n  rm -rf ~/.freehold   (then re-run rebuild)",
			filepath.Join(rbRunnerPkgs(), e.f.target))
	}
	instr := fmt.Sprintf("echo '%s' >> /root/.ssh/authorized_keys", key)
	return fmt.Errorf(
		"the door needs its ssh key before rebuild can continue — install it on %s, then re-run rebuild:\n\n    %s\n\n  (on the host: mkdir -p /root/.ssh && %s)",
		e.f.host, key, instr)
}

// recoverDoorKey re-derives the door ssh PUBLIC line from the existing
// runner package — the same material the runner decrypts at boot. "" when
// the package is missing or unusable (the caller falls back to the
// fresh-start message).
func (e *rebuildEngine) recoverDoorKey() string {
	key, err := doorKeyFromPackage(filepath.Join(rbRunnerPkgs(), e.f.target), e.f.target)
	if err != nil {
		return ""
	}
	return key
}

// doorKeyFromPackage opens the sealed door credential with the runner's OWN
// enc key (identity.json opens secrets.json — the runner's boot path) and
// re-derives only the PUBLIC authorized_keys line. The private half is
// parsed past and never returned, written, or shipped — nothing new leaves
// the machine. Prefer the target's own entry (provision seals under the
// runner name); fall back to any ssh target in the package.
func doorKeyFromPackage(runnerDir, target string) (string, error) {
	id, err := flows.LoadIdentity(runnerDir)
	if err != nil {
		return "", fmt.Errorf("read identity: %w", err)
	}
	encSecret, err := hexDecode(id.EncSecretHex)
	if err != nil {
		return "", fmt.Errorf("bad enc secret: %w", err)
	}
	pkg, err := wire.Load(runnerDir)
	if err != nil {
		return "", fmt.Errorf("read package: %w", err)
	}
	names := []string{target}
	for name := range pkg.Targets {
		if name != target {
			names = append(names, name)
		}
	}
	for _, name := range names {
		meta, ok := pkg.Targets[name]
		if !ok || meta.Kind != "ssh" {
			continue
		}
		ctHex, ok := pkg.Secrets[meta.Secret]
		if !ok {
			continue
		}
		sealed, err := hexDecode(ctHex)
		if err != nil {
			continue
		}
		// aad = the secret NAME the CP sealed with (provisioner: req.Name).
		pem, err := crypto.Open(encSecret, []byte(meta.Secret), sealed)
		if err != nil {
			continue
		}
		line, err := crypto.ExtractED25519PublicKeyLine(pem)
		if err != nil {
			continue
		}
		return line, nil
	}
	return "", fmt.Errorf("no usable ssh credential in %s", runnerDir)
}

func (e *rebuildEngine) prompt(label string) (string, error) {
	fmt.Fprintf(e.out, "%s: ", label)
	if e.stdin == nil {
		e.stdin = bufio.NewReader(e.in)
	}
	line, err := e.stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no input (EOF)")
	}
	if strings.TrimSpace(line) == "q" {
		return "", fmt.Errorf("aborted by the operator")
	}
	return line, nil
}

// parseGB parses a positive size-in-GB answer; blank takes the default.
func parseGB(s string, def uint64, what string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a number >= 1 (got %q; blank = %d)", what, s, def)
	}
	return n, nil
}

func (e *rebuildEngine) stageGrant() error {
	ok, out := e.runBin(e.bins.ControlPlane, []string{
		"grant", e.f.target, "--state-dir", rbStateDir(),
	})
	if !ok {
		return fmt.Errorf("grant failed:\n%s", out)
	}
	return nil
}

// stageServe kills any stale serve on the addr and spawns a fresh detached
// one for the CURRENT package; returns the pid.
func (e *rebuildEngine) stageServe() (string, error) {
	e.killServeOn(e.f.addr)
	if err := os.MkdirAll(filepath.Dir(rbServeLog()), 0o755); err != nil {
		return "", err
	}
	pkg := filepath.Join(rbRunnerPkgs(), e.f.target)
	if _, err := os.Stat(filepath.Join(pkg, "identity.json")); err != nil {
		return "", fmt.Errorf("runner package %s is missing — the provision stage created it, something is off", pkg)
	}
	script := fmt.Sprintf("nohup '%s' serve --state-dir %s --addr %s > %s 2>&1 & echo $!",
		e.bins.Runner, pkg, e.f.addr, rbServeLog())
	out, _ := e.runSh(script)
	pid := strings.TrimSpace(out)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if e.portOpen(e.f.addr) {
			return pid, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", fmt.Errorf("the runner didn't come up on %s within 20s — see %s for why", e.f.addr, rbServeLog())
}

// killServeOn pkills any runner serve bound to this MCP address and WAITS
// for the port to actually close (a fresh spawn while the old listener
// still holds the address dies with "Address already in use").
func (e *rebuildEngine) killServeOn(addr string) {
	pat := fmt.Sprintf("runner serve.*--addr %s", regexEscape(addr))
	_, _ = exec.Command("pkill", "-f", pat).CombinedOutput()
	deadline := time.Now().Add(5 * time.Second)
	for e.portOpen(addr) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
}

func regexEscape(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('\\')
			b.WriteRune(c)
		}
	}
	return b.String()
}

// stageVerify probes the door through the runner (one real exec).
func (e *rebuildEngine) stageVerify() error {
	failures := 0
	for {
		ok, out := e.runBin(e.bins.Self, e.execArgs("echo freehold-door-ok", 60))
		if ok && strings.Contains(out, "freehold-door-ok") {
			return nil
		}
		failures++
		if e.f.yes {
			// An ssh AUTH refusal after a provision reuse is the classic
			// "pressed B again before installing the key" trap: the gate
			// was skipped, so the operator never saw the key. Recover it
			// from the package and bail actionably — the SAME waiting
			// state the fresh-key gate produces.
			if isSshAuthFailure(out) {
				return e.doorKeyNotInstalled()
			}
			return fmt.Errorf("the door check failed (non-interactive):\n%s", printTail(out, 6))
		}
		fmt.Fprintf(e.out, "  ✗ the exec failed (auth or otherwise)\n%s\n", printTail(out, 6))
		if isSshAuthFailure(out) {
			if key := e.recoverDoorKey(); key != "" {
				fmt.Fprintf(e.out, "  (ssh authentication was refused — the door key is probably not installed yet)\n")
				e.printDoorKey(key)
			}
		}
		if failures >= 3 {
			fmt.Fprintf(e.out, `
  Still failing after %d tries. If this runner predates the ssh-key
  serialization fix, its PRIVATE key may be unloadable by the SSH client —
  authorized_keys edits can't help that.
  Fresh start:  rm -rf ~/.freehold && freehold build ...
`, failures)
		}
		if _, err := e.prompt("Fix authorized_keys on the host, then press ENTER to retry ('q' to quit)"); err != nil {
			return fmt.Errorf("aborted at the door check")
		}

	}
}

// isSshAuthFailure detects the runner's ssh auth refusal in exec output
// (runner/src/ssh.rs SshError::Auth: "authentication failed").
func isSshAuthFailure(out string) bool {
	return strings.Contains(out, "authentication failed")
}

// execArgs builds `exec --addr --agent-dir [--timeout] <target> <cmd>`.
func (e *rebuildEngine) execArgs(cmd string, timeoutS int) []string {
	args := []string{"exec", "--addr", e.f.addr, "--agent-dir", rbOpsDir()}
	if timeoutS > 0 {
		args = append(args, "--timeout", strconv.Itoa(timeoutS))
	}
	return append(args, e.f.target, cmd)
}

// selfStage runs one of THIS binary's subcommands with the per-subcommand
// --addr/--agent-dir injected right after the subcommand name (Rust
// stage_any: the orchestrator's flattened CommonArgs go AFTER the name).
func (e *rebuildEngine) selfStage(name string, args []string) (string, error) {
	full := make([]string, 0, len(args)+5)
	full = append(full, args[0], "--addr", e.f.addr, "--agent-dir", rbOpsDir())
	full = append(full, args[1:]...)
	ok, out := e.runBin(e.bins.Self, full)
	if !ok {
		return "", fmt.Errorf("%s failed:\n%s", name, out)
	}
	return out, nil
}

// stageServeRunner serves an ADDITIONAL runner package on a dedicated
// loopback addr (C0: the litellm runner at 127.0.0.1:8788 — its exec reaches
// the gateway NodePort and carrries the 3-secret package). Returns the pid.
func (e *rebuildEngine) stageServeRunner(name, pkg string) (string, error) {
	addr := "127.0.0.1:8788"
	e.killServeOn(addr)
	logPath := filepath.Join(freeholdHome(), "installer", name+".serve.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", err
	}
	script := fmt.Sprintf("nohup '%s' serve --state-dir %s --addr %s > %s 2>&1 & echo $!",
		e.bins.Runner, pkg, addr, logPath)
	out, _ := e.runSh(script)
	pid := strings.TrimSpace(out)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if e.portOpen(addr) {
			return pid, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", fmt.Errorf("the %s runner didn't come up on %s within 20s — see %s", name, addr, logPath)
}

// stopRunner kills a runner pid (best-effort; the serve was detached).
func (e *rebuildEngine) stopRunner(pid string) {
	if pid == "" {
		return
	}
	_, _ = e.runSh(fmt.Sprintf("kill '%s' >/dev/null 2>&1 || true", pid))
}

// ---- config writes ---------------------------------------------------------

// fromAnswers is the GREENFIELD config the rebuild answers describe (Rust
// config::Config::from_answers). runner pubkey resolved from the package.
func (e *rebuildEngine) fromAnswers() *config.Config {
	runnerPK, _ := loadRPubkey(filepath.Join(rbRunnerPkgs(), e.f.target))
	// The relay + CP hosts are LITERAL inputs, never derived, and there is no
	// world/base domain. Everything behind is a single static proxy IP.
	relayDomain := e.f.relayDomain
	cpDomain := e.f.cpDomain
	// RelayWsURL is the CPA pod's relay origin: wss://<relayDomain> — the
	// public, TLS-fronted form the edge serves.
	cfg := &config.Config{
		RelayURL:       "https://" + relayDomain,
		RelayWsURL:     "wss://" + relayDomain,
		CPURL:          "https://" + cpDomain,
		OperatorPubkey: e.f.operatorPubkey,
		Runner: config.RunnerRef{
			Addr:   e.f.addr,
			Pubkey: runnerPK,
			Target: e.f.target,
		},
		Managed: []string{"relay", "cp"},
		CPAName: e.f.agentName,
	}
	if e.f.proxyIP != "" {
		p := e.f.proxyIP
		cfg.Proxy = config.ProxySpec{Ip: &p}
	}
	if e.f.operatorIdentity != "" {
		v := e.f.operatorIdentity
		cfg.OperatorIdentity = &v
	}
	return cfg
}

// mergeFromAnswers rebuilds from answers WITHOUT wiping the world facts a
// prior run recorded on disk: the durable plane, the relay pubkey, the
// operator identity dir, and any managed piece beyond the baseline (Rust
// merge_from_answers).
func mergeFromAnswers(ans *config.Config, prev *config.Config) *config.Config {
	if prev == nil {
		return ans
	}
	cfg := *ans
	cfg.Plane = prev.Plane
	// The mid-pipeline recorders (recordLitellm, stageDnsRegister) persist
	// their sections to disk BEFORE finalSave rebuilds from answers from
	// scratch — dropping them here would silently erase the gateway coords
	// and the resolver mirror on every run. Keep them, like Plane. Also keep
	// Caddy (coords + the per-host LE cert-domains + expiry) so a rebuild does
	// not drop the wildcard cert-domain config or an existing cert's metadata.
	// Dns.Records (resolver mirror) + Dns.Manager survive; a run that manages
	// DNS itself overrides the manager, otherwise the recorded one stays so
	// teardown --remove-dns still knows whom to ask.
	records := prev.Dns.Records
	if records == nil {
		records = ans.Dns.Records
	}
	mgr := prev.Dns.Manager
	if ans.Dns.Manager != nil {
		mgr = ans.Dns.Manager
	}
	cfg.Dns = config.DnsSpec{Records: records, Manager: mgr}
	cfg.Litellm = prev.Litellm
	cfg.Caddy = prev.Caddy
	if cfg.RelayPubkey == nil {
		cfg.RelayPubkey = prev.RelayPubkey
	}
	if cfg.OperatorIdentity == nil {
		cfg.OperatorIdentity = prev.OperatorIdentity
	}
	// answers win (a fresh boot's coords); prev fills the Nones.
	for _, pair := range [][2]*config.LxcGuest{
		{&cfg.Lxc.Relay, &prev.Lxc.Relay},
		{&cfg.Lxc.Cp, &prev.Lxc.Cp},
		{&cfg.Lxc.K3s, &prev.Lxc.K3s},
	} {
		if pair[0].Vmid == nil {
			pair[0].Vmid = pair[1].Vmid
		}
		if pair[0].Ip == nil {
			pair[0].Ip = pair[1].Ip
		}
	}
	for _, m := range prev.Managed {
		if !containsStr(cfg.Managed, m) {
			cfg.Managed = append(cfg.Managed, m)
		}
	}
	return &cfg
}

// managedForFlags is the rebuild's world manifest: relay + cp always,
// k3s/litellm exactly when their flags were given. The pipeline's mid-stage
// recorders append as they go; finalSave replaces the list wholesale so a
// withheld flag drops its survivor.
func managedForFlags(noK3s, noLitellm bool) []string {
	m := []string{"relay", "cp"}
	if !noK3s {
		m = append(m, "k3s")
	}
	if !noK3s && !noLitellm {
		m = append(m, "litellm")
	}
	return m
}

// worldManaged is managedForFlags plus the k3s-guard: a k3s LXC that a prior
// run recorded (vmid present) stays in the manifest even when this run opted
// it out --no-k3s, so teardown still owns it. litellm needs no such carve-out —
// it is a kube workload with no guest of its own; its pods ride the k3s
// guest's teardown.
func worldManaged(noK3s, noLitellm bool, k3sVmid *uint32) []string {
	m := managedForFlags(noK3s, noLitellm)
	if noK3s && k3sVmid != nil && !containsStr(m, "k3s") {
		m = append(m, "k3s")
	}
	return m
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// writeInitialConfig lands the config BEFORE storage (stage_storage bails
// without one) — merging any surviving config's facts.
func (e *rebuildEngine) writeInitialConfig() error {
	prev, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	return mergeFromAnswers(e.fromAnswers(), prev).Save(e.f.configPath)
}

// finalSave is the end-of-pipeline MERGE save (fresh load; mid-pipeline
// facts live on disk).
func (e *rebuildEngine) finalSave() error {
	prev, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	cfg := mergeFromAnswers(e.fromAnswers(), prev)
	// The rebuild OWNS the world manifest: managed = the pieces THIS run
	// deployed. A k3s LXC recorded by a PRIOR run is still ours to tear
	// down even when this run opted it out (`--no-k3s`) — dropping it
	// would make teardown say "skipped k3s LXC (not managed)" and leak the
	// guest + its thin LV forever.
	cfg.Managed = worldManaged(e.f.noK3s, e.f.noLitellm, cfg.Lxc.K3s.Vmid)
	if rpk, ok := e.relayPubkeyNip11(); ok {
		cfg.RelayPubkey = &rpk
	}
	return cfg.Save(e.f.configPath)
}

// ---- the storage stage -----------------------------------------------------

// stagePlacement runs `storage resolve` and applies the plane-placement
// gate: the tenant LVs must land in a named thin pool — reuse the VG's
// detected one, or carve a dedicated new pool (then at --pool-size-gb and
// recorded as freehold-created). The --thin-pool flag answers it headless;
// without it the interactive pipeline prompts, and --yes takes the
// reuse-detected / carve-default path.
func (e *rebuildEngine) stagePlacement() (*placement, error) {
	resolveArgs := []string{"storage", "resolve",
		"--addr", e.f.addr, "--agent-dir", rbOpsDir(), "--target", e.f.target}
	if e.f.confirmStorage {
		resolveArgs = append(resolveArgs, "--confirm-storage")
	}
	ok, out := e.runBin(e.bins.Self, resolveArgs)
	if !ok {
		return nil, fmt.Errorf("storage resolution failed:\n%s", out)
	}
	pool := parseStoragePool(out)
	detected, isLvm := parseStorageThinPools(out)

	// The STORAGE-THINPOOL line exists only on the LVM-thin backend; its
	// absence is ZFS, where the placement gate does not apply (datasets
	// carve themselves). A named --thin-pool on ZFS is an operator error.
	if !isLvm {
		if e.f.thinPool != "" {
			return nil, fmt.Errorf("--thin-pool applies only to the LVM-thin backend; this host resolved %q (ZFS)", pool)
		}
		return &placement{pool: pool}, nil
	}
	has := func(name string) bool {
		for _, d := range detected {
			if d == name {
				return true
			}
		}
		return false
	}

	// The operator named the pool up front: adopt if it ALREADY EXISTS
	// (membership in the VG's pool set — the honest probe), carve at
	// --pool-size-gb when it doesn't. No flag + no pool yet: carve the
	// default. Deriving `created` from a NAME-vs-FIRST-POOL comparison is
	// wrong in a multi-pool VG (a named existing second pool would be
	// misreported created=true and recorded for teardown --data).
	if e.f.thinPool != "" || len(detected) == 0 {
		name := e.f.thinPool
		if name == "" {
			name = drive.FreshThinPool
		}
		return &placement{pool: pool, thinPool: name, created: !has(name)}, nil
	}
	if e.f.yes {
		return &placement{pool: pool, thinPool: detected[0], created: false}, nil
	}

	first := detected[0]
	fmt.Fprintf(e.out, `
  ─ plane placement ───────────────────────────────────────
  VG %s currently holds the thin pool %q.
  The tenant LVs need a thin pool to live in:
    r      reuse it
    <name> carve a NEW dedicated pool of that name (%d GB)
  ─────────────────────────────────────────────────────────
`, pool, first, e.f.poolSizeGB)
	answer, err := e.prompt("pool choice [r = reuse / type a new pool name]")
	if err != nil {
		return nil, err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" || answer == "r" || answer == "R" || answer == first {
		return &placement{pool: pool, thinPool: first, created: false}, nil
	}
	// A name the operator types is an ADOPT if it already exists in the VG
	// (membership probe), a CARVE otherwise — same rule as the flag path.
	if has(answer) {
		return &placement{pool: pool, thinPool: answer, created: false}, nil
	}
	sizeAnswer, err := e.prompt(fmt.Sprintf("new pool %q size GB (blank = %d)", answer, e.f.poolSizeGB))
	if err != nil {
		return nil, err
	}
	size, err := parseGB(sizeAnswer, e.f.poolSizeGB, "new thin-pool size GB")
	if err != nil {
		return nil, err
	}
	e.f.poolSizeGB = size
	return &placement{pool: pool, thinPool: answer, created: true}, nil
}

// stageStorage ensures each tenant's dataset onto the placement gate's
// pool and records the mapping into the config (Rust stage_storage —
// loads the config FRESH, bails when absent).
func (e *rebuildEngine) stageStorage(placement *placement) error {
	pool := placement.pool

	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s — cannot record the durable-plane mapping", e.f.configPath)
	}

	// tenant -> LXC role (the k3s role rides the k3s-volumes tenant).
	roleFor := [][2]string{{"relay", "relay"}, {"cp", "cp"}, {"k3s-volumes", "k3s"}}
	resolvedAny := false
	for _, tr := range roleFor {
		tenant, role := tr[0], tr[1]
		ensureArgs := []string{"storage", "ensure",
			"--addr", e.f.addr, "--agent-dir", rbOpsDir(),
			"--target", e.f.target,
			"--tenant", tenant,
			"--domain", e.f.relayDomain,
			"--pool", pool,
			"--size-gb", strconv.FormatUint(e.f.sizeGB, 10),
			"--pool-size-gb", strconv.FormatUint(e.f.poolSizeGB, 10),
		}
		// Honor the RECORDED backend kind: on a host with BOTH a zpool and a
		// VG, re-detection would always pick ZFS and drive an LVM-backed
		// tenant the wrong way.
		if cfg.Plane.BackendKind != nil && *cfg.Plane.BackendKind != "" {
			ensureArgs = append(ensureArgs, "--kind", *cfg.Plane.BackendKind)
		}
		if placement.thinPool != "" {
			ensureArgs = append(ensureArgs, "--thin-pool", placement.thinPool)
		}
		ok, out := e.runBin(e.bins.Self, ensureArgs)
		if !ok {
			return fmt.Errorf("storage ensure %s failed:\n%s", tenant, out)
		}
		if mounts := parseStorageMounts(out); len(mounts) > 0 {
			p := pool
			cfg.Plane.Backend = &p
			if cfg.Plane.Mounts == nil {
				cfg.Plane.Mounts = map[string][]config.PlaneMount{}
			}
			cfg.Plane.Mounts[role] = mounts
			resolvedAny = true
		}
		if kind := parseStorageBackend(out); kind != "" {
			k := kind
			cfg.Plane.BackendKind = &k
		}
	}
	if placement.created {
		tp := placement.thinPool
		cfg.Plane.ThinPool = &tp
	}
	if resolvedAny {
		_ = cfg.Save(e.f.configPath)
	} else if e.f.confirmStorage {
		return fmt.Errorf("storage resolve/ensure recorded no mounts — resolve said create but ensure produced none")
	}
	if placement.created {
		if err := e.stageLocalLvmRepoint(placement); err != nil {
			return err
		}
	}
	return nil
}

// stageLocalLvmRepoint keeps PVE's stock local-lvm storage pointed at the
// pool freehold just carved. `pct create --rootfs local-lvm:…` (both LXC
// boots) resolves through storage.cfg — after the operator wiped the VG's
// only thin pool the carve leaves local-lvm dangling unless we re-point
// it here. The probe/edit/readback discipline lives ONCE in
// drive.RepointLocalLvm (shared with teardown's RemoveThinPool); this
// method only supplies the subprocess transport. Idempotent: the probe
// skips an already-correct pointer.
func (e *rebuildEngine) stageLocalLvmRepoint(placement *placement) error {
	run := func(script string, timeoutS uint64) (string, error) {
		ok, out := e.runBin(e.bins.Self, e.execArgs(script, int(timeoutS)))
		if !ok {
			return out, fmt.Errorf("local-lvm storage.cfg step failed on %s:\n%s", e.f.host, out)
		}
		return out, nil
	}
	return drive.RepointLocalLvm(run, placement.thinPool)
}

// placement is the plane-placement gate's resolved answer.
type placement struct {
	pool     string // the backend name (LVM: the VG)
	thinPool string // the thin pool the tenant LVs land in ("" on ZFS)
	created  bool   // true => freehold carves it (recorded for teardown --data)
}

// parseStoragePool reads `STORAGE-POOL: <name>` (default rpool).
func parseStoragePool(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(l, "STORAGE-POOL: "); ok {
			if p := strings.TrimSpace(rest); p != "" {
				return p
			}
		}
	}
	return "rpool"
}

// parseStorageThinPools reads `STORAGE-THINPOOL: <name>[,<name>…]`, emitted
// by the LVM-thin backend only ("-" = the VG has no thin pool yet). The
// FULL list is the placement gate's adopt-or-carve probe: a named pool that
// matches ANY member is adopted (created=false); a name matching NONE is
// carved. Comparing against only the first pool misreports an existing
// second pool as created — and teardown --data would then destroy an
// operator-owned pool. lvm is true only when the line is present — its
// ABSENCE means the backend is not LVM-thin (ZFS), and the gate does not
// apply.
func parseStorageThinPools(out string) (pools []string, lvm bool) {
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(l, "STORAGE-THINPOOL: "); ok {
			p := strings.TrimSpace(rest)
			if p != "-" && p != "" {
				pools = strings.Split(p, ",")
			}
			return pools, true
		}
	}
	return nil, false
}

// parseStorageMounts reads the `STORAGE-MOUNT <src>:<guest>` lines.
func parseStorageMounts(out string) []config.PlaneMount {
	var mounts []config.PlaneMount
	for _, l := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(l, "STORAGE-MOUNT ")
		if !ok {
			continue
		}
		src, guest, ok := strings.Cut(rest, ":")
		if ok && src != "" && guest != "" {
			mounts = append(mounts, config.PlaneMount{Source: src, GuestPath: guest})
		}
	}
	return mounts
}

// parseStorageBackend reads the kind from `STORAGE-BACKEND: <kind> <pool>`.
// A present-but-empty value (malformed resolve/ensure output) is treated as
// absent rather than panicking on the field index.
func parseStorageBackend(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(l, "STORAGE-BACKEND: "); ok {
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				return ""
			}
			return fields[0]
		}
	}
	return ""
}

// stageBootstrap boots the role's LXC via THIS binary's bootstrap driver;
// durable-plane mounts are baked from the config FRESH-loaded (born at
// create — the reuse path skips re-baking). A per-role --lxc-ip rides the
// bootstrap --lxc-ip/--lxc-gw pair (Rust: a SPECIFIED ip is STATIC — the
// operator owns the addressing + the proxy target); absent => DHCP and the
// real coordinate is read back + recorded after boot.
func (e *rebuildEngine) stageBootstrap(role string) error {
	args := []string{"bootstrap",
		"--kind", "proxmox-lxc",
		"--role", role,
		"--target", e.f.target,
		"--domain", e.f.relayDomain,
		"--rootfs-gb", strconv.FormatUint(uint64(e.f.rootfsGB), 10),
		"--memory-mb", strconv.FormatUint(uint64(e.f.memoryMB), 10),
		"--operator-pubkey", e.f.operatorPubkey,
	}
	cfg, _ := config.Load(e.f.configPath)
	if ip := bootstrapStaticIP(role, e.f, cfg); ip != "" {
		args = append(args, "--lxc-ip", ip, "--lxc-gw", e.f.relayGw)
	}
	if cfg != nil {
		// A RECORDED vmid rides on resume: the driver's reuse path then finds
		// the existing guest (hostname match) instead of picking a new id and
		// refusing the collision — Rust rebuild re-booted the SAME vmid.
		g := map[string]config.LxcGuest{"relay": cfg.Lxc.Relay, "cp": cfg.Lxc.Cp, "k3s": cfg.Lxc.K3s}[role]
		if g.Vmid != nil {
			args = append(args, "--vmid", strconv.FormatUint(uint64(*g.Vmid), 10))
		}
		for _, m := range cfg.Plane.Mounts[role] {
			args = append(args, "--mount", m.Source+":"+m.GuestPath)
		}
	}
	_, err := e.selfStage("booting the "+role+" LXC", args)
	return err
}

// bootstrapStaticIP returns the role's STATIC address, OR "" = DHCP. With a
// single static proxy now, ONLY the proxy node ("k3s") is static (f.proxyIP,
// or the recorded cfg.Proxy.Ip riding again); relay + cp are always DHCP.
func bootstrapStaticIP(role string, f rebuildFlags, cfg *config.Config) string {
	if role != "k3s" {
		return "" // relay/cp are behind the proxy
	}
	if f.proxyIP != "" {
		return f.proxyIP
	}
	if cfg != nil && cfg.Proxy.Ip != nil {
		return *cfg.Proxy.Ip
	}
	return ""
}

// stageRecordLxc persists the real post-boot coordinates into the config ON
// DISK: load FRESH, resolve vmid+ip through the runner, mutate, save, return
// the merged result (Rust record_lxc's fresh-load discipline).
func (e *rebuildEngine) stageRecordLxc(role string) (*config.Config, error) {
	return e.recordLxcWith(role, func(role string) (uint32, string, error) {
		vmid, err := e.findLxcVmidExact(role)
		if err != nil {
			return 0, "", err
		}
		ip, err := e.readLxcIP(vmid)
		if err != nil {
			return 0, "", err
		}
		return vmid, ip, nil
	})
}

// recordLxcWith is the seam that lets the load→mutate→save discipline be
// tested hermetically (Rust record_lxc_with).
func (e *rebuildEngine) recordLxcWith(role string, resolve func(string) (uint32, string, error)) (*config.Config, error) {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("no config at %s — cannot record the %s LXC's coordinates", e.f.configPath, role)
	}
	vmid, ip, err := resolve(role)
	if err != nil {
		return nil, err
	}
	applyLxcCoords(cfg, role, vmid, ip)
	if err := cfg.Save(e.f.configPath); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyLxcCoords is the mutation half of a write-back: one guest's
// coordinates + the managed piece it implies (Rust apply_lxc_coords).
func applyLxcCoords(cfg *config.Config, role string, vmid uint32, ip string) {
	var guest *config.LxcGuest
	switch role {
	case "relay":
		guest = &cfg.Lxc.Relay
	case "k3s":
		guest = &cfg.Lxc.K3s
	default:
		guest = &cfg.Lxc.Cp
	}
	guest.Vmid = &vmid
	guest.Ip = &ip
	if role == "k3s" && !containsStr(cfg.Managed, "k3s") {
		cfg.Managed = append(cfg.Managed, "k3s")
	}
}

// lxcName is the guest's FULL name: <domain-with-dashes>-<role>.
func lxcName(domain, role string) string {
	return strings.ReplaceAll(domain, ".", "-") + "-" + role
}

// findLxcVmidExact finds the role's vmid by FULL name match on `pct list`
// (a suffix-only match can hit ANOTHER world's container on a multi-world
// host).
func (e *rebuildEngine) findLxcVmidExact(role string) (uint32, error) {
	exact := lxcName(e.f.relayDomain, role)
	ok, out := e.runBin(e.bins.Self, e.execArgs("pct list", 0))
	if !ok {
		return 0, fmt.Errorf("pct list unreadable through the runner:\n%s", out)
	}
	return findVmidInList(out, exact)
}

// findVmidInList parses a `pct list` dump for an exact-name row.
func findVmidInList(out, exact string) (uint32, error) {
	lines := strings.Split(out, "\n")
	for _, line := range lines[1:] {
		cols := strings.Fields(line)
		if len(cols) < 2 || cols[len(cols)-1] != exact {
			continue
		}
		vmid, err := strconv.ParseUint(cols[0], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("unparseable vmid %q in %q", cols[0], line)
		}
		return uint32(vmid), nil
	}
	return 0, fmt.Errorf("no container named %s found on the host:\n%s", exact, out)
}

// readLxcIP reads the guest's current IPv4 (CIDR) off eth0.
func (e *rebuildEngine) readLxcIP(vmid uint32) (string, error) {
	ok, out := e.runBin(e.bins.Self, e.execArgs(fmt.Sprintf("pct exec %d -- ip -4 -o addr show eth0", vmid), 0))
	if !ok {
		return "", fmt.Errorf("ip readback failed on LXC %d:\n%s", vmid, out)
	}
	return parseLxcIP(out, vmid)
}

// parseLxcIP extracts the first `/`-token, refusing loopback (Rust
// read_lxc_ip).
func parseLxcIP(out string, vmid uint32) (string, error) {
	for _, t := range strings.Fields(out) {
		if strings.Contains(t, "/") {
			if t == "127.0.0.1/8" {
				break
			}
			return t, nil
		}
	}
	return "", fmt.Errorf("no ipv4 on LXC %d eth0:\n%s", vmid, out)
}

// ---- the k3s stage -----------------------------------------------------------

// k3sInstallScript is the in-guest install script, verbatim from the Rust
// installer (single-quote-free: it travels inside a single-quoted bash -c
// through the runner; the unit heredoc is unquoted-safe).
const k3sInstallScript = `set -euo pipefail
export PATH=/usr/local/bin:/root/.cargo/bin:$PATH
DEBIAN_FRONTEND=noninteractive apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl jq
if ! command -v kubectl >/dev/null 2>&1; then
  curl -sfL https://get.k3s.io -o /tmp/k3s-install.sh
  INSTALL_K3S_EXEC="server --disable traefik --disable servicelb --kubelet-arg feature-gates=KubeletInUserNamespace=true" sh /tmp/k3s-install.sh
fi
if ! grep -q KubeletInUserNamespace /etc/systemd/system/k3s.service 2>/dev/null; then
cat > /etc/systemd/system/k3s.service <<UNIT
[Unit]
Description=Lightweight Kubernetes
Documentation=https://k3s.io
Wants=network-online.target
After=network-online.target
[Install]
WantedBy=multi-user.target
[Service]
Type=notify
EnvironmentFile=-/etc/default/%N
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k3s server --disable traefik --disable servicelb --kubelet-arg feature-gates=KubeletInUserNamespace=true
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
UNIT
  systemctl daemon-reload
  systemctl restart k3s
fi
KUBECTL=$(command -v kubectl)
K="$KUBECTL --kubeconfig /etc/rancher/k3s/k3s.yaml"
for i in $(seq 1 30); do
  $K get nodes >/dev/null 2>&1 && break
  sleep 10
done
$K get nodes 2>&1 | tail -2 | head -1
mkdir -p /srv/data/k8s-volumes
`

// stageK3s boots the k3s LXC if missing, installs k3s inside it
// (unprivileged-LXC posture: KubeletInUserNamespace), and records the guest
// coords + the managed piece (best-effort write-back, Rust stage_k3s).
func (e *rebuildEngine) stageK3s() error {
	// boot if missing: find by NAME first, then probe.
	if vmid, err := e.findLxcVmidExact("k3s"); err != nil {
		if err := e.stageBootstrap("k3s"); err != nil {
			return err
		}
	} else if exists, perr := e.probeLxc(vmid); perr != nil {
		return perr
	} else if !exists {
		if err := e.stageBootstrap("k3s"); err != nil {
			return err
		}
	}
	vmid, err := e.findLxcVmidExact("k3s")
	if err != nil {
		return err
	}
	// install k3s in the guest when absent.
	ok, out := e.runBin(e.bins.Self, e.execArgs(fmt.Sprintf("pct exec %d -- command -v k3s", vmid), 0))
	if (!ok || strings.TrimSpace(out) == "") && !strings.Contains(out, "/usr/local/bin/k3s") {
		ok, out := e.runBin(e.bins.Self, e.execArgs(
			fmt.Sprintf("pct exec %d -- bash -c '%s'", vmid, strings.TrimSpace(k3sInstallScript)), 900))
		if !ok {
			return fmt.Errorf("k3s install failed:\n%s", out)
		}
	}
	// best-effort write-back: a failed record must not sink the stage.
	_, _ = e.stageRecordLxc("k3s")
	return nil
}

// probeLxc reports whether the vmid's LXC is present on the host.
func (e *rebuildEngine) probeLxc(vmid uint32) (bool, error) {
	ok, out := e.runBin(e.bins.Self, e.execArgs(fmt.Sprintf("pct status %d", vmid), 0))
	if ok {
		return true, nil
	}
	if strings.Contains(out, "does not exist") {
		return false, nil
	}
	return false, fmt.Errorf("pct status %d unreadable through the runner:\n%s", vmid, out)
}

// ---- the deploy stages --------------------------------------------------------

// stageDeployRelay deploys the Buzz relay into the relay LXC; the deploy dir
// follows the guest's ACTUAL mounts (never a hardcoded default).
func (e *rebuildEngine) stageDeployRelay() error {
	vmid, err := e.findLxcVmidExact("relay")
	if err != nil {
		return err
	}
	deployDir := ""
	for _, g := range e.guestMounts(vmid) {
		if g != "/var/lib/docker" {
			deployDir = g
			break
		}
	}
	args := []string{"deploy-relay",
		"--target", e.f.target,
		"--lxc", strconv.FormatUint(uint64(vmid), 10),
		"--domain", e.f.relayDomain,
		"--relay-url", "https://" + e.f.relayDomain,
		"--owner-pubkey", e.f.operatorPubkey,
		"--operator-pubkey", e.f.operatorPubkey,
	}
	if deployDir != "" {
		args = append(args, "--deploy-dir", deployDir)
	}
	_, err = e.selfStage("deploy-relay", args)
	return err
}

// stageDeployCp deploys the control plane into the cp LXC from the RELEASE
// binaries; state/bin dirs follow the guest's last mount.
func (e *rebuildEngine) stageDeployCp() error {
	vmid, err := e.findLxcVmidExact("cp")
	if err != nil {
		return err
	}
	mounts := e.guestMounts(vmid)
	var cpRoot string
	if len(mounts) > 0 {
		cpRoot = mounts[len(mounts)-1]
	}
	args := []string{"deploy-cp",
		"--target", e.f.target,
		"--lxc", strconv.FormatUint(uint64(vmid), 10),
		"--relay-url", "https://" + e.f.relayDomain,
		"--binary", e.bins.ReleaseCP,
		"--runner-binary", e.bins.ReleaseRun,
		"--operator-pubkey", e.f.operatorPubkey,
	}
	if cpRoot != "" {
		args = append(args, "--state-dir", cpRoot+"/control-plane", "--bin-dir", cpRoot+"/bin")
	}
	_, err = e.selfStage("deploy-cp", args)
	return err
}

// guestMounts reads the `mp=` guest paths of the LXC's pct config (Rust
// guest_mounts) — ground truth for where PVE binds the durable dataset.
func (e *rebuildEngine) guestMounts(vmid uint32) []string {
	ok, out := e.runBin(e.bins.Self, e.execArgs(fmt.Sprintf("pct config %d", vmid), 0))
	if !ok {
		return nil
	}
	return parsePctMounts(out)
}

// parsePctMounts extracts the `mp=` guest paths of `mp<digits>:` lines, in
// order (a loose `mp` prefix would catch unrelated keys).
func parsePctMounts(out string) []string {
	var mounts []string
	for _, l := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(l, "mp")
		if !ok {
			continue
		}
		idx, rest, ok := strings.Cut(rest, ":")
		if !ok || idx == "" {
			continue
		}
		digits := true
		for _, c := range idx {
			if c < '0' || c > '9' {
				digits = false
				break
			}
		}
		if !digits {
			continue
		}
		for _, kv := range strings.Split(rest, ",") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(kv), "mp="); ok {
				mounts = append(mounts, v)
				break
			}
		}
	}
	return mounts
}

// cpBinDir resolves the DEPLOYED control-plane binary + state dir inside the
// cp LXC (the same mounts stageDeployCp uses): the last plane mount is the CP
// root, bin/ + control-plane/ live under it.
func (e *rebuildEngine) cpGuestDirs() (binDir, stateDir string, err error) {
	vmid, err := e.findLxcVmidExact("cp")
	if err != nil {
		return "", "", err
	}
	mounts := e.guestMounts(vmid)
	if len(mounts) == 0 {
		return "", "", fmt.Errorf("cp LXC has no plane mount — cannot find the deployed CP")
	}
	root := mounts[len(mounts)-1]
	return root + "/bin", root + "/control-plane", nil
}

// stageCpExec runs a command via the DEPLOYED CP binary inside its LXC
// (through the runner's pct exec): `pct exec <cp> -- <bin>/control-plane ARGS
// --state-dir <state>`. The pattern deploy-cp already uses for adopt/grant.
func (e *rebuildEngine) stageCpExec(cpBinArgs ...string) (string, error) {
	binDir, stateDir, err := e.cpGuestDirs()
	if err != nil {
		return "", err
	}
	vmid, err := e.findLxcVmidExact("cp")
	if err != nil {
		return "", err
	}
	// cpBinArgs[0] is the PARENT subcommand (e.g. dns) and --state-dir lives
	// on IT, before its sub-subcommand. The caller passes the full chain
	// ("dns", "add", ...), so quote each and insert --state-dir after the
	// parent without duplicating it.
	quoted := make([]string, len(cpBinArgs))
	for i, a := range cpBinArgs {
		quoted[i] = shellQuote(a)
	}
	inner := fmt.Sprintf("'%s/control-plane' %s --state-dir '%s' %s",
		binDir, quoted[0], stateDir, strings.Join(quoted[1:], " "))
	cmd := fmt.Sprintf("pct exec %d -- sh -c %s", vmid, shellQuote(inner))
	ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 120))
	if !ok {
		return "", fmt.Errorf("in-LXC cp command failed:\n%s", out)
	}
	return out, nil
}

// escapeSingle makes a value safe inside a single-quoted shell fragment
// (close-quote, quoted quote, reopen) — used for the multi-line corefile.
func escapeSingle(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}

// shellQuote single-quotes a string for sh (no embedded single quotes in the
// values we pass — names/IPs are validated before reaching here).
func shellQuote(s string) string {
	return "'" + s + "'"
}

// stageDnsRegister records the resolver's EXPLICIT names inside the deployed
// CP: relay/cp/k3s (their live coords) + litellm (the k3s node) — the same
// trigger points that write the LXC coords. Idempotent (upsert).
func (e *rebuildEngine) stageDnsRegister() error {
	type rec struct{ name, ip, source string }
	var recs []rec
	cfg, _ := config.Load(e.f.configPath)
	// The guests' resolv.conf carries PVE's `search` line; register it as the
	// resolver's world domain so addn-hosts serves <name>.<search> FIRST (the
	// glibc search-first lookup gets the split-horizon answer, not the
	// public/tailscale record through upstream).
	searchBase := e.guestSearchBase()
	if cfg != nil {
		for _, role := range []string{"relay", "cp"} {
			g := map[string]config.LxcGuest{"relay": cfg.Lxc.Relay, "cp": cfg.Lxc.Cp}[role]
			if g.Ip != nil {
				recs = append(recs, rec{name: role, ip: config.StripCIDR(*g.Ip), source: "record_lxc " + role})
			}
		}
		// The proxy node (k3s) is the ONE static address.
		if cfg.Proxy.Ip != nil {
			recs = append(recs, rec{name: "proxy", ip: config.StripCIDR(*cfg.Proxy.Ip), source: "proxy-static"})
			recs = append(recs, rec{name: "k3s", ip: config.StripCIDR(*cfg.Proxy.Ip), source: "proxy-static"})
		}
		if cfg.Litellm.Host != "" {
			recs = append(recs, rec{name: "litellm", ip: cfg.Litellm.Host, source: "litellm-apply"})
		}
		// Split horizon: the relay + CP hosts resolve to the PROXY (Caddy),
		// which fronts TLS and reverse-proxies to the LXCs behind it — never
		// directly to a LXC. Both are explicit dotted records.
		if rh := cfg.RelayHost(); rh != "" && cfg.Proxy.Ip != nil {
			recs = append(recs, rec{name: rh, ip: config.StripCIDR(*cfg.Proxy.Ip), source: "relay-via-proxy"})
		}
		if ch := cfg.CPHost(); ch != "" && cfg.Proxy.Ip != nil {
			recs = append(recs, rec{name: ch, ip: config.StripCIDR(*cfg.Proxy.Ip), source: "cp-via-proxy"})
		}
	}
	// Configure the mirror BEFORE the remote sync: if the CP is unreachable
	// the config still records the intent (the panel + teardown see it; a
	// re-run re-syncs the resolver).
	if cfg != nil {
		if cfg.Dns.Records == nil {
			cfg.Dns.Records = map[string]string{}
		}
		for _, r := range recs {
			cfg.Dns.Records[r.name] = r.ip
		}
		if err := cfg.Save(e.f.configPath); err != nil {
			return err
		}
	}
	if len(recs) == 0 {
		return nil
	}
	for _, r := range recs {
		args := []string{"dns", "add", r.name, r.ip, r.source}
		if searchBase != "" {
			args = append(args, "--domain", searchBase)
		}
		if _, err := e.stageCpExec(args...); err != nil {
			return err
		}
	}
	return nil
}

// relayDomainHost is the bare (dotless) label prepended to the resolver's// search base to reproduce cfg.Domain — e.g. `freehold-test` for the domain
// `freehold-test.darcydev.net` under search `darcydev.net`. Empty when the
// domain isn't a subdomain of the search base (no clean split-horizon label
// exists without dotted-record support in the resolver).
func relayDomainHost(domain, searchBase string) string {
	if searchBase == "" || !strings.HasSuffix(domain, "."+searchBase) {
		return ""
	}
	host := strings.TrimSuffix(domain, "."+searchBase)
	if host == "" || strings.Contains(host, ".") {
		return ""
	}
	return host
}

// guestSearchBase reads the `search` line from the CP LXC's resolv.conf (PVE
// writes the same search domain to every guest it manages). Empty when the
// line is absent — the resolver then stays bare-name only.
func (e *rebuildEngine) guestSearchBase() string {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Lxc.Cp.Vmid == nil {
		return ""
	}
	cmd := fmt.Sprintf(
		"pct exec %d -- sh -c \"grep '^search' /etc/resolv.conf | head -1 | cut -d' ' -f2-\"",
		*cfg.Lxc.Cp.Vmid)
	ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 30))
	if !ok {
		return ""
	}
	base := strings.TrimSpace(out)
	if base == "" || strings.ContainsAny(base, " \"'`$;(){}") {
		return ""
	}
	// a single label is not a useful search base
	if !strings.Contains(base, ".") {
		return ""
	}
	return base
}

// guestNameserver returns the CP LXC's dnsmasq UPSTREAM (the router), tried
// in order so BOTH static and DHCP worlds keep external resolution:
//
//  1. the PVE-owned `net0` `gw=` line (`pct config`) — STATIC guests only;
//     the default world (no --cp-ip, DHCP) has no gw= key at all.
//  2. the guest's default route — works on every network shape.
//  3. the CP resolv.conf's first nameserver that is NOT the resolver's own
//     IP (its first entry is commonly the resolver from a prior world; a
//     duplicated `nameserver .9 .9` made dnsmasq ignore its only entry and
//     fed "no upstream" back to every guest pointed at the resolver).
func (e *rebuildEngine) guestNameserver() string {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Lxc.Cp.Vmid == nil {
		return ""
	}
	if gw := e.cpGuestGateway(); gw != "" {
		return gw
	}
	// 2. default route via `cut` — awk '{print $3}' would be expanded by the
	// outer runner shell (same trap that corrupted the earlier probe).
	cmd := fmt.Sprintf(
		"pct exec %d -- sh -c \"ip route show default | head -1 | cut -d' ' -f3\"",
		*cfg.Lxc.Cp.Vmid)
	if ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 30)); ok {
		if ns := strings.TrimSpace(out); ns != "" && strings.ContainsAny(ns, "0123456789") {
			return ns
		}
	}
	// 3. resolv.conf last resort, skipping the resolver's own address.
	own := ""
	if cfg.Lxc.Cp.Ip != nil {
		own = config.StripCIDR(*cfg.Lxc.Cp.Ip)
	}
	cmd = fmt.Sprintf(
		"pct exec %d -- sh -c \"grep '^nameserver' /etc/resolv.conf | cut -d' ' -f2\"",
		*cfg.Lxc.Cp.Vmid)
	if ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 30)); ok {
		for _, l := range strings.Split(out, "\n") {
			ns := strings.TrimSpace(l)
			if ns != "" && ns != own && strings.ContainsAny(ns, "0123456789") {
				return ns
			}
		}
	}
	return ""
}

// cpGuestGateway reads the CP LXC's PVE-owned `net0` `gw=` value (`pct
// config`). Pure for tests: parsePctGateway covers the line format.
func (e *rebuildEngine) cpGuestGateway() string {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Lxc.Cp.Vmid == nil {
		return ""
	}
	ok, out := e.runBin(e.bins.Self, e.execArgs(fmt.Sprintf("pct config %d", *cfg.Lxc.Cp.Vmid), 30))
	if !ok {
		return ""
	}
	return parsePctGateway(out)
}

// parsePctGateway extracts the `net0` `gw=` value from a `pct config` dump.
// Static guests carry `gw=<router>`; DHCP guests (`ip=dhcp`) have NO gw= —
// an empty result routes the caller to the default-route fallback.
func parsePctGateway(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "net0:") {
			continue
		}
		for _, kv := range strings.Split(l, ",") {
			v, found := strings.CutPrefix(kv, "gw=")
			if found {
				v = strings.TrimSpace(v)
				if v != "" && strings.ContainsAny(v, "0123456789") {
					return v
				}
			}
		}
	}
	return ""
}

// stageDnsPoint points every managed guest at the CP resolver: write
// nameserver into each LXC's resolv.conf (idempotent) and set k3s coredns's
// `forward .` to the resolver so pods resolve *.freehold.internal.
func (e *rebuildEngine) stageDnsPoint() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Lxc.Cp.Ip == nil {
		return fmt.Errorf("no control-plane IP recorded — cannot point guests at the resolver")
	}
	resolver := config.StripCIDR(*cfg.Lxc.Cp.Ip)

	// LXCs: the CP resolver becomes each guest's PRIMARY nameserver via
	// `pct set --nameserver` — PVE-managed, durable across guest reboots (a
	// hand-appended line dies on boot). glibc is search-first (ndots=1), and
	// the resolver now serves <name>.<search> records, so e.g. "litellm"
	// resolves to the internal k3s IP, never the public/tailscale record via
	// the router. The CP itself KEEPS the router as a secondary so its
	// dnsmasq still has an upstream for external names.
	searchBase := e.guestSearchBase()
	router := e.guestNameserver()
	for _, role := range []string{"relay", "cp", "k3s"} {
		g := map[string]config.LxcGuest{
			"relay": cfg.Lxc.Relay, "cp": cfg.Lxc.Cp, "k3s": cfg.Lxc.K3s,
		}[role]
		if g.Vmid == nil {
			continue
		}
		nsList := resolver
		if role == "cp" && router != "" {
			nsList += " " + router
		}
		parts := []string{"set", strconv.FormatUint(uint64(*g.Vmid), 10), "--nameserver", nsList}
		if searchBase != "" {
			parts = append(parts, "--searchdomain", searchBase)
		}
		quoted := make([]string, len(parts))
		for i, a := range parts {
			quoted[i] = shellQuote(a)
		}
		cmd := "pct " + strings.Join(quoted, " ")
		fmt.Fprintf(e.out, "  · pct: %s\n", cmd)
		if ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 60)); !ok {
			return fmt.Errorf("pointing %s at the resolver failed:\n%s", role, out)
		}
		// pct only regenerates resolv.conf at the NEXT boot — write it now.
		// Values are validated IPs / charset-checked search base: single-quote
		// at the innermost level only (no nested-quote hang).
		body := "nameserver " + resolver + "\n"
		if role == "cp" && router != "" {
			body += "nameserver " + router + "\n"
		}
		if searchBase != "" {
			body = "search " + searchBase + "\n" + body
		}
		rcmd := fmt.Sprintf(
			"pct exec %d -- sh -c \"printf '%s' > /etc/resolv.conf\"",
			*g.Vmid, body)
		if ok, out := e.runBin(e.bins.Self, e.execArgs(rcmd, 60)); !ok {
			return fmt.Errorf("writing %s resolv.conf failed:\n%s", role, out)
		}
	}

	// Honest gate: the resolver must ANSWER a record from the CP's own
	// loopback, not merely have tcp/53 open. dnsmasq serves addn-hosts only
	// if it could READ the file at start — a 0700 state dir makes it fail
	// silently ("Permission denied") while the port still probes green. The
	// deployed CP's dns sync repairs perms; asking for a real answer proves
	// the whole chain (render -> write -> dnsmasq load) landed.
	relayIP := ""
	if cfg.Lxc.Relay.Ip != nil {
		relayIP = config.StripCIDR(*cfg.Lxc.Relay.Ip)
	}
	litellmIP := cfg.Litellm.Host
	var cpVmid *uint32
	cpVmid = cfg.Lxc.Cp.Vmid
	for _, q := range []struct{ name, want string }{
		{"relay", relayIP},
		{"litellm", litellmIP},
	} {
		if q.want == "" || cpVmid == nil {
			continue
		}
		cmd := fmt.Sprintf(
			"pct exec %d -- sh -c \"dig +short +time=2 +tries=1 %s @127.0.0.1 2>/dev/null | grep -qx '%s'\"",
			*cpVmid, q.name, q.want)
		if ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 30)); !ok {
			return fmt.Errorf("resolver did not answer %s -> %s (dnsmasq addn-hosts load failed?):\n%s",
				q.name, q.want, out)
		}
	}
	return nil
}

// stageLitellm deploys the litellm gateway in two legs:
//
//	Leg 1 (kube workloads, no agent secrets): the orchestrator writes the
//	k8s Secrets (master key + postgres pw are ITS generated material; the
//	provider key is the operator's bootstrap supply) and applies the
//	postgres + litellm manifests inside the k3s LXC via the proxmox runner.
//	Leg 2 (admin call, runner-decrypted): the litellm runner SERVES on
//	loopback with the three-secret package; the model registration curl runs
//	THROUGH it with the secrets requested BY NAME — the runner decrypts,
//	injects env, redacts output (the locked agent-vs-secret shape).
func (e *rebuildEngine) stageLitellm() error {
	runnerDir := filepath.Join(rbRunnerPkgs(), "litellm")

	// The provider key (operator's fireworks/upstream supply) is needed
	// ONCE, at first shipment, to seed the litellm runner — after that it is
	// sealed in the runner package (ciphertext) and reused on rebuilds, so a
	// `freehold build` does not demand a fresh supply every time.
	reusingKey := e.f.litellmProviderKey == "" && litellmHasProviderKey(runnerDir)
	if e.f.litellmProviderKey == "" && !reusingKey {
		if e.f.yes {
			return fmt.Errorf("litellm needs the provider key and none is sealed: --litellm-provider-key or FREEHOLD_LITELLM_PROVIDER_KEY (headless --yes)")
		}
		// Interactive: prompt once for the operator's supply on this cold
		// world (it is sealed into the runner and never asked for again).
		// Uses e.prompt, which reads the engine's ONE shared buffered stdin
		// (a fresh bufio.Reader would drain bytes from the shared pipe — see
		// the stdin field comment).
		answer, err := e.prompt("litellm first provision: the provider (fireworks) API key")
		if err != nil {
			return err
		}
		e.f.litellmProviderKey = strings.TrimSpace(answer)
		if e.f.litellmProviderKey == "" {
			return fmt.Errorf("no litellm provider key supplied")
		}
	}

	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Proxy.Ip == nil || cfg.Lxc.K3s.Vmid == nil {
		return fmt.Errorf("no k3s coords recorded — cannot place the litellm gateway")
	}
	k3sIP := config.StripCIDR(*cfg.Proxy.Ip)
	k3sVmid := *cfg.Lxc.K3s.Vmid
	gwURL := "http://" + k3sIP + ":31400"

	// The gateway master key must MATCH the running gateway's LITELLM_MASTER_KEY
	// (the first-run-wins k8s `litellm-keys` Secret is canonical across
	// rebuilds). Resolve it back and re-seal the runner to it below; only a
	// fresh world (no Secret yet) mints a brand-new one.
	masterKey := e.litellmMasterKey(k3sVmid)
	if masterKey == "" {
		masterKey = genSecretHex()
	}
	postgresPw := genSecretHex()          // postgres password
	providerKey := e.f.litellmProviderKey // "" when reusing the sealed key

	// ---- Leg 1: kube workloads. The k8s Secrets (master + postgres pw) are
	// CP-GENERATED installer material (like deploy flags) — they cross the
	// ssh runner as shell-quoted literals in the script. The OPERATOR's
	// provider key is NOT here: it rides ONLY the runner package (leg 2).
	leg1 := litellmManifestScript(k3sVmid, masterKey, postgresPw, providerKey)
	ok, out := e.runBin(e.bins.Self, e.execArgs(leg1, 420))
	if !ok {
		return fmt.Errorf("litellm kube apply failed:\n%s", out)
	}

	// ---- Leg 2: model registration through the litellm runner. -----------
	agentPK := e.opsAgentPubkey()
	env := append(
		[]string{"FREEHOLD_LITELLM_MASTER=" + masterKey},
		os.Environ()...,
	)
	ok, out = e.runEnv(e.bins.ControlPlane, env, []string{
		"provision", "litellm",
		"--kind", "litellm",
		"--address", gwURL,
		"--state-dir", rbStateDir(),
		"--runner-dir", runnerDir,
		"--grant", agentPK,
		"--secret-env", "FREEHOLD_LITELLM_MASTER",
	})
	if !ok && !isProvisionReuse(out) {
		return fmt.Errorf("provision litellm runner failed:\n%s", out)
	}
	for _, extra := range []struct{ name, env string }{
		{"provider-key", "FREEHOLD_LITELLM_PROVIDER"},
		{"postgres-pw", "FREEHOLD_LITELLM_PG"},
	} {
		val := map[string]string{"provider-key": providerKey, "postgres-pw": postgresPw}[extra.name]
		if val == "" && extra.name == "provider-key" && reusingKey {
			// The provider key is already sealed in the runner package —
			// re-shipping an empty value would clobber nothing but must be
			// skipped so a rebuild reuses the stored one.
			continue
		}
		extraEnv := append(
			[]string{extra.env + "=" + val},
			os.Environ()...,
		)
		if ok, out := e.runEnv(e.bins.ControlPlane, extraEnv, []string{
			"add-secret", "litellm", extra.name,
			"--state-dir", rbStateDir(),
			"--secret-env", extra.env,
		}); !ok {
			return fmt.Errorf("add-secret %s failed:\n%s", extra.name, out)
		}
	}

	// Re-seal the runner's `litellm` secret to the gateway's canonical master
	// every run: `provision` on a reused package is a no-op, so the runner
	// keeps an OLD (possibly divergent) sealed master — exactly the mismatch
	// that makes every admin call 401. Driving it with add-secret re-encrypts
	// the SAME value, keeping $LITELLM in lockstep with the live gateway.
	masterEnv := append(
		[]string{"FREEHOLD_LITELLM_MASTER=" + masterKey},
		os.Environ()...,
	)
	if ok, out := e.runEnv(e.bins.ControlPlane, masterEnv, []string{
		"add-secret", "litellm", "litellm",
		"--state-dir", rbStateDir(),
		"--secret-env", "FREEHOLD_LITELLM_MASTER",
	}); !ok {
		return fmt.Errorf("add-secret litellm (master) failed:\n%s", out)
	}

	// Serve the litellm runner on loopback, exec the registration through it.
	pid, err := e.stageServeRunner("litellm", runnerDir)
	if err != nil {
		return err
	}
	defer e.stopRunner(pid)

	// Register the model THROUGH the litellm runner (its own ciphertext: master
	// + provider-key), against the gateway's real URL.
	ok, out = e.litellmRun(litellmRegisterScript(gwURL), 120, "litellm", "provider-key")
	if !ok {
		return fmt.Errorf("litellm model registration failed:\n%s", out)
	}

	// The CPA's litellm key is the gateway master itself: litellm /key/generate
	// now requires a pre-existing virtual key to mint scoped keys (quadrant
	// blocked until one exists), while the master is accepted for chat AND
	// admin. The pod reads it as its OPENAI_COMPAT_API_KEY from the
	// <pod>-litellm-key Secret (seeded first-run-wins via pct). Minting scoped
	// virtual keys is the named follow-up once a bootstrap virtual key exists.
	cpaName := cfg.CPAName
	if cpaName == "" {
		cpaName = agent.DefaultCPAName
	}
	ok, out = e.runBin(e.bins.Self, e.execArgs(
		agent.AgentLiteLLMKeyScript(k3sVmid, masterKey, cpaName), 60))
	if !ok {
		return fmt.Errorf("seed CPA litellm key secret failed:\n%s", out)
	}

	return e.recordLitellm(gwURL, k3sIP)
}

// litellmManifestScript applies the postgres + litellm kube resources inside
// the k3s LXC: namespace, Secrets (values from exec env), PVC (local-path ->
// the durable plane), deployments, NodePort service. No secrets in argv.
func litellmManifestScript(k3sVmid uint32, masterKey, postgresPw, providerKey string) string {
	sb := strings.ReplaceAll(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec __VMID__ -- sh -c"
$EX "mkdir -p /tmp/litellm-manifests"
$EX "$K create ns litellm 2>/dev/null || true"
# Secrets: CP-generated values are shell-quoted literals (deploy-flag shape);
# the operator's provider key is deliberately absent here (runner-only).
# Secrets are created ONLY when absent: the first run's values are the
# authoritative ones (postgres initializes PGDATA against them, and the
# reused runner package keeps them) — a re-run must never re-roll them.
$EX "$K get secret litellm-keys -n litellm >/dev/null 2>&1 || $K create secret generic litellm-keys -n litellm --from-literal=master-key=__MASTER_ESC__ --from-literal=provider-key=__PROVIDER_ESC__"
$EX "$K get secret litellm-pg -n litellm >/dev/null 2>&1 || $K create secret generic litellm-pg -n litellm --from-literal=postgres-pw=__PG_ESC__"
# Manifests: written HOST-side (this exec runs on the PVE host where pct
# lives), pushed INTO the guest, then applied with the full kubectl path.
mkdir -p /tmp/litellm-manifests
cat >/tmp/litellm-manifests/postgres.yaml <<'YAML'
__POSTGRES__
YAML
cat >/tmp/litellm-manifests/litellm.yaml <<'YAML'
__LITELLM__
YAML
pct push __VMID__ /tmp/litellm-manifests/postgres.yaml /tmp/litellm-manifests/postgres.yaml
pct push __VMID__ /tmp/litellm-manifests/litellm.yaml /tmp/litellm-manifests/litellm.yaml
$EX "$K apply -f /tmp/litellm-manifests/postgres.yaml"
$EX "$K apply -f /tmp/litellm-manifests/litellm.yaml"
$EX "$K rollout status deploy/litellm -n litellm --timeout=300s"
echo LEG1_OK`,
		"__VMID__", strconv.FormatUint(uint64(k3sVmid), 10),
	)
	sb = strings.ReplaceAll(sb, "__POSTGRES__", litellmPostgresManifest)
	sb = strings.ReplaceAll(sb, "__LITELLM__", litellmGatewayManifest)
	sb = strings.ReplaceAll(sb, "__MASTER_ESC__", shQuoteLiteral(masterKey))
	sb = strings.ReplaceAll(sb, "__PG_ESC__", shQuoteLiteral(postgresPw))
	sb = strings.ReplaceAll(sb, "__PROVIDER_ESC__", shQuoteLiteral(providerKey))
	return sb
}

// shQuoteLiteral single-quotes a value for a shell command embedded in the
// exec script (CP-generated secrets only; the operator's keys never come
// here — they ride the runner package).
func shQuoteLiteral(v string) string {
	return "'" + v + "'"
}

// litellmPostgresManifest is the postgres Deployment on the durable plane
// (local-path -> /srv/data/k8s-volumes), password from the k8s Secret.
const litellmPostgresManifest = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: litellm-pg-data
  namespace: litellm
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: local-path
  resources:
    requests:
      storage: 10Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: litellm
spec:
  replicas: 1
  selector:
    matchLabels: {app: postgres}
  template:
    metadata:
      labels: {app: postgres}
    spec:
      containers:
      - name: postgres
        image: postgres:16
        env:
        - {name: POSTGRES_DB, value: litellm}
        - {name: POSTGRES_USER, value: llmproxy}
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef: {name: litellm-pg, key: postgres-pw}
        - {name: PGDATA, value: /var/lib/postgresql/data/pgdata}
        volumeMounts:
        - {name: data, mountPath: /var/lib/postgresql/data}
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: litellm-pg-data
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: litellm
spec:
  selector: {app: postgres}
  ports:
  - {port: 5432}`

// litellmGatewayManifest is the litellm proxy (master key from the k8s
// Secret, fireworks egress pinned, NodePort 31400 for the LAN/agents).
const litellmGatewayManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: litellm
  namespace: litellm
spec:
  replicas: 1
  selector:
    matchLabels: {app: litellm}
  template:
    metadata:
      labels: {app: litellm}
    spec:
      containers:
      - name: litellm
        image: docker.litellm.ai/berriai/litellm:main-stable
        ports:
        - {containerPort: 4000}
        env:
        # The postgres password is the CP-GENERATED value from the litellm-pg
        # Secret — the URL must reference it, never a hardcoded literal.
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef: {name: litellm-pg, key: postgres-pw}
        - {name: DATABASE_URL, value: "postgresql://llmproxy:$(POSTGRES_PASSWORD)@postgres.litellm:5432/litellm"}
        - {name: STORE_MODEL_IN_DB, value: "True"}
        - name: LITELLM_MASTER_KEY
          valueFrom:
            secretKeyRef: {name: litellm-keys, key: master-key}
        readinessProbe:
          httpGet:
            path: /health/liveliness
            port: 4000
          periodSeconds: 10
          failureThreshold: 6
      hostAliases:
      - ip: "35.207.52.96"
        hostnames: ["api.fireworks.ai"]
---
apiVersion: v1
kind: Service
metadata:
  name: litellm
  namespace: litellm
spec:
  type: NodePort
  selector: {app: litellm}
  ports:
  - {port: 4000, targetPort: 4000, nodePort: 31400}`

// litellmRegisterScript registers the model through the litellm runner: the
// runner injects LITELLM (master, Bearer) + PROVIDER_KEY (body) by name. The
// model_name is the ControlPlaneAgent alias the CPA pod talks to (agent.go's
// CpaLiteLLMModel); litellm maps it to the deepseek route behind the scenes.
// litellmRegisterScript registers the model through the litellm runner: the
// runner injects LITELLM (master, Bearer) + PROVIDER_KEY (body) by name, and
// curl's the gateway's REAL URL (gwURL — never 127.0.0.1, which is dead on the
// runner host; the gateway is reached at its k3s-node NodePort). The model_name
// is the ControlPlaneAgent alias the CPA pod talks to (agent.go's
// CpaLiteLLMModel); litellm maps it to the deepseek route behind the scenes.
func litellmRegisterScript(gwURL string) string {
	return fmt.Sprintf(`set -euo pipefail
BODY=$(printf '{"model_name":"%s","litellm_params":{"model":"fireworks_ai/accounts/fireworks/models/deepseek-v4-flash-0731","api_key":"%%s"}}' "$PROVIDER_KEY")
curl -s -m 30 -X POST -H "Authorization: Bearer $LITELLM" -H "Content-Type: application/json" -d "$BODY" "%s/model/new" | head -c 300
echo
echo LEG2_OK
`, agent.CpaLiteLLMModel, gwURL)
}

// litellmHasProviderKey reports whether the litellm runner package already
// carries a sealed provider-key (so a rebuild can reuse it instead of demanding
// a fresh supply). It inspects only the ciphertext map's secret NAMES — never
// any value.
func litellmHasProviderKey(runnerDir string) bool {
	b, err := os.ReadFile(filepath.Join(runnerDir, "secrets.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return false
	}
	_, ok := pkg.Secrets["provider-key"]
	return ok
}

// litellmMasterKey resolves the litellm gateway's ACTUAL admin master key: the
// canonical copy is the first-run-wins k8s `litellm-keys` Secret (it survives
// rebuilds while provisioning reuses the old sealed runner key), so on a reuse
// run we read the canonical one back and re-seal the runner to it — a re-minted
// master would diverge from the running gateway and every admin call would 401.
// Returns "" only when the Secret doesn't exist yet (a fresh run mints it).
func (e *rebuildEngine) litellmMasterKey(k3sVmid uint32) string {
	cmd := fmt.Sprintf(`pct exec %d -- /usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get secret litellm-keys -n litellm -o jsonpath='{.data.master-key}' 2>/dev/null | base64 -d`,
		k3sVmid)
	ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 30))
	if !ok {
		return "" // a read failure is treated as "no canonical master yet" — a fresh run mints one
	}
	return strings.TrimSpace(out)
}

// litellmRun executes a script THROUGH the dedicated litellm runner (served on
// loopback 127.0.0.1:8788, target "litellm"), injecting the named secrets by
// env. This is where the litellm admin calls run: the runner host is this
// machine — from which the gateway URL is reachable — and the secrets
// (litellm = master, provider-key) are the litellm runner package's own
// ciphertext. (Not the main proxmox-box runner, and not a nested
// "exec --target …" prefix — that prefix is a shell no-op the old code leaned
// on and never injected the secrets at all.)
func (e *rebuildEngine) litellmRun(script string, timeoutS int, secrets ...string) (bool, string) {
	args := []string{"exec", "--addr", "127.0.0.1:8788", "--agent-dir", rbOpsDir()}
	if timeoutS > 0 {
		args = append(args, "--timeout", strconv.Itoa(timeoutS))
	}
	for _, s := range secrets {
		args = append(args, "--secret", s)
	}
	args = append(args, "litellm", script)
	return e.runEnv(e.bins.Self, os.Environ(), args)
}

// opsAgentPubkey returns the ops-agent's pubkey (the rebuild's signing
// identity — same as deploy-cp grants).
func (e *rebuildEngine) opsAgentPubkey() string {
	id, err := flows.LoadIdentity(rbOpsDir())
	if err != nil {
		return ""
	}
	pk, _ := id.NostrPubkeyHex()
	return pk
}

// genSecretHex mints a 32-byte random hex secret (master key / postgres pw).
func genSecretHex() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "" // caller validates
	}
	return hex.EncodeToString(b)
}

// recordLitellm writes the gateway coords into the config (litellm section +
// managed), so the Services row + teardown see it.
func (e *rebuildEngine) recordLitellm(url, host string) error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s", e.f.configPath)
	}
	cfg.Litellm = config.LitellmSpec{URL: url, Host: host}
	if !containsStr(cfg.Managed, "litellm") {
		cfg.Managed = append(cfg.Managed, "litellm")
	}
	return cfg.Save(e.f.configPath)
}

// ---- the core Caddy TLS fronting proxy (roadmap/CORE_TLS.md, F2) ----------
//
// Caddy is freehold's own TLS edge installed as a hostNetwork kube Deployment
// on the k3s node. It fronts the relay LXC over TLS using the wildcard cert
// that F3 (embedded lego DNS-01) writes into the caddy-data PVC. This stage
// only deploys the proxy + the relay vhost; the cert issuance/reload is F3.

// recordCaddy persists the proxy's coords into the config (caddy section +
// managed) so the Services row + teardown see it.
func (e *rebuildEngine) recordCaddy(url, host string) error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s", e.f.configPath)
	}
	// Preserve the recorded cert-domain/issuer/expiry — only coords refresh here.
	prev := cfg.Caddy
	cfg.Caddy = config.CaddySpec{
		URL:             url,
		Host:            host,
		RelayCert:       prev.RelayCert,
		CPCert:          prev.CPCert,
		CertIssuer:      prev.CertIssuer,
		RelayLegoDomain: prev.RelayLegoDomain,
		CPLegoDomain:    prev.CPLegoDomain,
	}
	if !containsStr(cfg.Managed, "caddy") {
		cfg.Managed = append(cfg.Managed, "caddy")
	}
	return cfg.Save(e.f.configPath)
}

// caddyManifestScript applies the Caddy kube resources inside the k3s LXC:
// namespace, durable PVC, ConfigMap with the rendered Caddyfile, hostNetwork
// Deployment, NodePort service. No secrets in argv (the Caddyfile is plain).
func caddyManifestScript(k3sVmid uint32, caddyfile string) string {
	script := strings.ReplaceAll(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec __VMID__ -- sh -c"
$EX "$K create ns caddy 2>/dev/null || true"
# The Caddyfile is written HOST-side then pushed in + applied (same shape as
# the litellm/postgres manifests). The certs are NOT here: F3 writes them into
# the caddy-data PVC at /data/tls on each issuance.
mkdir -p /tmp/caddy-manifests
$EX "mkdir -p /tmp/caddy-manifests"
cat >/tmp/caddy-manifests/caddy.yaml <<'YAML'
__CADDY__
YAML
pct push __VMID__ /tmp/caddy-manifests/caddy.yaml /tmp/caddy-manifests/caddy.yaml
$EX "$K apply -f /tmp/caddy-manifests/caddy.yaml"
$EX "$K rollout status deploy/caddy -n caddy --timeout=120s || true"
echo CADDY_OK`,
		"__VMID__", strconv.FormatUint(uint64(k3sVmid), 10),
	)
	script = strings.ReplaceAll(script, "__CADDY__", deploy.CaddyManifest(caddyfile))
	return script
}

// stageCaddy deploys the core TLS fronting proxy + the relay vhost. It needs
// the world domain + the relay LXC IP (the upstream) + the k3s substrate.
// Idempotent (apply + record). Runs whenever k3s is on (a relay-fronting edge
// is part of the desired world; cert issuance in F3).
func (e *rebuildEngine) stageCaddy() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s", e.f.configPath)
	}
	if cfg.Lxc.K3s.Vmid == nil {
		return fmt.Errorf("no k3s coords recorded — Caddy needs the k3s substrate")
	}
	if cfg.RelayHost() == "" {
		return fmt.Errorf("no relay domain in config — Caddy fronts the relay's own host")
	}
	if cfg.Lxc.Relay.Ip == nil {
		return fmt.Errorf("no relay LXC coords recorded — Caddy fronts the relay at its LAN IP")
	}
	if cfg.Lxc.Cp.Ip == nil {
		return fmt.Errorf("no cp LXC coords recorded — Caddy fronts the control plane at its LAN IP")
	}
	k3sVmid := *cfg.Lxc.K3s.Vmid
	relayIP := config.StripCIDR(*cfg.Lxc.Relay.Ip)
	relayUpstream := fmt.Sprintf("%s:3000", relayIP)
	cpIP := config.StripCIDR(*cfg.Lxc.Cp.Ip)
	cpUpstream := fmt.Sprintf("%s:8080", cpIP)
	// The relay + CP hosts from the config (never derived).
	relayHost := cfg.RelayHost()
	cpHost := cfg.CPHost()
	caddyfile := deploy.RenderCaddyfile(relayHost, relayUpstream, cpHost, cpUpstream)
	ok, out := e.runBin(e.bins.Self, e.execArgs(caddyManifestScript(k3sVmid, caddyfile), 180))
	if !ok {
		return fmt.Errorf("caddy kube apply failed:\n%s", out)
	}
	if err := e.recordCaddy(cfg.RelayURL, config.StripCIDR(*cfg.Proxy.Ip)); err != nil {
		return err
	}
	return nil
}

// ---- the wildcard cert stage (F3: embedded lego DNS-01) ---------------------
//
// Issues / reuses the relay wildcard cert for the Caddy edge. The DNS provider
// token is sealed to the ops identity (freehold's own encryption key) and held
// in the durable state dir; lego runs in-process. Reconcile-always: a valid
// cert already on the PVC (>= 30d left) is reused and DNS-01 is skipped.

// certCredPath is the sealed DNS provider credential for a slot ("relay" or
// "cp") — kept SEPARATE (they may differ), durable.
func (e *rebuildEngine) certCredPath(slot string) string {
	return filepath.Join(rbStateDir(), "dns-provider-"+slot+".json")
}

// clearStoredDNSCreds removes the stored DNS provider credentials (per-slot +
// the legacy single-file copy) so the build prompts for them again. Used by
// --reset-dns when the stored credential is stale/wrong.
func clearStoredDNSCreds() {
	dir := rbStateDir()
	for _, name := range []string{"dns-provider-relay.json", "dns-provider-cp.json", "dns-provider.json"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// certIdentSecret returns the ops identity's encryption secret (raw bytes), the
// identity, or an error. The ops identity is freehold's own — the only key that
// must be able to reopen the sealed DNS token (lego runs in-process, not in a
// runner).
func (e *rebuildEngine) certIdent() (*flows.Identity, []byte, []byte, error) {
	id, err := flows.LoadIdentity(rbOpsDir())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("no ops identity for cert storage: %w", err)
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ops identity enc secret: %w", err)
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return nil, nil, nil, err
	}
	return id, secret, pub, nil
}

// certRun is one in-flight LE DNS-01 issuance. Pursued as a RESUMABLE order:
// the challenge TXT is placed early (so a slow DNS provider has the whole build
// to serve it) and the ACME order identity is persisted, so a re-run after a
// timeout can RESUME the same order and re-check whether the already-placed
// challenge finally landed — instead of re-challenging everything from zero.
type certRun struct {
	slot      string            // owning slot (for issuer messaging)
	leDomain  string            // effective LE cert-domain (may be "*.base")
	challenge string            // full challenge fqdn: _acme-challenge.<target>
	reuse     bool              // a valid cert already exists — skip issuance
	reuseExp  time.Time         // the reused cert's expiry (reuse==true)
	prepared  chan struct{}     // closed once the resume handle is ready
	resume    *cert.Resume      // the resumable-driver handle
	pending   *cert.PendingOrder // the begun/resumed ACME order
	issued    *cert.Issued
	err       error
}

// startCertIssuance kicks off the edge's DNS-01 issuance for EACH host in the
// background NOW: it begins (or RESUMES) a resumable ACME order and pre-places
// its challenge TXT, so a slow DNS provider (e.g. a freshly-activated Cloudflare
// zone that takes minutes to serve a new _acme-challenge TXT) has the whole
// pipeline to propagate before the Caddy edge awaits validation at the end. If a
// valid cert is already on the durable edge PVC (reuse gate >= 30d), the slot is
// marked for reuse and NO challenge is placed. Completed by awaitCertIssuance.
func (e *rebuildEngine) startCertIssuance() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil || cfg.Caddy.Host == "" {
		return nil // no Caddy edge to certify yet
	}
	_, secret, pub, err := e.certIdent()
	if err != nil {
		return err
	}
	e.certRuns = map[string]*certRun{}
	for _, svc := range []struct{ slot, host string }{
		{"relay", cfg.RelayHost()},
		{"cp", cfg.CPHost()},
	} {
		if svc.host == "" {
			continue
		}
		leDomain := e.legoDomain(svc.slot)
		if _, ok := e.certRuns[leDomain]; ok {
			continue // shared cert-domain — one order, one challenge
		}
		verifyTarget := svc.host
		if strings.HasPrefix(leDomain, "*.") {
			verifyTarget = strings.TrimPrefix(leDomain, "*.")
		}
		run := &certRun{
			slot:      svc.slot,
			leDomain:  leDomain,
			challenge: "_acme-challenge." + verifyTarget,
			prepared:  make(chan struct{}),
		}
		// Reuse gate: a valid cert already on the durable edge PVC means "we have
		// got our cert" — don't begin/resume an order or place a challenge. Only
		// meaningful when the k3s node (and thus Caddy's PVC) is up (a reconcile);
		// on a cold boot there is nothing to read, so we issue.
		if cfg.Lxc.K3s.Vmid != nil {
			k3s := *cfg.Lxc.K3s.Vmid
			if existing, rerr := e.caddyFullchain(k3s, svc.slot); rerr == nil && len(existing) > 0 {
				if exp, ok := cert.ReuseIfValidBytes(existing, time.Now(), 30*24*time.Hour); ok {
					fmt.Fprintf(e.out, "  · %s: reusing existing cert (expires %s) — not re-issuing\n", svc.slot, exp.UTC().Format(time.RFC3339))
					run.reuse = true
					run.reuseExp = exp
					e.certRuns[leDomain] = run
					continue
				}
			}
		}
		// The slot's stored credential (promptDNSCred reuses the sealed copy after
		// 6.6, so this does not prompt again).
		provider, env, perr := e.promptDNSCred(svc.slot, verifyTarget, "")
		if perr != nil {
			return perr
		}
		// Challenge records stay SHORT-TTL so rebuilds don't overlap leftovers.
		if _, ok := env["CLOUDFLARE_TTL"]; !ok {
			env["CLOUDFLARE_TTL"] = "120"
		}
		statePath := filepath.Join(rbStateDir(), "cert-pending-"+certSlug(leDomain)+".json")
		seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
		open := func(secret, aad, blob []byte) ([]byte, error) { return crypto.Open(secret, aad, blob) }
		dp, derr := cert.NewDNSProvider(provider, env)
		if derr != nil {
			return fmt.Errorf("%s dns provider: %w", svc.slot, derr)
		}
		run.resume = &cert.Resume{
			Domain:    leDomain,
			Wildcard:  strings.HasPrefix(leDomain, "*."),
			Provider:  dp,
			Seal:      seal,
			Open:      open,
			SealPub:   pub,
			OpenSec:   secret,
			Path:      statePath,
		}
		fmt.Fprintf(e.out, "  · %s: preparing the %s certificate challenge (lego %s, resumable)…\n", svc.slot, leDomain, provider)
		go e.prepareCertRun(run, provider, cloneMap(env))
		e.certRuns[leDomain] = run
	}
	return nil
}

// certSlug maps a cert-domain to a safe state-file basename.
func certSlug(domain string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(domain) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// prepareCertRun begins (or RESUMES) a run's ACME order in the background. A
// resumed order does NOT re-place a challenge — its record is already in the
// zone and is what we want to re-check. A fresh order first clears any leftover
// challenge record at the name, then begins (place + persist). Signals prepared.
func (e *rebuildEngine) prepareCertRun(run *certRun, provider string, env map[string]string) {
	defer close(run.prepared)
	po, ok, err := run.resume.TryLoad()
	if err != nil {
		run.err = fmt.Errorf("%s resume state: %w", run.slot, err)
		return
	}
	if ok {
		fmt.Fprintf(e.out, "  · %s: resuming pending order %s — will re-check whether the challenge finally landed\n", run.slot, run.leDomain)
		run.pending = po
		return
	}
	// Fresh: clear ANY leftover challenge at the name (a stale dead digest would
	// shadow the value we place), then begin a new resumable order.
	if provider == "cloudflare" {
		if mgr, merr := dnsman.For(provider, env); merr == nil {
			if dErr := mgr.DeleteTXT(run.challenge); dErr != nil {
				fmt.Fprintf(e.out, "  · %s: clearing stale challenge %s: %v\n", run.slot, run.challenge, dErr)
			}
		}
	}
	po, err = run.resume.Begin()
	if err != nil {
		run.err = fmt.Errorf("%s begin issuance: %w", run.slot, err)
		return
	}
	fmt.Fprintf(e.out, "  · %s: challenge pre-placed + persisted (resumable order %s)\n", run.slot, run.leDomain)
	run.pending = po
}

// awaitCertIssuance runs after the Caddy edge is up: for each slot it takes the
// background run's result (polling every 30s while a slow DNS provider
// propagates), installs the per-slot cert into the edge pod, and records it. A
// slot marked for reuse records the already-present cert without reinstalling.
func (e *rebuildEngine) awaitCertIssuance() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil || cfg.Lxc.K3s.Vmid == nil {
		return nil
	}
	if cfg.Caddy.Host == "" {
		fmt.Fprintln(e.out, "  · Caddy edge not recorded — skipping cert installation")
		return nil
	}
	k3sVmid := *cfg.Lxc.K3s.Vmid
	issuer := cfg.Caddy.CertIssuer
	if issuer == "" {
		issuer = "lego (DNS-01)"
	}
	// Phase A: resolve EVERY slot's certificate first (Reuse slots only record).
	// Reuse (a valid cert already on the PVC) is recorded and skipped; issued
	// slots are collected for sealed install afterwards.
	type installed struct {
		slot     string
		leDomain string
		issued   *cert.Issued
	}
	var installs []installed
	for _, svc := range []struct{ slot, host string }{
		{"relay", cfg.RelayHost()},
		{"cp", cfg.CPHost()},
	} {
		if svc.host == "" {
			continue
		}
		leDomain := e.legoDomain(svc.slot)
		run := e.certRuns[leDomain]
		if run == nil {
			continue // no issuance was started for this domain
		}
		if run.reuse {
			if err := e.recordCert(svc.slot, run.reuseExp, issuer, leDomain); err != nil {
				return err
			}
			fmt.Fprintf(e.out, "  · %s: using existing cert (expires %s)\n", svc.slot, run.reuseExp.UTC().Format(time.RFC3339))
			continue
		}
		// Wait (polling every 30s) for the run's resume handle, then RESOLVE it:
		// accept the (already-placed, possibly only-now-propagated) challenge so
		// Let's Encrypt validates it, finalize, download.
		e.waitSignal(run.prepared, fmt.Sprintf("%s: awaiting %s order preparation", svc.slot, leDomain))
		if run.err != nil {
			return fmt.Errorf("%s cert issuance: %w", svc.slot, run.err)
		}
		if run.pending == nil {
			return fmt.Errorf("%s: no resumable order prepared", svc.slot)
		}
		resolved := make(chan struct{})
		go func() {
			defer close(resolved)
			run.issued, run.err = run.resume.Resolve(run.pending)
		}()
		e.waitSignal(resolved, fmt.Sprintf("%s: awaiting %s validation", svc.slot, leDomain))
		if run.err != nil {
			return fmt.Errorf("%s cert issuance: %w", svc.slot, run.err)
		}
		installs = append(installs, installed{slot: svc.slot, leDomain: leDomain, issued: run.issued})
	}
	// Phase B–D: seal every cert key, restart the runner so its in-memory package
	// (loaded ONCE at boot) picks up the freshly-sealed keys, then install each.
	if len(installs) > 0 {
		for _, ic := range installs {
			if err := e.sealCertKey(ic.slot, ic.issued.Key); err != nil {
				return err
			}
		}
		// The runner loads its package at boot only; secrets added via add-secret
		// after boot are NOT visible to it. A fresh serve re-reads secrets.json
		// (which now holds every cert-key-<slot>) so the install execs resolve.
		if _, err := e.stageServe(); err != nil {
			return fmt.Errorf("restart runner for cert install: %w", err)
		}
		for _, ic := range installs {
			if err := e.installCaddyCert(k3sVmid, ic.slot, ic.issued.Fullchain); err != nil {
				return err
			}
			if err := e.recordCert(ic.slot, ic.issued.NotAfter, issuer, ic.leDomain); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitSignal blocks until sig closes (or is already closed), printing msg with a
// "…(polling every 30s)" heartbeat so a long ACME wait stays observable.
func (e *rebuildEngine) waitSignal(sig <-chan struct{}, msg string) {
	if sig == nil {
		return
	}
	select {
	case <-sig:
		return
	default:
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-sig:
			return
		case <-t.C:
			fmt.Fprintf(e.out, "  · %s (polling every 30s)…\n", msg)
		}
	}
}

// cloneMap returns a shallow copy of env (issuance goroutines must not mutate a
// shared credential map).
func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// legoDomain is the per-slot LE cert-domain for the rebuild engine's current
// config (relay slot default = the relay host; cp default = the cp host;
// "*.base" = a wildcard covering the host). The recorded value is VALIDATED
// against the CURRENT host so a stale per-host wildcard (e.g. the previous
// world's *.freehold-test.darcydev.net) surviving a domain change cannot issue
// for the wrong zone.
func (e *rebuildEngine) legoDomain(slot string) string {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil {
		return ""
	}
	var host, configured string
	if slot == "cp" {
		host = cfg.CPHost()
		configured = cfg.Caddy.CPLegoDomain
	} else {
		host = cfg.RelayHost()
		configured = cfg.Caddy.RelayLegoDomain
	}
	return legoDomainForHost(host, configured)
}

// legoDomainForHost returns the effective LE cert-domain for a host: the
// configured value when it still covers the CURRENT host (the host exactly, or
// a wildcard "*.base" whose base is the host or one of its parents); otherwise
// the host itself (a single-name cert for the current host). Empty host -> "".
func legoDomainForHost(host, configured string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return host
	}
	if configured == host {
		return configured
	}
	base := strings.TrimPrefix(configured, "*.")
	if base != configured && (host == base || strings.HasSuffix(host, "."+base)) {
		return configured
	}
	return host
}

// recordCert persists the per-slot cert expiry + issuer into the config. leDomain
// (when non-empty) records the EFFECTIVE LE cert-domain actually used, so a stale
// recorded domain is self-healed on the first issuance of the current host.
func (e *rebuildEngine) recordCert(slot string, exp time.Time, issuer, leDomain string) error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s", e.f.configPath)
	}
	if slot == "cp" {
		cfg.Caddy.CPCert = exp.UTC().Format(time.RFC3339)
		if leDomain != "" {
			cfg.Caddy.CPLegoDomain = leDomain
		}
	} else {
		cfg.Caddy.RelayCert = exp.UTC().Format(time.RFC3339)
		if leDomain != "" {
			cfg.Caddy.RelayLegoDomain = leDomain
		}
	}
	if issuer != "" {
		cfg.Caddy.CertIssuer = issuer
	}
	return cfg.Save(e.f.configPath)
}

// caddyFullchain cats a slot's durable fullchain out of the pod (base64) so the
// reuse gate can parse its expiry without re-issuing.
func (e *rebuildEngine) caddyFullchain(k3sVmid uint32, slot string) ([]byte, error) {
	cmd := fmt.Sprintf(
		"pct exec %d -- /usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml -n caddy exec deploy/caddy -- sh -c 'cat /data/tls/%s/fullchain.pem' 2>/dev/null | base64 -w0",
		k3sVmid, slot)
	ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 60))
	if !ok {
		return nil, fmt.Errorf("caddy %s fullchain unreadable: %s", slot, strings.TrimSpace(out))
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("caddy %s fullchain base64: %w", slot, err)
	}
	return dec, nil
}

// sealCertKey seals a slot's cert private key into the runner's secrets package
// under "cert-key-<slot>", so a later `exec --secret cert-key-<slot>` injects it.
func (e *rebuildEngine) sealCertKey(slot string, key []byte) error {
	return e.sealRunnerSecret("cert-key-"+slot, slotCertKeyEnv(slot), string(key))
}

// installCaddyCert writes the chain into a slot's dir on the Caddy durable PVC
// via a short-lived helper pod that mounts the same caddy-data volume (so it
// works even while Caddy itself is crash-looping on the very first boot, before
// any cert exists), then reloads the edge. The PRIVATE KEY must ALREADY be sealed
// under "cert-key-<slot>" (sealCertKey) — the runner injects it as an env var,
// never bytes in the audited command; only the public fullchain is embedded here.
func (e *rebuildEngine) installCaddyCert(k3sVmid uint32, slot string, fullchain []byte) error {
	args := []string{"exec", "--addr", e.f.addr, "--agent-dir", rbOpsDir(),
		"--timeout", "180",
		// The runner requires the SSH target's OWN credential among the requested
		// secrets whenever --secret refs are given (it does not default to it
		// then); otherwise exec refuses with "requires secret <target>".
		"--secret", e.f.target, "--secret", "cert-key-" + slot, e.f.target,
		caddyCertInstallScript(k3sVmid, slot, fullchain)}
	ok, out := e.runBin(e.bins.Self, args)
	if !ok {
		return fmt.Errorf("caddy %s cert install failed:\n%s", slot, out)
	}
	return nil
}

// slotCertKeyEnv is the env var the runner injects for the slot's cert key.
func slotCertKeyEnv(slot string) string { return "CERT_KEY_" + strings.ToUpper(slot) }

// sealRunnerSecret seals a value into the current target runner's secrets
// package under `name`, so a later `exec --secret <name>` injects it as
// $<UPPER_SNAKE> into the command's environment (values never in command text).
func (e *rebuildEngine) sealRunnerSecret(name, envVar, val string) error {
	env := append([]string{envVar + "=" + val}, os.Environ()...)
	ok, out := e.runEnv(e.bins.ControlPlane, env, []string{
		"add-secret", e.f.target, name,
		"--state-dir", rbStateDir(),
		"--secret-env", envVar,
	})
	if !ok {
		return fmt.Errorf("add-secret %s failed:\n%s", name, out)
	}
	return nil
}

// caddyCertInstallScript installs the chain into /data/tls/<slot>/ within the
// Caddy durable PVC then reloads the edge.
//
// The cert files are written DIRECTLY into the local-path backing directory on
// the k3s node — the very directory Caddy's hostNetwork pod bind-mounts at
// /data. (The prior helper-pod approach fed the files in over `kubectl exec ...
// < file` stdin, but stdin is never forwarded through the runner→pct→guest→
// kubectl chain, so `cat` read EOF and every install left 0-byte certs. Writing
// to the node directory is the same on-disk bytes, without the stdin hop.) This
// works even while Caddy crash-loops on an empty cert, before it is healthy.
//
// PVT KEY VIA SEALED SECRET: the runner injects $CERT_KEY_<SLOT> into this
// command's env ON THE PVE HOST. A `pct exec` guest shell would NOT inherit it,
// so the key is written to a HOST temp file first and `pct push`ed into the
// guest — the audited command text never carries the key. The public fullchain
// is base64-embedded (public data). Both shells run set -e.
func caddyCertInstallScript(k3sVmid uint32, slot string, fullchain []byte) string {
	fcB64 := base64.StdEncoding.EncodeToString(fullchain)
	keyEnv := slotCertKeyEnv(slot)
	return fmt.Sprintf(`set -e
printf '%%s' "${%s}" > /tmp/fh-key.pem
printf %s | base64 -d > /tmp/fh-fc.pem
pct push %d /tmp/fh-fc.pem /tmp/fc-%s.pem
pct push %d /tmp/fh-key.pem /tmp/key-%s.pem
pct exec %d -- sh -c '
set -e
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
PV=$($K get pvc caddy-data -n caddy -o jsonpath={.spec.volumeName})
DIR=/var/lib/rancher/k3s/storage/${PV}_caddy_caddy-data/tls/%s
mkdir -p "$DIR"
cp /tmp/fc-%s.pem "$DIR/fullchain.pem"
cp /tmp/key-%s.pem "$DIR/key.pem"
chmod 600 "$DIR/key.pem"
$K -n caddy rollout restart deploy/caddy >/dev/null 2>&1 || true
rm -f /tmp/fc-%s.pem /tmp/key-%s.pem
'
rm -f /tmp/fh-key.pem /tmp/fh-fc.pem
`, keyEnv, shellSingleQuote(fcB64), k3sVmid, slot, k3sVmid, slot, k3sVmid, slot, slot, slot, slot, slot)
}

// shellSingleQuote single-quotes an arg for the embedded sh -c command.
func shellSingleQuote(s string) string { return "'" + s + "'" }

// firstNonEmpty returns the first non-empty of its arguments.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// manageDomainDNS runs the --manage-dns branch: ensures the relay slot's DNS
// credential (the one LE will reuse), builds the provider's dnsman Manager,
// upserts relay.<d> + cp.<d> A records -> the proxy's static IP, and records
// the manager assertion into the config. Only Cloudflare can manage records
// today; a different stored provider is an actionable error.
func (e *rebuildEngine) manageDomainDNS() error {
	ip := config.StripCIDR(e.f.proxyIP)
	provider, env, err := e.promptDNSCred("relay", e.f.relayDomain, "")
	if err != nil {
		return fmt.Errorf("relay DNS provider credential (for management): %w", err)
	}
	if provider != "cloudflare" {
		return fmt.Errorf(
			"freehold can manage DNS on Cloudflare only right now, but the relay credential uses %q — run `freehold dns-cred --provider cloudflare --domain %s` (or pick No for manual DNS)",
			provider, e.f.relayDomain)
	}
	m, err := dnsman.For(provider, env)
	if err != nil {
		return err
	}
	for _, host := range []string{e.f.relayDomain, e.f.cpDomain} {
		if host == "" {
			continue
		}
		if err := m.UpsertA(host, ip); err != nil {
			return fmt.Errorf("manage DNS for %s: %w", host, err)
		}
	}
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg != nil {
		cfg.Dns.Manager = &config.DnsManager{Provider: provider, Managed: true, IP: ip}
		if err := cfg.Save(e.f.configPath); err != nil {
			return err
		}
	}
	return nil
}

// worldHasEdge reports whether this rebuild's desired world includes the Caddy
// TLS edge (and therefore needs a DNS provider credential): k3s is on and a
// world domain is set.
func (e *rebuildEngine) worldHasEdge() bool {
	return !e.f.noK3s && e.f.relayDomain != ""
}

// promptDomains asks for the relay + control-plane hosts up front. There is
// NO world/base domain and no derivation — the two hosts are required, literal
// inputs. A flag already set (or a headless --yes run) is honored; under --yes
// a missing one hard-errors (interactive collection is the only source).
func (e *rebuildEngine) promptDomains() error {
	if e.f.relayDomain == "" && e.f.yes {
		return fmt.Errorf("no relay domain supplied and interactive collection is disabled (--yes); pass --relay-domain")
	}
	if e.f.relayDomain == "" {
		ans, err := e.prompt("relay domain (its Buzz origin — REQUIRED)")
		if err != nil {
			return err
		}
		if a := strings.TrimSpace(ans); a != "" {
			e.f.relayDomain = a
		}
	}
	if e.f.relayDomain == "" {
		return fmt.Errorf("relay domain is required")
	}
	if e.f.cpDomain == "" && e.f.yes {
		return fmt.Errorf("no control-plane domain supplied and interactive collection is disabled (--yes); pass --cp-domain")
	}
	if e.f.cpDomain == "" {
		ans, err := e.prompt("control-plane domain (REQUIRED)")
		if err != nil {
			return err
		}
		if a := strings.TrimSpace(ans); a != "" {
			e.f.cpDomain = a
		}
	}
	if e.f.cpDomain == "" {
		return fmt.Errorf("control-plane domain is required")
	}
	return nil
}

// promptDNSCred returns the DNS provider name + its env map for one slot
// ("relay"/"cp"), reusing the sealed copy when present. When the slot has no
// credential, reuseFrom (if given + stored) offers to COPY that slot's
// credential into this one without re-entering — but the two stay separate and
// may differ. Otherwise it interactively collects (provider from lego's full
// registry, the provider's own env-var names), pre-verifies against host, and
// saves sealed. Under --yes a missing credential is a hard error.
func (e *rebuildEngine) promptDNSCred(slot, host, reuseFrom string) (string, map[string]string, error) {
	path := e.certCredPath(slot)
	_, secret, pub, err := e.certIdent()
	if err != nil {
		return "", nil, err
	}
	seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
	open := func(secret, aad, blob []byte) ([]byte, error) { return crypto.Open(secret, aad, blob) }

	// Migrate the legacy single-file credential (dns-provider.json) into the
	// relay slot, then fall through to the normal reuse so an already-entered
	// credential keeps working after the per-slot split. Best-effort: if the
	// legacy blob can't be unsealed, ignore it and collect a fresh one.
	if slot == "relay" && !cert.CredExists(path) {
		if legacy := filepath.Join(rbStateDir(), "dns-provider.json"); cert.CredExists(legacy) {
			if provider, env, lerr := cert.LoadCreds(legacy, open, secret); lerr == nil {
				if serr := cert.SaveCreds(path, provider, env, seal, pub, "cert-dns-"+slot); serr == nil {
					fmt.Fprintf(e.out, "  · migrated your stored DNS credential (%s) into the %s slot\n", provider, slot)
				}
			}
		}
	}

	if cert.CredExists(path) {
		provider, env, err := cert.LoadCreds(path, open, secret)
		if err != nil {
			return "", nil, fmt.Errorf("reusing %s DNS credential: %w", slot, err)
		}
		fmt.Fprintf(e.out, "  · reusing sealed %s DNS credential (%s)\n", slot, provider)
		return provider, env, nil
	}

	if reuseFrom != "" {
		fromPath := e.certCredPath(reuseFrom)
		if cert.CredExists(fromPath) {
			reuse := e.f.yes // headless: auto-copy the shared credential
			if !e.f.yes {
				ans, err := e.prompt(fmt.Sprintf("%s has no DNS credential — reuse the %s one? (y/n)", slot, reuseFrom))
				if err != nil {
					return "", nil, err
				}
				reuse = strings.EqualFold(strings.TrimSpace(ans), "y")
			}
			if reuse {
				provider, env, err := cert.LoadCreds(fromPath, open, secret)
				if err != nil {
					return "", nil, err
				}
				if err := cert.SaveCreds(path, provider, env, seal, pub, "cert-dns-"+slot); err != nil {
					return "", nil, fmt.Errorf("storing %s DNS credential: %w", slot, err)
				}
				fmt.Fprintf(e.out, "  · %s reuses the %s DNS credential (%s) — copies kept separate\n", slot, reuseFrom, provider)
				return provider, env, nil
			}
		}
	}

	if e.f.yes {
		return "", nil, fmt.Errorf("no %s DNS provider credential stored and interactive collection is disabled (--yes); run without --yes once to store it", slot)
	}

	provider, err := e.promptProvider()
	if err != nil {
		return "", nil, err
	}
	env, err := e.promptProviderEnv(provider)
	if err != nil {
		return "", nil, err
	}
	if host != "" {
		fmt.Fprintf(e.out, "  · pre-verifying %s credentials (throwaway TXT round-trip)…\n", provider)
		if err := cert.Verify(host, provider, env); err != nil {
			return "", nil, fmt.Errorf("DNS provider pre-verify failed — fix the credential and try again: %w", err)
		}
	}
	if err := cert.SaveCreds(path, provider, env, seal, pub, "cert-dns-"+slot); err != nil {
		return "", nil, fmt.Errorf("storing DNS credential: %w", err)
	}
	return provider, env, nil
}

// promptProvider asks the operator to pick a DNS-01 provider from lego's full
// registry via an interactive, scrollable + type-ahead-searchable list picker
// (bubbletea list). The 201-provider registry is otherwise unreadable when
// enumerated inline.
func (e *rebuildEngine) promptProvider() (string, error) {
	return runProviderPicker(cert.Providers())
}

// promptProviderEnv collects the provider's env-var fields (from lego-derived
// names; freeform KEY=VAL lines for unknown/auto-detecting providers). The
// credential trio is asked FIRST and REQUIRED (no blank); every other field is
// collected after with "(optional)" (blank = unset).
func (e *rebuildEngine) promptProviderEnv(provider string) (map[string]string, error) {
	names := cert.ProviderEnvNames(provider)
	env := map[string]string{}
	if len(names) == 0 {
		fmt.Fprintln(e.out, "  this provider has no enumerated env fields — paste KEY=VAL entries (one per line; empty line to finish):")
		for {
			line, err := e.prompt("KEY=VAL (or blank to finish)")
			if err != nil {
				return nil, err
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) == "" {
				fmt.Fprintln(e.out, "  expected KEY=VAL")
				continue
			}
			env[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		return env, nil
	}
	// The credential fields asked FIRST + REQUIRED. AWS/Route53: the access
	// key, secret, and hosted zone are required to issue the wildcard cert.
	// Cloudflare: the DNS API token is the primary credential (also used by
	// --manage-dns's record management).
	required := map[string][]string{
		"route53":    {"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_HOSTED_ZONE_ID"},
		"cloudflare": {"CLOUDFLARE_DNS_API_TOKEN"},
	}[provider]
	var requiredSet []string
	for _, n := range required {
		if containsStr(names, n) {
			requiredSet = append(requiredSet, n)
		}
	}
	fmt.Fprintln(e.out, "  enter the REQUIRED credential fields:")
	for _, n := range requiredSet {
		v, err := e.prompt(n + " (required)")
		if err != nil {
			return nil, err
		}
		if v = strings.TrimSpace(v); v == "" {
			return nil, fmt.Errorf("%s is required for the %s credential", n, provider)
		}
		env[n] = v
	}
	// Everything else, marked optional.
	var rest []string
	for _, n := range names {
		if !containsStr(requiredSet, n) {
			rest = append(rest, n)
		}
	}
	if len(rest) > 0 {
		fmt.Fprintln(e.out, "  optional fields (blank = unset):")
		for _, n := range rest {
			v, err := e.prompt(n + " (optional)")
			if err != nil {
				return nil, err
			}
			if v = strings.TrimSpace(v); v != "" {
				env[n] = v
			}
		}
	}
	return env, nil
}

// ---- the CPA stage (Chunk 4 Phase A) ---------------------------------------

// cpaIdentityDir is where the CPA's durable Nostr identity lives. It sits on
// the CP's durable-plane area so a compute-only teardown/rebuild (Phase 0.12)
// reattaches the SAME keypair — the CPA's identity survives the body, exactly
// as the remote-agent vision requires (VISION_REMOTE_AGENTS: the keypair is
// the agent; the pod is disposable).
func cpaIdentityDir() string {
	return filepath.Join(rbStateDir(), "agent-cpa")
}

// ensureCPAIdentity mints the CPA's Nostr keypair on first use and returns its
// pubkey. Rebuilds reuse the recorded identity (identity continuity), so the
// CPA's Buzz profile, presence, and DMs all survive.
func ensureCPAIdentity() (string, error) {
	return agent.EnsureIdentity(cpaIdentityDir())
}

// stageCpa deploys the CPA as a k3s Pod running the buzz-sprig harness (A2),
// wires its Buzz display name (A3), and registers it in the agent registry
// (A5). Identity is durable across rebuilds so the same agent returns.
func (e *rebuildEngine) stageCpa() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil || cfg.Proxy.Ip == nil || cfg.Lxc.K3s.Vmid == nil {
		return fmt.Errorf("no k3s coords recorded — the CPA pod needs the k3s substrate")
	}
	if cfg.RelayURL == "" {
		return fmt.Errorf("no relay URL in config — the CPA must join the community relay")
	}
	if cfg.Litellm.URL == "" {
		return fmt.Errorf("no litellm gateway recorded — the CPA needs a reasoning model (litellm is part of the world unless --no-litellm)")
	}
	k3sVmid := *cfg.Lxc.K3s.Vmid
	cpaName := cfg.CPAName
	if cpaName == "" {
		cpaName = agent.DefaultCPAName
	}
	cpaPub, err := ensureCPAIdentity()
	if err != nil {
		return err
	}
	// The harness speaks WS to the relay. The pod joins the community relay
	// over the LAN: resolve the domain internally to the relay LXC (the CP
	// resolver's split-horizon record wired before this stage) and speak
	// plain ws on the relay's HTTP port (:3000 — the deployment's
	// BUZZ_HTTP_PORT). The Caddy edge fronts TLS at wss://relay.<domain>, but
	// the pod keeps the LAN ws origin until F5 flips it (roadmap/CORE_TLS.md)
	// — a wss:443 host with no live cert yet would be a dead pod.
	// cfg.RelayWsURL records this internal origin; fall back to deriving wss
	// from the public URL for worlds that do reach the relay over TLS.
	relayURL := cfg.RelayWsURL
	if relayURL == "" {
		relayURL = strings.Replace(cfg.RelayURL, "https://", "wss://", 1)
	}
	// B1/B3: the CPA's purpose lives in the orchestrator's prompts package
	// (prompts/CPA_SYSTEM_PROMPT.md), embedded into this binary at compile
	// time and shipped into the pod's ConfigMap at spawn; the pod re-reads
	// the mounted copy on every restart — never cached, never a host-side
	// file read (which would break on CWD).
	promptText := prompts.CPASystemPrompt

	// Ensure the cpa-identity Secret (nsec + owner) exists in the namespace.
	id, err := flows.LoadIdentity(cpaIdentityDir())
	if err != nil {
		return fmt.Errorf("cpa identity unreadable after mint: %w", err)
	}
	ok, out := e.runBin(e.bins.Self, e.execArgs(agent.AgentIdentityScript(
		k3sVmid, id.NostrSecretHex, e.f.operatorPubkey, cpaName), 120))
	if !ok {
		return fmt.Errorf("cpa identity secret failed:\n%s", out)
	}

	// Apply the CPA pod via THIS binary self-exec'd through the runner (the
	// same transport stageLitellm's exec uses), then record the agent. The
	// runner executes this verbatim on the k3s guest; the pod's ConfigMap
	// carries the prompt, so nothing ships the .md to the CP LXC.
	ok, out = e.runBin(e.bins.Self, e.execArgs(agent.CPAManifestScript(
		k3sVmid, relayURL, promptText, cpaName), 420))
	if !ok {
		return fmt.Errorf("cpa pod apply failed:\n%s", out)
	}
	return e.recordCpa(cpaPub, cpaName)
}

// recordCpa writes the CPA's identity + name into the config (managed) so the
// Services row + teardown + console see the agent, and registers it in the
// control-plane agent registry (A5) so it shows up like any named agent. The
// rest of the presence dot is the CPA's own relay kind:20001 publication —
// the registry row carries identity, the relay carries liveness.
func (e *rebuildEngine) recordCpa(cpaPub, cpaName string) error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s", e.f.configPath)
	}
	cfg.CPAName = cpaName
	if !containsStr(cfg.Managed, "cpa") {
		cfg.Managed = append(cfg.Managed, "cpa")
	}
	if err := cfg.Save(e.f.configPath); err != nil {
		return err
	}
	// A5: register the CPA in the CP agent registry (the state store the
	// console reads). Non-fatal if the store isn't present yet (a rebuild
	// run may not have a full CP state); the presence dot is relay-side.
	if store, err := state.Open(rbStateDir()); err == nil {
		_ = agent.RegisterAgent(store, cpaName, cpaPub)
		_ = store.Save()
	}
	return nil
}

// relayPubkeyNip11 reads the relay's signing pubkey via NIP-11 (best-effort
// trust anchor; not fatal when unreadable).
func (e *rebuildEngine) relayPubkeyNip11() (string, bool) {
	text, ok := e.curlGet("https://" + e.f.relayDomain + "/")
	if !ok {
		return "", false
	}
	return extractNip11Pubkey(text)
}

// extractNip11Pubkey pulls "pubkey" out of a NIP-11 JSON body without a
// JSON dep: a minimal scan for the 64-hex value.
func extractNip11Pubkey(text string) (string, bool) {
	const key = `"pubkey"`
	i := strings.Index(text, key)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(key):]
	start := strings.Index(rest, `"`)
	if start < 0 {
		return "", false
	}
	rest = rest[start+1:]
	end := strings.Index(rest, `"`)
	if end != 64 {
		return "", false
	}
	pk := rest[:64]
	for _, c := range pk {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", false
		}
	}
	return pk, true
}

// printTail returns the last n non-empty lines, indented (Rust print_tail).
func printTail(text string, n int) string {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("    " + l + "\n")
	}
	return b.String()
}
