// The install surface: `freehold install` — getting a control plane up in an
// environment (Proxmox today; Vultr/Hetzner providers come later) and a door to
// it. It builds box.Flags and drives the SHARED provisioning engine
// (platform/provisioning/box) which boots the CP LXC and deploy-cp's it. After
// install, WORLD bring-up is `freehold build` from any box via the CP — install
// is done, the environment no longer matters.
package install

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/freehold-cli/internal/stages"
	"freehold/platform/provisioning/box"
	"freehold/providers/proxmox"
	"freehold/providers/proxmox/drive"
)

// installConfigPath is the config path the install writes: the selected
// profile's config (profiles/<name>/config.toml) once selectProfile has pinned
// it, else the default config path.
func installConfigPath() string { return config.ConfigPath() }

// selectProfile validates a world/profile name and pins the process to it,
// creating the profile on first install. It MUST run before any path helper
// (installConfigPath, operatorDir, box.StateDir) so the config, state, and
// operator identity all land under profiles/<name>/. An EXISTING profile is
// tolerated: the lifecycle gate (gateInstall) has already refused a live CP, so
// reaching here means a re-adopt — the plane holds the runner identity and only
// the substrate door rotates.
func selectProfile(name string) error {
	if !config.ValidProfileName(name) {
		return fmt.Errorf("invalid --name %q (letters, digits, dash, underscore; no leading/trailing dash or underscore)", name)
	}
	if p := config.Resolve(name); p != nil {
		config.SetCurrent(p)
		return nil
	}
	config.SetCurrent(&config.Profile{
		Name:       name,
		ConfigPath: config.NewProfilePath(name),
		StateDir:   config.NewProfileState(name),
	})
	return nil
}

// seedOperatorLedger materializes the operator identity ledger the local CLI
// logs in from (`oplogin` reads <state>/control-plane/operator): a headless
// install that was handed --operator-identity copies that identity in
// (verified against --operator-pubkey) instead of leaving build to fail with
// "no operator identity". First-run-wins: an existing ledger is never touched.
func seedOperatorLedger(f *box.Flags) error {
	if f.OperatorIdentity == "" {
		return nil
	}
	ledger := operatorDir()
	if _, err := os.Stat(filepath.Join(ledger, "identity.json")); err == nil {
		return nil
	}
	src := filepath.Join(f.OperatorIdentity, "identity.json")
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("--operator-identity %s unreadable: %w", f.OperatorIdentity, err)
	}
	var doc map[string]string
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("--operator-identity %s malformed: %w", f.OperatorIdentity, err)
	}
	pk, err := box.LoadPubkey(f.OperatorIdentity)
	if err != nil {
		return fmt.Errorf("--operator-identity %s unusable: %w", f.OperatorIdentity, err)
	}
	if pk != f.OperatorPubkey {
		return fmt.Errorf("--operator-identity's key (%s) does not derive --operator-pubkey (%s) — paste a matching pair", pk, f.OperatorPubkey)
	}
	if err := wire.EnsurePrivateDir(ledger); err != nil {
		return err
	}
	return wire.WriteJSON0600(filepath.Join(ledger, "identity.json"), doc)
}

// lifecycleAction is the install gate's verdict.
type lifecycleAction int

const (
	lifecycleMint lifecycleAction = iota
	lifecycleReAdopt
)

// gateInstall refuses an install over a LIVE control plane and otherwise picks
// mint vs re-adopt. PR1 can only probe a KNOWN CP (a profile's recorded cp_url);
// the authoritative host-side check (`pct list` for the <name>-cp guest, which
// also covers a profile-less box pointed at a live world) needs the transient
// Access seam and lands in PR3.
func gateInstall(name string) (lifecycleAction, error) {
	p := config.Resolve(name)
	if p == nil {
		return lifecycleMint, nil
	}
	cfg, err := config.Load(p.ConfigPath)
	if err != nil {
		return 0, fmt.Errorf("read profile %q config: %w", name, err)
	}
	if cpLive(cfg) {
		return 0, fmt.Errorf(
			"a live control plane already exists for profile %q at %s:\n"+
				"  freehold build       bring up / reconcile the world\n"+
				"  freehold teardown    drop the world (the control plane stays)\n"+
				"  freehold uninstall   drop the control plane\n"+
				"  freehold login       join it from this box",
			name, cfg.CPURL)
	}
	return lifecycleReAdopt, nil
}

