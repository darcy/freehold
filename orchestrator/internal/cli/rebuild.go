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
	"encoding/hex"
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

	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/crypto"
	"freehold/orchestrator/internal/drive"
	"freehold/orchestrator/internal/flows"
	"freehold/orchestrator/internal/wire"
)

var rebuildCmd = &cobra.Command{
	Use:   "rebuild",
	Short: "Bring the whole world up end to end: door, runner, durable plane, relay + CP + k3s LXCs, deploys — the installer pipeline as one command",
	RunE: func(cmd *cobra.Command, args []string) error {
		f := rebuildFlags{}
		f.addr, _ = cmd.Flags().GetString("addr")
		f.target, _ = cmd.Flags().GetString("target")
		f.host, _ = cmd.Flags().GetString("host")
		f.domain, _ = cmd.Flags().GetString("domain")
		f.operatorPubkey, _ = cmd.Flags().GetString("operator-pubkey")
		f.operatorIdentity, _ = cmd.Flags().GetString("operator-identity")
		f.sizeGB, _ = cmd.Flags().GetUint64("size-gb")
		f.poolSizeGB, _ = cmd.Flags().GetUint64("pool-size-gb")
		f.thinPool, _ = cmd.Flags().GetString("thin-pool")
		f.withK3s, _ = cmd.Flags().GetBool("with-k3s")
		f.withLitellm, _ = cmd.Flags().GetBool("with-litellm")
		f.litellmProviderKey = os.Getenv("FREEHOLD_LITELLM_PROVIDER_KEY")
		if v, _ := cmd.Flags().GetString("litellm-provider-key"); v != "" {
			f.litellmProviderKey = v
		}
		f.rootfsGB, _ = cmd.Flags().GetUint32("rootfs-gb")
		f.memoryMB, _ = cmd.Flags().GetUint32("memory-mb")
		f.relayGw, _ = cmd.Flags().GetString("relay-gw")
		f.relayIP, _ = cmd.Flags().GetString("relay-ip")
		f.cpIP, _ = cmd.Flags().GetString("cp-ip")
		f.k3sIP, _ = cmd.Flags().GetString("k3s-ip")
		f.configPath, _ = cmd.Flags().GetString("config")
		f.confirmStorage, _ = cmd.Flags().GetBool("confirm-storage")
		f.yes, _ = cmd.Flags().GetBool("yes")
		// STATIC IPs must be CIDR — pct create's net0=ip= wants host/prefix,
		// and failing that at stage 7 is a long-run failure; say it up front.
		for _, ip := range []string{f.relayIP, f.cpIP, f.k3sIP} {
			if ip != "" && !strings.Contains(ip, "/") {
				return fmt.Errorf("STATIC guest IPs are CIDR (host/prefix) — got %q", ip)
			}
		}

		eng, err := newRebuildEngine(f)
		if err != nil {
			return err
		}
		return eng.run()
	},
}

