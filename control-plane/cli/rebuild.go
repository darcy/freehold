// The operator CLI's build surface: `freehold build` (drive the CP's world
// bring-up through its co-located runner) + the box-side bookkeeping + DNS
// credential management. The provisioning ENGINE (LXC boot, storage plane,
// deploy-cp, the CP bootstrap) is shared in platform/provisioning/box; this
// file is the build-side orchestration ON TOP of it (box.Engine), plus the
// agent-tools-facing stages that must stay in the operator module (stageCpa,
// reconcile, register world facts). CP creation (bootstrap/install) moved out
// to the top-level `install` module / freehold-install binary.
package cli

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/state"
	"freehold/agents"
	"freehold/contract/crypto"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/cli/flows"
	oplogin "freehold/control-plane/cli/login"
	"freehold/platform/provisioning/box"
	"freehold/platform/provisioning/drive"
	"freehold/platform/provisioning/stages"
	"freehold/platform/services/certificates/letsencrypt"
	"freehold/platform/services/externaldns/cloudflare"
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

func (e *buildEngine) agentToolsMcp(cfg *config.Config) (*client.McpClient, error) {
	if cfg == nil || cfg.AgentToolsURL == "" || cfg.AgentToolsPubkey == "" {
		return nil, fmt.Errorf("no freehold-agent-tools coords recorded — deploy the agent-tools stage first")
	}
	auth, err := flows.AgentAuth(box.OpsDir())
	if err != nil {
		return nil, err
	}
	return client.New(client.ConnectURL(cfg.AgentToolsURL), auth, cfg.AgentToolsPubkey)
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

func (e *buildEngine) consoleEncPubkey() ([]byte, error) {
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
	pub, err := e.consoleEncPubkey()
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
		if slot == "cp" {
			host = e.F.CpDomain
		}
		provider, env, err := e.promptDNSCred(slot, host, "")
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
		// The co-located runner holds its keyring in memory from boot, so it must
		// be re-seeded + restarted to serve $LITELLM/$POSTGRES_PW/$PROVIDER_KEY to
		// the world-build's litellm/model steps. Seed its package + restart it.
		if err := e.seedCpRunnerSecrets(cfg, master, pg, providerKey); err != nil {
			return err
		}
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
		answer, err := e.Prompt("litellm first provision: the provider (fireworks) API key")
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

func (e *buildEngine) manageDomainDNS() error {
	ip := config.StripCIDR(e.F.ProxyIP)
	provider, env, err := e.promptDNSCred("relay", e.F.RelayDomain, "")
	if err != nil {
		return fmt.Errorf("relay DNS provider credential (for management): %w", err)
	}
	if provider != "cloudflare" {
		return fmt.Errorf(
			"freehold can manage DNS on Cloudflare only right now, but the relay credential uses %q — run `freehold dns-cred --provider cloudflare --domain %s` (or pick No for manual DNS)",
			provider, e.F.RelayDomain)
	}
	m, err := dnsman.For(provider, env)
	if err != nil {
		return err
	}
	for _, host := range []string{e.F.RelayDomain, e.F.CpDomain} {
		if host == "" {
			continue
		}
		if err := m.UpsertA(host, ip); err != nil {
			return fmt.Errorf("manage DNS for %s: %w", host, err)
		}
	}
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return err
	}
	if cfg != nil {
		cfg.Dns.Manager = &config.DnsManager{Provider: provider, Managed: true, IP: ip}
		if err := cfg.Save(e.F.ConfigPath); err != nil {
			return err
		}
	}
	return nil
}

func (e *buildEngine) opsAgentPubkey() string {
	id, err := box.LoadIdentity(box.OpsDir())
	if err != nil {
		return ""
	}
	pk, _ := id.NostrPubkeyHex()
	return pk
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
		v, err := e.Prompt(n + " (required)")
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
			v, err := e.Prompt(n + " (optional)")
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

func (e *buildEngine) reconcileCreatedAgents() error {
	cfg, err := config.Load(e.F.ConfigPath)
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
	mc, err := worldMcp(cfg)
	if err != nil {
		return err
	}
	listText, err := callAgentToolsText(mc, "manage_agent", map[string]interface{}{})
	if err != nil {
		return fmt.Errorf("list agents over agent-tools: %w", err)
	}
	var list []console.AgentInfo
	if err := json.Unmarshal([]byte(listText), &list); err != nil {
		return fmt.Errorf("parse agent list: %w: %s", err, listText)
	}
	for _, a := range list {
		if a.Name == "" || a.Name == cpaName {
			continue
		}
		// A reserved department name re-derives its fixed channels (#freehold +
		// its private #<name>); any other agent rejoins the full list the registry
		// preserved (falling back to the single recorded channel for rows written
		// before Channels was persisted).
		channels := agents.DepartmentChannels(a.Name)
		private := channels != nil
		if !private {
			channels = a.Channels
			private = a.Private
			if len(channels) == 0 && strings.TrimSpace(a.Channel) != "" {
				channels = []string{a.Channel}
			}
		}
		// create_agent is idempotent (the CP-durable identity is reused), so
		// re-creating reseats the pod with the same pubkey across a rebuild.
		// Thread the registry's preserved purpose through so the agent's
		// system-prompt purpose line survives, not just its identity.
		text, cerr := callAgentToolsText(mc, "create_agent", map[string]interface{}{"name": a.Name, "purpose": a.Purpose, "channels": channels, "private": private})
		if cerr != nil {
			return fmt.Errorf("reconcile created agent %s: %w", a.Name, cerr)
		}
		fmt.Fprintf(e.Out, "  · reconciled agent %s (pubkey %s)\n", a.Name, firstHex(text))
	}
	return nil
}

func (e *buildEngine) recordCpa(cpaPub, cpaName string) error {
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s", e.F.ConfigPath)
	}
	cfg.CPAName = cpaName
	if !containsStr(cfg.Managed, "cpa") {
		cfg.Managed = append(cfg.Managed, "cpa")
	}
	if err := cfg.Save(e.F.ConfigPath); err != nil {
		return err
	}
	// A5: register the CPA in the CP agent registry (the state store the
	// console reads). Non-fatal if the store isn't present yet (a rebuild
	// run may not have a full CP state); the presence dot is relay-side.
	if store, err := state.Open(box.StateDir()); err == nil {
		_ = agent.RegisterAgent(store, cpaName, cpaPub)
		_ = store.Save()
	}
	return nil
}

func (e *buildEngine) registerWorldFacts() error {
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil || cfg == nil {
		return fmt.Errorf("no config at %s", e.F.ConfigPath)
	}
	// world_register_facts is OPERATOR-scoped on the agent-tools server: the box
	// must sign as the OPERATOR (console-admin, in the toolset roster), NOT the
	// ops-agent build identity (which gets world_* denied with -32001). worldMcp
	// signs as the operator exactly like `freehold world status`, which is
	// granted.
	mc, err := worldMcp(cfg)
	if err != nil {
		return err
	}
	facts := agenttools.WorldFacts{
		Domains: agenttools.WorldDomains{
			Relay: cfg.RelayHost(),
			CP:    cfg.CPHost(),
			Proxy: config.StripCIDR(derefStrPtr(cfg.Proxy.Ip)),
		},
		Plane: agenttools.WorldPlane{
			Backend:     derefStrPtr(cfg.Plane.Backend),
			BackendKind: derefStrPtr(cfg.Plane.BackendKind),
			ThinPool:    derefStrPtr(cfg.Plane.ThinPool),
		},
	}
	// The durable tenants are all backup=1 volume mounts (the /srv/data
	// convention) — the facts carry the layout so a management box renders it.
	for tenant, mts := range cfg.Plane.Mounts {
		for _, m := range mts {
			facts.Plane.Mounts = append(facts.Plane.Mounts, agenttools.WorldPlaneMount{
				Tenant: tenant, Source: m.Source, GuestPath: m.GuestPath, Backup: true,
			})
		}
	}
	issuer := cfg.Caddy.CertIssuer
	if issuer == "" {
		issuer = "lego (DNS-01)"
	}
	for _, slot := range []struct{ slot, host string }{
		{"relay", cfg.RelayHost()}, {"cp", cfg.CPHost()},
	} {
		if slot.host == "" {
			continue
		}
		facts.Certs = append(facts.Certs, agenttools.WorldCert{
			Slot: slot.slot, Domain: slot.host, Issuer: issuer,
			Expiry: e.CertExpiry(cfg, slot.slot),
		})
	}
	if _, err := callAgentToolsText(mc, "world_register_facts", map[string]interface{}{"facts": facts}); err != nil {
		return fmt.Errorf("world-register-facts: %w", err)
	}
	fmt.Fprintln(e.Out, "  ✓ world facts registered on the CP (plane/certs/domains)")
	return nil
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
	// The DNS-manage opt-in + public A records (relay/cp <domain> -> proxy) is
	// a world bring-up concern, so it lives here now, not at bootstrap.
	if e.WorldHasEdge() {
		manage := e.F.ManageDNS || (cfg.Dns.Manager != nil && cfg.Dns.Manager.Managed)
		if !manage && !e.F.Yes {
			ans, err := e.Prompt("manage the domain's DNS? freehold can point relay/cp A records at the proxy on Cloudflare. (y/n)")
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

	fmt.Fprintf(e.Out, "building world %s through the CP…\n", cfg.RelayHost())
	report, err := client.WorldBuild()
	if err != nil {
		return fmt.Errorf("world_build (console /api/world-build): %v", err)
	}
	fmt.Fprintf(e.Out, "CP world_build report:\n%s\n", report)

	// The agent-tools AUDIENCE the box must sign to is the server's OWN identity
	// pubkey, which a --data rebuild mints FRESH (the durable identity is wiped
	// with /srv/data). The config's recorded agent_tools_pubkey goes stale, so
	// box-side agent-tools MCP calls (facts, create_agent) sign with a wrong
	// audience and VerifyRequest fails "signature does not verify". Re-read the
	// CP's live agent-tools identity now and adopt it into the config.
	if cp := cfg.Lxc.Cp.Vmid; cp != nil {
		ok, out := e.RunBin(e.Bins.Self, e.ExecArgs(fmt.Sprintf("pct exec %d -- /srv/data/cp/bin/freehold-agent-tools identity --state-dir /srv/data/cp/agent-tools", *cp), 30))
		if ok {
			pk := strings.TrimSpace(out)
			if isHex64(pk) && pk != cfg.AgentToolsPubkey {
				cfg.AgentToolsPubkey = pk
				_ = cfg.Save(e.F.ConfigPath)
				fmt.Fprintf(e.Out, "  · adopted the CP agent-tools audience %s\n", pk)
			}
		}
	}

	// Box-side bookkeeping (world coords are the same on every box) is the
	// DRIVING box's job: it drives the runner (record LXC coords) + the CP
	// toolset (facts/CPA/agent reconcile) as its ops identity. A THIN client
	// box (box two — no local runner/agent-ops) just triggered the CP-owned
	// build; the CP already holds everything, so skip with a note rather than
	// fail the build for bookkeeping the CP owns.
	owner := cfg.Runner.Addr != "" && cfg.Runner.Pubkey != ""
	if owner {
		if err := e.RecordPostWorld(); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(box.OpsDir(), "identity.json")); err == nil {
			// The CPA is the system's main touchpoint - create it FIRST (it signs
			// as the operator, which IS granted create_agent on the agent-tools
			// server), then reconcile the other created agents. World facts are
			// bookkeeping for the DATA/Certs views - never fatal: a facts failure
			// must not block the CPA/agents from being created.
			if err := e.stageCpa(); err != nil {
				return err
			}
			fmt.Fprintln(e.Out, "  ✓ CPA live in Buzz ("+e.F.AgentName+")")
			if err := e.stageDepartments(); err != nil {
				return err
			}
			if err := e.reconcileCreatedAgents(); err != nil {
				return err
			}
			if err := e.registerWorldFacts(); err != nil {
				fmt.Fprintf(e.Out, "  (WARN: world facts not registered on the CP — bookkeeping only, does not affect the world/agents: %v)\n", err)
			}
		}
	} else {
		fmt.Fprintln(e.Out, "  (thin client box: the CP already owns coords + facts + CPA/agent reconcile — skipping box-side bookkeeping)")
	}
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


func (e *buildEngine) seedCpRunnerSecrets(cfg *config.Config, master, pg, providerKey string) error {
	if cfg == nil || cfg.Lxc.Cp.Vmid == nil {
		return fmt.Errorf("no cp coords to seed the co-located runner")
	}
	cp := *cfg.Lxc.Cp.Vmid
	target := e.F.Target
	for _, s := range []struct{ n, ev, val string }{
		{"litellm", "LITELLM", master},
		{"postgres-pw", "POSTGRES_PW", pg},
		{"provider-key", "PROVIDER_KEY", providerKey},
	} {
		// base64 the value so it can never break the sh -c quoting (injection
		// safe — base64 is [A-Za-z0-9+/=] with no shell metacharacters); decode
		// inside the CP into the env, then add-secret reads --secret-env.
		b64 := base64.StdEncoding.EncodeToString([]byte(s.val))
		cmd := fmt.Sprintf("pct exec %d -- sh -c 'export %s=$(printf %%s %s | base64 -d); /srv/data/cp/bin/freehold-console add-secret %s %s --state-dir /srv/data/cp/control-plane/runner/%s --secret-env %s; true'",
			cp, s.ev, b64, target, s.n, target, s.ev)
		if ok, out := e.RunBin(e.Bins.Self, e.ExecArgs(cmd, 60)); !ok {
			return fmt.Errorf("seed co-located runner %s: %s", s.n, strings.TrimSpace(out))
		}
	}
	if ok, out := e.RunBin(e.Bins.Self, e.ExecArgs(fmt.Sprintf("pct exec %d -- systemctl restart freehold-runner", cp), 60)); !ok {
		return fmt.Errorf("restart co-located runner: %s", strings.TrimSpace(out))
	}
	fmt.Fprintln(e.Out, "  · co-located runner re-seeded with litellm secrets")
	return nil
}


func (e *buildEngine) stageCpa() error {
	cfg, err := config.Load(e.F.ConfigPath)
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
	// create_agent is OPERATOR-scoped on the agent-tools server: sign as the
	// operator (worldMcp), NOT the ops-agent build identity (which is only
	// granted on the runner/console and gets world_* / create_agent denied
	// with -32001). This is what actually creates the CPA in Buzz.
	mc, err := worldMcp(cfg)
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
	fmt.Fprintf(e.Out, "  · CPA live in Buzz (%s)\n", cpaName)
	return e.recordCpa(cpaPub, cpaName)
}

// stageDepartments installs the four fixed departments as part of the core
// build — the CPA's direct reports (see AGENTS.md "Locked model"). Service
// lifecycle is not a department: whichever agent created a service owns it.
// Each is
// created through the SAME audited create_agent the CPA itself uses, with its
// reserved prompt (selected by name in the server) and its channels: the shared
// #freehold channel plus its own PRIVATE #<department> channel, with the CPA
// added to every channel. Runs after stageCpa (the CPA must exist before it can
// be added to the channels); create_agent is idempotent, so a rebuild reseats
// each department with the same durable pubkey, and reconcileCreatedAgents also
// picks them up from the registry. Departments carry no capability tooling in
// this phase (conversation + orientation only).
func (e *buildEngine) stageDepartments() error {
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("no config at %s", e.F.ConfigPath)
	}
	// Fail loudly, never silently skip: stageCpa runs first and already requires
	// these (so a partial world would have failed there), but a silent nil here
	// would let a build look successful with the departments missing.
	if cfg.Lxc.Cp.Vmid == nil || cfg.Lxc.K3s.Vmid == nil {
		return fmt.Errorf("no cp/k3s coords recorded — the department pods need the k3s substrate")
	}
	if cfg.RelayURL == "" {
		return fmt.Errorf("no relay URL in config — departments must join the community relay")
	}
	if cfg.Litellm.URL == "" {
		return fmt.Errorf("no litellm gateway recorded — departments need a reasoning model")
	}
	mc, err := worldMcp(cfg)
	if err != nil {
		return err
	}
	for _, name := range agents.DepartmentNames() {
		purpose, _ := agents.DepartmentPurpose(name)
		args := map[string]interface{}{
			"name":     name,
			"purpose":  purpose,
			"channels": agents.DepartmentChannels(name),
			"private":  true,
		}
		text, cerr := callAgentToolsText(mc, "create_agent", args)
		if cerr != nil {
			return fmt.Errorf("create department %s: %w", name, cerr)
		}
		fmt.Fprintf(e.Out, "  · department %s live in Buzz (pubkey %s)\n", name, firstHex(text))
	}
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

func derefStrPtr(p *string) string { if p == nil { return "" }; return *p }
func derefU32(p *uint32) uint32 { if p == nil { return 0 }; return *p }
