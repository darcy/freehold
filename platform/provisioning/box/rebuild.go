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
package box

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

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/planebase"
	"freehold/platform/provisioning/stages"
)

// Flags is a command's collected install/bootstrap answers.
type Flags struct {
	// AccessMode names the install access strategy that reaches Host
	// ("ssh-root-proxmox" today, provider-API modes later) and is recorded in
	// the profile config beside the host.
	AccessMode         string
	Name               string
	Addr               string
	Target             string
	Host               string
	Domain             string
	RelayDomain        string
	CpDomain           string
	ProxyIP            string
	RelayIP            string
	CpIP               string
	OperatorPubkey     string
	OperatorIdentity   string
	AgentName          string
	SizeGB             uint64
	PoolSizeGB         uint64
	ThinPool           string
	PlanePool          string // select a detected backend by name (VG or zpool)
	ConfirmSharedPool  bool   // headless consent to share a pool with live guests
	EraseFreehold      bool   // headless consent to erase a detected freehold plane
	NoK3s              bool
	NoLitellm          bool
	LitellmProviderKey string
	RootfsGB           uint32
	MemoryMB           uint32
	RelayGw            string
	StorageName        string
	Bridge             string
	ConfigPath         string
	ConfirmStorage     bool
	ResetDNS           bool
	// ManageDNSExplicit records whether --manage-dns was EXPLICITLY passed (a
	// bool flag reads false for both omitted and --manage-dns=false; the config
	// seed must only fill the omitted case so an operator can still opt out).
	ManageDNSExplicit bool
	ManageDNS         bool
	Yes               bool
}

// Bins are the resolved sibling binary paths. Go has no
// CARGO_MANIFEST_DIR: everything is resolved relative to THIS executable's
// dir (the layout ships all bins together in target/debug/, release pairs
// in target/release/). Resolved per-CLI (install vs operator resolve different
// siblings); passed in by the caller, never resolved here.
type Bins struct {
	Self              string // this binary
	Console           string // target/debug/freehold-console (Go CP CLI + console server)
	Runner            string // target/debug/runner (Rust)
	ReleaseConsole    string // target/release/freehold-console (deploy-cp --binary)
	ReleaseRun        string // target/release/runner (deploy-cp --runner-binary)
	ReleaseAgentTools string // target/release/freehold-agent-tools (the CP's agent-tools MCP server)
}

// ResolveBins checks the sibling binaries THIS pipeline execs and returns
// their paths, or the exact build one-liner when any is missing. needRelease
// controls whether the target/release sibling pair (console/runner/agent-tools)
// is required — deploy-cp ships them, so only the CP bootstrap needs them.
func ResolveBins() (Bins, error) {
	self, err := os.Executable()
	if err != nil {
		return Bins{}, fmt.Errorf("cannot resolve own binary path: %w", err)
	}
	selfDir := filepath.Dir(self)
	releaseDir := filepath.Join(selfDir, "..", "release")
	b := Bins{
		Self:              self,
		Console:           filepath.Join(selfDir, "freehold-console"),
		Runner:            filepath.Join(selfDir, "runner"),
		ReleaseConsole:    filepath.Join(releaseDir, "freehold-console"),
		ReleaseRun:        filepath.Join(releaseDir, "runner"),
		ReleaseAgentTools: filepath.Join(releaseDir, "freehold-agent-tools"),
	}
	var missing []string
	for _, x := range []struct{ path, label string }{
		{b.Console, "freehold-console"},
		{b.Runner, "runner"},
		{b.ReleaseConsole, "../release/freehold-console"},
		{b.ReleaseRun, "../release/runner"},
		{b.ReleaseAgentTools, "../release/freehold-agent-tools"},
	} {
		if _, err := os.Stat(x.path); err != nil {
			missing = append(missing, filepath.Join(filepath.Base(selfDir), x.label))
		}
	}
	if len(missing) > 0 {
		return b, fmt.Errorf(
			"sibling binaries missing: %s\n  build them once, then re-run:\n    go build -C control-plane -o target/debug/freehold-console ./api/cmd/freehold-console && go build -C control-plane -o target/release/freehold-console ./api/cmd/freehold-console && cargo build --bin runner && cargo build --release --bin runner && CGO_ENABLED=0 go build -C control-plane -o target/release/freehold-agent-tools ./api/cmd/freehold-agent-tools",
			strings.Join(missing, ", "))
	}
	return b, nil
}

// Engine runs the pipeline; every side effect goes through an
// injectable seam so the pure discipline (record/merge/parse) is testable
// hermetically (mirrors Rust record_lxc_with).
type Engine struct {
	F    Flags
	Bins Bins

	Out io.Writer
	In  io.Reader
	// Stdin is the ONE buffered reader over In. prompt() must not build a
	// fresh bufio.Reader per call: a fresh one reads ahead past the first
	// newline into its own buffer, so a back-to-back prompt (the carve
	// size after the pool name) would see an already-drained in and EOF.
	Stdin *bufio.Reader

	// seams
	RunBin   func(bin string, args []string) (bool, string)
	RunEnv   func(bin string, env []string, args []string) (bool, string)
	RunSh    func(script string) (string, error)
	PortOpen func(addr string) bool
	CurlGet  func(url string) (string, bool)

	// Provider is the substrate ops seam (guest list/ip/mounts + storage
	// repoint). Composition roots inject the concrete provider; nil means the
	// pct-dependent stages fail with a clear error.
	Provider provisioning.Provider
}

// HostExecFunc adapts the engine's self-exec transport into the provider's
// host-command seam (used by the composition root to build the provider).
func (e *Engine) HostExecFunc() provisioning.ExecFunc {
	return func(cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
		ok, out := e.RunBin(e.Bins.Self, e.ExecArgs(cmd, int(timeoutS)))
		code := 0
		if !ok {
			code = 1
		}
		return &client.ExecOutcome{Stdout: out, ExitCode: &code}, nil
	}
}