func init() {
	rebuildCmd.Flags().String("addr", "127.0.0.1:8787", "Runner MCP address (loopback)")
	rebuildCmd.Flags().String("target", "proxmox-box", "Runner name (the package + grant + target name)")
	rebuildCmd.Flags().String("host", "root@192.168.30.224", "Proxmox host address the runner SSH's into")
	rebuildCmd.Flags().String("domain", "", "Relay identity domain (must resolve to the host — the A4 gate; REQUIRED)")
	rebuildCmd.Flags().String("operator-pubkey", "", "Operator Nostr pubkey (64-hex) — console admin + relay owner (REQUIRED)")
	rebuildCmd.Flags().String("operator-identity", "", "Operator identity dir to record in the config (optional)")
	rebuildCmd.Flags().Uint64("size-gb", drive.TenantLVSizeGB, "Per-tenant thin LV size in GiB (LVM-thin backend)")
	rebuildCmd.Flags().Uint64("pool-size-gb", drive.FreshPoolSizeGB, "Thin-pool size in GiB when a NEW pool is carved")
	rebuildCmd.Flags().String("thin-pool", "", "Plane placement: the thin pool the tenant LVs land in — the name of an EXISTING pool to reuse, or a NEW name to carve (then carved at --pool-size-gb). Absent => interactive prompt, or reuse-detected/carve-default under --yes")
	rebuildCmd.Flags().Bool("with-k3s", true, "Boot + install the k3s substrate LXC as part of the world")
	rebuildCmd.Flags().Uint32("rootfs-gb", 16, "LXC rootfs size in GB")
	rebuildCmd.Flags().Uint32("memory-mb", 2048, "LXC memory in MB")
	rebuildCmd.Flags().String("relay-gw", "192.168.30.1", "Gateway for STATIC guest IPs (unused with DHCP)")
	rebuildCmd.Flags().Bool("with-litellm", false, "C0: deploy the litellm gateway (kube workloads + runner + model registration) during the rebuild")
	rebuildCmd.Flags().String("litellm-provider-key", "", "Fireworks/upstream provider API key for litellm's model (C0 — supplied at bootstrap, never committed; read from --litellm-provider-key or FREEHOLD_LITELLM_PROVIDER_KEY)")
	rebuildCmd.Flags().String("relay-ip", "", "STATIC relay LXC IP (CIDR, e.g. 192.168.30.8/24) — absent => DHCP (Rust: the recorded config ip is re-booted static)")
	rebuildCmd.Flags().String("cp-ip", "", "STATIC control-plane LXC IP (CIDR) — absent => DHCP")
	rebuildCmd.Flags().String("k3s-ip", "", "STATIC k3s LXC IP (CIDR) — absent => DHCP")
	rebuildCmd.Flags().String("config", defaultConfigPath(), "Config path (default: ~/.config/freehold/config.toml)")
	rebuildCmd.Flags().Bool("confirm-storage", false, "Operator consent to CREATE a storage backend when none is detected")
	rebuildCmd.Flags().Bool("yes", false, "Non-interactive: bail (actionably) where the interactive pipeline would prompt")
}

// rebuildFlags is the command's collected answers.
type rebuildFlags struct {
	addr               string
	target             string
	host               string
	domain             string
	operatorPubkey     string
	operatorIdentity   string
	sizeGB             uint64
	poolSizeGB         uint64
	thinPool           string
	withK3s            bool
	withLitellm        bool
	litellmProviderKey string
	rootfsGB           uint32
	memoryMB           uint32
	relayGw            string
	relayIP            string
	cpIP               string
	k3sIP              string
	configPath         string
	confirmStorage     bool
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
}

