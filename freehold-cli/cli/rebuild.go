// The operator CLI's build surface: `freehold build` (a thin trigger that
// drives the CP's world bring-up through its co-located runner) + DNS
// credential collection + the local config cache. The provisioning ENGINE
// (LXC boot, storage plane, deploy-cp, the CP bootstrap) is shared in
// platform/provisioning/box; the agent org, world facts, and DNS A-record
// management are CP-owned (cpbuild). CP creation (`freehold install`) lives in
// this module's install package.
package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/crypto"
	oplogin "freehold/freehold-cli/cli/login"
	"freehold/platform/provisioning/box"
	"freehold/platform/provisioning/stages"
	"freehold/platform/services/certificates/letsencrypt"
	"freehold/providers/proxmox"
	"freehold/providers/proxmox/drive"
)

// buildEngine wraps the shared provisioning engine with the build-side
// (agent-tools / DNS / litellm) stages that live in the operator module.
type buildEngine struct {
	*box.Engine
}

// newBuildEngine resolves the sibling binaries and builds the engine.
func newBuildEngine(f box.Flags) (*buildEngine, error) {
	bins, err := box.ResolveBins()
	if err != nil {
		return nil, err
	}
	eng, err := box.NewEngine(f, bins)
	if err != nil {
		return nil, err
	}
	eng.Provider = proxmox.New(eng.HostExecFunc())
	return &buildEngine{eng}, nil
}

var buildCmd = &cobra.Command{Use: "build",
	Short: "Bring the whole world up through the CP: login-gated trigger of the console's /api/world-build (the CP owns relay/agent-tools/k3s/storage/DNS/litellm/caddy/cert through its co-located runner)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := negotiateProfile(cmd, "build")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
		}
		eng, err := setupBuild(cmd)
		if err != nil {
			return err
		}
		return eng.runBuild()
	},
}

func init() {
	registerBuildFlags(buildCmd)
}

func registerBuildFlags(cmd *cobra.Command) {
	cmd.Flags().String("addr", "127.0.0.1:8787", "Runner MCP address (loopback)")
	cmd.Flags().String("target", "proxmox-box", "Runner name (the package + grant + target name)")
	cmd.Flags().String("host", "root@192.168.30.224", "Proxmox host address the runner SSH's into")
	cmd.Flags().String("domain", "", "DEPRECATED - use --relay-domain. Kept for old scripts.")
	cmd.Flags().String("relay-domain", "", "The RELAY's own public host (its Buzz origin) — REQUIRED, never derived")
	cmd.Flags().String("cp-domain", "", "The CONTROL PLANE's public host — REQUIRED, never derived")
	cmd.Flags().String("operator-pubkey", "", "Operator Nostr pubkey (64-hex) — console admin + relay owner (REQUIRED)")
	cmd.Flags().String("operator-identity", "", "Operator identity dir to record in the config (optional)")
	cmd.Flags().String("agent-name", "freehold", "The CPA's display name in Buzz (default 'freehold')")
	cmd.Flags().Uint64("size-gb", drive.TenantLVSizeGB, "Per-tenant thin LV size in GiB (LVM-thin backend)")
	cmd.Flags().Uint64("pool-size-gb", drive.FreshPoolSizeGB, "Thin-pool size in GiB when a NEW pool is carved")
	cmd.Flags().String("thin-pool", "", "Plane placement: the thin pool the tenant LVs land in")
	cmd.Flags().Bool("no-k3s", false, "Opt-out: do NOT boot/install the k3s substrate LXC")
	cmd.Flags().Uint32("rootfs-gb", 16, "LXC rootfs size in GB")
	cmd.Flags().Uint32("memory-mb", 2048, "LXC memory in MB")
	cmd.Flags().String("relay-gw", "192.168.30.1", "Gateway for the proxy's STATIC guest IP (unused with DHCP)")
	cmd.Flags().String("storage", "local-lvm", "PVE LXC storage (relay/k3s boots)")
	cmd.Flags().String("bridge", "vmbr0", "PVE LXC network bridge (relay/k3s boots)")
	cmd.Flags().Bool("no-litellm", false, "Opt-out: do NOT deploy the litellm gateway")
	cmd.Flags().String("litellm-provider-key", "", "Fireworks/upstream provider API key for litellm's model")
	cmd.Flags().String("proxy-ip", "", "STATIC proxy (Caddy/k3s node) IP (CIDR, e.g. 192.168.30.7/24)")
	cmd.Flags().String("relay-ip", "", "STATIC relay LXC IP (CIDR)")
	cmd.Flags().String("cp-ip", "", "STATIC CP LXC IP (CIDR)")
	cmd.Flags().String("config", config.DefaultPath(), "Config path (default: ~/.config/freehold/config.toml)")
	cmd.Flags().Bool("confirm-storage", false, "Operator consent to CREATE a storage backend when none is detected")
	cmd.Flags().Bool("yes", false, "Non-interactive: bail (actionably) where the interactive pipeline would prompt")
	cmd.Flags().Bool("reset-dns", false, "Forget any stored DNS provider credentials so the build prompts for them again")
	cmd.Flags().Bool("manage-dns", false, "Opt-in: freehold MANAGEs the world's DNS")
}

