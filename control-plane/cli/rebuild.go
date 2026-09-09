// The world-rebuild pipeline: bring the whole appliance up end to end in
// one shot — the Rust installer's stage set (installer/src/{main,lib}.rs)
// ported to Go. Shells the REAL sibling binaries exactly like the Rust
// installer did: `control-plane` + `runner` (still Rust) and THIS binary
// (os.Executable(), the teardown-engine pattern) for the operator CLI
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

	"freehold/control-plane/api/agent"
	"freehold/platform/services/certificates/letsencrypt"
	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/crypto"
	cpdeploy "freehold/control-plane/cli/bootstrap-cp"
	"freehold/platform/services/externaldns/cloudflare"
	"freehold/platform/provisioning/drive"
	"freehold/control-plane/cli/flows"
	"freehold/platform/provisioning/stages"
	"freehold/contract/state"
	"freehold/contract/wire"
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
	Short: "Bring the whole world up: CP-bring-up + trigger (the CP owns relay/k3s/storage/DNS/litellm/caddy/cert through its co-located runner)",
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
		f.storageName, _ = cmd.Flags().GetString("storage")
		f.bridge, _ = cmd.Flags().GetString("bridge")
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
		return eng.runSlim()
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
	buildCmd.Flags().String("storage", "local-lvm", "PVE LXC storage (relay/k3s boots)")
	buildCmd.Flags().String("bridge", "vmbr0", "PVE LXC network bridge (relay/k3s boots)")
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
	storageName        string
	bridge             string
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
	Self              string // this binary — the operator CLI (teardown-engine pattern)
	ControlPlane      string // target/debug/control-plane (Rust)
	Runner            string // target/debug/runner (Rust)
	ReleaseCP         string // target/release/control-plane (deploy-cp --binary)
	ReleaseRun        string // target/release/runner (deploy-cp --runner-binary)
	ReleaseAgentTools string // target/release/freehold-agent-tools (the CP's agent-tools MCP server)
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
		Self:              self,
		ControlPlane:      filepath.Join(selfDir, "control-plane"),
		Runner:            filepath.Join(selfDir, "runner"),
		ReleaseCP:         filepath.Join(releaseDir, "control-plane"),
		ReleaseRun:        filepath.Join(releaseDir, "runner"),
		ReleaseAgentTools: filepath.Join(releaseDir, "freehold-agent-tools"),
	}
	var missing []string
	for _, p := range []struct{ path, label string }{
		{b.ControlPlane, "control-plane"},
		{b.Runner, "runner"},
		{b.ReleaseCP, "../release/control-plane"},
		{b.ReleaseRun, "../release/runner"},
		{b.ReleaseAgentTools, "../release/freehold-agent-tools"},
	} {
		if _, err := os.Stat(p.path); err != nil {
			missing = append(missing, filepath.Join(filepath.Base(selfDir), p.label))
		}
	}
	if len(missing) > 0 {
		return b, fmt.Errorf(
			"sibling binaries missing: %s\n  build them once, then re-run:\n    cargo build --bin control-plane --bin runner && cargo build --release --bin control-plane --bin runner && CGO_ENABLED=0 go build -C control-plane -o target/release/freehold-agent-tools ./api/cmd/freehold-agent-tools",
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

func (e *rebuildEngine) runSlim() error {
	fmt.Fprintf(e.out, "building world %s (CP-bring-up + trigger — the CP owns relay/k3s/DNS/litellm/caddy/cert)\n", e.f.relayDomain)

	// 1. the ops agent identity + the door (same as run()).
	ensureAgentIdentity(rbOpsDir())
	agentPK, err := loadRPubkey(rbOpsDir())
	if err != nil {
		return fmt.Errorf("ops agent identity unreadable at %s: %w", rbOpsDir(), err)
	}
	doorKey, err := e.stageProvision(agentPK)
	if err != nil {
		return err
	}
	if doorKey != "" {
		if err := e.doorGate(doorKey); err != nil {
			return err
		}
	}
	if err := e.stageGrant(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ ops agent granted on %s\n", e.f.target)
	if _, err := e.stageServe(); err != nil {
		return err
	}
	if err := e.stageVerify(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ the door works — %s is reachable\n", e.f.host)

	// 5.5. domains + 5.6. the ONE static proxy IP (same as run()).
	if !e.f.yes {
		if err := e.promptDomains(); err != nil {
			return err
		}
	}
	if e.f.relayDomain == "" || e.f.cpDomain == "" {
		return fmt.Errorf("rebuild needs --relay-domain and --cp-domain (or an interactive run)")
	}
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

	// 6. initial config so the plane mapping has somewhere to record.
	if err := e.writeInitialConfig(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ wrote config %s\n", e.f.configPath)

	// 6.4/6.6. DNS-manage opt-in + the DNS provider creds (captured IN MEMORY,
	// sealed for the hand-off; promptDNSCred reuses the sealed box copies).
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
		if _, _, err := e.promptDNSCred("relay", e.f.relayDomain, ""); err != nil {
			return fmt.Errorf("relay DNS provider credential: %w", err)
		}
		if _, _, err := e.promptDNSCred("cp", e.f.cpDomain, "relay"); err != nil {
			return fmt.Errorf("control-plane DNS provider credential: %w", err)
		}
	}

	// 7. the durable volume plane (the CP boot needs the cp dataset; the
	// relay/k3s datasets are re-ensured by world_build, idempotently).
	placement, err := e.stagePlacement()
	if err != nil {
		return err
	}
	if err := e.stageStorage(placement); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ durable volume plane ready")

	// 8. Seal the litellm secrets into the box runner package BEFORE deploy-cp
	// ships it as the CP's co-located runner (world_build's litellm step then
	// applies + registers by name).
	if err := e.slimSeedLiteLLM(); err != nil {
		return err
	}

	// 9. boot the CP LXC + record its coordinates, then boot + deploy the RELAY
	// (its IP must be recorded BEFORE deploy-cp so the CP guest's /etc/hosts
	// pin reaches it; the agent-tools roster also needs the relay live).
	fmt.Fprintln(e.out, "  · booting the cp LXC (create → docker; can take minutes)…")
	if err := e.stageBootstrap("cp"); err != nil {
		return err
	}
	if _, err := e.stageRecordLxc("cp"); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ cp LXC booted + recorded")
	fmt.Fprintln(e.out, "  · booting the relay LXC (create → docker; can take minutes)…")
	if err := e.stageBootstrap("relay"); err != nil {
		return err
	}
	if _, err := e.stageRecordLxc("relay"); err != nil {
		return err
	}
	if err := e.stageDeployRelay(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ relay live at https://%s\n", e.f.relayDomain)

	// 10. deploy the CP + its co-located runner (the relay IP is now recorded,
	// so the CP guest pins the domain for pre-Caddy relay ops).
	if err := e.stageDeployCp(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ control plane live at https://%s\n", e.f.cpDomain)

	// 11. deploy freehold-agent-tools (the trigger surface + world_build home).
	// Its seed/roster dial the relay DOMAIN (buzz keys the community to the
	// Host header) — re-pin the relay LAN IP into the CP guest's /etc/hosts so
	// that dial reaches the relay directly, pre-Caddy (a DHCP re-lease between
	// the relay boot/record and here would otherwise leave a stale pin).
	if err := e.pinRelayInCp(); err != nil {
		return err
	}
	if err := e.stageDeployAgentTools(); err != nil {
		return err
	}

	// 12. hand the world's secrets to the CP: the DNS provider creds sealed to
	// the agent-tools identity (world_build's cert issue path reads them).
	if err := e.handoffDNS(); err != nil {
		return err
	}

	// 13. TRIGGER world_build — the CP brings up the whole world through its
	// co-located runner.
	if err := e.triggerWorldBuild(); err != nil {
		return err
	}

	// 14. record the post-world coordinates (relay/k3s coords + litellm/caddy
	// coords world_build established) so the config + TUI + teardown agree.
	if err := e.recordPostWorld(); err != nil {
		return err
	}

	// 15. the CPA + created-agent reconcile (through the CP toolset, the audited
	// create path).
	if err := e.stageCpa(); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ CPA live in Buzz ("+e.f.agentName+")")
	if err := e.reconcileCreatedAgents(); err != nil {
		return err
	}

	// 16. the relay's signing key (best-effort) + final merge save.
	if rpk, ok := e.relayPubkeyNip11(); ok {
		fmt.Fprintf(e.out, "  ✓ relay signing key: %s\n", rpk)
	} else {
		fmt.Fprintln(e.out, "  (relay signing key unreadable via NIP-11 — read it from the relay's data dir when you need --relay-pubkey)")
	}
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
  (the CP owns relay/k3s/storage/DNS/litellm/caddy/cert — a fresh box only
   needs: freehold login → freehold)
`, e.f.relayDomain, e.f.cpDomain, e.f.addr, e.f.operatorPubkey)
	return nil
}

// slimSeedLiteLLM seals the litellm master + postgres pw + provider key into
// the PROXMOX-BOX runner package, which deploy-cp re-ships as the CP's
// co-located runner — world_build's litellm step then applies the kube
// workloads + registers the model with the secrets requested BY NAME. The
// master is read back from the canonical k8s Secret when the k3s node is up,
// else minted (first-run-wins, preserved in the runner package).
func (e *rebuildEngine) slimSeedLiteLLM() error {
	masterKey := ""
	if cfg, _ := config.Load(e.f.configPath); cfg != nil && cfg.Lxc.K3s.Vmid != nil {
		masterKey = e.litellmMasterKey(*cfg.Lxc.K3s.Vmid)
	}
	if masterKey == "" {
		masterKey = stages.GenSecretHex()
	}
	postgresPw := stages.GenSecretHex()
	runnerDir := filepath.Join(rbRunnerPkgs(), e.f.target)
	reusing := e.f.litellmProviderKey == "" && litellmHasProviderKey(runnerDir)
	providerKey := e.f.litellmProviderKey
	if !reusing && providerKey == "" {
		if e.f.yes {
			return fmt.Errorf("litellm needs the provider key and none is sealed: --litellm-provider-key or FREEHOLD_LITELLM_PROVIDER_KEY (headless --yes)")
		}
		answer, err := e.prompt("litellm first provision: the provider (fireworks) API key")
		if err != nil {
			return err
		}
		providerKey = strings.TrimSpace(answer)
		if providerKey == "" {
			return fmt.Errorf("no litellm provider key supplied")
		}
	}
	// Seal into the box runner package (shipped to the co-located runner).
	if err := e.sealRunnerSecret("litellm", "FREEHOLD_LITELLM_MASTER", masterKey); err != nil {
		return err
	}
	if err := e.sealRunnerSecret("postgres-pw", "FREEHOLD_LITELLM_PG", postgresPw); err != nil {
		return err
	}
	if !reusing && providerKey != "" {
		if err := e.sealRunnerSecret("provider-key", "FREEHOLD_LITELLM_PROVIDER", providerKey); err != nil {
			return err
		}
	}
	return nil
}

// agentToolsEncPubkey returns the agent-tools identity's ENCRYPTION pubkey —
// read from the deployed server IN THE cp LXC via its own `identity --enc-pubkey`
// subcommand (the private enc secret NEVER leaves the CP; only the derived
// pubkey is returned). It is the recipient the box seals the world's secrets to
// for the hand-off (world_build opens them with the same identity's enc secret,
// in-process).
func (e *rebuildEngine) agentToolsEncPubkey() ([]byte, error) {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Lxc.Cp.Vmid == nil {
		return nil, fmt.Errorf("no cp coords for the agent-tools hand-off")
	}
	ok, out := e.runBin(e.bins.Self, e.execArgs(fmt.Sprintf(
		"pct exec %d -- sh -c '/srv/data/cp/bin/freehold-agent-tools identity --state-dir /srv/data/cp/agent-tools --enc-pubkey'",
		*cfg.Lxc.Cp.Vmid), 30))
	if !ok {
		return nil, fmt.Errorf("agent-tools enc pubkey unreadable in the cp LXC:\n%s", out)
	}
	pk := strings.TrimSpace(out)
	if len(pk) != 64 {
		return nil, fmt.Errorf("agent-tools enc pubkey readback not 64-hex: %q", pk)
	}
	return hex.DecodeString(pk)
}

// handoffDNS ships the world's DNS provider creds (relay + cp slots) to the CP,
// sealed to the agent-tools identity via the established cert.SaveCreds record,
// under the CP's world-secrets dir (world_build's cert issue path opens them).
func (e *rebuildEngine) handoffDNS() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Lxc.Cp.Vmid == nil {
		return fmt.Errorf("no cp coords for the DNS hand-off")
	}
	pub, err := e.agentToolsEncPubkey()
	if err != nil {
		return err
	}
	seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
	for _, slot := range []string{"relay", "cp"} {
		host := e.f.relayDomain
		if slot == "cp" {
			host = e.f.cpDomain
		}
		provider, env, err := e.promptDNSCred(slot, host, "")
		if err != nil {
			return fmt.Errorf("%s DNS provider credential: %w", slot, err)
		}
		local := filepath.Join(os.TempDir(), "fh-dns-"+slot+".json")
		if err := cert.SaveCreds(local, provider, env, seal, pub, "cert-dns-"+slot); err != nil {
			return err
		}
		remote := "/srv/data/cp/agent-tools/world-secrets/dns-" + slot + ".json"
		cp := *cfg.Lxc.Cp.Vmid
		if ok, out := e.runBin(e.bins.Self, e.execArgs(fmt.Sprintf("pct exec %d -- sh -c 'mkdir -p /srv/data/cp/agent-tools/world-secrets'", cp), 30)); !ok {
			_ = os.Remove(local)
			return fmt.Errorf("mkdir CP world-secrets failed:\n%s", out)
		}
		ok, out := e.runBin(e.bins.Self, e.execArgs(fmt.Sprintf("pct push %d %s %s", cp, local, remote), 60))
		_ = os.Remove(local)
		if !ok {
			return fmt.Errorf("ship %s DNS cred to the CP failed:\n%s", slot, out)
		}
		fmt.Fprintf(e.out, "  ✓ %s DNS credential handed off to the CP\n", slot)
	}
	return nil
}

// pinRelayInCp (re-)pins the relay LAN IP into the CP guest's /etc/hosts so
// the agent-tools seed/roster dial `http://<relayDomain>:3000` directly,
// pre-Caddy (buzz keys the community to the Host header — a raw IP gets
// "no community is configured for this host"). Idempotent (grep -Fq guard);
// a DHCP re-lease between the relay boot/record and here is corrected.
func (e *rebuildEngine) pinRelayInCp() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Lxc.Cp.Vmid == nil || cfg.Lxc.Relay.Ip == nil {
		return fmt.Errorf("no cp/relay coords to pin the relay host")
	}
	relayIP := config.StripCIDR(*cfg.Lxc.Relay.Ip)
	host := cfg.RelayHost()
	cmd := fmt.Sprintf("pct exec %d -- sh -c \"grep -Fq '%s' /etc/hosts 2>/dev/null || echo '%s %s' >> /etc/hosts\"",
		*cfg.Lxc.Cp.Vmid, host, relayIP, host)
	ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 30))
	if !ok {
		return fmt.Errorf("pin the relay host into the cp LXC failed:\n%s", out)
	}
	return nil
}

// triggerWorldBuild signs world_build over the agent-tools MCP (the box ops
// identity — roster-granted at bootstrap) and prints the CP's report.
func (e *rebuildEngine) triggerWorldBuild() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	mc, err := e.agentToolsMcp(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  · triggering the CP world_build (relay/k3s/storage/DNS/litellm/caddy/cert through the co-located runner; several minutes)…")
	text, err := callAgentToolsText(mc, "world_build", map[string]interface{}{})
	if err != nil {
		return fmt.Errorf("world_build trigger: %w", err)
	}
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			fmt.Fprintf(e.out, "  ✓ %s\n", strings.TrimSpace(l))
		}
	}
	return nil
}

// recordPostWorld records what world_build established: the relay/k3s coords
// (read back through the runner) + the litellm/caddy coords (deterministic
// from the proxy IP) + the DNS record mirror, so the config + TUI + teardown
// agree with the CP-built world.
func (e *rebuildEngine) recordPostWorld() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil {
		return fmt.Errorf("no config at %s", e.f.configPath)
	}
	for _, role := range []string{"relay", "k3s"} {
		if vmid, verr := e.findLxcVmidExact(role); verr == nil {
			if ip, ierr := e.readLxcIP(vmid); ierr == nil {
				ipCIDR := ip
				if !strings.Contains(ip, "/") {
					ipCIDR = ip + "/24"
				}
				switch role {
				case "relay":
					cfg.Lxc.Relay = config.LxcGuest{Vmid: &vmid, Ip: &ipCIDR}
				case "k3s":
					cfg.Lxc.K3s = config.LxcGuest{Vmid: &vmid, Ip: &ipCIDR}
				}
			} else {
				fmt.Fprintf(e.out, "  (record %s coords: ip readback failed: %v)\n", role, ierr)
			}
		} else {
			fmt.Fprintf(e.out, "  (record %s coords: no guest found by hostname — is the world_build boot complete? %v)\n", role, verr)
		}
	}
	proxyIP := config.StripCIDR(e.f.proxyIP)
	cfg.Litellm = config.LitellmSpec{URL: "http://" + proxyIP + ":31400", Host: proxyIP}
	if !containsStr(cfg.Managed, "litellm") {
		cfg.Managed = append(cfg.Managed, "litellm")
	}
	cfg.Caddy = config.CaddySpec{URL: cfg.RelayURL, Host: proxyIP}
	if !containsStr(cfg.Managed, "caddy") {
		cfg.Managed = append(cfg.Managed, "caddy")
	}
	// DNS mirror (the records world_build registered).
	recs := map[string]string{}
	for _, r := range stages.DnsRecords(cfg.RelayHost(), lxcIP(cfg.Lxc.Relay), cfg.CPHost(), lxcIP(cfg.Lxc.Cp), proxyIP, proxyIP) {
		recs[r.Name] = r.IP
	}
	if cfg.Dns.Records == nil {
		cfg.Dns.Records = map[string]string{}
	}
	for k, v := range recs {
		cfg.Dns.Records[k] = v
	}
	return cfg.Save(e.f.configPath)
}

func lxcIP(g config.LxcGuest) string {
	if g.Ip == nil {
		return ""
	}
	return config.StripCIDR(*g.Ip)
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
// stage_any: the CLI's flattened CommonArgs go AFTER the name).
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
	// The post-world recorder (recordPostWorld) persists the coords + sections
	// world_build established to disk; finalSave rebuilds from answers and must
	// keep them (like Plane) or it would silently erase them on every run —
	// Caddy coords, Litellm, Dns.Records (resolver mirror) + Dns.Manager
	// survive; a run that manages DNS itself overrides the manager, otherwise
	// the recorded one stays so teardown --remove-dns still knows whom to ask.
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
	// The CP's freehold-agent-tools coords survive across teardown+rebuild so
	// the reconciler can call the (re-seeded) server; a fresh run fills them in.
	if cfg.AgentToolsURL == "" {
		cfg.AgentToolsURL = prev.AgentToolsURL
	}
	if cfg.AgentToolsPubkey == "" {
		cfg.AgentToolsPubkey = prev.AgentToolsPubkey
	}
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

// stages.K3sLocalPathDurableScript re-points the local-path StorageClass's backing
// store at the DURABLE plane mount (/srv/data/k8s-volumes — backup=1, survives
// a compute teardown) instead of the default /var/lib/rancher/k3s/storage on the
// ephemeral rootfs, so k8s PVCs (Caddy's cert, litellm postgres) survive a
// compute teardown and a rebuild. The WHOLE ConfigMap is re-emitted (config.json
// + helperPod/setup/teardown) so `apply` replaces the k3s-managed default
// wholesale rather than dropping the helper-pod config. Re-asserted on every
// reconcile: the k3s local-storage Addon can reset the ConfigMap on a k3s
// restart. Idempotent.

// See stages.K3sLocalPathDurableScript for the re-point script (shared so the
// CP world_build executor runs the same bytes).

// stageDeployRelay deploys the Buzz relay into the relay LXC (box-side — the
// slim build boots + deploys the relay before agent-tools, whose roster lives
// on it).
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
		"--runner-package", rbRunnerPkgs() + "/" + e.f.target,
		"--operator-pubkey", e.f.operatorPubkey,
	}
	// The relay signing pubkey is the /api/world trust anchor a fresh login box
	// seeds — read it from the relay's own compose .env (deterministic, unlike
	// NIP-11) so the deployed CP serves WITH its relay coords recorded.
	if rpk := e.relaySigningPubkey(); rpk != "" {
		args = append(args, "--relay-pubkey", rpk)
	}
	// The agent-tools coords survive a rebuild (recorded after the first
	// deploy-cp, before agent-tools exists) — re-pass them so the CP serves
	// /api/world WITH the toolset coords a fresh login box needs.
	if cfg, _ := config.Load(e.f.configPath); cfg != nil && cfg.AgentToolsURL != "" && cfg.AgentToolsPubkey != "" {
		args = append(args, "--agent-tools-url", cfg.AgentToolsURL, "--agent-tools-pubkey", cfg.AgentToolsPubkey)
	}
	// Pin the relay's LAN IP into the CP guest's /etc/hosts so the console +
	// agent-tools can RESOLVE + reach the relay DOMAIN directly (pre-Caddy):
	// buzz keys the community to the Host header, so a raw-IP URL fails.
	if cfg, _ := config.Load(e.f.configPath); cfg != nil && cfg.Lxc.Relay.Ip != nil {
		args = append(args, "--relay-host-ip", config.StripCIDR(*cfg.Lxc.Relay.Ip))
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

// certIdent returns the ops identity's encryption secret (raw bytes), the
// identity, or an error. The ops identity is freehold's own — the only key that
// must be able to reopen the sealed DNS token (the DNS-cred collection + the
// hand-off seal the relay/cp creds to it).
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

// Secret minting (master / postgres) lives in stages.GenSecretHex.

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

// The cert install script lives in stages.CaddyCertInstallScript (shared so the
// CP world_build executor runs the same install).

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

// agentToolsMcp returns a signed MCP client to the CP's freehold-agent-tools
// server, authenticated as the build/ops identity (the roster grant seeded at
// bootstrap). The audience is the agent-tools server's own pubkey — the same
// signed-header scheme a runner caller uses.
func (e *rebuildEngine) agentToolsMcp(cfg *config.Config) (*client.McpClient, error) {
	if cfg == nil || cfg.AgentToolsURL == "" || cfg.AgentToolsPubkey == "" {
		return nil, fmt.Errorf("no freehold-agent-tools coords recorded — deploy the agent-tools stage first")
	}
	auth, err := flows.AgentAuth(rbOpsDir())
	if err != nil {
		return nil, err
	}
	return client.New(client.ConnectURL(cfg.AgentToolsURL), auth, cfg.AgentToolsPubkey)
}

// callAgentToolsText issues one tool call to a freehold-agent-tools client and
// returns the result.content[0].text payload (e.g. the new agent's pubkey).
func callAgentToolsText(mc *client.McpClient, tool string, args map[string]interface{}) (string, error) {
	// world_build runs its stages synchronously for minutes — use the long
	// deadline (the short default would time out awaiting the response). Call
	// exactly ONE path: the 30s Call would ALSO start the tool server-side and
	// leave it running while a second call raced it.
	long := map[string]bool{"world_build": true}
	var raw json.RawMessage
	var err error
	if long[tool] {
		raw, err = mc.CallLong(tool, args)
	} else {
		raw, err = mc.Call(tool, args)
	}
	if err != nil {
		return "", err
	}
	var env struct {
		Result *struct {
			Content []map[string]any `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("agent-tools %s: bad envelope: %w", tool, err)
	}
	if env.Result == nil || len(env.Result.Content) == 0 {
		return "", fmt.Errorf("agent-tools %s: empty result", tool)
	}
	t, _ := env.Result.Content[0]["text"].(string)
	return t, nil
}

// stageDeployAgentTools ships + seeds + launches the CP's freehold-agent-tools
// MCP server (after the CP + co-located runner + relay + DNS/Caddy are up), and
// records its coords so stageCpa/reconcile can call it. The server's roster is
// seeded with the build/ops + operator identities, and the server is granted on
// the CP's co-located runner so it can apply agent pods.
func (e *rebuildEngine) stageDeployAgentTools() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s", e.f.configPath)
	}
	// Fresh-build path: deploy-cp ran BEFORE agent-tools existed, so the CP
	// serve does not yet record the toolset coords. After the deploy below we
	// re-run deploy-cp (idempotent — stops the prior serve, re-ships, restarts
	// WITH the new flags) so /api/world serves them for a future login box. On
	// rebuild the coords survive in config, stageDeployCp passed them up front,
	// and this re-deploy is skipped.
	hadCoords := cfg.AgentToolsURL != "" && cfg.AgentToolsPubkey != ""
	if cfg.Lxc.Cp.Vmid == nil {
		return fmt.Errorf("need cp coords to deploy agent-tools")
	}
	binDir, stateDir, err := e.cpGuestDirs()
	if err != nil {
		return err
	}
	root := filepath.Dir(stateDir)
	agentToolsState := filepath.Join(root, "agent-tools")

	auth, err := flows.AgentAuth(rbOpsDir())
	if err != nil {
		return err
	}
	dclient, err := client.New(client.ConnectURL(e.f.addr), auth, cfg.Runner.Pubkey)
	if err != nil {
		return err
	}
	opsPK, err := loadRPubkey(rbOpsDir())
	if err != nil {
		return err
	}
	grantsCSV := strings.Join([]string{opsPK, cfg.OperatorPubkey}, ",")
	litellmBase := ""
	if cfg.Litellm.URL != "" {
		litellmBase = strings.TrimSuffix(cfg.Litellm.URL, "/")
	}
	cpaName := cfg.CPAName
	if cpaName == "" {
		cpaName = agent.DefaultCPAName
	}
	cpIP := ""
	if cfg.Lxc.Cp.Ip != nil {
		cpIP = config.StripCIDR(*cfg.Lxc.Cp.Ip)
	}
	res, err := cpdeploy.DeployAgentTools(dclient, e.f.target, &cpdeploy.DeployAgentToolsSpec{
		LXc:                cfg.Lxc.Cp.Vmid,
		BinDir:             binDir,
		StateDir:           stateDir,
		AgentToolsBinary:   e.bins.ReleaseAgentTools,
		AgentToolsStateDir: agentToolsState,
		BindAddr:           "0.0.0.0:8089",
		// The agent-tools serve's OWN relay ops (roster, seed, create-agent
		// publish) use the relay's LAN-REACHABLE DOMAIN URL — the CP guest's
		// /etc/hosts (deploy-cp pins it) maps the domain to the relay LXC IP,
		// so it works BEFORE the Caddy edge (world_build deploys it later) AND
		// the buzz Host header is the configured community domain (a raw-IP
		// Host gets "no community is configured for this host"). The pods dial
		// the PUBLIC --relay-ws (unchanged below).
		RelayURL:     "http://" + cfg.RelayHost() + ":3000",
		RelayAuthURL: cfg.RelayURL,
		RelayPubkey:  derefStrPtr(cfg.RelayPubkey),
		RelayWS:      cfg.RelayWsURL,
		RelayHost:    cfg.RelayHost(),
		RelayIP:      config.StripCIDR(derefStrPtr(cfg.Lxc.Relay.Ip)),
		CpHost:       cfg.CPHost(),
		CpIP:         config.StripCIDR(derefStrPtr(cfg.Lxc.Cp.Ip)),
		RelayLxc:     cfg.Lxc.Relay.Vmid,
		RelayCompose: stages.RelayComposeDir,
		K3sVmid:      derefU32(cfg.Lxc.K3s.Vmid),
		CpLxc:        cfg.Lxc.Cp.Vmid,
		ProxyIP:      config.StripCIDR(derefStrPtr(cfg.Proxy.Ip)),
		LiteLLMIP:    cfg.Litellm.Host,
		RunnerAddr:   "127.0.0.1:8787",
		RunnerPubkey: cfg.Runner.Pubkey,
		RunnerTarget: cfg.Runner.Target,
		RunnerName:   cfg.Runner.Target,
		CpaName:      cpaName,
		OwnerPubkey:  cfg.OperatorPubkey,
		LiteLLMBase:  litellmBase,
		PlanePool:    derefStrPtr(cfg.Plane.Backend),
		PlaneKind:    derefStrPtr(cfg.Plane.BackendKind),
		ThinPool:     derefStrPtr(cfg.Plane.ThinPool),
		SizeGB:       e.f.sizeGB,
		PoolSizeGB:   e.f.poolSizeGB,
		RootfsGB:     e.f.rootfsGB,
		MemoryMB:     e.f.memoryMB,
		Storage:      e.f.storageName,
		RelayGW:      e.f.relayGw,
		Bridge:       e.f.bridge,
		SelfURL:      "http://" + cpIP + ":8089",
		GrantsCSV:    grantsCSV,
	})
	if err != nil {
		return fmt.Errorf("deploy agent-tools: %w", err)
	}
	if cpIP == "" {
		return fmt.Errorf("no cp IP recorded to reach the agent-tools server")
	}
	cfg.AgentToolsURL = fmt.Sprintf("http://%s:8089", cpIP)
	cfg.AgentToolsPubkey = res.Pubkey
	if err := cfg.Save(e.f.configPath); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ freehold-agent-tools live on the CP (audience %s)\n", res.Pubkey[:12])

	if !hadCoords {
		fmt.Fprintln(e.out, "  · re-running deploy-cp to record the agent-tools coords on the CP serve…")
		if err := e.stageDeployCp(); err != nil {
			return fmt.Errorf("re-deploy-cp to record agent-tools coords: %w", err)
		}
	}
	return nil
}

func derefStrPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefU32(p *uint32) uint32 {
	if p == nil {
		return 0
	}
	return *p
}

// stageCpa provisions the CPA through the CP's freehold-agent-tools MCP server
// — the SAME audited create_agent the CPA itself will use for agent-creates-
// agent. The agent-tools server mints the CPA identity on the CP's durable
// plane, onboards it as a relay member, seats it in #freehold, applies its pod
// (branching to the CPA manifest/prompt for the CPA name), and registers it.
// The build signs the MCP call as the build/ops identity — the roster grant
// seeded at bootstrap. Identity is CP-durable, so the same agent returns across
// a rebuild.
func (e *rebuildEngine) stageCpa() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil || cfg.Lxc.Cp.Vmid == nil || cfg.Lxc.K3s.Vmid == nil {
		return fmt.Errorf("no cp/k3s coords recorded — the CPA pod needs the k3s substrate")
	}
	if cfg.RelayURL == "" {
		return fmt.Errorf("no relay URL in config — the CPA must join the community relay")
	}
	if cfg.Litellm.URL == "" {
		return fmt.Errorf("no litellm gateway recorded — the CPA needs a reasoning model (litellm is part of the world unless --no-litellm)")
	}
	cpaName := cfg.CPAName
	if cpaName == "" {
		cpaName = agent.DefaultCPAName
	}
	mc, err := e.agentToolsMcp(cfg)
	if err != nil {
		return err
	}
	text, err := callAgentToolsText(mc, "create_agent", map[string]interface{}{
		"name":    cpaName,
		"purpose": "the control plane agent — freehold's main reasoning touchpoint",
	})
	if err != nil {
		return fmt.Errorf("create CPA over agent-tools: %w", err)
	}
	cpaPub := strings.TrimSpace(text)
	if !isHex64(cpaPub) {
		return fmt.Errorf("create CPA returned a non-pubkey result: %q", cpaPub)
	}
	fmt.Fprintf(e.out, "  · CPA live in Buzz (%s)\n", cpaName)
	return e.recordCpa(cpaPub, cpaName)
}

// reconcileCreatedAgents redeploys every agent the CP's freehold-agent-tools
// registry holds (the CP-durable source of truth — the CPA plus everything
// created through create_agent), so a rebuild resurrects them idempotently with
// the same durable pubkeys (E3). The CPA name itself is created by stageCpa.
func (e *rebuildEngine) reconcileCreatedAgents() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil {
		return err
	}
	if cfg == nil || cfg.AgentToolsURL == "" {
		return nil
	}
	cpaName := cfg.CPAName
	if cpaName == "" {
		cpaName = agent.DefaultCPAName
	}
	mc, err := e.agentToolsMcp(cfg)
	if err != nil {
		return err
	}
	listText, err := callAgentToolsText(mc, "manage_agent", map[string]interface{}{})
	if err != nil {
		return fmt.Errorf("list agents over agent-tools: %w", err)
	}
	var agents []console.AgentInfo
	if err := json.Unmarshal([]byte(listText), &agents); err != nil {
		return fmt.Errorf("parse agent list: %w: %s", err, listText)
	}
	for _, a := range agents {
		if a.Name == "" || a.Name == cpaName {
			continue
		}
		// create_agent is idempotent (the CP-durable identity is reused), so
		// re-creating reseats the pod with the same pubkey across a rebuild.
		// Thread the registry's preserved purpose through so the agent's
		// system-prompt purpose line survives, not just its identity.
		text, cerr := callAgentToolsText(mc, "create_agent", map[string]interface{}{"name": a.Name, "purpose": a.Purpose})
		if cerr != nil {
			return fmt.Errorf("reconcile created agent %s: %w", a.Name, cerr)
		}
		fmt.Fprintf(e.out, "  · reconciled agent %s (pubkey %s)\n", a.Name, firstHex(text))
	}
	return nil
}

func firstHex(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		return s[:64]
	}
	return s
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

// relaySigningPubkey resolves the relay's signing pubkey (the 39002 trust
// anchor /api/world reports) DETERMINISTICALLY at deploy-cp time, instead of
// the NIP-11 best-effort that often comes back empty. The authoritative source
// is the relay's own signing secret (BUZZ_RELAY_PRIVATE_KEY in the relay LXC's
// compose .env, written by deploy-relay) — read in-guest, the secret never
// leaves the relay; only the DERIVED pubkey is returned. Falls back to NIP-11,
// then to the recorded config value. Returns "" when unreadable (the serve
// relay-url/pubkey coupling is relaxed, so a missing pubkey no longer blocks).
func (e *rebuildEngine) relaySigningPubkey() string {
	if cfg, err := config.Load(e.f.configPath); err == nil && cfg != nil && cfg.Lxc.Relay.Vmid != nil {
		ok, out := e.runBin(e.bins.Self, e.execArgs(fmt.Sprintf(
			"pct exec %d -- sh -c \"sed -n 's/^BUZZ_RELAY_PRIVATE_KEY=//p' %s/.env 2>/dev/null\"",
			*cfg.Lxc.Relay.Vmid, stages.RelayComposeDir), 30))
		if ok {
			secret := strings.TrimSpace(out)
			if _, err := hex.DecodeString(secret); err == nil && len(secret) == 64 {
				if raw, err := hex.DecodeString(secret); err == nil {
					if pk, err := crypto.PubkeyFromSecret(raw); err == nil && len(pk) == 64 {
						return pk
					}
				}
			}
		}
	}
	if rpk, ok := e.relayPubkeyNip11(); ok {
		return rpk
	}
	if cfg, err := config.Load(e.f.configPath); err == nil && cfg != nil && cfg.RelayPubkey != nil {
		return *cfg.RelayPubkey
	}
	return ""
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