func newRebuildEngine(f rebuildFlags) (*rebuildEngine, error) {
	if f.domain == "" {
		return nil, fmt.Errorf("rebuild needs --domain (the relay's identity)")
	}
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
	fmt.Fprintf(e.out, "rebuilding world %s (runner %s @ %s)\n", e.f.domain, e.f.target, e.f.addr)

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

	// 6. write the INITIAL config so stage_storage has somewhere to record
	// the durable-plane mapping (merge preserves a surviving config's facts).
	if err := e.writeInitialConfig(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ wrote config %s\n", e.f.configPath)

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
	if err := e.stageBootstrap("relay"); err != nil {
		return err
	}
	if _, err := e.stageRecordLxc("relay"); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ relay LXC booted + recorded")

	// 10-11. boot the cp LXC, record.
	if err := e.stageBootstrap("cp"); err != nil {
		return err
	}
	if _, err := e.stageRecordLxc("cp"); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ cp LXC booted + recorded")

	// 12. the k3s substrate (boot-if-missing + in-guest install + record).
	if e.f.withK3s {
		if err := e.stageK3s(); err != nil {
			return err
		}
		fmt.Fprintln(e.out, "  ✓ k3s substrate ready")
	}

	// 13-14. deploy the relay + the control plane.
	if err := e.stageDeployRelay(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ relay live at https://%s\n", e.f.domain)
	if err := e.stageDeployCp(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "  ✓ control plane live at https://cp-%s\n", e.f.domain)

	// 14.5. C0: the litellm gateway — provision the litellm runner (master +
	// provider + postgres secrets sealed to it), apply the kube workloads,
	// register the model, and record the coords for the Services row.
	if e.f.withLitellm {
		if err := e.stageLitellm(); err != nil {
			return err
		}
		fmt.Fprintln(e.out, "  ✓ litellm gateway live")
	}

	// 14.7. C0: register the CP resolver's explicit records (relay/cp/k3s +
	// litellm) INSIDE the deployed CP, then point every guest at it.
	if err := e.stageDnsRegister(); err != nil {
		return err
	}
	if err := e.stageDnsPoint(); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "  ✓ internal DNS resolver live (CP-owned)")

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
  control plane:  https://cp-%s
  runner:         serving on %s
  operator pk:    %s
`, e.f.domain, e.f.domain, e.f.addr, e.f.operatorPubkey)
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
  Fresh start:  rm -rf ~/.freehold && freehold rebuild ...
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
	cfg := &config.Config{
		Domain:         e.f.domain,
		RelayURL:       "https://" + e.f.domain,
		CPURL:          "https://cp-" + e.f.domain,
		OperatorPubkey: e.f.operatorPubkey,
		Runner: config.RunnerRef{
			Addr:   e.f.addr,
			Pubkey: runnerPK,
			Target: e.f.target,
		},
		Managed: []string{"relay", "cp"},
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
			"--domain", e.f.domain,
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
		"--domain", e.f.domain,
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

// bootstrapStaticIP is the role's STATIC guest IP: an explicit --lxc-ip flag
// wins; else the RECORDED config ip rides again (Rust Answers::from_config —
// the operator owns the addressing + the proxy target); else "" = DHCP.
func bootstrapStaticIP(role string, f rebuildFlags, cfg *config.Config) string {
	ip := map[string]string{"relay": f.relayIP, "cp": f.cpIP, "k3s": f.k3sIP}[role]
	if ip == "" && cfg != nil {
		g := map[string]config.LxcGuest{"relay": cfg.Lxc.Relay, "cp": cfg.Lxc.Cp, "k3s": cfg.Lxc.K3s}[role]
		if g.Ip != nil && *g.Ip != "" {
			ip = *g.Ip
		}
	}
	return ip
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
	exact := lxcName(e.f.domain, role)
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
  INSTALL_K3S_EXEC="server --kubelet-arg feature-gates=KubeletInUserNamespace=true" sh /tmp/k3s-install.sh
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
ExecStart=/usr/local/bin/k3s server --kubelet-arg feature-gates=KubeletInUserNamespace=true
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
		"--domain", e.f.domain,
		"--relay-url", "https://" + e.f.domain,
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
		"--relay-url", "https://" + e.f.domain,
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
	if cfg != nil {
		for _, role := range []string{"relay", "cp", "k3s"} {
			g := map[string]config.LxcGuest{
				"relay": cfg.Lxc.Relay, "cp": cfg.Lxc.Cp, "k3s": cfg.Lxc.K3s,
			}[role]
			if g.Ip != nil {
				recs = append(recs, rec{name: role, ip: config.StripCIDR(*g.Ip), source: "record_lxc " + role})
			}
		}
		if cfg.Litellm.Host != "" {
			recs = append(recs, rec{name: "litellm", ip: cfg.Litellm.Host, source: "litellm-apply"})
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
	// The guests' resolv.conf carries PVE's `search` line; register it as the
	// resolver's world domain so addn-hosts serves <name>.<search> FIRST (the
	// glibc search-first lookup gets the split-horizon answer, not the
	// public/tailscale record through upstream).
	searchBase := e.guestSearchBase()
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

// stageDnsPoint points every managed guest at the CP resolver: write
// nameserver into each LXC's resolv.conf (idempotent) and set k3s coredns's
// `forward .` to the resolver so pods resolve *.freehold.internal.
func (e *rebuildEngine) stageDnsPoint() error {
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Lxc.Cp.Ip == nil {
		return fmt.Errorf("no control-plane IP recorded — cannot point guests at the resolver")
	}
	resolver := config.StripCIDR(*cfg.Lxc.Cp.Ip)

	// LXCs: resolv.conf gains the resolver as the first nameserver.
	for _, role := range []string{"relay", "cp", "k3s"} {
		g := map[string]config.LxcGuest{
			"relay": cfg.Lxc.Relay, "cp": cfg.Lxc.Cp, "k3s": cfg.Lxc.K3s,
		}[role]
		if g.Vmid == nil {
			continue
		}
		ns := fmt.Sprintf("nameserver %s", resolver)
		cmd := fmt.Sprintf(
			"pct exec %d -- sh -c \"grep -qF '%s' /etc/resolv.conf 2>/dev/null || echo '%s' >> /etc/resolv.conf\"",
			*g.Vmid, resolver, ns)
		if ok, out := e.runBin(e.bins.Self, e.execArgs(cmd, 60)); !ok {
			return fmt.Errorf("pointing %s at the resolver failed:\n%s", role, out)
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
	if e.f.litellmProviderKey == "" {
		return fmt.Errorf("litellm needs the provider key at bootstrap: --litellm-provider-key or FREEHOLD_LITELLM_PROVIDER_KEY")
	}
	cfg, err := config.Load(e.f.configPath)
	if err != nil || cfg == nil || cfg.Lxc.K3s.Ip == nil || cfg.Lxc.K3s.Vmid == nil {
		return fmt.Errorf("no k3s coords recorded — cannot place the litellm gateway")
	}
	k3sIP := config.StripCIDR(*cfg.Lxc.K3s.Ip)
	k3sVmid := *cfg.Lxc.K3s.Vmid
	gwURL := "http://" + k3sIP + ":31400"

	masterKey := genSecretHex()  // litellm's admin key (re-mint)
	postgresPw := genSecretHex() // postgres password
	providerKey := e.f.litellmProviderKey

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
	runnerDir := filepath.Join(rbRunnerPkgs(), "litellm")
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
		extraEnv := append(
			[]string{extra.env + "=" + map[string]string{"provider-key": providerKey, "postgres-pw": postgresPw}[extra.name]},
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

	// Serve the litellm runner on loopback, exec the registration through it.
	pid, err := e.stageServeRunner("litellm", runnerDir)
	if err != nil {
		return err
	}
	defer e.stopRunner(pid)

	regScript := litellmRegisterScript()
	ok, out = e.runEnv(e.bins.Self, os.Environ(), e.execArgs(
		fmt.Sprintf("exec --target litellm --secrets litellm,provider-key %s", regScript), 120))
	if !ok {
		return fmt.Errorf("litellm model registration failed:\n%s", out)
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
// runner injects LITELLM (master, Bearer) + PROVIDER_KEY (body) by name.
func litellmRegisterScript() string {
	return `set -euo pipefail
BODY=$(printf '{"model_name":"deepseek-v4-flash","litellm_params":{"model":"fireworks_ai/accounts/fireworks/models/deepseek-v4-flash-0731","api_key":"%s"}}' "$PROVIDER_KEY")
curl -s -m 30 -X POST -H "Authorization: Bearer $LITELLM" -H "Content-Type: application/json" -d "$BODY" "http://127.0.0.1:31400/model/new" | head -c 300
echo
echo LEG2_OK
`
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

// relayPubkeyNip11 reads the relay's signing pubkey via NIP-11 (best-effort
// trust anchor; not fatal when unreadable).
func (e *rebuildEngine) relayPubkeyNip11() (string, bool) {
	text, ok := e.curlGet("https://" + e.f.domain + "/")
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