// cpLive probes a recorded control plane's /healthz (liveness only — the config
// probe posture, TLS not pinned).
func cpLive(cfg *config.Config) bool {
	if cfg == nil || cfg.CPURL == "" {
		return false
	}
	return config.HTTPOK(strings.TrimSuffix(cfg.CPURL, "/") + "/healthz")
}

// seedFromProfile fills the inputs a re-adopt resolves from the surviving
// profile/plane — the relay/CP hosts, the static proxy IP, and the operator
// identity. Explicit flags still win (callers pass only empty fields).
func seedFromProfile(f *box.Flags, cfg *config.Config) {
	if cfg == nil {
		return
	}
	if f.RelayDomain == "" {
		f.RelayDomain = cfg.RelayHost()
	}
	if f.CpDomain == "" {
		f.CpDomain = cfg.CPHost()
	}
	if f.ProxyIP == "" && cfg.Proxy.Ip != nil {
		f.ProxyIP = *cfg.Proxy.Ip
	}
	if f.OperatorPubkey == "" {
		f.OperatorPubkey = cfg.OperatorPubkey
	}
	if f.OperatorIdentity == "" && cfg.OperatorIdentity != nil {
		f.OperatorIdentity = *cfg.OperatorIdentity
	}
}

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Bring up a control plane (guided; --non-interactive for headless) — then `freehold build` brings up the world via the CP",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runInstallCmd(cmd)
	},
}

// runInstallCmd is the install surface: the guided flow when the flags are
// incomplete, else the headless pipeline.
func runInstallCmd(cmd *cobra.Command) error {
	f := flagsFromCmd(cmd)
	name := f.Name
	out := cmd.OutOrStdout()

	// A fresh plane has nothing to resolve from, so headless needs the full
	// answer set; with gaps (and no --non-interactive), fall back to the guided flow.
	if !f.Yes && (name == "" || f.Host == "" || f.RelayDomain == "" || f.CpDomain == "" ||
		f.ProxyIP == "" || f.OperatorPubkey == "") {
		return runInstall(cmd.InOrStdin(), out, name)
	}
	if name == "" {
		return fmt.Errorf("install needs --name (the world/profile name — isolates this world's config and state)")
	}
	if f.Host == "" {
		return fmt.Errorf("install needs --host (the environment freehold reaches, e.g. root@192.168.30.224)")
	}
	action, err := gateInstall(name)
	if err != nil {
		return err
	}
	if err := selectProfile(name); err != nil {
		return err
	}
	f.Name = name
	if action == lifecycleReAdopt {
		// A re-adopt resolves its inputs from the surviving profile/plane:
		// the recorded runner coords + the relay/CP hosts + proxy IP. The
		// plane keeps the runner identity — its name/addr come from the
		// config, never the (removed) defaults; --local-port overrides.
		prev, _ := config.Load(installConfigPath())
		if prev != nil {
			if prev.Runner.Target != "" {
				f.Target = prev.Runner.Target
			}
			if !cmd.Flags().Changed("local-port") && prev.Runner.Addr != "" {
				f.Addr = prev.Runner.Addr
			}
		}
		seedFromProfile(&f, prev)
		fmt.Fprintf(out, "  re-adopting profile %q (control plane absent) — the plane keeps the runner identity; only the substrate door rotates\n", name)
	}
	applyInstallDefaults(&f)
	f.ConfigPath = installConfigPath()
	if err := seedOperatorLedger(&f); err != nil {
		return err
	}
	if err := stages.HostSideLiveCheck(name, f.Host); err != nil {
		return err
	}
	bins, err := defaultBins()
	if err != nil {
		return err
	}
	eng, err := box.NewEngine(f, bins)
	if err != nil {
		return err
	}
	eng.Provider = proxmox.New(eng.HostExecFunc())
	eng.ProviderFactory = stages.TransientFactory(eng)
	eng.Out = out
	eng.Stdin = bufio.NewReader(cmd.InOrStdin())
	return eng.RunBootstrap()
}

