// Package build implements `freehold build` — a thin trigger that drives the
// CP's world bring-up through its co-located runner, plus the DNS credential
// and litellm secret seeding the CP owns.
package build

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	cmd.Flags().String("addr", "", "Runner MCP address (loopback; default: the config's [runner] addr, else 127.0.0.1:8787)")
	cmd.Flags().String("target", "", "Runner name (default: the config's [runner] target, else proxmox-box)")
	cmd.Flags().String("host", "", "Proxmox host address the runner SSH's into (default: the recorded one; only update --dev's binary ship needs it)")
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
	cmd.Flags().String("proxy-ip", "", "STATIC proxy (Caddy/k3s node) IP (CIDR, e.g. 192.168.30.7/24)")
	cmd.Flags().String("relay-ip", "", "STATIC relay LXC IP (CIDR)")
	cmd.Flags().String("cp-ip", "", "STATIC CP LXC IP (CIDR)")
	cmd.Flags().String("config", config.DefaultPath(), "Config path (default: ~/.config/freehold/config.toml)")
	cmd.Flags().Bool("confirm-storage", false, "Operator consent to CREATE a storage backend when none is detected")
	cmd.Flags().Bool("non-interactive", false, "Bail (actionably) where the interactive pipeline would prompt")
	cmd.Flags().Bool("reset-dns", false, "Forget any stored DNS provider credentials so the build prompts for them again")
	cmd.Flags().Bool("manage-dns", false, "Opt-in: freehold MANAGEs the world's DNS")
}

