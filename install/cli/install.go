// The install CLI: `freehold-install install`/`bootstrap` — getting a control
// plane up in an environment (Proxmox today; Vultr/Hetzner providers come
// later) and a door to it. It builds box.Flags and drives the SHARED
// provisioning engine (platform/provisioning/box) which boots the CP LXC and
// deploy-cp's it. After bootstrap, WORLD bring-up is `freehold build` from any
// box via the CP — install is done, the environment no longer matters.
package cli

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/platform/provisioning/box"
	"freehold/platform/provisioning/drive"
)

// installConfigPath is the config path the install writes: the selected
// profile's config (profiles/<name>/config.toml) once selectProfile has pinned
// it, else the default config path.
func installConfigPath() string { return config.ConfigPath() }

// selectProfile validates a world/profile name and pins the process to it,
// creating the profile on first install. It MUST run before any path helper
// (installConfigPath, operatorDir, box.StateDir) so the config, state, and
// operator identity all land under profiles/<name>/. Fail closed when the
// profile already exists: reconciling an existing world is `freehold build`'s
// job, not a fresh install's.
func selectProfile(name string) error {
	if !config.ValidProfileName(name) {
		return fmt.Errorf("invalid --name %q (letters, digits, dash, underscore; no leading/trailing dash or underscore)", name)
	}
	if p := config.Resolve(name); p != nil {
		return fmt.Errorf("profile %q already exists (%s) — use `freehold build` to reconcile it, or pick another --name", name, p.ConfigPath)
	}
	config.SetCurrent(&config.Profile{
		Name:       name,
		ConfigPath: config.NewProfilePath(name),
		StateDir:   config.NewProfileState(name),
	})
	return nil
}

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Bring up ONLY a control plane in an environment + the box's door — then `freehold build` brings up the world via the CP",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			return fmt.Errorf("bootstrap needs --name (the world/profile name — isolates this world's config and state)")
		}
		if err := selectProfile(name); err != nil {
			return err
		}
		f := flagsFromCmd(cmd)
		applyInstallDefaults(&f)
		f.ConfigPath = installConfigPath()
		bins, err := defaultBins()
		if err != nil {
			return err
		}
		eng, err := box.NewEngine(f, bins)
		if err != nil {
			return err
		}
		eng.Out = cmd.OutOrStdout()
		return eng.RunBootstrap()
	},
}

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Bring up a control plane interactively (asks a few questions, then the bootstrap pipeline)",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		return runInstall(os.Stdin, cmd.OutOrStdout(), name)
	},
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
	if err := selectProfile(name); err != nil {
		return err
	}
	f, err := collectAnswers(ui)
	if err != nil {
		return err
	}
	f.Name = name
	applyInstallDefaults(&f)
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
	eng.Out = out
	eng.Stdin = ui.in
	return eng.RunBootstrap()
}

// collectAnswers gathers the install inputs (host, runner, domains, identity,
// storage) into ready-to-run box.Flags.
func collectAnswers(ui *installerUI) (box.Flags, error) {
	fmt.Fprintln(ui.out, "  A few details about your world. Defaults in [brackets].")
	host, err := ui.ask("Host (address the runner will SSH into)", "root@192.168.30.224")
	if err != nil {
		return box.Flags{}, err
	}
	runner, err := ui.ask("Runner name", "proxmox-box")
	if err != nil {
		return box.Flags{}, err
	}
	serve, err := ui.ask("Runner MCP address (loopback)", "127.0.0.1:8787")
	if err != nil {
		return box.Flags{}, err
	}
	relayDomain, err := ui.ask("Relay domain (must resolve to your host)", "")
	if err != nil {
		return box.Flags{}, err
	}
	cpDomain, err := ui.ask("Control-plane domain (REQUIRED)", "")
	if err != nil {
		return box.Flags{}, err
	}
	proxyIP, err := ui.ask("proxy static IP (CIDR, e.g. 192.168.30.8/24) — REQUIRED", "")
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
		Addr:               serve,
		Target:             runner,
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
// its answers). Empty storage/bridge/agent-name boot the CP LXC malformed.
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
}