var installBanner = `
  ╭──────────────────────────────────────────────────────────────╮
  │                     Welcome to Freehold                      │
  │   This installer brings up your control plane end to end:    │
  │     • provisions the door into your host                     │
  │     • boots + deploys the control plane (its own LXC)        │
  │   Once the CP is up, run ` + "`freehold login` + `freehold build`" + `  │
  │   from any box — the CP brings up the world from anywhere.   │
  ╰──────────────────────────────────────────────────────────────╯
`

func runInstall(in io.Reader, out io.Writer, name string) error {
	ui := &installerUI{out: out, raw: in, in: bufio.NewReader(in)}
	fmt.Fprint(out, installBanner)
	if name == "" {
		var err error
		name, err = ui.ask("World name (profile — isolates this world's config + state)", "")
		if err != nil {
			return err
		}
	}
	action, err := gateInstall(name)
	if err != nil {
		return err
	}
	if err := selectProfile(name); err != nil {
		return err
	}
	var seed *config.Config
	if action == lifecycleReAdopt {
		if prev, _ := config.Load(installConfigPath()); prev != nil {
			seed = prev
			fmt.Fprintf(out, "  re-adopting profile %q (control plane absent) — the plane keeps the runner identity\n", name)
		}
	}
	f, err := collectAnswers(ui, seed)
	if err != nil {
		return err
	}
	f.Name = name
	applyInstallDefaults(&f)
	if err := stages.HostSideLiveCheck(name, f.Host); err != nil {
		return err
	}
	consent := "no"
	if f.ConfirmStorage {
		consent = "yes"
	}
	fmt.Fprintf(out, "  host: %s\n  runner: %s\n  domain: %s\n  operator pk: %s\n  storage consent: %s\n",
		f.Host, f.Target, f.RelayDomain, f.OperatorPubkey, consent)
	if proceed, err := ui.confirm("Proceed?", true); err != nil || !proceed {
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "aborted.")
		return nil
	}
	bins, err := defaultBins()
	if err != nil {
		return err
	}
	eng, err := box.NewEngine(f, bins)
	if err != nil {
		return err
	}
	eng.Provider = proxmox.New(eng.HostExecFunc())
	eng.ProviderFactory = stages.TransientFactory(eng)
	eng.Out = out
	eng.Stdin = ui.in
	return eng.RunBootstrap()
}