func setupBuild(cmd *cobra.Command) (*buildEngine, error) {
	f := box.Flags{}
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
	f.Yes, _ = cmd.Flags().GetBool("non-interactive")
	f.ResetDNS, _ = cmd.Flags().GetBool("reset-dns")
	f.ManageDNS, _ = cmd.Flags().GetBool("manage-dns")
	f.ManageDNSExplicit = cmd.Flags().Changed("manage-dns")
	f.Addr, _ = cmd.Flags().GetString("addr")
	f.Target, _ = cmd.Flags().GetString("target")
	if err := box.ApplyConfigDefaults(&f, f.ConfigPath); err != nil {
		return nil, err
	}
	if f.Addr == "" {
		f.Addr = "127.0.0.1:8787"
	}
	if f.Target == "" {
		f.Target = "proxmox-box"
	}
	if f.ResetDNS {
		certcred.ClearStoredDNSCreds()
		fmt.Fprintln(cmd.OutOrStdout(), "  (cleared stored DNS provider credentials — the build will ask for them again)")
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

	// The cert PRE-SEED ships whenever the box cache is fresh — and it
	// OVERWRITES (unlike the CP-owned secrets below, the box cache is this
	// record's durable owner; a renewed cert must replace the stale seed).
	// A world whose plane was destroyed then pre-seeds from it with no LE
	// order. Absent/stale caches ship nothing: the build issues as before and
	// the cache refills after. The cache is BASE-scoped and HOST-keyed (see
	// certCachePath): every profile's build on this box reads the same store,
	// so a fresh world's run leaves its certs for the next fresh run.
	if _, baseSec, _, serr := baseCacheIdent(); serr != nil {
		fmt.Fprintf(e.Out, "  · no base ops identity for the cert cache (%v) — the build will issue\n", serr)
	} else {
		open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
		for _, sl := range []struct{ slot, host string }{{"relay", e.F.RelayDomain}, {"cp", e.F.CpDomain}} {
			if sl.host == "" {
				continue
			}
			path := certCachePath(sl.host)
			if !cert.CredExists(path) {
				continue
			}
			_, env, lerr := cert.LoadCreds(path, open, baseSec)
			if lerr != nil {
				fmt.Fprintf(e.Out, "  · cert cache %s unreadable (%v) — the build will issue\n", sl.slot, lerr)
				continue
			}
			if _, ok := cert.ReuseIfValidBytes([]byte(env["fullchain"]), time.Now(), 30*24*time.Hour); !ok {
				fmt.Fprintf(e.Out, "  · cached %s cert under 30d remaining — the build will issue and refill the cache\n", sl.slot)
				continue
			}
			blob, berr := certcred.CPSecretBlob("cert-seed-"+sl.slot, "cert-cache", env, seal, pub)
			if berr != nil {
				return berr
			}
			if err := client.PutSecret("cert-seed-"+sl.slot, blob); err != nil {
				// The pre-seed is an optimization, never a gate: a CP whose
				// secret allowlist predates the cert-seed names (mixed-version
				// skew — the box updates before the CP) just issues as before.
				if strings.Contains(err.Error(), "refusing secret name") {
					fmt.Fprintf(e.Out, "  · CP predates the cert cache — the build will issue\n")
					continue
				}
				return fmt.Errorf("ship %s cert cache to CP: %w", sl.slot, err)
			}
			fmt.Fprintf(e.Out, "  · %s cert cache shipped for pre-seed\n", sl.slot)
		}
	}

	if !have["litellm"] && !e.F.NoLitellm {
		master, pg, gw, err := e.litellmSecretMaterial(cfg)
		if err != nil {
			return err
		}
		blob, err := certcred.CPSecretBlob("litellm", "litellm", map[string]string{
			"master": master, "pg": pg,
			"provider":        gw.Key,
			"provider-prefix": gw.Provider,
			"provider-model":  gw.Model,
		}, seal, pub)
		if err != nil {
			return err
		}
		if err := client.PutSecret("litellm", blob); err != nil {
			return fmt.Errorf("seed litellm on CP: %w", err)
		}
		fmt.Fprintln(e.Out, "  · litellm secrets stored on the CP")
		fmt.Fprintf(e.Out, "  · gateway provider: %s/%s (all aliases)\n", gw.Provider, gw.Model)
	}

	// The owner identity (the world's operator key, the relay's owner-role
	// member) ships sealed alongside the creds the CP owns: the CP mints every
	// agent pod's memory-plane attestation with it, and an unshipped key is a
	// world whose agents boot with no writable memory — the exact silent shape
	// this fixes. Sealed to the console identity like the rest, opened only
	// in-memory at mint time; the raw secret never appears in the report, a
	// command, or the audit. Only the NOSTR half ships: it is all the mint
	// needs, and the operator's X25519 enc key buys a reader nothing but risk.
	//
	// Seeded only when absent — a PRESENT record is never overwritten by a
	// build (the CP is the durable owner; a box-side blind overwrite could
	// clobber a deliberate rotate). When the sealed record no longer serves
	// (operator key rotated, wrong restore), ownerKey fails the create with a
	// message naming the one file to remove; the next build then re-seeds it.
	if !have["operator"] {
		opID, err := box.LoadIdentity(oplogin.Dir())
		if err != nil {
			return fmt.Errorf("read the operator identity ledger: %w (the CP needs it to attest agent memory — run `freehold login`)", err)
		}
		blob, err := certcred.CPSecretBlob("operator", "operator", map[string]string{
			"nostr": opID.NostrSecretHex,
		}, seal, pub)
		if err != nil {
			return err
		}
		if err := client.PutSecret("operator", blob); err != nil {
			if strings.Contains(err.Error(), "refusing secret name") {
				return fmt.Errorf("seed the operator identity on CP: %w — the CP predates this release (it rejects the operator secret the memory plane needs); run `freehold update` first (it ships the current console), then re-run `freehold build`", err)
			}
			return fmt.Errorf("seed the operator identity on CP: %w", err)
		}
		fmt.Fprintln(e.Out, "  · operator identity stored on the CP (attests agent memory)")
	}
	return nil
}

// certCachePath is the box-side sealed cache of a slot's issued edge cert
// (fullchain + key + host, SaveCreds-shaped), written after a successful
// build so a world whose plane was destroyed pre-seeds from it. HOST-keyed
// (the cert's SAN) and BASE-scoped (DefaultStateHome — the freehold state
// root no world lifecycle touches): a profile's uninstall --remove-data wipes
// profiles/<name>/ wholesale, so a profile-scoped cache dies with the very
// fresh run that filled it; keying by host keeps worlds sharing a box from
// clobbering each other's entries.
func certCachePath(host string) string {
	return filepath.Join(config.DefaultStateHome(), "cert-cache", host+".json")
}

// baseCacheIdent loads (minting on first use) the box's BASE ops identity —
// the state root's agent-ops, not the active profile's: the cert cache must
// be openable by every future profile's build on this box, so it seals to an
// identity no world lifecycle can wipe. No flow mints it today (login and
// install both pin a profile before their ops mint) — the FIRST fill creates
// it; a box where the mint fails degrades loudly to issuing.
func baseCacheIdent() (*box.Identity, []byte, []byte, error) {
	dir := filepath.Join(config.DefaultStateHome(), "control-plane", "agent-ops")
	if err := box.EnsureIdentity(dir); err != nil {
		return nil, nil, nil, fmt.Errorf("no base ops identity at %s: %w", dir, err)
	}
	id, err := box.LoadIdentity(dir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("no base ops identity at %s", dir)
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("base ops identity enc secret: %w", err)
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return nil, nil, nil, err
	}
	return id, secret, pub, nil
}

// cacheEdgeCerts copies each slot's issued cert off the durable mirror (read
// through the runner) into the box's sealed cache. A mirror that is missing,
// stale (<30d left), or whose leaf does not cover the slot's host (renamed
// domains) is skipped loudly — the next issue refills the cache.
func (e *buildEngine) cacheEdgeCerts() error {
	if !e.WorldHasEdge() {
		return nil
	}
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil || cfg == nil {
		return fmt.Errorf("load config: %w", err)
	}
	k3s := uint32(0)
	if cfg.Lxc.K3s.Vmid != nil {
		k3s = *cfg.Lxc.K3s.Vmid
	}
	if k3s == 0 {
		return fmt.Errorf("no k3s vmid recorded")
	}
	_, _, pub, err := baseCacheIdent()
	if err != nil {
		return err
	}
	seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
	for _, sl := range []struct{ slot, host string }{
		{"relay", e.F.RelayDomain}, {"cp", e.F.CpDomain},
	} {
		if sl.host == "" {
			continue
		}
		dir := stages.CaddyEdgeDurableDir(sl.slot)
		fc, ferr := e.readGuestFileB64(k3s, dir+"/fullchain.pem")
		key, kerr := e.readGuestFileB64(k3s, dir+"/key.pem")
		if ferr != nil || kerr != nil {
			fmt.Fprintf(e.Out, "  · no %s edge cert on the durable mirror yet — cache skips\n", sl.slot)
			continue
		}
		exp, ok := cert.ReuseIfValidBytes(fc, time.Now(), 30*24*time.Hour)
		if !ok {
			fmt.Fprintf(e.Out, "  · %s edge cert under 30d remaining — cache skips (the next issue refills it)\n", sl.slot)
			continue
		}
		names, nerr := cert.LoadDNSNames(fc)
		if nerr != nil || !cert.CoversHost(names, sl.host) {
			fmt.Fprintf(e.Out, "  · %s mirror cert does not cover %s — cache skips\n", sl.slot, sl.host)
			continue
		}
		env := map[string]string{"host": sl.host, "fullchain": string(fc), "key": string(key)}
		if err := os.MkdirAll(filepath.Dir(certCachePath(sl.host)), 0o700); err != nil {
			return err
		}
		if err := cert.SaveCreds(certCachePath(sl.host), "cert-cache", env, seal, pub, "cert-cache-"+sl.host); err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "  · cached the %s edge cert for pre-seed (valid until %s)\n", sl.slot, exp.UTC().Format("2006-01-02"))
	}
	return nil
}