func setupBuild(cmd *cobra.Command) (*buildEngine, error) {
	f := box.Flags{}
	f.Addr, _ = cmd.Flags().GetString("addr")
	f.Target, _ = cmd.Flags().GetString("target")
	f.Host, _ = cmd.Flags().GetString("host")
	f.Domain, _ = cmd.Flags().GetString("domain")
	f.RelayDomain, _ = cmd.Flags().GetString("relay-domain")
	f.CpDomain, _ = cmd.Flags().GetString("cp-domain")
	f.OperatorPubkey, _ = cmd.Flags().GetString("operator-pubkey")
	f.OperatorIdentity, _ = cmd.Flags().GetString("operator-identity")
	f.AgentName, _ = cmd.Flags().GetString("agent-name")
	f.SizeGB, _ = cmd.Flags().GetUint64("size-gb")
	f.PoolSizeGB, _ = cmd.Flags().GetUint64("pool-size-gb")
	f.ThinPool, _ = cmd.Flags().GetString("thin-pool")
	f.NoK3s, _ = cmd.Flags().GetBool("no-k3s")
	f.NoLitellm, _ = cmd.Flags().GetBool("no-litellm")
	f.LitellmProviderKey = os.Getenv("FREEHOLD_LITELLM_PROVIDER_KEY")
	if v, _ := cmd.Flags().GetString("litellm-provider-key"); v != "" {
		f.LitellmProviderKey = v
	}
	f.RootfsGB, _ = cmd.Flags().GetUint32("rootfs-gb")
	f.MemoryMB, _ = cmd.Flags().GetUint32("memory-mb")
	f.RelayGw, _ = cmd.Flags().GetString("relay-gw")
	f.StorageName, _ = cmd.Flags().GetString("storage")
	f.Bridge, _ = cmd.Flags().GetString("bridge")
	f.ProxyIP, _ = cmd.Flags().GetString("proxy-ip")
	f.RelayIP, _ = cmd.Flags().GetString("relay-ip")
	f.CpIP, _ = cmd.Flags().GetString("cp-ip")
	f.ConfigPath = profileConfigPath(cmd)
	f.ConfirmStorage, _ = cmd.Flags().GetBool("confirm-storage")
	f.Yes, _ = cmd.Flags().GetBool("yes")
	f.ResetDNS, _ = cmd.Flags().GetBool("reset-dns")
	f.ManageDNS, _ = cmd.Flags().GetBool("manage-dns")
	f.ManageDNSExplicit = cmd.Flags().Changed("manage-dns")
	if err := box.ApplyConfigDefaults(&f, f.ConfigPath); err != nil {
		return nil, err
	}
	if f.ResetDNS {
		clearStoredDNSCreds()
		fmt.Fprintln(cmd.OutOrStdout(), "  (cleared stored DNS provider credentials — the build will ask for them again)")
	}
	// The --target default (proxmox-box) must not override a recorded runner: a
	// rebuild of an already-provisioned world drives ITS runner.
	if !cmd.Flags().Changed("target") {
		if cfg2, _ := config.Load(f.ConfigPath); cfg2 != nil && cfg2.Runner.Target != "" {
			f.Target = cfg2.Runner.Target
		}
	}
	return newBuildEngine(f)
}