// collectAnswers gathers the install inputs (host, runner, domains, identity,
// storage) into ready-to-run box.Flags. seed is the surviving profile config on
// a re-adopt (nil when minting) — its recorded facts become the prompt defaults,
// so a re-install does not re-ask what the plane already knows.
func collectAnswers(ui *installerUI, seed *config.Config) (box.Flags, error) {
	fmt.Fprintln(ui.out, "  A few details about your world. Defaults in [brackets].")
	hostDef := "root@192.168.30.224"
	relayDef, cpDef, proxyDef := "", "", ""
	if seed != nil {
		if seed.Host != "" {
			hostDef = seed.Host
		}
		relayDef, cpDef = seed.RelayHost(), seed.CPHost()
		if seed.Proxy.Ip != nil {
			proxyDef = *seed.Proxy.Ip
		}
	}
	host, err := ui.ask("Host (address the runner will SSH into)", hostDef)
	if err != nil {
		return box.Flags{}, err
	}
	port, err := ui.askUint32("Runner MCP port (local loopback)", box.DefaultRunnerPort)
	if err != nil {
		return box.Flags{}, err
	}
	relayDomain, err := ui.ask("Relay domain (must resolve to your host)", relayDef)
	if err != nil {
		return box.Flags{}, err
	}
	cpDomain, err := ui.ask("Control-plane domain (REQUIRED)", cpDef)
	if err != nil {
		return box.Flags{}, err
	}
	proxyIP, err := ui.ask("proxy static IP (CIDR, e.g. 192.168.30.8/24) — REQUIRED", proxyDef)
	if err != nil {
		return box.Flags{}, err
	}
	rootfs, err := ui.askUint32("LXC rootfs size (GB)", 16)
	if err != nil {
		return box.Flags{}, err
	}
	memory, err := ui.askUint32("LXC memory (MB)", 2048)
	if err != nil {
		return box.Flags{}, err
	}
	if relayDomain == "" || cpDomain == "" || proxyIP == "" {
		return box.Flags{}, fmt.Errorf("relay/CP domains and the proxy IP are required")
	}
	if !strings.Contains(proxyIP, "/") {
		return box.Flags{}, fmt.Errorf("proxy static IP must be CIDR (host/prefix) — got %q", proxyIP)
	}
	pk, opDir, err := collectOperatorIdentity(ui)
	if err != nil {
		return box.Flags{}, err
	}
	consent, err := ui.confirm("If this host has no usable storage, may freehold create a new one? (freehold never erases existing data)", false)
	if err != nil {
		return box.Flags{}, err
	}
	return box.Flags{
		Addr:               box.LoopbackAddr(uint16(port)),
		Host:               host,
		RelayDomain:        relayDomain,
		CpDomain:           cpDomain,
		ProxyIP:            proxyIP,
		OperatorPubkey:     pk,
		OperatorIdentity:   opDir,
		SizeGB:             drive.TenantLVSizeGB,
		PoolSizeGB:         drive.FreshPoolSizeGB,
		RootfsGB:           rootfs,
		MemoryMB:           memory,
		RelayGw:            "192.168.30.1",
		Bridge:             "vmbr0",
		LitellmProviderKey: os.Getenv("FREEHOLD_LITELLM_PROVIDER_KEY"),
		ConfigPath:         installConfigPath(),
		ConfirmStorage:     consent,
	}, nil
}

// applyInstallDefaults fills the static defaults install's own answers would
// otherwise leave empty (flag-layer defaults apply to bootstrap; install owns
// its answers). Empty storage/bridge/agent-name/runner boot the CP LXC
// malformed.
func applyInstallDefaults(f *box.Flags) {
	if f.StorageName == "" {
		f.StorageName = "local-lvm"
	}
	if f.Bridge == "" {
		f.Bridge = "vmbr0"
	}
	if f.AgentName == "" {
		f.AgentName = "freehold"
	}
	// The provisioning runner's name is not an operator input: it is the
	// ssh-as-root-to-the-PVE-host capability (`<target>-<protocol>-<identity>`),
	// fixed so the build's capability-runner stage adopts this same runner.
	if f.Target == "" {
		f.Target = box.RunnerTarget
	}
	// Proxmox-over-root-SSH is the only implemented access mode today;
	// provider-API modes (api-vultr) arrive with the Access seam (PR3).
	if f.AccessMode == "" {
		f.AccessMode = "ssh-root-proxmox"
	}
}