// readGuestFileB64 reads a guest file through the runner (base64 on the wire).
func (e *buildEngine) readGuestFileB64(k3s uint32, path string) ([]byte, error) {
	ok, out := e.RunBin(e.Bins.Self, e.ExecArgs(fmt.Sprintf(
		"pct exec %d -- bash -c 'test -s %s 2>/dev/null && base64 -w0 < %s'", k3s, path, path), 60))
	if !ok {
		return nil, fmt.Errorf("unreadable: %s", path)
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(out))
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

// gatewaySetup is one collected provider choice (gateway.go).

// litellmSecretMaterial returns the litellm master key, postgres password, and
// the operator's gateway provider choice (prompting for all three on first
// provision). The CP is now the durable owner: the caller seeds these to the
// CP (ensureCpSecrets); world_build re-seeds the co-located runner from the CP
// store so the existing $LITELLM/$PROVIDER_KEY injection path is unchanged.
func (e *buildEngine) litellmSecretMaterial(cfg *config.Config) (string, string, gatewaySetup, error) {
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
	if e.F.Yes {
		return "", "", gatewaySetup{}, fmt.Errorf("the litellm gateway needs a provider + model, collected interactively — run `freehold build` once without --non-interactive (the AI department retargets it later through litellm-api-admin)")
	}
	gw, err := e.collectGatewaySetup()
	if err != nil {
		return "", "", gatewaySetup{}, err
	}
	return masterKey, postgresPw, gw, nil
}

func (e *buildEngine) runBuild() error {
	cfg, err := config.Load(e.F.ConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil || cfg.CPURL == "" {
		return fmt.Errorf("no CP configured — run `freehold install` first (an operator box only), then `freehold login` here")
	}
	loginURL := common.ConsoleLoginURL(cfg)
	client, err := e.consoleLogin(loginURL)
	if err != nil {
		return err
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
	// Cache each slot's issued edge cert box-side (best-effort): the FRESH
	// lifecycle's full-destroy uninstall wipes the plane — this sealed copy is
	// what pre-seeds the next world without a new LE order.
	if err := e.cacheEdgeCerts(); err != nil {
		fmt.Fprintf(e.Out, "  (cert cache update skipped: %v)\n", err)
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

// consoleLogin establishes the operator's NIP-98 console session: the stored
// identity when it works; otherwise (interactive only) a no-echo nsec prompt,
// persisted ONLY after a successful login — a mistyped key never poisons the
// ledger, and a refused stored key is re-asked (overwritten on success).
// Headless (--non-interactive) never prompts: it fails actionably instead.
func (e *buildEngine) consoleLogin(loginURL string) (*console.Client, error) {
	for attempt := 0; attempt < 3; attempt++ {
		secStr, err := oplogin.SecretHex()
		if err == nil {
			key, kerr := oplogin.NsecToSecret(secStr)
			if kerr != nil {
				if e.F.Yes {
					return nil, fmt.Errorf("stored operator identity is invalid: %v", kerr)
				}
				fmt.Fprintf(e.Out, "  stored operator identity is invalid (%v) — enter the operator key\n", kerr)
			} else if client, lerr := oplogin.Login(loginURL, key); lerr == nil {
				return client, nil
			} else if e.F.Yes {
				return nil, fmt.Errorf("console login at %s failed with the stored operator identity: %v", loginURL, lerr)
			} else {
				fmt.Fprintf(e.Out, "  login refused (%v) — enter the operator key again\n", lerr)
			}
		} else if e.F.Yes {
			return nil, fmt.Errorf("no operator identity — run `freehold login` first: %v", err)
		}
		raw, err := oplogin.ReadNsec(e.Stdin)
		if err != nil {
			return nil, err
		}
		if raw == "" {
			return nil, fmt.Errorf("no nsec provided")
		}
		key, err := oplogin.NsecToSecret(raw)
		if err != nil {
			fmt.Fprintf(e.Out, "  (bad nsec: %v)\n", err)
			continue
		}
		client, err := oplogin.Login(loginURL, key)
		if err != nil {
			fmt.Fprintf(e.Out, "  login refused (%v) — try again\n", err)
			continue
		}
		if err := oplogin.SaveOverwrite(key); err != nil {
			return nil, fmt.Errorf("save operator identity: %w", err)
		}
		return client, nil
	}
	return nil, fmt.Errorf("console login at %s: too many failed attempts", loginURL)
}