func applyConfigDefaults(f *box.Flags, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	if f.OperatorPubkey == "" {
		f.OperatorPubkey = cfg.OperatorPubkey
	}
	if f.RelayDomain == "" {
		f.RelayDomain = cfg.RelayHost()
	}
	if f.CpDomain == "" {
		f.CpDomain = cfg.CPHost()
	}
	if f.ThinPool == "" && cfg.Plane.ThinPool != nil {
		f.ThinPool = *cfg.Plane.ThinPool
	}
	if f.AgentName == "" && cfg.CPAName != "" {
		f.AgentName = cfg.CPAName
	}
	if f.ProxyIP == "" && cfg.Proxy.Ip != nil {
		f.ProxyIP = *cfg.Proxy.Ip
	}
	if !f.ManageDNSExplicit && !f.ManageDNS && cfg.Dns.Manager != nil && cfg.Dns.Manager.Managed {
		f.ManageDNS = true
	}
	return nil
}

// dnsCredCmd stores the Caddy edge's DNS provider credential for a slot ahead
// of any build, so a headless/--yes or TUI build reuses it without prompting.
var dnsCredCmd = &cobra.Command{
	Use:   "dns-cred",
	Short: "Store (and pre-verify) a DNS provider credential for a cert slot — one-time seeding a build reuses",
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
		e := &buildEngine{Engine: newZeroEngine()}
		e.F = box.Flags{RelayDomain: domain, CpDomain: domain}
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
			fmt.Fprintf(e.Out, "  ✓ %s DNS provider credential saved (%s) — builds will reuse it\n", slot, provider)
			fmt.Fprintf(e.Out, "  stored sealed at %s\n", e.certCredPath(slot))
			return nil
		}
		// Interactive: provider picker + env collection.
		if slot == "cp" {
			e.F.RelayDomain = domain
			provider, _, err := e.promptDNSCred(slot, domain, "relay")
			if err != nil {
				return err
			}
			fmt.Fprintf(e.Out, "  ✓ %s DNS provider credential saved (%s) — builds will reuse it\n", slot, provider)
			return nil
		}
		provider, _, err := e.promptDNSCred("relay", domain, "")
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "  ✓ relay DNS provider credential saved (%s) — builds will reuse it\n", provider)
		fmt.Fprintf(e.Out, "  stored sealed at %s\n", e.certCredPath("relay"))
		return nil
	},
}

func init() {
	dnsCredCmd.Flags().String("domain", "", "Host the pre-verify targets (default: relay host from the recorded config, or the cp host with --slot cp)")
	dnsCredCmd.Flags().String("slot", "relay", "Credential slot: relay | cp")
	dnsCredCmd.Flags().String("provider", "", "DNS provider name (lego registry) — omit for the interactive picker")
	dnsCredCmd.Flags().String("env", "", "Provider env as KEY=VAL,KEY=VAL (omit/empty for auto-detecting providers like route53)")
	dnsCredCmd.Flags().String("config", config.DefaultPath(), "Config path to read the host from")
}

// newZeroEngine returns an engine without sibling resolution (for standalone
// commands like dns-cred that don't exec sibling binaries), defaulting seams.
func newZeroEngine() *box.Engine {
	return &box.Engine{Out: os.Stdout, In: os.Stdin}
}

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

func (e *buildEngine) certCredPath(slot string) string {
	return filepath.Join(box.StateDir(), "dns-provider-"+slot+".json")
}

