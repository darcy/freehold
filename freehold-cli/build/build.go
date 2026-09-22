// Package build implements `freehold build` — a thin trigger that drives the
// CP's world bring-up through its co-located runner, plus the DNS credential
// and litellm secret seeding the CP owns.
package build

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/crypto"
	"freehold/freehold-cli/internal/certcred"
	"freehold/freehold-cli/internal/common"
	oplogin "freehold/freehold-cli/login"
	"freehold/platform/provisioning/box"
	"freehold/platform/provisioning/stages"
	cert "freehold/platform/services/certificates/letsencrypt"
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

// cc returns the shared DNS-credential engine bound to this build's I/O.
func (e *buildEngine) cc() *certcred.Engine {
	return &certcred.Engine{Out: e.Out, Yes: e.F.Yes, Prompt: e.Prompt}
}

var buildCmd = &cobra.Command{Use: "build",
	Short: "Bring the whole world up through the CP: login-gated trigger of the console's /api/world-build (the CP owns relay/agent-tools/k3s/storage/DNS/litellm/caddy/cert through its co-located runner)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := common.NegotiateProfile(cmd, "build")
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
	f.ConfigPath = common.ProfileConfigPath(cmd)
	f.ConfirmStorage, _ = cmd.Flags().GetBool("confirm-storage")
	f.Yes, _ = cmd.Flags().GetBool("yes")
	f.ResetDNS, _ = cmd.Flags().GetBool("reset-dns")
	f.ManageDNS, _ = cmd.Flags().GetBool("manage-dns")
	f.ManageDNSExplicit = cmd.Flags().Changed("manage-dns")
	if err := box.ApplyConfigDefaults(&f, f.ConfigPath); err != nil {
		return nil, err
	}
	if f.ResetDNS {
		certcred.ClearStoredDNSCreds()
		fmt.Fprintln(cmd.OutOrStdout(), "  (cleared stored DNS provider credentials — the build will ask for them again)")
	}
	if !cmd.Flags().Changed("target") {
		if cfg2, _ := config.Load(f.ConfigPath); cfg2 != nil && cfg2.Runner.Target != "" {
			f.Target = cfg2.Runner.Target
		}
	}
	return newBuildEngine(f)
}

func (e *buildEngine) certIdent() (*box.Identity, []byte, []byte, error) {
	return e.cc().CertIdent()
}

// consoleEncPubkey returns the console identity's encryption public key — the
// recipient the CP-owned secrets are sealed to.
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
	b, err := hex.DecodeString(pk)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("console enc pubkey readback not 64-hex: %q", pk)
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
	cc := e.cc()

	for _, slot := range []string{"relay", "cp"} {
		name := "dns-" + slot
		if have[name] {
			continue
		}
		host := e.F.RelayDomain
		reuseFrom := ""
		if slot == "cp" {
			host = e.F.CpDomain
			if z := certcred.DNSZone(e.F.RelayDomain); z != "" && z == certcred.DNSZone(e.F.CpDomain) &&
				cert.CredExists(cc.CertCredPath("relay")) {
				reuseFrom = "relay"
			}
		}
		provider, env, err := cc.PromptDNSCred(slot, host, reuseFrom)
		if err != nil {
			return fmt.Errorf("%s DNS provider credential: %w", slot, err)
		}
		blob, err := certcred.CPSecretBlob(name, provider, env, seal, pub)
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
		blob, err := certcred.CPSecretBlob("litellm", "litellm", map[string]string{
			"master": master, "pg": pg, "provider": providerKey,
		}, seal, pub)
		if err != nil {
			return err
		}
		if err := client.PutSecret("litellm", blob); err != nil {
			return fmt.Errorf("seed litellm on CP: %w", err)
		}
		fmt.Fprintln(e.Out, "  · litellm secrets stored on the CP")
	}
	return nil
}

func (e *buildEngine) litellmMasterKey(k3sVmid uint32) string {
	cmd := fmt.Sprintf(`pct exec %d -- /usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get secret litellm-keys -n litellm -o jsonpath='{.data.master-key}' 2>/dev/null | base64 -d`,
		k3sVmid)
	ok, out := e.RunBin(e.Bins.Self, e.ExecArgs(cmd, 30))
	if !ok {
		return ""
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
			postgresPw = pw
		}
	}
	providerKey := e.F.LitellmProviderKey
	if providerKey == "" {
		if e.F.Yes {
			return "", "", "", fmt.Errorf("litellm needs the provider key: --litellm-provider-key or FREEHOLD_LITELLM_PROVIDER_KEY (headless --yes)")
		}
		answer, err := e.cc().PromptSecret("litellm first provision: the provider (fireworks) API key")
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

func (e *buildEngine) runBuild() error {
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil || cfg.CPURL == "" {
		return fmt.Errorf("no CP configured — run `freehold install` first (an operator box only), then `freehold login` here")
	}
	secStr, err := oplogin.SecretHex()
	if err != nil {
		return fmt.Errorf("no operator identity — run `freehold login` first: %v", err)
	}
	key, err := oplogin.NsecToSecret(secStr)
	if err != nil {
		return fmt.Errorf("operator secret invalid: %v", err)
	}
	loginURL := cfg.CPURL
	if ip := config.LxcIP(cfg.Lxc.Cp); ip != "" {
		loginURL = "http://" + ip + ":8080"
	}
	client, err := oplogin.Login(loginURL, key)
	if err != nil {
		return fmt.Errorf("console login at %s: %v", loginURL, err)
	}
	if err := e.ensureCpSecrets(client, cfg); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "building world %s through the CP…\n", cfg.RelayHost())
	res, err := client.WorldBuild()
	if err != nil {
		return fmt.Errorf("world_build (console /api/world-build): %v", err)
	}
	fmt.Fprintf(e.Out, "CP world_build report:\n%s\n", res.Report)
	// A teardown clears this profile's recorded relay/k3s coords ("the next
	// build re-creates them"); the CP resolved the freshly-booted guests during
	// the build, so write them back BEFORE FinalSave (which preserves prev's
	// coords + derives Managed from the recorded k3s vmid). Without this the
	// next uninstall reports "never created (no vmid recorded)" and leaks them.
	if err := e.RecordCoords(res.Coords); err != nil {
		return err
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

// Command returns the build command for root registration.
func Command() *cobra.Command { return buildCmd }