// flagsFromCmd maps the bootstrap command's flags into box.Flags.
func flagsFromCmd(cmd *cobra.Command) box.Flags {
	f := box.Flags{}
	f.Name, _ = cmd.Flags().GetString("name")
	f.Addr, _ = cmd.Flags().GetString("addr")
	f.Target, _ = cmd.Flags().GetString("target")
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
	f.Yes, _ = cmd.Flags().GetBool("yes")
	f.SizeGB, _ = cmd.Flags().GetUint64("size-gb")
	f.PoolSizeGB, _ = cmd.Flags().GetUint64("pool-size-gb")
	f.LitellmProviderKey = os.Getenv("FREEHOLD_LITELLM_PROVIDER_KEY")
	if v, _ := cmd.Flags().GetString("litellm-provider-key"); v != "" {
		f.LitellmProviderKey = v
	}
	return f
}

// defaultBins resolves the sibling binaries freehold-install execs; ResolveBins
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
	bootstrapCmd.Flags().String("name", "", "World/profile name (REQUIRED — isolates config + state under profiles/<name>)")
	installCmd.Flags().String("name", "", "World/profile name (prompted if omitted)")
	bootstrapCmd.Flags().String("addr", "127.0.0.1:8787", "Runner MCP address (loopback)")
	bootstrapCmd.Flags().String("target", "proxmox-box", "Runner name")
	bootstrapCmd.Flags().String("host", "root@192.168.30.224", "Host address the runner SSH's into")
	bootstrapCmd.Flags().String("relay-domain", "", "The RELAY's own public host (REQUIRED)")
	bootstrapCmd.Flags().String("cp-domain", "", "The CONTROL PLANE's public host (REQUIRED)")
	bootstrapCmd.Flags().String("proxy-ip", "", "STATIC proxy IP (CIDR) — REQUIRED")
	bootstrapCmd.Flags().String("operator-pubkey", "", "Operator Nostr pubkey (64-hex) — REQUIRED")
	bootstrapCmd.Flags().String("operator-identity", "", "Operator identity dir to record (optional)")
	bootstrapCmd.Flags().Uint32("rootfs-gb", 16, "LXC rootfs size in GB")
	bootstrapCmd.Flags().Uint32("memory-mb", 2048, "LXC memory in MB")
	bootstrapCmd.Flags().String("relay-gw", "192.168.30.1", "Gateway for static guest IPs")
	bootstrapCmd.Flags().String("storage", "local-lvm", "PVE LXC storage")
	bootstrapCmd.Flags().String("bridge", "vmbr0", "PVE LXC network bridge")
	bootstrapCmd.Flags().String("thin-pool", "", "Plane placement: existing pool to reuse, or a new name to carve")
	bootstrapCmd.Flags().String("plane-pool", "", "Select the storage backend to use by name (VG or zpool)")
	bootstrapCmd.Flags().Bool("confirm-shared-pool", false, "Consent to share a thin pool that already holds live volumes")
	bootstrapCmd.Flags().Bool("erase-freehold", false, "Erase a detected previous freehold data plane on the chosen backend and start fresh")
	bootstrapCmd.Flags().Uint64("size-gb", drive.TenantLVSizeGB, "Per-tenant thin LV size in GiB (LVM-thin backend)")
	bootstrapCmd.Flags().Uint64("pool-size-gb", drive.FreshPoolSizeGB, "Thin-pool size in GiB when a NEW pool is carved")
	bootstrapCmd.Flags().String("litellm-provider-key", "", "Fireworks/upstream provider API key (or FREEHOLD_LITELLM_PROVIDER_KEY)")
	bootstrapCmd.Flags().Bool("confirm-storage", false, "Operator consent to CREATE a storage backend when none is detected")
	bootstrapCmd.Flags().Bool("yes", false, "Non-interactive: bail (actionably) where the pipeline would prompt")
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
	return pk, "", nil
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

func (u *installerUI) readLine() (string, error) {
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no input (EOF)")
	}
	return line, nil
}