// flagsFromCmd maps the bootstrap command's flags into box.Flags.
func flagsFromCmd(cmd *cobra.Command) box.Flags {
	f := box.Flags{}
	f.Name, _ = cmd.Flags().GetString("name")
	f.LocalPort, _ = cmd.Flags().GetUint32("local-port")
	f.Addr = box.LoopbackAddr(uint16(f.LocalPort))
	f.Host, _ = cmd.Flags().GetString("host")
	f.RelayDomain, _ = cmd.Flags().GetString("relay-domain")
	f.CpDomain, _ = cmd.Flags().GetString("cp-domain")
	f.ProxyIP, _ = cmd.Flags().GetString("proxy-ip")
	f.OperatorPubkey, _ = cmd.Flags().GetString("operator-pubkey")
	f.OperatorIdentity, _ = cmd.Flags().GetString("operator-identity")
	f.RootfsGB, _ = cmd.Flags().GetUint32("rootfs-gb")
	f.MemoryMB, _ = cmd.Flags().GetUint32("memory-mb")
	f.RelayGw, _ = cmd.Flags().GetString("relay-gw")
	f.StorageName, _ = cmd.Flags().GetString("storage")
	f.Bridge, _ = cmd.Flags().GetString("bridge")
	f.ThinPool, _ = cmd.Flags().GetString("thin-pool")
	f.PlanePool, _ = cmd.Flags().GetString("plane-pool")
	f.ConfirmSharedPool, _ = cmd.Flags().GetBool("confirm-shared-pool")
	f.EraseFreehold, _ = cmd.Flags().GetBool("erase-freehold")
	f.ConfirmStorage, _ = cmd.Flags().GetBool("confirm-storage")
	f.Yes, _ = cmd.Flags().GetBool("non-interactive")
	f.Version, _ = cmd.Flags().GetString("version")
	f.Channel, _ = cmd.Flags().GetString("channel")
	f.SizeGB, _ = cmd.Flags().GetUint64("size-gb")
	f.PoolSizeGB, _ = cmd.Flags().GetUint64("pool-size-gb")
	f.LitellmProviderKey = os.Getenv("FREEHOLD_LITELLM_PROVIDER_KEY")
	if v, _ := cmd.Flags().GetString("litellm-provider-key"); v != "" {
		f.LitellmProviderKey = v
	}
	return f
}

// defaultBins resolves the sibling binaries the freehold binary execs; ResolveBins
// returns the fully-populated Bins alongside its error, and we propagate that
// error so a missing sibling surfaces ResolveBins' actionable "build them once"
// message instead of a silent empty-path exec failure.
func defaultBins() (box.Bins, error) {
	b, err := box.ResolveBins()
	if err != nil {
		return b, err
	}
	return b, nil
}

func init() {
	addInstallFlags(installCmd)
}

// addInstallFlags registers the install surface: --name/--host are the
// non-negotiables, the relay/CP domains + proxy IP are the fresh-plane inputs a
// re-adopt resolves from the plane.
func addInstallFlags(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "World/profile name (REQUIRED — isolates config + state under profiles/<name>)")
	cmd.Flags().String("host", "", "Host address freehold reaches (REQUIRED, e.g. root@192.168.30.224)")
	cmd.Flags().Uint32("local-port", box.DefaultRunnerPort, "Runner MCP port on the box's loopback (127.0.0.1:<port>)")
	cmd.Flags().String("relay-domain", "", "The RELAY's own public host (REQUIRED on a fresh plane)")
	cmd.Flags().String("cp-domain", "", "The CONTROL PLANE's public host (REQUIRED on a fresh plane)")
	cmd.Flags().String("proxy-ip", "", "STATIC proxy IP (CIDR) — REQUIRED on a fresh plane")
	cmd.Flags().String("operator-pubkey", "", "Operator Nostr pubkey (64-hex) — REQUIRED")
	cmd.Flags().String("operator-identity", "", "Operator identity dir to record (optional)")
	cmd.Flags().Uint32("rootfs-gb", 16, "LXC rootfs size in GB")
	cmd.Flags().Uint32("memory-mb", 2048, "LXC memory in MB")
	cmd.Flags().String("relay-gw", "192.168.30.1", "Gateway for static guest IPs")
	cmd.Flags().String("storage", "local-lvm", "PVE LXC storage")
	cmd.Flags().String("bridge", "vmbr0", "PVE LXC network bridge")
	cmd.Flags().String("thin-pool", "", "Plane placement: existing pool to reuse, or a new name to carve")
	cmd.Flags().String("plane-pool", "", "Select the storage backend to use by name (VG or zpool)")
	cmd.Flags().Bool("confirm-shared-pool", false, "Consent to share a thin pool that already holds live volumes")
	cmd.Flags().Bool("erase-freehold", false, "Erase a detected previous freehold data plane on the chosen backend and start fresh")
	cmd.Flags().Uint64("size-gb", drive.TenantLVSizeGB, "Per-tenant thin LV size in GiB (LVM-thin backend)")
	cmd.Flags().Uint64("pool-size-gb", drive.FreshPoolSizeGB, "Thin-pool size in GiB when a NEW pool is carved")
	cmd.Flags().String("litellm-provider-key", "", "Fireworks/upstream provider API key (or FREEHOLD_LITELLM_PROVIDER_KEY)")
	cmd.Flags().Bool("confirm-storage", false, "Operator consent to CREATE a storage backend when none is detected")
	cmd.Flags().String("channel", "", "Release channel to record on the CP (stable|dev; default: derived from the build). Install deploys the LOCAL build; it does not fetch")
	cmd.Flags().String("version", "", "Version to record on the CP (default: this build's version). Install deploys the LOCAL build; it does not fetch")
	cmd.Flags().Bool("non-interactive", false, "Run headless: fail actionably instead of prompting")
}

