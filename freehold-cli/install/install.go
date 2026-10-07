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
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/box"
	"freehold/providers/proxmox"
	"freehold/providers/proxmox/drive"
	"freehold/providers/registry"
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
	// The recorded host + its provider: the re-adopt verifies + reuses a
	// created instance (a gone one re-creates in the host stage). Without
	// this, a re-adopt would mint a SECOND instance while the first keeps
	// billing with the plane on it.
	if cfg.HostProvider.Provider != "" {
		f.Provider = cfg.HostProvider.Provider
		f.HostID = cfg.HostProvider.ID
		f.HostsGateway = cfg.HostProvider.Gateway
		for k, v := range cfg.HostProvider.Answers {
			if f.HostAnswers == nil {
				f.HostAnswers = map[string]string{}
			}
			if _, ok := f.HostAnswers[k]; !ok {
				f.HostAnswers[k] = v
			}
		}
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

	// The provider resolves from the RECORDED profile first: the headless
	// re-adopt gate must gate against the RIGHT provider's needs (a vultr
	// world without --provider would otherwise be demanded --host/--proxy-ip
	// — needs it does not declare), and the recorded answers must beat the
	// provider's defaults (a 404 re-create re-creates the recorded SHAPE).
	if p := config.Resolve(name); p != nil {
		if prev, _ := config.Load(p.ConfigPath); prev != nil {
			seedFromProfile(&f, prev)
		}
	}

	// The provider gates the headless answer set: its Needs() decide what
	// install cannot derive (proxmox: the host + the edge address; a
	// created-host provider: its key + answers). Gaps (and no
	// --non-interactive) fall back to the guided flow — the parsed flags
	// ride in: what the operator already answered is a prompt default,
	// never silently dropped.
	prov, perr := registry.ByName(f.Provider)
	if perr != nil {
		return perr
	}
	missing := missingAnswers(prov, &f)
	if !f.Yes && (name == "" || f.RelayDomain == "" || f.CpDomain == "" || f.OperatorPubkey == "" || len(missing) > 0) {
		return runInstall(cmd.InOrStdin(), out, f, cmd)
	}
	if name == "" {
		return fmt.Errorf("install needs --name (the world/profile name — isolates this world's config and state)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("install --non-interactive needs %s (the provider %q's unanswered needs; secrets ride the env)", strings.Join(missing, ", "), prov.Name())
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
	session, err := ensureHost(&f, out, nil)
	if err != nil {
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
	eng.HostSession = session
	eng.InstallDoorKey = func(key string) error { return prov.InstallDoorKey(eng.HostSession, key) }
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
	// Display candidate: the pipeline's authoritative derive (+ its L2
	// collision probe) runs after the confirm and prints any bump — this is
	// the LAN-overlap-safe candidate the probe starts from.
	gwDisplay := f.GatewayCIDR
	if f.Mint && gwDisplay == "" && f.ProxyIP != "" {
		gwDisplay = box.DefaultGatewayCIDR(f.ProxyIP)
	}
	fmt.Fprintf(out, "  host: %s\n  runner: %s\n  domain: %s\n  gateway: %s (%s)\n  operator pk: %s\n  storage consent: %s\n",
		f.Host, f.Target, f.RelayDomain, gwDisplay, vlan, f.OperatorPubkey, consent)
	if proceed, err := ui.confirm("Proceed?", true); err != nil || !proceed {
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "aborted.")
		return nil
	}
	// AFTER consent: on a created-host mint the next step creates a BILLED
	// instance.
	session, err := ensureHost(&f, out, ui)
	if err != nil {
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
	eng.HostSession = session
	prov, perr := registry.ByName(f.Provider)
	if perr != nil {
		return perr
	}
	eng.InstallDoorKey = func(key string) error { return prov.InstallDoorKey(eng.HostSession, key) }
	eng.Out = out
	eng.Stdin = ui.in
	return eng.RunBootstrap()
}

// collectAnswers gathers the install inputs into ready-to-run box.Flags:
// the core asks (world identity, domains, operator) are the installer's; the
// PROVIDER's Needs() drive the substrate asks (the host address, the edge's
// LAN address, a created host's credentials + region/plan — secrets pasted
// no-echo, never stored). seed is the surviving profile config on a
// re-adopt (nil when minting); flags are what the operator already answered
// on the command line — a set flag is the prompt's default (Enter keeps it).
func collectAnswers(ui *installerUI, seed *config.Config, flags box.Flags) (box.Flags, error) {
	fmt.Fprintln(ui.out, "  A few details about your world. Defaults in [brackets].")
	// The substrate is chosen up front: the provider owns the asks that
	// follow (its Needs()) — a reached host (proxmox) is named; a created
	// host (vultr) is described to its API. A re-adopt rides the recorded
	// provider — asking would offer a default that contradicts the profile.
	prov, err := registry.ByName(flags.Provider)
	if err != nil {
		return box.Flags{}, err
	}
	if flags.Provider == "" && seed != nil && seed.HostProvider.Provider != "" {
		if prov, err = registry.ByName(seed.HostProvider.Provider); err != nil {
			return box.Flags{}, err
		}
	} else if flags.Provider == "" {
		var ans string
		ans, err = ui.ask("Provider ("+strings.Join(registry.Names(), " | ")+")", "proxmox")
		if err != nil {
			return box.Flags{}, err
		}
		if prov, err = registry.ByName(strings.TrimSpace(ans)); err != nil {
			return box.Flags{}, err
		}
	}
	// A re-adopt's recorded answers (and host) seed the asks.
	var recorded map[string]string
	var seedHost string
	if seed != nil {
		recorded = seed.HostProvider.Answers
		seedHost = seed.Host
	}

	hostDef := needDefault(prov, provisioning.NeedHost, recorded, seedHost, flags.Host)
	relayDef, cpDef, proxyDef := "", "", ""
	if seed != nil {
		relayDef, cpDef = seed.RelayHost(), seed.CPHost()
		if seed.Proxy.Ip != nil {
			proxyDef = *seed.Proxy.Ip
		}
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

	var host, proxyIP string
	var consent bool
	answers, secrets := map[string]string{}, map[string]string{}
	// The asks keep the guided flow's established order: the host, the
	// provider's own needs, the domains, the edge address, the guest sizes,
	// the identity, the display name, and the storage consent last.
	askNeed := func(need provisioning.HostNeed) error {
		switch need.Name {
		case provisioning.NeedHost:
			var err error
			if host, err = ui.ask(need.Label, hostDef); err != nil {
				return err
			}
		case provisioning.NeedProxyIP:
			var err error
			if proxyIP, err = ui.ask(need.Label, proxyDef); err != nil {
				return err
			}
		case provisioning.NeedConfirmStorage:
			var err error
			if consent, err = ui.confirm(need.Label, false); err != nil {
				return err
			}
		default:
			if need.Secret {
				if env := os.Getenv(need.Name); env != "" {
					secrets[need.Name] = env
					fmt.Fprintf(ui.out, "  %s: (from %s — not shown)\n", need.Label, need.Name)
					return nil
				}
				v, serr := ui.askSecret(need.Label)
				if serr != nil {
					return serr
				}
				secrets[need.Name] = v
				return nil
			}
			def := need.Default
			if recorded != nil && recorded[need.Name] != "" {
				def = recorded[need.Name]
			}
			if flags.HostAnswers[need.Name] != "" {
				def = flags.HostAnswers[need.Name]
			}
			v, err := ui.ask(need.Label, def)
			if err != nil {
				return err
			}
			answers[need.Name] = v
		}
		return nil
	}
	// pass 1: the host + the provider's own needs (secrets first — the
	// operator pastes before anything long runs).
	for _, need := range prov.Needs() {
		if need.Name == provisioning.NeedHost || (need.Name != provisioning.NeedProxyIP && need.Name != provisioning.NeedConfirmStorage) {
			if err := askNeed(need); err != nil {
				return box.Flags{}, err
			}
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
	// pass 2: the edge address.
	for _, need := range prov.Needs() {
		if need.Name == provisioning.NeedProxyIP {
			if err := askNeed(need); err != nil {
				return box.Flags{}, err
			}
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
	if needPresent(prov, provisioning.NeedProxyIP) {
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
	// pass 3: the storage consent, last (it is about the host's disks, and
	// a created-host provider that never touches them never asks it).
	for _, need := range prov.Needs() {
		if need.Name == provisioning.NeedConfirmStorage {
			if err := askNeed(need); err != nil {
				return box.Flags{}, err
			}
		}
	}
	// The runner MCP port (an implementation detail — picked free, not asked)
	// and the gateway subnet (derived; --gateway-cidr/--gateway-vlan override)
	// are filled by the caller.
	f := box.Flags{
		Provider:         prov.Name(),
		HostAnswers:      answers,
		HostSecrets:      secrets,
		Host:             host,
		RelayDomain:      relayDomain,
		CpDomain:         cpDomain,
		ProxyIP:          proxyIP,
		OperatorPubkey:   pk,
		OperatorIdentity: opDir,
		OperatorName:     displayName,
		SizeGB:           drive.TenantLVSizeGB,
		PoolSizeGB:       drive.FreshPoolSizeGB,
		RootfsGB:         rootfs,
		MemoryMB:         memory,
		ConfigPath:       installConfigPath(),
		ConfirmStorage:   consent,
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
// needDefault resolves a plain need's guided default: the flag answer, the
// profile's recorded answer, the seed host, then the provider's own default.
func needDefault(prov provisioning.HostProvider, name string, recorded map[string]string, seedHost, flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if name == provisioning.NeedHost && seedHost != "" {
		return seedHost
	}
	if recorded != nil && recorded[name] != "" {
		return recorded[name]
	}
	for _, n := range prov.Needs() {
		if n.Name == name {
			return n.Default
		}
	}
	return ""
}

// needPresent reports whether the provider declares a need at all (the
// installer's required-field validation only applies when it does).
func needPresent(prov provisioning.HostProvider, name string) bool {
	for _, n := range prov.Needs() {
		if n.Name == name {
			return true
		}
	}
	return false
}

func applyInstallDefaults(f *box.Flags, mint bool) {
	prov, perr := registry.ByName(f.Provider)
	if perr != nil {
		f.AccessMode = "ssh-root-proxmox"
	} else {
		f.Provider = prov.Name()
		f.AccessMode = prov.AccessMode()
		f.HostsGateway = prov.HostsGateway()
		// The provider's substrate defaults fill what the operator/flags
		// left unset (vultr: the dir storage; proxmox: local-lvm/vmbr0).
		for k, v := range prov.Defaults() {
			switch k {
			case "storage":
				if f.StorageName == "" {
					f.StorageName = v
				}
			case "bridge":
				if f.Bridge == "" {
					f.Bridge = v
				}
			case "relay_gw":
				if f.RelayGw == "" {
					f.RelayGw = v
				}
			}
		}
	}
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
	// The gateway is FORCED on mint: the subnet is derived in the PIPELINE
	// (RunBootstrap — the collision probe needs the host SSH, live-verified:
	// two boxes on one LAN both derived 10.77.0.0/24 and their guests
	// ARP-collided); --gateway-cidr/--gateway-vlan override, and a re-adopt's
	// recorded gateway was seeded before this runs. RunBootstrap re-validates
	// before any config write.
	f.Mint = mint
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
	if v, _ := cmd.Flags().GetString("provider"); v != "" {
		f.Provider = v
	}
	if ha, _ := cmd.Flags().GetStringArray("host-answer"); len(ha) > 0 {
		f.HostAnswers = map[string]string{}
		for _, a := range ha {
			name, val, ok := strings.Cut(a, "=")
			if !ok || name == "" || val == "" {
				continue
			}
			f.HostAnswers[name] = val
		}
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
	cmd.Flags().Bool("confirm-storage", false, "Operator consent to CREATE a storage backend when none is detected")
	cmd.Flags().String("channel", "", "Release channel to record on the CP (stable|dev; default: derived from the build). Install deploys the LOCAL build; it does not fetch")
	cmd.Flags().String("version", "", "Version to record on the CP (default: this build's version). Install deploys the LOCAL build; it does not fetch")
	cmd.Flags().Bool("non-interactive", false, "Run headless: fail actionably instead of prompting")
	cmd.Flags().String("provider", "", "The host provider (registry key: "+strings.Join(registry.Names(), " | ")+") — a created-host provider mints the world's host; the default (proxmox) reaches yours")
	cmd.Flags().StringArray("host-answer", nil, "A provider need's non-secret answer, name=value (repeatable; secrets ride their env var; defaults come from the provider)")
}

// ---- the host-provider lifecycle (generic; no substrate names) --------------

// missingAnswers fills the HEADLESS answer set from the env + --host-answer
// + the provider's own defaults, and returns what is still missing (the
// caller either falls back to the guided flow or errors actionably). Secret
// needs ride their env var; plain needs take --host-answer, then the
// provider default; the conventional core needs ride the core flags.
func missingAnswers(prov provisioning.HostProvider, f *box.Flags) []string {
	var missing []string
	if f.HostAnswers == nil {
		f.HostAnswers = map[string]string{}
	}
	if f.HostSecrets == nil {
		f.HostSecrets = map[string]string{}
	}
	for _, need := range prov.Needs() {
		switch need.Name {
		case provisioning.NeedHost:
			if f.Host == "" {
				missing = append(missing, "--host (or --host-answer host=…)")
			}
		case provisioning.NeedProxyIP:
			if f.ProxyIP == "" {
				missing = append(missing, "--proxy-ip (or --host-answer proxy_ip=…)")
			}
		case provisioning.NeedConfirmStorage:
			// Optional consent; unset is a valid answer.
		default:
			if need.Secret {
				if v := os.Getenv(need.Name); v != "" {
					f.HostSecrets[need.Name] = v
				} else {
					missing = append(missing, need.Name+" (env)")
				}
				continue
			}
			if f.HostAnswers[need.Name] == "" && need.Default != "" {
				f.HostAnswers[need.Name] = need.Default
			}
		}
	}
	return missing
}

// doorPubkeyLine renders this box's DOOR key's authorized_keys line — the
// key the provider's host must carry (born-with for a created host, pasted
// for a reached one).
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

// tempDoorKey writes the DOOR private key to a 0600 temp file.
func tempDoorKey() (string, func(), error) {
	pem, err := box.DoorKeyPEM()
	if err != nil {
		return "", nil, err
	}
	return proxmox.WriteTempKey(pem)
}

// buildSession assembles the provider's provisioning handle: the answers
// (secrets included — memory only), the door line, and the installer-owned
// transports. ExecOnHost dials the session's CURRENT host over the DOOR key
// — a created host sets session.Host before its install scripts run.
func buildSession(f *box.Flags, ui *installerUI) (*provisioning.HostSession, error) {
	line, err := doorPubkeyLine()
	if err != nil {
		return nil, fmt.Errorf("door key: %w", err)
	}
	answers := map[string]string{}
	for k, v := range f.HostAnswers {
		answers[k] = v
	}
	for k, v := range f.HostSecrets {
		answers[k] = v
	}
	session := &provisioning.HostSession{
		Answers:  answers,
		DoorLine: line,
		Host:     f.Host,
		World:    f.Name,
	}
	session.ExecOnHost = func(script string, timeoutSecs uint64) error {
		ip := strings.TrimPrefix(session.Host, "root@")
		keyPath, cleanup, err := tempDoorKey()
		if err != nil {
			return err
		}
		defer cleanup()
		out, err := proxmox.SSHExec(ip, keyPath)(script, timeoutSecs)
		if err != nil {
			return err
		}
		if out.ExitCode == nil || *out.ExitCode != 0 {
			return fmt.Errorf("host script failed: %s", strings.TrimSpace(out.Stderr))
		}
		return nil
	}
	session.Print = func(format string, args ...any) { fmt.Fprintf(uiOut(ui), format, args...) }
	if ui != nil {
		session.Prompt = ui.Prompt
		session.Interactive = true
	}
	return session, nil
}

// uiOut resolves the writer behind a possibly-nil ui (headless keeps the
// provider's prints flowing).
func uiOut(ui *installerUI) io.Writer {
	if ui == nil {
		return os.Stdout
	}
	return ui.out
}

// ensureHost runs the provider's Prepare: a mint creates the host (and
// installs what it needs); a re-adopt verifies or re-creates the recorded
// one. Fills Host/ProxyIP/HostID from the answer, and records the handle
// IMMEDIATELY when one is created — a mid-prepare failure must never
// strand a billed host the tooling cannot see.
func ensureHost(f *box.Flags, out io.Writer, ui *installerUI) (*provisioning.HostSession, error) {
	prov, err := registry.ByName(f.Provider)
	if err != nil {
		return nil, err
	}
	if f.HostSecrets == nil {
		f.HostSecrets = map[string]string{}
	}
	// Fill the headless answer set (env + --host-answer + defaults); the
	// missing list is the caller's gate — here every answer is already in.
	_ = missingAnswers(prov, f)
	session, err := buildSession(f, ui)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	host, err := prov.Prepare(ctx, session, f.HostID)
	if err != nil {
		// A post-create failure (the address wait, the PVE install) still
		// leaves a BILLED instance — the session carries its handle the
		// moment the create succeeded; record it before surfacing the
		// failure, so the stranded host is always recoverable by tooling.
		if session.CreatedID != "" {
			f.Host = session.Host
			f.HostID = session.CreatedID
			if rerr := recordHostHandle(f, &provisioning.Host{ID: session.CreatedID}, out); rerr != nil {
				return session, fmt.Errorf("%w\n  (AND the handle could not be recorded: %v — note the id %s by hand)", err, rerr, session.CreatedID)
			}
			return session, fmt.Errorf("%w\n  (the instance's handle is recorded in the profile — a re-run re-adopts it instead of minting a second one)", err)
		}
		return session, err
	}
	if host == nil {
		return nil, fmt.Errorf("provider %q prepared no host", prov.Name())
	}
	f.Host = session.Host
	f.HostID = host.ID
	if !needPresent(prov, provisioning.NeedProxyIP) && host.IP != "" {
		// A created host DERIVES the edge address (the instance's IP) —
		// there is no LAN to name.
		f.ProxyIP = host.IP + "/32"
	}
	if host.ID != "" {
		if err := recordHostHandle(f, host, out); err != nil {
			return session, fmt.Errorf("host %s is LIVE AND BILLING but its handle could not be recorded to %s (%w) — record it by hand before re-running, or destroy it in the provider's console", host.ID, f.ConfigPath, err)
		}
	}
	return session, nil
}

// recordHostHandle persists the [host_provider] record (and, once known,
// the host address) into the profile config without disturbing anything
// else on disk — the pipeline's own merges keep prev's fields; this only
// ever fills the host block.
func recordHostHandle(f *box.Flags, host *provisioning.Host, out io.Writer) error {
	cfg, err := config.Load(f.ConfigPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &config.Config{Name: f.Name, AccessMode: f.AccessMode}
	}
	cfg.AccessMode = f.AccessMode
	cfg.HostProvider = config.HostSpec{
		Provider: f.Provider,
		ID:       host.ID,
		Gateway:  f.HostsGateway,
		Answers:  f.HostAnswers,
	}
	if f.Host != "" {
		cfg.Host = f.Host
	}
	if err := cfg.Save(f.ConfigPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "  recorded: %s host %s (%s)\n", f.Provider, host.ID, f.Host)
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

// Prompt prints a label and reads a line — the provider-facing primitive
// (the door paste-gate's "press ENTER / 'r' / 'q" loop). No default, no
// re-ask; the caller interprets the answer.
func (u *installerUI) Prompt(label string) (string, error) {
	fmt.Fprintf(u.out, "%s: ", label)
	line, err := u.readLine()
	if err != nil {
		return "", err
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
