// The install surface: `freehold install` — getting a control plane up in an
// environment (Proxmox today; Vultr/Hetzner providers come later) and a door to
// it. It builds box.Flags and drives the SHARED provisioning engine
// (platform/provisioning/box) which boots the CP LXC and deploy-cp's it. After
// install, WORLD bring-up is `freehold build` from any box via the CP — install
// is done, the environment no longer matters.
package install

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/freehold-cli/internal/stages"
	"freehold/platform/provisioning/box"
	"freehold/providers/proxmox"
	"freehold/providers/proxmox/drive"
	"freehold/providers/vultr"
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
	if f.GatewayCIDR == "" && cfg.Gateway.Cidr != nil {
		f.GatewayCIDR = *cfg.Gateway.Cidr
	}
	if f.GatewayVlan == 0 && cfg.Gateway.Vlan != nil && *cfg.Gateway.Vlan >= 0 {
		f.GatewayVlan = *cfg.Gateway.Vlan
	}
	// The recorded Vultr instance: the re-adopt verifies + reuses it (a gone
	// instance re-creates in the host stage). Without this, a re-adopt would
	// mint a SECOND instance while the first keeps billing with the plane on
	// it.
	if cfg.AccessMode == "api-vultr" {
		f.AccessMode = "api-vultr"
		f.VultrRegion = cfg.Vultr.Region
		f.VultrPlan = cfg.Vultr.Plan
		f.VultrOsID = cfg.Vultr.OsID
		f.VultrInstance = cfg.Vultr.Instance
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
	// answer set; with gaps (and no --non-interactive), fall back to the guided
	// flow — the parsed flags ride in: what the operator already answered is a
	// prompt default, never silently dropped.
	if !f.Yes && (name == "" || f.Host == "" || f.RelayDomain == "" || f.CpDomain == "" ||
		f.ProxyIP == "" || f.OperatorPubkey == "") {
		return runInstall(cmd.InOrStdin(), out, f, cmd)
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
	if action == lifecycleMint && !cmd.Flags().Changed("local-port") {
		pickRunnerPort(&f)
	}
	applyInstallDefaults(&f, action == lifecycleMint)
	f.ConfigPath = installConfigPath()
	if err := seedOperatorLedger(&f); err != nil {
		return err
	}
	if err := ensureVultrHost(&f, out); err != nil {
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
	eng.InstallDoorKey = func(key string) error { return installDoorKeyOnVultr(f.Host, key) }
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

// runInstall is the guided flow. flagIn carries the ALREADY-PARSED flags: what
// the operator set explicitly is honored here too (as the prompt defaults and,
// for port/gateway, without re-derivation) — the guided surface is not a place
// where flags silently vanish.
func runInstall(in io.Reader, out io.Writer, flagIn box.Flags, cmd *cobra.Command) error {
	ui := &installerUI{out: out, raw: in, in: bufio.NewReader(in)}
	fmt.Fprint(out, installBanner)
	name := flagIn.Name
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
	f, err := collectAnswers(ui, seed, flagIn)
	if err != nil {
		return err
	}
	f.Name = name
	if action == lifecycleReAdopt {
		// The plane's recorded facts (gateway, runner addr) survive a
		// re-adopt — only the substrate door rotates.
		seedFromProfile(&f, seed)
		if seed != nil && seed.Runner.Addr != "" && !cmd.Flags().Changed("local-port") {
			f.Addr = seed.Runner.Addr
		}
	}
	// The port is picked, not asked: the default unless busy — unless the
	// operator pinned it (--local-port), here as much as headless.
	if action == lifecycleMint && !cmd.Flags().Changed("local-port") {
		pickRunnerPort(&f)
	}
	applyInstallDefaults(&f, action == lifecycleMint)
	consent := "no"
	if f.ConfirmStorage {
		consent = "yes"
	}
	vlan := "untagged"
	if f.GatewayVlan > 0 {
		vlan = fmt.Sprintf("vlan %d", f.GatewayVlan)
	}
	fmt.Fprintf(out, "  host: %s\n  runner: %s\n  domain: %s\n  gateway: %s (%s)\n  operator pk: %s\n  storage consent: %s\n",
		f.Host, f.Target, f.RelayDomain, f.GatewayCIDR, vlan, f.OperatorPubkey, consent)
	if proceed, err := ui.confirm("Proceed?", true); err != nil || !proceed {
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "aborted.")
		return nil
	}
	// AFTER consent: on a vultr mint the next step creates a BILLED instance.
	if err := ensureVultrHost(&f, out); err != nil {
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
	eng.InstallDoorKey = func(key string) error { return installDoorKeyOnVultr(f.Host, key) }
	eng.Out = out
	eng.Stdin = ui.in
	return eng.RunBootstrap()
}

// collectAnswers gathers the install inputs (host, runner, domains, identity,
// storage) into ready-to-run box.Flags. seed is the surviving profile config on
// a re-adopt (nil when minting); flags are what the operator already answered
// on the command line — a set flag is the prompt's default (Enter keeps it).
func collectAnswers(ui *installerUI, seed *config.Config, flags box.Flags) (box.Flags, error) {
	fmt.Fprintln(ui.out, "  A few details about your world. Defaults in [brackets].")
	isVultr := flags.AccessMode == "api-vultr"
	if !isVultr && flags.Host == "" {
		// The substrate is chosen up front: the answers (and the asks) diverge
		// — a Vultr mint creates its host, a Proxmox install reaches one.
		provider, err := ui.ask("Provider (proxmox | vultr)", "proxmox")
		if err != nil {
			return box.Flags{}, err
		}
		if strings.TrimSpace(provider) == "vultr" {
			isVultr = true
		}
	}
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
	if flags.Host != "" {
		hostDef = flags.Host
	}
	if flags.RelayDomain != "" {
		relayDef = flags.RelayDomain
	}
	if flags.CpDomain != "" {
		cpDef = flags.CpDomain
	}
	if flags.ProxyIP != "" {
		proxyDef = flags.ProxyIP
	}
	var region, plan string
	var osID uint32
	var host string
	if isVultr {
		var regionDef, planDef string
		var osDef uint32
		regionDef, planDef, osDef = "ewr", "vc2-4c-8gb", vultr.DefaultOsID
		if seed != nil && seed.Vultr.Region != "" {
			regionDef, planDef = seed.Vultr.Region, seed.Vultr.Plan
			if seed.Vultr.OsID != 0 {
				osDef = seed.Vultr.OsID
			}
		}
		if flags.VultrRegion != "" {
			regionDef = flags.VultrRegion
		}
		if flags.VultrPlan != "" {
			planDef = flags.VultrPlan
		}
		if flags.VultrOsID != 0 {
			osDef = flags.VultrOsID
		}
		var err error
		region, err = ui.ask("Vultr region", regionDef)
		if err != nil {
			return box.Flags{}, err
		}
		plan, err = ui.ask("Vultr plan (the whole world lives on this host)", planDef)
		if err != nil {
			return box.Flags{}, err
		}
		osID = osDef
		host = ""
	} else {
		var err error
		host, err = ui.ask("Host (address the runner will SSH into)", hostDef)
		if err != nil {
			return box.Flags{}, err
		}
	}
	relayDomain, err := ui.ask("Relay domain (must resolve to your host)", relayDef)
	if err != nil {
		return box.Flags{}, err
	}
	cpDomain, err := ui.ask("Control-plane domain (REQUIRED)", cpDef)
	if err != nil {
		return box.Flags{}, err
	}
	var proxyIP string
	if !isVultr {
		proxyIP, err = ui.ask("the ONE LAN address — the gateway's (CIDR, e.g. 192.168.30.8/24) — REQUIRED; everything public resolves here", proxyDef)
		if err != nil {
			return box.Flags{}, err
		}
	}
	rootfs, err := ui.askUint32("LXC rootfs size (GB)", 16)
	if err != nil {
		return box.Flags{}, err
	}
	memory, err := ui.askUint32("LXC memory (MB)", 2048)
	if err != nil {
		return box.Flags{}, err
	}
	if relayDomain == "" || cpDomain == "" {
		return box.Flags{}, fmt.Errorf("relay/CP domains are required")
	}
	if !isVultr {
		if proxyIP == "" {
			return box.Flags{}, fmt.Errorf("the proxy IP is required")
		}
		if !strings.Contains(proxyIP, "/") {
			return box.Flags{}, fmt.Errorf("proxy static IP must be CIDR (host/prefix) — got %q", proxyIP)
		}
	}
	pk, opDir, err := resolveOperatorIdentity(ui, seed, flags)
	if err != nil {
		return box.Flags{}, err
	}
	// The operator's display name in Buzz: published as their kind:0 profile
	// at build, which is what makes the desktop app skip its first-run
	// onboarding (starter channels, private Welcome, built-in welcome team).
	nameDef := "Operator"
	if seed != nil && seed.OperatorName != "" {
		nameDef = seed.OperatorName
	}
	if flags.OperatorName != "" {
		nameDef = flags.OperatorName
	}
	displayName, err := ui.ask("Your display name in Buzz (how agents address you)", nameDef)
	if err != nil {
		return box.Flags{}, err
	}
	consent, err := ui.confirm("If this host has no usable storage, may freehold create a new one? (freehold never erases existing data)", false)
	if err != nil {
		return box.Flags{}, err
	}
	// The runner MCP port (an implementation detail — picked free, not asked)
	// and the gateway subnet (derived; --gateway-cidr/--gateway-vlan override)
	// are filled by the caller.
	f := box.Flags{
		Host:               host,
		RelayDomain:        relayDomain,
		CpDomain:           cpDomain,
		ProxyIP:            proxyIP,
		OperatorPubkey:     pk,
		OperatorIdentity:   opDir,
		OperatorName:       displayName,
		SizeGB:             drive.TenantLVSizeGB,
		PoolSizeGB:         drive.FreshPoolSizeGB,
		RootfsGB:           rootfs,
		MemoryMB:           memory,
		RelayGw:            "192.168.30.1",
		Bridge:             "vmbr0",
		LitellmProviderKey: os.Getenv("FREEHOLD_LITELLM_PROVIDER_KEY"),
		ConfigPath:         installConfigPath(),
		ConfirmStorage:     consent,
	}
	if isVultr {
		f.AccessMode = "api-vultr"
		f.VultrRegion, f.VultrPlan, f.VultrOsID = region, plan, osID
	}
	// The flag fields the prompts don't cover ride through verbatim — dropping
	// them here would make --local-port/--gateway-cidr/--gateway-vlan vanish
	// on the guided path (the sentinel -1 included; RunBootstrap rejects it).
	if flags.LocalPort != 0 {
		f.LocalPort = flags.LocalPort
		f.Addr = flags.Addr
	}
	if flags.GatewayCIDR != "" {
		f.GatewayCIDR = flags.GatewayCIDR
	}
	if flags.GatewayVlan != 0 {
		f.GatewayVlan = flags.GatewayVlan
	}
	return f, nil
}

// applyInstallDefaults fills the static defaults install's own answers would
// otherwise leave empty (flag-layer defaults apply to bootstrap; install owns
// its answers). Empty storage/bridge/agent-name/runner boot the CP LXC
// malformed. mint gates the FORCED gateway: a fresh world derives its
// freehold-subnet (a re-adopt rides the recorded gateway — a pre-gateway
// world stays flat; forcing one mid-life would collide with its live LAN
// guests).
func applyInstallDefaults(f *box.Flags, mint bool) {
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
	// The gateway is FORCED on mint: the subnet is derived (10.77.0.0/24,
	// bumped past LAN overlap); --gateway-cidr/--gateway-vlan override, and a
	// re-adopt's recorded gateway was seeded before this runs. RunBootstrap
	// re-validates before any config write.
	if mint && f.GatewayCIDR == "" && f.ProxyIP != "" {
		f.GatewayCIDR = box.DefaultGatewayCIDR(f.ProxyIP)
	}
	// Proxmox-over-root-SSH is the default access mode; --provider vultr
	// switches to the API mode (the host is created, not reached) and rides
	// the dir storage backend (no local-lvm on a cloud VPS).
	if f.AccessMode == "" {
		f.AccessMode = "ssh-root-proxmox"
	}
	if f.AccessMode == "api-vultr" && (f.StorageName == "" || f.StorageName == "local-lvm") {
		// A cloud VPS has no local-lvm; the dir storage `local` hosts the
		// rootfs (an explicit --storage passes through, e.g. an attached
		// block volume's PVE id).
		f.StorageName = "local"
	}
	if f.AccessMode == "api-vultr" && f.VultrOsID == 0 {
		f.VultrOsID = vultr.DefaultOsID
	}
	if f.AccessMode == "api-vultr" {
		if f.VultrRegion == "" {
			f.VultrRegion = "ewr"
		}
		if f.VultrPlan == "" {
			f.VultrPlan = "vc2-4c-8gb"
		}
	}
}

// pickRunnerPort pins the runner MCP bind for a MINT: the default port unless
// it is taken, then the next free loopback port — the port is an
// implementation detail, never an operator decision. A re-adopt keeps its
// recorded addr (killServeOn reclaims the port), and an explicit --local-port
// stays (a busy explicit port fails the serve, loudly).
func pickRunnerPort(f *box.Flags) {
	f.LocalPort = box.PickFreeLoopbackPort(box.DefaultRunnerPort)
	f.Addr = box.LoopbackAddr(uint16(f.LocalPort))
}

// flagsFromCmd maps the bootstrap command's flags into box.Flags.
func flagsFromCmd(cmd *cobra.Command) box.Flags {
	f := box.Flags{}
	f.Name, _ = cmd.Flags().GetString("name")
	f.Target, _ = cmd.Flags().GetString("target")
	f.LocalPort, _ = cmd.Flags().GetUint32("local-port")
	f.Addr = box.LoopbackAddr(uint16(f.LocalPort))
	f.Host, _ = cmd.Flags().GetString("host")
	f.RelayDomain, _ = cmd.Flags().GetString("relay-domain")
	f.CpDomain, _ = cmd.Flags().GetString("cp-domain")
	f.ProxyIP, _ = cmd.Flags().GetString("proxy-ip")
	f.GatewayCIDR, _ = cmd.Flags().GetString("gateway-cidr")
	if v, _ := cmd.Flags().GetString("gateway-vlan"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			f.GatewayVlan = -1 // sentinel: rejected by RunBootstrap's validation
		} else {
			f.GatewayVlan = n
		}
	}
	f.OperatorPubkey, _ = cmd.Flags().GetString("operator-pubkey")
	f.OperatorIdentity, _ = cmd.Flags().GetString("operator-identity")
	f.OperatorName, _ = cmd.Flags().GetString("display-name")
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
	if v, _ := cmd.Flags().GetString("provider"); v == "vultr" {
		f.AccessMode = "api-vultr"
	}
	f.VultrRegion, _ = cmd.Flags().GetString("vultr-region")
	f.VultrPlan, _ = cmd.Flags().GetString("vultr-plan")
	if v, _ := cmd.Flags().GetUint32("vultr-os-id"); v != 0 {
		f.VultrOsID = v
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
	// The runner NAME is usually the default (box.RunnerTarget), but a world
	// installed under a named runner (e.g. freehold-live-install) re-adopts
	// THAT one — the flag records it instead of forcing the default.
	cmd.Flags().String("target", "", "Provisioning runner name (default: the standard substrate runner)")
	cmd.Flags().Uint32("local-port", box.DefaultRunnerPort, "Runner MCP port on the box's loopback (default: 8787, next free port when busy)")
	cmd.Flags().String("relay-domain", "", "The RELAY's own public host (REQUIRED on a fresh plane)")
	cmd.Flags().String("cp-domain", "", "The CONTROL PLANE's public host (REQUIRED on a fresh plane)")
	cmd.Flags().String("proxy-ip", "", "the ONE LAN address (CIDR) — REQUIRED on a fresh plane; the gateway's when a subnet is set, the k3s/proxy node otherwise")
	cmd.Flags().String("gateway-cidr", "", "override the internal subnet CIDR for the freehold-subnet gateway (default: derived, e.g. 10.77.0.0/24)")
	cmd.Flags().String("gateway-vlan", "", "in-host bridge VLAN tag for the internal subnet (0/blank = untagged)")
	cmd.Flags().String("operator-pubkey", "", "Operator Nostr pubkey (64-hex) — REQUIRED")
	cmd.Flags().String("operator-identity", "", "Operator identity dir to record (optional)")
	cmd.Flags().String("display-name", "", "Operator display name in Buzz (default \"Operator\"; published as the kind:0 profile that skips the desktop app's first-run onboarding)")
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
	cmd.Flags().String("provider", "", "Substrate provider: proxmox (default) | vultr (creates a cloud instance and installs PVE on it)")
	cmd.Flags().String("vultr-region", "", "Vultr region for a --provider vultr host (default: ewr)")
	cmd.Flags().String("vultr-plan", "", "Vultr plan for a --provider vultr host (default: vc2-4c-8gb — the whole world lives on this host)")
	cmd.Flags().Uint32("vultr-os-id", vultr.DefaultOsID, "Vultr os_id (default: Debian 12 x64)")
}

// ---- vultr: the created-host lifecycle --------------------------------------

// vultrToken reads the Vultr API key from the environment. It is NEVER
// persisted: the verbs that need it (install create, uninstall destroy) read
// it fresh, like the DNS/litellm secrets.
func vultrToken() (string, error) {
	tok := strings.TrimSpace(os.Getenv("VULTR_API_KEY"))
	if tok == "" {
		return "", fmt.Errorf("VULTR_API_KEY is not set — export the Vultr API key (it is never stored)")
	}
	return tok, nil
}

// vultrClient builds a client from the environment token.
func vultrClient() (*vultr.Client, error) {
	tok, err := vultrToken()
	if err != nil {
		return nil, err
	}
	return &vultr.Client{Token: tok}, nil
}

// doorPubkeyLine renders this box's DOOR key's authorized_keys line — the
// key the Vultr instance is born with (stable across re-adopts, so the
// data verbs + live check keep working).
func doorPubkeyLine() (string, error) {
	if err := box.EnsureIdentity(box.OpsDir()); err != nil {
		return "", err
	}
	pem, err := box.DoorKeyPEM()
	if err != nil {
		return "", err
	}
	return crypto.ExtractED25519PublicKeyLine(pem)
}

// ensureVultrHost is the api-vultr mint/re-adopt host stage: make sure a
// live Vultr instance exists and is a PVE host, deriving Host (root@ip) and
// ProxyIP (ip/32) from it. A mint creates (region/plan/os from the flags, a
// re-adopt's from the profile); a re-adopt reuses a surviving instance and
// re-creates a destroyed one (the door key is re-authorized at birth, so the
// substrate door rotates transparently). After create: wait for the IP,
// wait for SSH, and run the PVE-on-Debian install (the spike recipe).
func ensureVultrHost(f *box.Flags, out io.Writer) error {
	if f.AccessMode != "api-vultr" {
		return nil
	}
	verified := false
	if f.VultrInstance != "" {
		// A re-adopt with a recorded instance: verify it still exists and
		// holds the recorded shape. ONLY a definitive 404 re-creates — a
		// rate-limit, a timeout, or a still-settling IP all mean the
		// instance EXISTS and billing, and minting a second one beside it
		// strands the plane. Fail loudly instead; the operator decides.
		c, err := vultrClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, ip, gerr := c.Instance(ctx, f.VultrInstance)
		switch {
		case gerr == nil && ip != "" && ip != "0.0.0.0":
			f.Host = "root@" + ip
			f.ProxyIP = ip + "/32"
			if f.GatewayCIDR == "" {
				f.GatewayCIDR = box.DefaultGatewayCIDR(f.ProxyIP)
			}
			fmt.Fprintf(out, "  vultr instance %s alive at %s — re-adopting it\n", f.VultrInstance, ip)
			verified = true
		case gerr != nil && strings.Contains(gerr.Error(), "HTTP 404"):
			fmt.Fprintf(out, "  vultr instance %s is gone — re-creating it\n", f.VultrInstance)
		case gerr != nil:
			return fmt.Errorf("cannot verify the recorded vultr instance %s: %w — refusing to mint a second one beside it; retry when the API answers (VULTR_API_KEY)", f.VultrInstance, gerr)
		default:
			return fmt.Errorf("vultr instance %s exists but has no address yet (still settling) — retry in a minute", f.VultrInstance)
		}
	}
	if verified {
		return nil
	}
	// A mint (or a re-adopt whose instance is gone): the recorded/flag shape
	// is re-created, so the flow below always ends with a live IP.
	key, err := doorPubkeyLine()
	if err != nil {
		return fmt.Errorf("door key: %w", err)
	}
	c, err := vultrClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	fmt.Fprintf(out, "  creating the vultr host (%s %s)…\n", f.VultrRegion, f.VultrPlan)
	keyID, err := c.EnsureSSHKey(ctx, "freehold-door", key)
	if err != nil {
		return fmt.Errorf("vultr ssh-key: %w", err)
	}
	label := "freehold-" + f.Name
	id, err := c.CreateInstance(ctx, f.VultrRegion, f.VultrPlan, f.VultrOsID, label, keyID)
	if err != nil {
		return fmt.Errorf("vultr create: %w", err)
	}
	f.VultrInstance = id
	fmt.Fprintf(out, "  instance %s — waiting for an address…\n", id)
	ip, err := c.WaitActive(ctx, id, 8*time.Minute)
	if err != nil {
		return err
	}
	f.Host = "root@" + ip
	f.ProxyIP = ip + "/32"
	if f.GatewayCIDR == "" {
		f.GatewayCIDR = box.DefaultGatewayCIDR(f.ProxyIP)
	}
	fmt.Fprintf(out, "  instance live at %s — installing PVE (apt route; this takes minutes)…\n", ip)
	if err := waitSSH(ip, 10*time.Minute); err != nil {
		return err
	}
	return installPVEOnHost(ip)
}

// waitSSH polls until the host answers over SSH with ANY key (cloud-init
// writes the authorized key at boot; the door key is pre-authorized via the
// create call).
func waitSSH(ip string, timeout time.Duration) error {
	keyPath, cleanup, err := tempDoorKey()
	if err != nil {
		return err
	}
	defer cleanup()
	deadline := time.Now().Add(timeout)
	for {
		if out, err := proxmox.SSHExec(ip, keyPath)("echo ssh-ok", 30); err == nil &&
			out.ExitCode != nil && *out.ExitCode == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s never answered SSH within %s (is the door key authorized?)", ip, timeout)
		}
		time.Sleep(5 * time.Second)
	}
}

// tempDoorKey writes the DOOR private key to a 0600 temp file.
func tempDoorKey() (string, func(), error) {
	pem, err := box.DoorKeyPEM()
	if err != nil {
		return "", nil, err
	}
	return proxmox.WriteTempKey(pem)
}

// installPVEOnHost runs the PVE-on-Debian install over the door key. The
// script is the spike's recipe (providers/vultr); the long timeout covers
// apt full-upgrade + proxmox-ve on a small plan.
func installPVEOnHost(ip string) error {
	keyPath, cleanup, err := tempDoorKey()
	if err != nil {
		return err
	}
	defer cleanup()
	out, err := proxmox.SSHExec(ip, keyPath)(vultr.PVEInstallScript(), 1800)
	if err != nil {
		return fmt.Errorf("pve install: %w", err)
	}
	if out.ExitCode == nil || *out.ExitCode != 0 {
		return fmt.Errorf("pve install failed: %s", strings.TrimSpace(out.Stderr))
	}
	if !strings.Contains(out.Stdout, "pve-install-ok") {
		return fmt.Errorf("pve install did not complete: %s", strings.TrimSpace(out.Stdout+"\n"+out.Stderr))
	}
	return nil
}

// installDoorKeyOnVultr appends a door public line to the host's
// authorized_keys over the DOOR key (the engine's api-vultr door stage —
// the paste gate is impossible on a headless cloud host). Idempotent.
func installDoorKeyOnVultr(host, key string) error {
	ip := strings.TrimPrefix(host, "root@")
	keyPath, cleanup, err := tempDoorKey()
	if err != nil {
		return err
	}
	defer cleanup()
	// grep -F the exact line before appending: a re-run must not duplicate.
	cmd := fmt.Sprintf(`mkdir -p /root/.ssh && chmod 700 /root/.ssh && touch /root/.ssh/authorized_keys && (grep -qF '%s' /root/.ssh/authorized_keys || echo '%s' >> /root/.ssh/authorized_keys)`, key, key)
	out, err := proxmox.SSHExec(ip, keyPath)(cmd, 60)
	if err != nil {
		return fmt.Errorf("door install: %w", err)
	}
	if out.ExitCode == nil || *out.ExitCode != 0 {
		return fmt.Errorf("door install failed: %s", strings.TrimSpace(out.Stderr))
	}
	return nil
}

// ---- operator identity + storage (shared helpers install needs) -------------

// operatorDir is where a minted/persisted operator identity lands. box.StateDir()
// already ends in "control-plane", so the canonical dir is <state>/control-plane/
// operator — the same path oplogin + the TUI read (installer operator_dir).
func operatorDir() string { return filepath.Join(box.StateDir(), "operator") }

// resolveOperatorIdentity answers the operator block. A re-adopt rides the
// RECORDED identity — the dir exists from the prior install and the profile
// records its pubkey — so it is not asked at all (re-asking would error on
// the existing dir or mint a pointless second identity). An explicit
// --operator-pubkey over a live ledger rides too. A mint prompts
// (paste/match/generate); a vanished dir falls through to the prompts.
func resolveOperatorIdentity(ui *installerUI, seed *config.Config, flags box.Flags) (string, string, error) {
	if seed != nil && seed.OperatorPubkey != "" {
		dir := operatorDir()
		if seed.OperatorIdentity != nil && *seed.OperatorIdentity != "" {
			dir = *seed.OperatorIdentity
		}
		if _, err := os.Stat(filepath.Join(dir, "identity.json")); err == nil {
			return seed.OperatorPubkey, dir, nil
		}
	}
	if flags.OperatorPubkey != "" {
		if _, err := os.Stat(filepath.Join(operatorDir(), "identity.json")); err == nil {
			return flags.OperatorPubkey, operatorDir(), nil
		}
	}
	return collectOperatorIdentity(ui)
}

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
		// Pasting the SAME key as the stored identity is a reuse, not a
		// conflict — only a DIFFERENT key is.
		stored, lerr := box.LoadPubkey(opDir)
		if lerr != nil {
			return "", "", fmt.Errorf("an operator identity exists at %s but is unreadable: %w", opDir, lerr)
		}
		if stored == pk {
			fmt.Fprintf(ui.out, "  reused: %s\n", opDir)
			return pk, opDir, nil
		}
		return "", "", fmt.Errorf("an operator identity already exists at %s with a different key — remove it or paste that key", opDir)
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