// ---- operator identity + storage (shared helpers install needs) -------------

// operatorDir is where a minted/persisted operator identity lands. box.StateDir()
// already ends in "control-plane", so the canonical dir is <state>/control-plane/
// operator — the same path oplogin + the TUI read (installer operator_dir).
func operatorDir() string { return filepath.Join(box.StateDir(), "operator") }

func collectOperatorIdentity(ui *installerUI) (pubkey, opDir string, err error) {
	fmt.Fprintln(ui.out, "  Operator identity:")
	fmt.Fprintln(ui.out, "    1) I have a Nostr key already (paste npub or hex)")
	fmt.Fprintln(ui.out, "    2) Generate one for me (an identity dir we keep for you)")
	for {
		choice, err := ui.ask("Choice", "1")
		if err != nil {
			return "", "", err
		}
		switch strings.TrimSpace(choice) {
		case "1":
			return collectHaveKey(ui)
		case "2":
			return collectGenerate(ui)
		}
	}
}

func collectHaveKey(ui *installerUI) (string, string, error) {
	pk, err := ui.askPubkey()
	if err != nil {
		return "", "", err
	}
	opDir := operatorDir()
	if _, err := os.Stat(filepath.Join(opDir, "identity.json")); err == nil {
		return "", "", fmt.Errorf("an operator identity already exists at %s — remove it or reuse that key", opDir)
	}
	// The pubkey alone cannot operate the world — capture the matching nsec
	// now (no-echo on a TTY), verify it derives to the pasted pubkey, and
	// persist the identity so `freehold build` works from this box without a
	// separate `freehold login`.
	nsec, err := ui.askSecret("Your Nostr secret key (nsec1… or 64-hex — saved 0600, never shown)")
	if err != nil {
		return "", "", err
	}
	secret, err := crypto.NsecToSecret(nsec)
	if err != nil {
		return "", "", fmt.Errorf("bad nsec: %w", err)
	}
	derived, err := crypto.PubkeyFromSecret(secret[:])
	if err != nil {
		return "", "", err
	}
	if derived != pk {
		return "", "", fmt.Errorf("the nsec derives %s, not the pubkey you entered (%s) — paste a matching pair", derived, pk)
	}
	if err := writeOperatorIdentity(opDir, secret[:]); err != nil {
		return "", "", err
	}
	fmt.Fprintf(ui.out, "  stored: %s (0600 — this IS your key, keep it safe)\n", opDir)
	return pk, opDir, nil
}