func (e *buildEngine) certIdent() (*box.Identity, []byte, []byte, error) {
	id, err := box.LoadIdentity(box.OpsDir())
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

func clearStoredDNSCreds() {
	dir := box.StateDir()
	for _, name := range []string{"dns-provider-relay.json", "dns-provider-cp.json", "dns-provider.json"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// consoleEncPubkey returns the console identity's encryption public key — the
// recipient the CP-owned secrets are sealed to. It reads it from the console's
// public /api/world (so a THIN box needs no runner to exec into the CP); an
// older CP that predates the field falls back to the pct-exec readback.
func (e *buildEngine) consoleEncPubkey(client *console.Client) ([]byte, error) {
	if w, err := client.World(); err == nil && w.ConsoleEncPubkey != "" {
		pk, derr := hex.DecodeString(w.ConsoleEncPubkey)
		if derr != nil || len(pk) != 32 {
			return nil, fmt.Errorf("console enc pubkey from /api/world not 32-byte hex: %q", w.ConsoleEncPubkey)
		}
		return pk, nil
	}
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil || cfg == nil || cfg.Lxc.Cp.Vmid == nil {
		return nil, fmt.Errorf("no cp coords for the console hand-off")
	}
	ok, out := e.RunBin(e.Bins.Self, e.ExecArgs(fmt.Sprintf(
		"pct exec %d -- sh -c '/srv/data/cp/bin/freehold-console identity --state-dir /srv/data/cp/control-plane --enc-pubkey'",
		*cfg.Lxc.Cp.Vmid), 30))
	if !ok {
		return nil, fmt.Errorf("console enc pubkey unreadable in the cp LXC:\n%s", out)
	}
	pk := strings.TrimSpace(out)
	if len(pk) != 64 {
		return nil, fmt.Errorf("console enc pubkey readback not 64-hex: %q", pk)
	}
	return hex.DecodeString(pk)
}

func cpSecretBlob(name, provider string, env map[string]string, seal cert.Sealer, pub []byte) (json.RawMessage, error) {
	p := filepath.Join(os.TempDir(), "fh-cp-"+name+".json")
	if err := cert.SaveCreds(p, provider, env, seal, pub, name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	_ = os.Remove(p)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (e *buildEngine) ensureCpSecrets(client *console.Client, cfg *config.Config) error {
	if !e.WorldHasEdge() {
		return nil // no TLS edge => no DNS creds or litellm needed
	}
	present, err := client.SecretNames()
	if err != nil {
		return fmt.Errorf("read CP secret inventory: %w", err)
	}
	have := map[string]bool{}
	for _, n := range present {
		have[n] = true
	}
	pub, err := e.consoleEncPubkey(client)
	if err != nil {
		return err
	}
	seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }

	for _, slot := range []string{"relay", "cp"} {
		name := "dns-" + slot
		if have[name] {
			continue
		}
		host := e.F.RelayDomain
		reuseFrom := ""
		if slot == "cp" {
			host = e.F.CpDomain
			// The CP host is almost always the same zone as the relay, so offer
			// to reuse the relay credential instead of asking twice.
			reuseFrom = "relay"
		}
		provider, env, err := e.promptDNSCred(slot, host, reuseFrom)
		if err != nil {
			return fmt.Errorf("%s DNS provider credential: %w", slot, err)
		}
		blob, err := cpSecretBlob(name, provider, env, seal, pub)
		if err != nil {
			return err
		}
		if err := client.PutSecret(name, blob); err != nil {
			return fmt.Errorf("seed %s on CP: %w", name, err)
		}
		fmt.Fprintf(e.Out, "  · %s DNS credential stored on the CP\n", slot)
	}

	if !have["litellm"] {
		master, pg, providerKey, err := e.litellmSecretMaterial(cfg)
		if err != nil {
			return err
		}
		blob, err := cpSecretBlob("litellm", "litellm", map[string]string{
			"master": master, "pg": pg, "provider": providerKey,
		}, seal, pub)
		if err != nil {
			return err
		}
		if err := client.PutSecret("litellm", blob); err != nil {
			return fmt.Errorf("seed litellm on CP: %w", err)
		}
		fmt.Fprintln(e.Out, "  · litellm secrets stored on the CP")
		// The co-located runner is re-seeded + restarted CP-side by the
		// world-build (cpbuild.reseedCoLocatedRunner) from this durable store, so
		// the box does NOT need a runner (or an exec into the CP) here.
	}
	return nil
}

func (e *buildEngine) litellmMasterKey(k3sVmid uint32) string {
	cmd := fmt.Sprintf(`pct exec %d -- /usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get secret litellm-keys -n litellm -o jsonpath='{.data.master-key}' 2>/dev/null | base64 -d`,
		k3sVmid)
	ok, out := e.RunBin(e.Bins.Self, e.ExecArgs(cmd, 30))
	if !ok {
		return "" // a read failure is treated as "no canonical master yet" — a fresh run mints one
	}
	return strings.TrimSpace(out)
}

func (e *buildEngine) litellmPostgresPw(k3sVmid uint32) string {
	cmd := fmt.Sprintf(`pct exec %d -- /usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get secret litellm-pg -n litellm -o jsonpath='{.data.postgres-pw}' 2>/dev/null | base64 -d`,
		k3sVmid)
	ok, out := e.RunBin(e.Bins.Self, e.ExecArgs(cmd, 30))
	if !ok {
		return ""
	}
	return strings.TrimSpace(out)
}

// promptSecret reads a credential value WITHOUT echo when stdin is a terminal,
// so secrets like the DNS API token and the litellm provider key are not
// echoed into the screen/scrollback. Falls back to the ordinary prompt for a
// non-terminal (piped) stdin.
func (e *buildEngine) promptSecret(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return e.Prompt(label)
	}
	fmt.Fprintf(e.Out, "%s: ", label)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(e.Out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (e *buildEngine) litellmSecretMaterial(cfg *config.Config) (string, string, string, error) {
	k3sVmid := uint32(0)
	if cfg != nil && cfg.Lxc.K3s.Vmid != nil {
		k3sVmid = *cfg.Lxc.K3s.Vmid
	}
	masterKey := ""
	if k3sVmid != 0 {
		masterKey = e.litellmMasterKey(k3sVmid)
	}
	if masterKey == "" {
		masterKey = stages.GenSecretHex()
	}
	postgresPw := stages.GenSecretHex()
	if k3sVmid != 0 {
		if pw := e.litellmPostgresPw(k3sVmid); pw != "" {
			postgresPw = pw // reuse the canonical password (first-run-wins)
		}
	}
	providerKey := e.F.LitellmProviderKey
	if providerKey == "" {
		if e.F.Yes {
			return "", "", "", fmt.Errorf("litellm needs the provider key: --litellm-provider-key or FREEHOLD_LITELLM_PROVIDER_KEY (headless --yes)")
		}
		answer, err := e.promptSecret("litellm first provision: the provider (fireworks) API key")
		if err != nil {
			return "", "", "", err
		}
		providerKey = strings.TrimSpace(answer)
		if providerKey == "" {
			return "", "", "", fmt.Errorf("no litellm provider key supplied")
		}
	}
	return masterKey, postgresPw, providerKey, nil
}

func (e *buildEngine) promptDNSCred(slot, host, reuseFrom string) (string, map[string]string, error) {
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
		if legacy := filepath.Join(box.StateDir(), "dns-provider.json"); cert.CredExists(legacy) {
			if provider, env, lerr := cert.LoadCreds(legacy, open, secret); lerr == nil {
				if serr := cert.SaveCreds(path, provider, env, seal, pub, "cert-dns-"+slot); serr == nil {
					fmt.Fprintf(e.Out, "  · migrated your stored DNS credential (%s) into the %s slot\n", provider, slot)
				}
			}
		}
	}

	if cert.CredExists(path) {
		provider, env, err := cert.LoadCreds(path, open, secret)
		if err != nil {
			return "", nil, fmt.Errorf("reusing %s DNS credential: %w", slot, err)
		}
		fmt.Fprintf(e.Out, "  · reusing sealed %s DNS credential (%s)\n", slot, provider)
		return provider, env, nil
	}

	if reuseFrom != "" {
		fromPath := e.certCredPath(reuseFrom)
		if cert.CredExists(fromPath) {
			reuse := e.F.Yes // headless: auto-copy the shared credential
			if !e.F.Yes {
				ans, err := e.Prompt(fmt.Sprintf("%s has no DNS credential — reuse the %s one? (y/n)", slot, reuseFrom))
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
				fmt.Fprintf(e.Out, "  · %s reuses the %s DNS credential (%s) — copies kept separate\n", slot, reuseFrom, provider)
				return provider, env, nil
			}
		}
	}

	if e.F.Yes {
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
		fmt.Fprintf(e.Out, "  · pre-verifying %s credentials (throwaway TXT round-trip)…\n", provider)
		if err := cert.Verify(host, provider, env); err != nil {
			return "", nil, fmt.Errorf("DNS provider pre-verify failed — fix the credential and try again: %w", err)
		}
	}
	if err := cert.SaveCreds(path, provider, env, seal, pub, "cert-dns-"+slot); err != nil {
		return "", nil, fmt.Errorf("storing DNS credential: %w", err)
	}
	return provider, env, nil
}

func (e *buildEngine) promptProvider() (string, error) {
	return runProviderPicker(cert.Providers())
}

func (e *buildEngine) promptProviderEnv(provider string) (map[string]string, error) {
	names := cert.ProviderEnvNames(provider)
	env := map[string]string{}
	if len(names) == 0 {
		fmt.Fprintln(e.Out, "  this provider has no enumerated env fields — paste KEY=VAL entries (one per line; empty line to finish):")
		for {
			line, err := e.Prompt("KEY=VAL (or blank to finish)")
			if err != nil {
				return nil, err
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) == "" {
				fmt.Fprintln(e.Out, "  expected KEY=VAL")
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
	fmt.Fprintln(e.Out, "  enter the REQUIRED credential fields:")
	for _, n := range requiredSet {
		v, err := e.promptSecret(n + " (required)")
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
		fmt.Fprintln(e.Out, "  optional fields (blank = unset):")
		for _, n := range rest {
			v, err := e.promptSecret(n + " (optional)")
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

func (e *buildEngine) runBuild() error {
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil || cfg.CPURL == "" {
		return fmt.Errorf("no CP configured — run `freehold bootstrap` first (an operator box only), then `freehold login` here")
	}
	secStr, err := oplogin.SecretHex()
	if err != nil {
		return fmt.Errorf("no operator identity — run `freehold login` first: %v", err)
	}
	key, err := oplogin.NsecToSecret(secStr)
	if err != nil {
		return fmt.Errorf("operator secret invalid: %v", err)
	}
	// The console is reachable at its LAN IP BEFORE the world's Caddy edge
	// (k3s) exists — the very thing build brings up. Use the recorded cp IP
	// when present (box one post-bootstrap / a LAN box); otherwise fall back
	// to the public CP URL (a remote box, world already up).
	loginURL := cfg.CPURL
	if ip := config.LxcIP(cfg.Lxc.Cp); ip != "" {
		loginURL = "http://" + ip + ":8080"
	}
	client, err := oplogin.Login(loginURL, key)
	if err != nil {
		return fmt.Errorf("console login at %s: %v", loginURL, err)
	}

	// Seed the CP-owned secrets (DNS creds + litellm) idempotently: ask the
	// operator only for what the CP doesn't already hold, then the world-build
	// uses the CP as the durable secret owner (bootstrap collects none).
	if err := e.ensureCpSecrets(client, cfg); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "building world %s through the CP…\n", cfg.RelayHost())
	report, err := client.WorldBuild()
	if err != nil {
		return fmt.Errorf("world_build (console /api/world-build): %v", err)
	}
	fmt.Fprintf(e.Out, "CP world_build report:\n%s\n", report)

	// The CP owns the world coords, the agent org, and world facts now — the
	// box is a uniform thin trigger (no local runner/operator identity needed
	// for bookkeeping). It only caches what it can resolve locally.
	if err := e.FinalSave(); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "  ✓ wrote config %s\n", e.F.ConfigPath)
	fmt.Fprintf(e.Out, `
  ╭─────────────────────────────────────────────────────────╮
  │                    Freehold is up                      │
  ╰─────────────────────────────────────────────────────────╯

  relay:          https://%s
  control plane:  %s
  operator pk:    %s
  (every box drives the world through the CP; a fresh box only needs
   `+"`freehold login`"+` then `+"`freehold build`"+`)
`, cfg.RelayHost(), cfg.CPURL, e.F.OperatorPubkey)
	return nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