// NewEngine builds the provisioning engine from flags + resolved sibling
// binaries. It normalizes the operator pubkey to 64-hex up front.
func NewEngine(f Flags, bins Bins) (*Engine, error) {
	if f.OperatorPubkey == "" {
		return nil, fmt.Errorf("install needs --operator-pubkey (64-hex or npub1…)")
	}
	pk, err := crypto.ParsePubkeyInput(f.OperatorPubkey)
	if err != nil {
		return nil, err
	}
	f.OperatorPubkey = pk
	e := &Engine{
		F:        f,
		Bins:     bins,
		Out:      os.Stdout,
		In:       os.Stdin,
		RunBin:   runBinDefault,
		RunEnv:   runEnvDefault,
		RunSh:    runShDefault,
		PortOpen: portOpenDefault,
		CurlGet:  curlGetDefault,
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

func StateDir() string   { return filepath.Join(StateRoot(), "control-plane") }
func StateRoot() string  { return config.StateDir() }
func OpsDir() string     { return filepath.Join(StateDir(), "agent-ops") }
func RunnerPkgs() string { return filepath.Join(config.StateDir(), "runner") }
func ServeLog() string   { return filepath.Join(config.StateDir(), "installer", "serve.log") }

// ---- the pipeline ---------------------------------------------------------

func (e *Engine) RunBootstrap() error {
	fmt.Fprintf(e.Out, "creating the CP at %s (door -> cp LXC + console + co-located runner + DNS creds; then run `freehold build` from any box)\n", e.F.RelayDomain)

	// 1. the ops agent identity + the door (same as run()).
	EnsureIdentity(OpsDir())
	agentPK, err := LoadPubkey(OpsDir())
	if err != nil {
		return fmt.Errorf("ops agent identity unreadable at %s: %w", OpsDir(), err)
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
	// 6. initial config so the plane mapping + the exec stages (grant/serve/
	// verify) have the runner coords to resolve: written BEFORE they run, else
	// the exec subprocess negotiates the (empty) profile and falls into the CP
	// driven path (no freehold-agent-tools coords).
	if err := e.writeInitialConfig(); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  ✓ wrote config %s\n", e.F.ConfigPath)
	if err := e.stageGrant(); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  ✓ ops agent granted on %s\n", e.F.Target)
	if _, err := e.stageServe(); err != nil {
		return err
	}
	if err := e.stageVerify(); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  ✓ the door works — %s is reachable\n", e.F.Host)

	// 5.5. domains + 5.6. the ONE static proxy IP (same as run()).
	if !e.F.Yes {
		if err := e.PromptDomains(); err != nil {
			return err
		}
	}
	if e.F.RelayDomain == "" || e.F.CpDomain == "" {
		return fmt.Errorf("rebuild needs --relay-domain and --cp-domain (or an interactive run)")
	}
	if e.F.ProxyIP == "" && !e.F.Yes {
		ans, err := e.Prompt("proxy static IP (CIDR, e.g. 192.168.30.8/24) — REQUIRED, the one address relay/CP resolve to")
		if err != nil {
			return err
		}
		if a := strings.TrimSpace(ans); a != "" {
			e.F.ProxyIP = a
		}
	}
	if e.F.ProxyIP == "" {
		return fmt.Errorf("rebuild needs --proxy-ip (the single static proxy/Caddy address)")
	}
	if !strings.Contains(e.F.ProxyIP, "/") {
		return fmt.Errorf("--proxy-ip must be CIDR (host/prefix) — got %q", e.F.ProxyIP)
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
	fmt.Fprintln(e.Out, "  ✓ durable volume plane ready")

	// 9. boot the CP LXC + record its coordinates, then boot + deploy the RELAY
	// (its IP must be recorded BEFORE deploy-cp so the CP guest's /etc/hosts
	// pin reaches it; the agent-tools roster also needs the relay live).
	fmt.Fprintln(e.Out, "  · booting the cp LXC (create → docker; can take minutes)…")
	if err := e.stageBootstrap("cp"); err != nil {
		return err
	}
	if _, err := e.stageRecordLxc("cp"); err != nil {
		return err
	}
	fmt.Fprintln(e.Out, "  ✓ cp LXC booted + recorded")

	// 10. deploy the CP + its co-located runner (the relay IP is now recorded,
	// so the CP guest pins the domain for pre-Caddy relay ops).
	if err := e.stageDeployCp(); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  ✓ control plane live at https://%s\n", e.F.CpDomain)

	// 14. record the post-world coordinates (relay/k3s coords + litellm/caddy
	// coords world_build established) so the config + TUI + teardown agree.
	if err := e.RecordPostWorld(); err != nil {
		return err
	}

	// 16. the relay's signing key (best-effort) + final merge save.
	if rpk, ok := e.relayPubkeyNip11(); ok {
		fmt.Fprintf(e.Out, "  ✓ relay signing key: %s\n", rpk)
	} else {
		fmt.Fprintln(e.Out, "  (relay signing key unreadable via NIP-11 — read it from the relay's data dir when you need --relay-pubkey)")
	}
	if err := e.FinalSave(); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  ✓ wrote config %s\n", e.F.ConfigPath)

	fmt.Fprintf(e.Out, `
  ╭─────────────────────────────────────────────────────────╮
  │             The control plane is up (bootstrap)          │
  ╰─────────────────────────────────────────────────────────╯

  control plane:  https://%s
  relay domain:   %s
  runner:         serving on %s
  operator pk:    %s

  The CP is created. Now run `+"`freehold build`"+` from ANY box after
  `+"`freehold login`"+` — the CP brings up relay/agent-tools/k3s/DNS/litellm/
  caddy/cert through its own co-located runner.
`, e.F.CpDomain, e.F.RelayDomain, e.F.Addr, e.F.OperatorPubkey)
	return nil
}

// runBuild triggers the CP-owned world bring-up (the console's /api/world-build,
// which runs the shared cpbuild engine through the co-located runner) and then
// does the box-side bookkeeping — identical from box one or a fresh box two
// after login (drive-through-CP). The relay + agent-tools + k3s + litellm +
// caddy + cert all come up HERE, CP-side; bootstrap only created the CP.

// litellmSecretMaterial returns the litellm master key, postgres password, and
// provider key (minting/reusing the canonical first-run-wins values), prompting
// for the provider key on first provision. The CP is now the durable owner: the
// caller seeds these to the CP (ensureCpSecrets); world_build re-seeds the
// co-located runner from the CP store so the existing $LITELLM/$PROVIDER_KEY
// injection path is unchanged.

// ensureCpSecrets asks the operator ONLY for the CP secrets the CP does not
// already hold (DNS creds + litellm), seeding each as the CP's durable owner via
// the console /api/secrets. Idempotent: a secret already present on the CP is
// never re-asked. The box also keeps its own sealed DNS copy (promptDNSCred
// reuses it), which the DNS-record management step reads.

// seedCpRunnerSecrets writes the litellm master / postgres pw / provider key
// into the CP's co-located runner package (freehold-console add-secret on the
// CP) and restarts the freehold-runner unit so the live runner loads them. This
// is what lets the existing $LITELLM/$PROVIDER_KEY injection path serve the
// CP-owned litellm store the world-build reads.

// cpSecretBlob renders a cert.SaveCreds-style sealed record ({provider,sealed,
// aad}) as raw JSON, for upload to the CP via /api/secrets.

// handoffDNS ships the world's DNS provider creds (relay + cp slots) to the CP,
// sealed to the agent-tools identity via the established cert.SaveCreds record,
// under the CP's world-secrets dir (world_build's cert issue path opens them).

// handoffDNS ships the world's DNS provider creds (relay + cp slots) to the CP,
// sealed to the CONSOLE identity (the console is the CP build executor, whose
// world_build cert issue path opens them) under the console's world-secrets dir.

// triggerWorldBuild signs world_build over the agent-tools MCP (the box ops
// identity — roster-granted at bootstrap) and prints the CP's report.

// recordPostWorld records what world_build established: the relay/k3s coords
// (read back through the runner) + the litellm/caddy coords (deterministic
// from the proxy IP) + the DNS record mirror, so the config + TUI + teardown
// agree with the CP-built world.
func (e *Engine) RecordPostWorld() error {
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil || cfg == nil {
		return fmt.Errorf("no config at %s", e.F.ConfigPath)
	}
	for _, role := range []string{"relay", "cp", "k3s"} {
		if vmid, verr := e.findLxcVmidExact(role); verr == nil {
			if ip, ierr := e.readLxcIP(vmid); ierr == nil {
				ipCIDR := ip
				if !strings.Contains(ip, "/") {
					ipCIDR = ip + "/24"
				}
				switch role {
				case "relay":
					cfg.Lxc.Relay = config.LxcGuest{Vmid: &vmid, Ip: &ipCIDR}
				case "cp":
					cfg.Lxc.Cp = config.LxcGuest{Vmid: &vmid, Ip: &ipCIDR}
				case "k3s":
					cfg.Lxc.K3s = config.LxcGuest{Vmid: &vmid, Ip: &ipCIDR}
				}
			} else {
				fmt.Fprintf(e.Out, "  (record %s coords: ip readback failed: %v)\n", role, ierr)
			}
		} else {
			fmt.Fprintf(e.Out, "  (record %s coords: no guest found by hostname — is the world_build boot complete? %v)\n", role, verr)
		}
	}
	// Reconcile the agent-tools URL to the CURRENT cp IP: it was recorded at
	// deploy-agent-tools time (frozen), and a DHCP-lease change mid-build (the
	// cp LXC is dhcp unless pinned) would otherwise leave it pointing at a dead
	// IP — breaking `world status` and the fresh-box Agents view until manually
	// corrected. The agent-tools pubkey is durable and unchanged.
	if ip := config.LxcIP(cfg.Lxc.Cp); ip != "" {
		cfg.AgentToolsURL = "http://" + ip + ":" + config.AgentToolsPort
	}
	proxyIP := config.StripCIDR(e.F.ProxyIP)
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
	return cfg.Save(e.F.ConfigPath)
}

func lxcIP(g config.LxcGuest) string { return config.LxcIP(g) }

// stageProvision runs control-plane provision; returns the fresh ssh public
// registerWorldFacts pushes the deployer-side world facts onto the CP
// (world_register_facts, operator-scoped): the durable-plane layout, the
// canonical domains, and the edge cert metadata. A management/login-only box
// then renders the DATA + Certs views from world_status instead of needing the
// deployer's local config + host probes.

// certExpiry reads a slot's edge cert notAfter from the durable mirror
// (/srv/data/k8s-volumes/caddy-edge/<slot>/fullchain.pem on the k3s node)
// through the provisioning runner, as RFC3339 ("" when unreadable).
func (e *Engine) CertExpiry(cfg *config.Config, slot string) string {
	k3s := derefU32(cfg.Lxc.K3s.Vmid)
	if k3s == 0 || e.Provider == nil {
		return ""
	}
	out, err := e.Provider.GuestExec(strconv.FormatUint(uint64(k3s), 10), fmt.Sprintf(
		"bash -c 'openssl x509 -enddate -noout -in %s/fullchain.pem 2>/dev/null'",
		stages.CaddyEdgeDurableDir(slot)), 0)
	if err != nil || out == nil || out.ExitCode == nil || *out.ExitCode != 0 {
		return ""
	}
	line := strings.TrimSpace(out.Stdout)
	const prefix = "notAfter="
	i := strings.Index(line, prefix)
	if i < 0 {
		return ""
	}
	notAfter := strings.TrimSpace(line[i+len(prefix):])
	// "Sep  8 12:00:00 2027 GMT" -> time.Parse
	if t, err := time.Parse("Jan _2 15:04:05 2006 MST", notAfter); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

// key line when a NEW door key was generated ("" on reuse).
func (e *Engine) stageProvision(agentPK string) (string, error) {
	runnerDir := filepath.Join(RunnerPkgs(), e.F.Target)
	ok, out := e.RunBin(e.Bins.Console, []string{
		"provision", e.F.Target,
		"--kind", "ssh",
		"--address", e.F.Host,
		"--state-dir", StateDir(),
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
				"a runner %q record exists but its package at %s is gone — wipe the world for a clean re-bootstrap:\n  rm -rf ~/.freehold\n(or revoke the record: freehold-console revoke %s --state-dir %s)",
				e.F.Target, runnerDir, e.F.Target, StateDir())
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
func (e *Engine) doorGate(key string) error {
	instr := fmt.Sprintf("echo '%s' >> /root/.ssh/authorized_keys", key)
	if e.F.Yes {
		return fmt.Errorf(
			"the door needs a NEW ssh key before rebuild can continue — install it on %s, then re-run rebuild:\n\n    %s\n\n  (on the host: mkdir -p /root/.ssh && %s)",
			e.F.Host, key, instr)
	}
	for {
		e.printDoorKey(key)
		answer, err := e.Prompt("Press ENTER when it's in place, or 'r' to show it again, 'q' to quit")
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
func (e *Engine) printDoorKey(key string) {
	instr := fmt.Sprintf("echo '%s' >> /root/.ssh/authorized_keys", key)
	fmt.Fprintf(e.Out, `
  ─────────────────────────────────────────────────────────
  Finish the door: add this line to %s's ~/.ssh/authorized_keys:

    %s

  (on the host: mkdir -p /root/.ssh && %s)
  ─────────────────────────────────────────────────────────
`, e.F.Host, key, instr)
}

// doorKeyNotInstalled is the REUSE path's door gate: provision skipped the
// gate (the package already exists), but the ssh AUTH failure proves the key
// was never installed. Recover the public line from the package and bail
// actionably exactly like doorGate's --yes — the operator must see the key
// again or they are stuck.
func (e *Engine) doorKeyNotInstalled() error {
	key := e.recoverDoorKey()
	if key == "" {
		return fmt.Errorf(
			"the door check failed: ssh authentication was refused and the door key could\nnot be recovered from the runner package at %s — wipe the world and start clean:\n  rm -rf ~/.freehold   (then re-run rebuild)",
			filepath.Join(RunnerPkgs(), e.F.Target))
	}
	instr := fmt.Sprintf("echo '%s' >> /root/.ssh/authorized_keys", key)
	return fmt.Errorf(
		"the door needs its ssh key before rebuild can continue — install it on %s, then re-run rebuild:\n\n    %s\n\n  (on the host: mkdir -p /root/.ssh && %s)",
		e.F.Host, key, instr)
}

// recoverDoorKey re-derives the door ssh PUBLIC line from the existing
// runner package — the same material the runner decrypts at boot. "" when
// the package is missing or unusable (the caller falls back to the
// fresh-start message).
func (e *Engine) recoverDoorKey() string {
	key, err := doorKeyFromPackage(filepath.Join(RunnerPkgs(), e.F.Target), e.F.Target)
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
	id, err := LoadIdentity(runnerDir)
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

func (e *Engine) Prompt(label string) (string, error) {
	fmt.Fprintf(e.Out, "%s: ", label)
	if e.Stdin == nil {
		e.Stdin = bufio.NewReader(e.In)
	}
	line, err := e.Stdin.ReadString('\n')
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

func (e *Engine) stageGrant() error {
	ok, out := e.RunBin(e.Bins.Console, []string{
		"grant", e.F.Target, "--state-dir", StateDir(),
	})
	if !ok {
		return fmt.Errorf("grant failed:\n%s", out)
	}
	return nil
}

// stageServe kills any stale serve on the addr and spawns a fresh detached
// one for the CURRENT package; returns the pid.
func (e *Engine) stageServe() (string, error) {
	e.killServeOn(e.F.Addr)
	if err := os.MkdirAll(filepath.Dir(ServeLog()), 0o755); err != nil {
		return "", err
	}
	pkg := filepath.Join(RunnerPkgs(), e.F.Target)
	if _, err := os.Stat(filepath.Join(pkg, "identity.json")); err != nil {
		return "", fmt.Errorf("runner package %s is missing — the provision stage created it, something is off", pkg)
	}
	script := fmt.Sprintf("nohup '%s' serve --state-dir %s --addr %s > %s 2>&1 & echo $!",
		e.Bins.Runner, pkg, e.F.Addr, ServeLog())
	out, _ := e.RunSh(script)
	pid := strings.TrimSpace(out)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if e.PortOpen(e.F.Addr) {
			return pid, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", fmt.Errorf("the runner didn't come up on %s within 20s — see %s for why", e.F.Addr, ServeLog())
}

// killServeOn pkills any runner serve bound to this MCP address and WAITS
// for the port to actually close (a fresh spawn while the old listener
// still holds the address dies with "Address already in use").
func (e *Engine) killServeOn(addr string) {
	pat := fmt.Sprintf("runner serve.*--addr %s", regexEscape(addr))
	_, _ = exec.Command("pkill", "-f", pat).CombinedOutput()
	deadline := time.Now().Add(5 * time.Second)
	for e.PortOpen(addr) && time.Now().Before(deadline) {
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
func (e *Engine) stageVerify() error {
	failures := 0
	for {
		ok, out := e.RunBin(e.Bins.Self, e.ExecArgs("echo freehold-door-ok", 60))
		if ok && strings.Contains(out, "freehold-door-ok") {
			return nil
		}
		failures++
		if e.F.Yes {
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
		fmt.Fprintf(e.Out, "  ✗ the exec failed (auth or otherwise)\n%s\n", printTail(out, 6))
		if isSshAuthFailure(out) {
			if key := e.recoverDoorKey(); key != "" {
				fmt.Fprintf(e.Out, "  (ssh authentication was refused — the door key is probably not installed yet)\n")
				e.printDoorKey(key)
			}
		}
		if failures >= 3 {
			fmt.Fprintf(e.Out, `
  Still failing after %d tries. If this runner predates the ssh-key
  serialization fix, its PRIVATE key may be unloadable by the SSH client —
  authorized_keys edits can't help that.
  Fresh start:  rm -rf ~/.freehold && freehold build ...
`, failures)
		}
		if _, err := e.Prompt("Fix authorized_keys on the host, then press ENTER to retry ('q' to quit)"); err != nil {
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
func (e *Engine) ExecArgs(cmd string, timeoutS int) []string {
	args := []string{"exec", "--addr", e.F.Addr, "--agent-dir", OpsDir()}
	if e.F.ConfigPath != "" {
		args = append(args, "--config", e.F.ConfigPath)
	}
	if timeoutS > 0 {
		args = append(args, "--timeout", strconv.Itoa(timeoutS))
	}
	return append(args, e.F.Target, cmd)
}

// selfStage runs one of THIS binary's subcommands with the per-subcommand
// --addr/--agent-dir injected right after the subcommand name (Rust
// stage_any: the CLI's flattened CommonArgs go AFTER the name).
func (e *Engine) selfStage(name string, args []string) (string, error) {
	full := make([]string, 0, len(args)+5)
	full = append(full, args[0], "--addr", e.F.Addr, "--agent-dir", OpsDir())
	full = append(full, args[1:]...)
	ok, out := e.RunBin(e.Bins.Self, full)
	if !ok {
		return "", fmt.Errorf("%s failed:\n%s", name, out)
	}
	return out, nil
}

// stageServeRunner serves an ADDITIONAL runner package on a dedicated
// loopback addr (C0: the litellm runner at 127.0.0.1:8788 — its exec reaches
// the gateway NodePort and carrries the 3-secret package). Returns the pid.

// stopRunner kills a runner pid (best-effort; the serve was detached).
func (e *Engine) stopRunner(pid string) {
	if pid == "" {
		return
	}
	_, _ = e.RunSh(fmt.Sprintf("kill '%s' >/dev/null 2>&1 || true", pid))
}

// ---- config writes ---------------------------------------------------------

// fromAnswers is the GREENFIELD config the rebuild answers describe (Rust
// config::Config::from_answers). runner pubkey resolved from the package.
func (e *Engine) fromAnswers() *config.Config {
	runnerPK, _ := LoadPubkey(filepath.Join(RunnerPkgs(), e.F.Target))
	// The relay + CP hosts are LITERAL inputs, never derived, and there is no
	// world/base domain. Everything behind is a single static proxy IP.
	relayDomain := e.F.RelayDomain
	cpDomain := e.F.CpDomain
	// RelayWsURL is the CPA pod's relay origin: wss://<relayDomain> — the
	// public, TLS-fronted form the edge serves. A host is built ONLY from a
	// supplied domain: the early writeInitialConfig lands before bootstrap's
	// interactive prompt on the sequential path, and an empty host must merge
	// prev's recorded value, not clobber it with the bare scheme.
	cfg := &config.Config{
		Name:           e.F.Name,
		Host:           e.F.Host,
		AccessMode:     e.F.AccessMode,
		OperatorPubkey: e.F.OperatorPubkey,
		Runner: config.RunnerRef{
			Addr:   e.F.Addr,
			Pubkey: runnerPK,
			Target: e.F.Target,
		},
		Managed: []string{"relay", "cp"},
		CPAName: e.F.AgentName,
	}
	if relayDomain != "" {
		cfg.RelayURL = "https://" + relayDomain
		cfg.RelayWsURL = "wss://" + relayDomain
	}
	if cpDomain != "" {
		cfg.CPURL = "https://" + cpDomain
	}
	if e.F.ProxyIP != "" {
		p := e.F.ProxyIP
		cfg.Proxy = config.ProxySpec{Ip: &p}
	}
	if e.F.OperatorIdentity != "" {
		v := e.F.OperatorIdentity
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
	// Hosts survive when this run supplied none (the early write before the
	// sequential path prompts its domains); answers still win when present.
	if cfg.RelayURL == "" {
		cfg.RelayURL = prev.RelayURL
	}
	if cfg.RelayWsURL == "" {
		cfg.RelayWsURL = prev.RelayWsURL
	}
	if cfg.CPURL == "" {
		cfg.CPURL = prev.CPURL
	}
	if cfg.OperatorIdentity == nil {
		cfg.OperatorIdentity = prev.OperatorIdentity
	}
	// The substrate host + access mode are install inputs; a run that supplied
	// none keeps the recorded ones (so `uninstall --name` still resolves).
	if cfg.Host == "" {
		cfg.Host = prev.Host
	}
	if cfg.AccessMode == "" {
		cfg.AccessMode = prev.AccessMode
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
func (e *Engine) writeInitialConfig() error {
	prev, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return err
	}
	return mergeFromAnswers(e.fromAnswers(), prev).Save(e.F.ConfigPath)
}

// finalSave is the end-of-pipeline MERGE save (fresh load; mid-pipeline
// facts live on disk).
func (e *Engine) FinalSave() error {
	prev, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return err
	}
	cfg := mergeFromAnswers(e.fromAnswers(), prev)
	// The rebuild OWNS the world manifest: managed = the pieces THIS run
	// deployed. A k3s LXC recorded by a PRIOR run is still ours to tear
	// down even when this run opted it out (`--no-k3s`) — dropping it
	// would make teardown say "skipped k3s LXC (not managed)" and leak the
	// guest + its thin LV forever.
	cfg.Managed = worldManaged(e.F.NoK3s, e.F.NoLitellm, cfg.Lxc.K3s.Vmid)
	if rpk, ok := e.relayPubkeyNip11(); ok {
		cfg.RelayPubkey = &rpk
	}
	return cfg.Save(e.F.ConfigPath)
}

// ---- the storage stage -----------------------------------------------------

// stagePlacement runs `storage resolve` and applies the plane-placement
// gate: the tenant LVs must land in a named thin pool — reuse the VG's
// detected one, or carve a dedicated new pool (then at --pool-size-gb and
// recorded as freehold-created). The --thin-pool flag answers it headless;
// without it the interactive pipeline prompts, and --yes takes the
// reuse-detected / carve-default path.
func (e *Engine) stagePlacement() (*placement, error) {
	resolveArgs := []string{"storage", "resolve",
		"--addr", e.F.Addr, "--agent-dir", OpsDir(), "--target", e.F.Target,
		"--relay-domain", e.F.RelayDomain}
	if e.F.ConfirmStorage {
		resolveArgs = append(resolveArgs, "--confirm-storage")
	}
	ok, out := e.RunBin(e.Bins.Self, resolveArgs)
	if !ok {
		return nil, fmt.Errorf("storage resolution failed:\n%s", out)
	}
	// The resolve stage emits a read-only INVENTORY (every backend + every
	// whole disk, classified by the data it carries). Drive the choice from
	// it; an older sibling (no inventory line) falls back to the legacy parse.
	inv, haveInv := planebase.DecodeInventory(out)
	if !haveInv || inv.Empty() {
		return e.legacyPlacement(out)
	}
	return e.selectPlacement(inv)
}

// legacyPlacement is the pre-inventory resolve parse: a single detected pool
// + its thin pools, or the bare-host create/bail branch.
func (e *Engine) legacyPlacement(out string) (*placement, error) {
	pool := parseStoragePool(out)
	detected, isLvm := parseStorageThinPools(out)

	// The STORAGE-THINPOOL line exists only on the LVM-thin backend; its
	// absence is ZFS, where the placement gate does not apply (datasets
	// carve themselves). A named --thin-pool on ZFS is an operator error.
	if !isLvm {
		if e.F.ThinPool != "" {
			return nil, fmt.Errorf("--thin-pool applies only to the LVM-thin backend; this host resolved %q (ZFS)", pool)
		}
		return &placement{pool: pool, kind: planebase.KindZfs}, nil
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
	if e.F.ThinPool != "" || len(detected) == 0 {
		name := e.F.ThinPool
		if name == "" {
			name = planebase.FreshThinPool
		}
		return &placement{pool: pool, thinPool: name, created: !has(name), kind: planebase.KindLvmThin}, nil
	}
	if e.F.Yes {
		return &placement{pool: pool, thinPool: detected[0], created: false, kind: planebase.KindLvmThin}, nil
	}

	first := detected[0]
	fmt.Fprintf(e.Out, `
  ─ plane placement ───────────────────────────────────────
  VG %s currently holds the thin pool %q.
  The tenant LVs need a thin pool to live in:
    r      reuse it
    <name> carve a NEW dedicated pool of that name (%d GB)
  ─────────────────────────────────────────────────────────
`, pool, first, e.F.PoolSizeGB)
	answer, err := e.Prompt("pool choice [r = reuse / type a new pool name]")
	if err != nil {
		return nil, err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" || answer == "r" || answer == "R" || answer == first {
		return &placement{pool: pool, thinPool: first, created: false, kind: planebase.KindLvmThin}, nil
	}
	// A name the operator types is an ADOPT if it already exists in the VG
	// (membership probe), a CARVE otherwise — same rule as the flag path.
	if has(answer) {
		return &placement{pool: pool, thinPool: answer, created: false, kind: planebase.KindLvmThin}, nil
	}
	sizeAnswer, err := e.Prompt(fmt.Sprintf("new pool %q size GB (blank = %d)", answer, e.F.PoolSizeGB))
	if err != nil {
		return nil, err
	}
	size, err := parseGB(sizeAnswer, e.F.PoolSizeGB, "new thin-pool size GB")
	if err != nil {
		return nil, err
	}
	e.F.PoolSizeGB = size
	return &placement{pool: pool, thinPool: answer, created: true, kind: planebase.KindLvmThin}, nil
}

// stageStorage ensures each tenant's dataset onto the placement gate's
// pool and records the mapping into the config (Rust stage_storage —
// loads the config FRESH, bails when absent).
func (e *Engine) stageStorage(placement *placement) error {
	pool := placement.pool

	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s — cannot record the durable-plane mapping", e.F.ConfigPath)
	}

	// tenant -> LXC role (the k3s role rides the k3s-volumes tenant).
	roleFor := [][2]string{{"relay", "relay"}, {"cp", "cp"}, {"k3s-volumes", "k3s"}}
	resolvedAny := false
	for _, tr := range roleFor {
		tenant, role := tr[0], tr[1]
		ensureArgs := []string{"storage", "ensure",
			"--addr", e.F.Addr, "--agent-dir", OpsDir(),
			"--target", e.F.Target,
			"--tenant", tenant,
			"--domain", e.F.RelayDomain,
			"--pool", pool,
			"--size-gb", strconv.FormatUint(e.F.SizeGB, 10),
			"--pool-size-gb", strconv.FormatUint(e.F.PoolSizeGB, 10),
		}
		// Honor the placement's chosen kind first, then the RECORDED kind: on
		// a host with BOTH a ZFS pool and a VG, re-detection would always pick
		// ZFS (or `vgs[0]`) and drive the tenant the wrong way.
		switch {
		case placement.kind != "":
			ensureArgs = append(ensureArgs, "--kind", string(placement.kind))
		case cfg.Plane.BackendKind != nil && *cfg.Plane.BackendKind != "":
			ensureArgs = append(ensureArgs, "--kind", *cfg.Plane.BackendKind)
		}
		if placement.thinPool != "" {
			ensureArgs = append(ensureArgs, "--thin-pool", placement.thinPool)
		}
		ok, out := e.RunBin(e.Bins.Self, ensureArgs)
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
		_ = cfg.Save(e.F.ConfigPath)
	} else if e.F.ConfirmStorage {
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
// pool freehold just carved. An LXC rootfs on `local-lvm` resolves through
// storage.cfg — after the operator wiped the VG's only thin pool the carve
// leaves local-lvm dangling unless we re-point it here. The probe/edit
// discipline lives in the provider; this method adds the stranding guard and
// the operator notice. Idempotent.
func (e *Engine) stageLocalLvmRepoint(placement *placement) error {
	if e.Provider == nil {
		return fmt.Errorf("no provisioning provider wired — cannot re-point PVE local-lvm")
	}
	current, riders, err := e.Provider.LocalLvmStatus()
	if err != nil {
		return err
	}
	if riders > 0 && current != placement.thinPool {
		fmt.Fprintf(e.Out, "  · leaving PVE local-lvm on %q — it holds %d live volume(s); freehold will not re-point it\n", current, riders)
		return nil
	}
	return e.Provider.RepointLocalLvm(placement.thinPool)
}

// placement is the plane-placement gate's resolved answer.
type placement struct {
	pool     string                // the backend name (LVM: the VG; ZFS: the zpool)
	thinPool string                // the thin pool the tenant LVs land in ("" on ZFS)
	created  bool                  // true => freehold carves it (recorded for teardown --data)
	kind     planebase.BackendKind // "" => let the ensure stage detect
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
func (e *Engine) stageBootstrap(role string) error {
	hostname, err := lxcName(e.F.Name, e.F.RelayDomain, role)
	if err != nil {
		return err
	}
	args := []string{"provision",
		"--kind", "proxmox-lxc",
		"--role", role,
		"--hostname", hostname,
		"--target", e.F.Target,
		"--domain", e.F.RelayDomain,
		"--rootfs-gb", strconv.FormatUint(uint64(e.F.RootfsGB), 10),
		"--memory-mb", strconv.FormatUint(uint64(e.F.MemoryMB), 10),
		"--operator-pubkey", e.F.OperatorPubkey,
	}
	cfg, _ := config.Load(e.F.ConfigPath)
	if ip := bootstrapStaticIP(role, e.F, cfg); ip != "" {
		args = append(args, "--lxc-ip", ip, "--lxc-gw", e.F.RelayGw)
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
	_, err = e.selfStage("booting the "+role+" LXC", args)
	return err
}

// bootstrapStaticIP returns the role's STATIC address, or "" = DHCP. k3s is
// the proxy node (--proxy-ip, or the recorded cfg.Proxy.Ip riding again);
// relay + cp are STATIC when --relay-ip / --cp-ip are supplied (running them
// OFF DHCP avoids exhausting a small LAN DHCP pool), else DHCP behind the
// proxy.
func bootstrapStaticIP(role string, f Flags, cfg *config.Config) string {
	switch role {
	case "cp":
		return f.CpIP
	case "relay":
		return f.RelayIP
	case "k3s":
		if f.ProxyIP != "" {
			return f.ProxyIP
		}
		if cfg != nil && cfg.Proxy.Ip != nil {
			return *cfg.Proxy.Ip
		}
	}
	return ""
}

// stageRecordLxc persists the real post-boot coordinates into the config ON
// DISK: load FRESH, resolve vmid+ip through the runner, mutate, save, return
// the merged result (Rust record_lxc's fresh-load discipline).
func (e *Engine) stageRecordLxc(role string) (*config.Config, error) {
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
func (e *Engine) recordLxcWith(role string, resolve func(string) (uint32, string, error)) (*config.Config, error) {
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("no config at %s — cannot record the %s LXC's coordinates", e.F.ConfigPath, role)
	}
	vmid, ip, err := resolve(role)
	if err != nil {
		return nil, err
	}
	applyLxcCoords(cfg, role, vmid, ip)
	if err := cfg.Save(e.F.ConfigPath); err != nil {
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

// lxcName is the guest's FULL name: <name>-<role> when the world has a profile
// name, else the domain-derived <domain-with-dashes>-<role>.
func lxcName(name, domain, role string) (string, error) {
	return bootstrap.LXCName(name, domain, role)
}

// findLxcVmidExact finds the role's vmid by FULL name match on the guest list
// (a suffix-only match can hit ANOTHER world's container on a multi-world
// host).
func (e *Engine) findLxcVmidExact(role string) (uint32, error) {
	exact, err := lxcName(e.F.Name, e.F.RelayDomain, role)
	if err != nil {
		return 0, err
	}
	if e.Provider == nil {
		return 0, fmt.Errorf("no provisioning provider wired — cannot list guests")
	}
	guests, err := e.Provider.ListGuests()
	if err != nil {
		return 0, fmt.Errorf("guest list unreadable through the runner: %w", err)
	}
	for _, g := range guests {
		if g.Name != exact {
			continue
		}
		vmid, err := strconv.ParseUint(g.ID, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("unparseable vmid %q for guest %q", g.ID, g.Name)
		}
		return uint32(vmid), nil
	}
	return 0, fmt.Errorf("no container named %s found on the host", exact)
}

// readLxcIP reads the guest's current IPv4 (CIDR) off eth0.
func (e *Engine) readLxcIP(vmid uint32) (string, error) {
	if e.Provider == nil {
		return "", fmt.Errorf("no provisioning provider wired — cannot read guest ip")
	}
	return e.Provider.GuestIPv4(strconv.FormatUint(uint64(vmid), 10))
}

// ---- the k3s stage -----------------------------------------------------------

// The k3s install + the durable local-path carve-out are owned by the CP's
// terraform module (k3s-bringup.sh): it re-points the local-path StorageClass's
// backing store at the DURABLE plane mount (/srv/data/k8s-volumes — backup=1,
// so k8s PVCs survive a compute teardown) and installs the pinned k3s build.

// stageDeployRelay deploys the Buzz relay into the relay LXC (box-side — the
// slim build boots + deploys the relay before agent-tools, whose roster lives
// on it).

// stageDeployCp deploys the control plane into the cp LXC from the RELEASE
// binaries; state/bin dirs follow the guest's last mount.
func (e *Engine) stageDeployCp() error {
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
		"--target", e.F.Target,
		"--lxc", strconv.FormatUint(uint64(vmid), 10),
		"--relay-url", "https://" + e.F.RelayDomain,
		"--binary", e.Bins.ReleaseConsole,
		"--runner-binary", e.Bins.ReleaseRun,
		"--runner-package", RunnerPkgs() + "/" + e.F.Target,
		"--operator-pubkey", e.F.OperatorPubkey,
		"--agent-tools-binary", e.Bins.ReleaseAgentTools,
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
	if cfg, _ := config.Load(e.F.ConfigPath); cfg != nil && cfg.AgentToolsURL != "" && cfg.AgentToolsPubkey != "" {
		args = append(args, "--agent-tools-url", cfg.AgentToolsURL, "--agent-tools-pubkey", cfg.AgentToolsPubkey)
	}
	// Pin the relay's LAN IP into the CP guest's /etc/hosts so the console +
	// agent-tools can RESOLVE + reach the relay DOMAIN directly (pre-Caddy):
	// buzz keys the community to the Host header, so a raw-IP URL fails.
	if cfg, _ := config.Load(e.F.ConfigPath); cfg != nil && cfg.Lxc.Relay.Ip != nil {
		args = append(args, "--relay-host-ip", config.StripCIDR(*cfg.Lxc.Relay.Ip))
	}
	if cpRoot != "" {
		args = append(args, "--state-dir", cpRoot+"/control-plane", "--bin-dir", cpRoot+"/bin")
	}
	// The world-config bounds the console as the CP build executor: it holds
	// the coords cpbuild needs (relay/plane/k3s/litellm/runner) so a thin box's
	// `build` can trigger /api/world-build on the console — no box-one or
	// agent-tools dependency. Build it from the box's recorded config + flags.
	cfg, _ := config.Load(e.F.ConfigPath)
	if cfg != nil {
		if wc := e.worldConfigJSON(cfg); wc != "" {
			args = append(args, "--world-config", wc)
		}
	}
	_, err = e.selfStage("deploy-cp", args)
	return err
}

// worldConfigJSON renders the console's build-executor coords (cpbuild.Coords)
// from the box's recorded config + build flags, as JSON. Empty when there are
// no runner coords to drive (the deploy then serves ops/status only).
func (e *Engine) worldConfigJSON(cfg *config.Config) string {
	cpIP := config.StripCIDR(derefStrPtr(cfg.Lxc.Cp.Ip))
	if cpIP == "" {
		// A freshly-assigned static CP IP (--cp-ip) isn't in the config yet on
		// the first bootstrap; prefer it so bootLxc bakes it into the create.
		cpIP = config.StripCIDR(e.F.CpIP)
	}
	if cpIP == "" || cfg.Runner.Pubkey == "" {
		return ""
	}
	relayIP := config.StripCIDR(derefStrPtr(cfg.Lxc.Relay.Ip))
	if relayIP == "" {
		relayIP = config.StripCIDR(e.F.RelayIP)
	}
	c := config.Coords{
		Name:           cfg.Name,
		StateDir:       "",
		RelayURL:       cfg.RelayURL,
		RelayAuthURL:   cfg.RelayURL,
		RelayPK:        derefStrPtr(cfg.RelayPubkey),
		RelayWS:        cfg.RelayWsURL,
		RelayHost:      cfg.RelayHost(),
		RelayIP:        relayIP,
		CpHost:         cfg.CPHost(),
		CpIP:           cpIP,
		CpLxc:          derefU32(cfg.Lxc.Cp.Vmid),
		ProxyIP:        config.StripCIDR(derefStrPtr(cfg.Proxy.Ip)),
		LitellmIP:      cfg.Litellm.Host,
		PlanePool:      derefStrPtr(cfg.Plane.Backend),
		PlaneKind:      derefStrPtr(cfg.Plane.BackendKind),
		ThinPool:       derefStrPtr(cfg.Plane.ThinPool),
		SizeGB:         e.F.SizeGB,
		PoolSizeGB:     e.F.PoolSizeGB,
		RootfsGB:       e.F.RootfsGB,
		MemoryMB:       e.F.MemoryMB,
		StorageName:    e.F.StorageName,
		RelayGW:        e.F.RelayGw,
		Bridge:         e.F.Bridge,
		RelayLxc:       derefU32(cfg.Lxc.Relay.Vmid),
		RelayCompose:   stages.RelayComposeDir,
		K3sVmid:        derefU32(cfg.Lxc.K3s.Vmid),
		RunnerAddr:     config.CoLocatedRunnerMCPAddr,
		RunnerPK:       cfg.Runner.Pubkey,
		RunnerTarget:   cfg.Runner.Target,
		CpaName:        e.F.AgentName,
		OwnerPub:       e.F.OperatorPubkey,
		LitellmBaseURL: cfg.Litellm.URL,
		// The agent-tools server's reachable URL, NOT the console's: this is the
		// `--self-url` the server reports AND the URL the CPA pod curls its
		// stdio bridge binary from (config.AgentToolsPort). The console's own
		// URL is not needed in the coords.
		SelfURL: "http://" + cpIP + ":" + config.AgentToolsPort,
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// guestMounts reads the `mp=` guest paths of the LXC config (Rust
// guest_mounts) — ground truth for where PVE binds the durable dataset.
func (e *Engine) guestMounts(vmid uint32) []string {
	if e.Provider == nil {
		return nil
	}
	mounts, err := e.Provider.GuestMounts(strconv.FormatUint(uint64(vmid), 10))
	if err != nil {
		return nil
	}
	return mounts
}

// cpBinDir resolves the DEPLOYED control-plane binary + state dir inside the
// cp LXC (the same mounts stageDeployCp uses): the last plane mount is the CP
// root, bin/ + control-plane/ live under it.

// stageCpExec runs a command via the DEPLOYED CP binary inside its LXC
// (through the runner's guest exec): `exec <cp> -- <bin>/control-plane ARGS
// --state-dir <state>`. The pattern deploy-cp already uses for adopt/grant.

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

// certIdent returns the ops identity's encryption secret (raw bytes), the
// identity, or an error. The ops identity is freehold's own — the only key that
// must be able to reopen the sealed DNS token (the DNS-cred collection + the
// hand-off seal the relay/cp creds to it).

// litellmPostgresPw reads back the CANONICAL postgres password from the k8s
// litellm-pg Secret so a rebuild REUSES it (first-run-wins): Postgres initializes
// PGDATA against the first password, so a re-mint + SSA re-apply would rotate it
// while Postgres still authenticates with the original — silently breaking
// litellm's DB auth on the next pod restart. Empty on a fresh world -> mint.

// litellmRun executes a script THROUGH the dedicated litellm runner (served on
// loopback 127.0.0.1:8788, target "litellm"), injecting the named secrets by
// env. This is where the litellm admin calls run: the runner host is this
// machine — from which the gateway URL is reachable — and the secrets
// (litellm = master, provider-key) are the litellm runner package's own
// ciphertext. (Not the main proxmox-box runner, and not a nested
// "exec --target …" prefix — that prefix is a shell no-op the old code leaned
// on and never injected the secrets at all.)

// opsAgentPubkey returns the ops-agent's pubkey (the rebuild's signing
// identity — same as deploy-cp grants).

// Secret minting (master / postgres) lives in stages.GenSecretHex.

// clearStoredDNSCreds removes the stored DNS provider credentials (per-slot +
// the legacy single-file copy) so the build prompts for them again. Used by
// --reset-dns when the stored credential is stale/wrong.

// certIdentSecret returns the ops identity's encryption secret (raw bytes), the
// identity, or an error. The ops identity is freehold's own — the only key that
// must be able to reopen the sealed DNS token (lego runs in-process, not in a
// runner).

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

// worldHasEdge reports whether this rebuild's desired world includes the Caddy
// TLS edge (and therefore needs a DNS provider credential): k3s is on and a
// world domain is set.
func (e *Engine) WorldHasEdge() bool {
	return !e.F.NoK3s && e.F.RelayDomain != ""
}

// promptDomains asks for the relay + control-plane hosts up front. There is
// NO world/base domain and no derivation — the two hosts are required, literal
// inputs. A flag already set (or a headless --yes run) is honored; under --yes
// a missing one hard-errors (interactive collection is the only source).
func (e *Engine) PromptDomains() error {
	if e.F.RelayDomain == "" && e.F.Yes {
		return fmt.Errorf("no relay domain supplied and interactive collection is disabled (--yes); pass --relay-domain")
	}
	if e.F.RelayDomain == "" {
		ans, err := e.Prompt("relay domain (its Buzz origin — REQUIRED)")
		if err != nil {
			return err
		}
		if a := strings.TrimSpace(ans); a != "" {
			e.F.RelayDomain = a
		}
	}
	if e.F.RelayDomain == "" {
		return fmt.Errorf("relay domain is required")
	}
	if e.F.CpDomain == "" && e.F.Yes {
		return fmt.Errorf("no control-plane domain supplied and interactive collection is disabled (--yes); pass --cp-domain")
	}
	if e.F.CpDomain == "" {
		ans, err := e.Prompt("control-plane domain (REQUIRED)")
		if err != nil {
			return err
		}
		if a := strings.TrimSpace(ans); a != "" {
			e.F.CpDomain = a
		}
	}
	if e.F.CpDomain == "" {
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

// promptProvider asks the operator to pick a DNS-01 provider from lego's full
// registry via an interactive, scrollable + type-ahead-searchable list picker
// (bubbletea list). The 201-provider registry is otherwise unreadable when
// enumerated inline.

// promptProviderEnv collects the provider's env-var fields (from lego-derived
// names; freeform KEY=VAL lines for unknown/auto-detecting providers). The
// credential trio is asked FIRST and REQUIRED (no blank); every other field is
// collected after with "(optional)" (blank = unset).

// ---- the CPA stage (Chunk 4 Phase A) ---------------------------------------

// agentToolsMcp returns a signed MCP client to the CP's freehold-agent-tools
// server, authenticated as the build/ops identity (the roster grant seeded at
// bootstrap). The audience is the agent-tools server's own pubkey — the same
// signed-header scheme a runner caller uses.

// callAgentToolsText issues one tool call to a freehold-agent-tools client and
// returns the result.content[0].text payload (e.g. the new agent's pubkey).

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

// reconcileCreatedAgents redeploys every agent the CP's freehold-agent-tools
// registry holds (the CP-durable source of truth — the CPA plus everything
// created through create_agent), so a rebuild resurrects them idempotently with
// the same durable pubkeys (E3). The CPA name itself is created by stageCpa.

// recordCpa writes the CPA's identity + name into the config (managed) so the
// Services row + teardown + console see the agent, and registers it in the
// control-plane agent registry (A5) so it shows up like any named agent. The
// rest of the presence dot is the CPA's own relay kind:20001 publication —
// the registry row carries identity, the relay carries liveness.

// relayPubkeyNip11 reads the relay's signing pubkey via NIP-11 (best-effort
// trust anchor; not fatal when unreadable).
func (e *Engine) relayPubkeyNip11() (string, bool) {
	text, ok := e.CurlGet("https://" + e.F.RelayDomain + "/")
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
func (e *Engine) relaySigningPubkey() string {
	if cfg, err := config.Load(e.F.ConfigPath); err == nil && cfg != nil && cfg.Lxc.Relay.Vmid != nil && e.Provider != nil {
		guest := strconv.FormatUint(uint64(*cfg.Lxc.Relay.Vmid), 10)
		out, err := e.Provider.GuestExec(guest, fmt.Sprintf(
			"sed -n 's/^BUZZ_RELAY_PRIVATE_KEY=//p' %s/.env 2>/dev/null", stages.RelayComposeDir), 30)
		if err == nil && out != nil && out.ExitCode != nil && *out.ExitCode == 0 {
			secret := strings.TrimSpace(out.Stdout)
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
	if cfg, err := config.Load(e.F.ConfigPath); err == nil && cfg != nil && cfg.RelayPubkey != nil {
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