func collectGenerate(ui *installerUI) (string, string, error) {
	dir := operatorDir()
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err == nil {
		// reuse
	} else if err := box.MintIdentity(dir); err != nil {
		return "", "", err
	}
	pk, err := box.LoadPubkey(dir)
	if err != nil {
		return "", "", err
	}
	fmt.Fprintf(ui.out, "  pubkey: %s\n  stored: %s (0600 — this IS your key, keep it safe)\n", pk, dir)
	return pk, dir, nil
}

// writeOperatorIdentity mirrors core Identity::from_nostr_secret + write 0600.
func writeOperatorIdentity(dir string, nostrSecret []byte) error {
	if err := wire.EnsurePrivateDir(dir); err != nil {
		return err
	}
	enc := make([]byte, 32)
	if _, err := rand.Read(enc); err != nil {
		return err
	}
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSecret),
		"enc_secret_hex":   hex.EncodeToString(enc),
	}
	return wire.WriteJSON0600(filepath.Join(dir, "identity.json"), doc)
}

// ---- the installerUI prompt (single buffered reader) -------------------------

type installerUI struct {
	out io.Writer
	raw io.Reader
	in  *bufio.Reader
}

func (u *installerUI) ask(label, def string) (string, error) {
	if def == "" {
		fmt.Fprintf(u.out, "  %s: ", label)
	} else {
		fmt.Fprintf(u.out, "  %s [%s]: ", label, def)
	}
	line, err := u.readLine()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(line) == "" {
		return def, nil
	}
	return strings.TrimSpace(line), nil
}

func (u *installerUI) askUint32(label string, def uint32) (uint32, error) {
	for {
		s, err := u.ask(label, strconv.FormatUint(uint64(def), 10))
		if err != nil {
			return 0, err
		}
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil || n < 1 {
			fmt.Fprintln(u.out, "  (must be a number >= 1)")
			continue
		}
		return uint32(n), nil
	}
}

func (u *installerUI) confirm(label string, def bool) (bool, error) {
	suffix := "[y/N]"
	if def {
		suffix = "[Y/n]"
	}
	for {
		fmt.Fprintf(u.out, "  %s %s: ", label, suffix)
		line, err := u.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}

func (u *installerUI) askPubkey() (string, error) {
	for {
		s, err := u.ask("Your Nostr public key (npub1… or 64 hex)", "")
		if err != nil {
			return "", err
		}
		if s == "" {
			fmt.Fprintln(u.out, "  (required — the console admin + relay owner)")
			continue
		}
		pk, err := crypto.ParsePubkeyInput(s)
		if err != nil {
			fmt.Fprintf(u.out, "  (invalid pubkey: %v)\n", err)
			continue
		}
		return pk, nil
	}
}

// askSecret reads a secret line WITHOUT echo when stdin is a terminal (the
// key must never appear on screen); piped input reads a plain line. Shares
// the ONE buffered reader discipline — on a TTY nothing buffers ahead, so
// the direct fd read cannot strand input in u.in.
func (u *installerUI) askSecret(label string) (string, error) {
	fmt.Fprintf(u.out, "  %s: ", label)
	if term.IsTerminal(os.Stdin.Fd()) {
		b, err := term.ReadPassword(os.Stdin.Fd())
		fmt.Fprintln(u.out) // the suppressed Enter
		if err != nil && len(b) == 0 {
			return "", fmt.Errorf("no input (EOF)")
		}
		s := strings.TrimSpace(string(b))
		for i := range b {
			b[i] = 0
		}
		return s, nil
	}
	line, err := u.readLine()
	if err != nil && line == "" {
		return "", fmt.Errorf("no input (EOF)")
	}
	return strings.TrimSpace(line), nil
}

func (u *installerUI) readLine() (string, error) {
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no input (EOF)")
	}
	return line, nil
}
