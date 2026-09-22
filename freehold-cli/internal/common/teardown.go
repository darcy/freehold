package common

import (
	"fmt"
	"path/filepath"

	"freehold/contract/config"
	"freehold/contract/crypto"
	oplogin "freehold/freehold-cli/login"
	cert "freehold/platform/services/certificates/letsencrypt"
	dnsman "freehold/platform/services/externaldns/cloudflare"
)

// ThinPoolOf returns the freehold-CREATED thin pool recorded in the config
// ("" when the plane reused a stock pool — nothing recorded).
func ThinPoolOf(cfg *config.Config) string {
	if cfg.Plane.ThinPool != nil {
		return *cfg.Plane.ThinPool
	}
	return ""
}

// RemoveManagedDNS deletes the world's freehold-managed RELAY A record on the
// provider recorded in config.Dns.Manager (teardown --remove-dns). The CP's
// record is deliberately KEPT: teardown is CP-preserving.
func RemoveManagedDNS(cfg *config.Config) error {
	m := cfg.Dns.Manager
	if m == nil || !m.Managed {
		fmt.Println("  (no freehold-managed DNS recorded — nothing to remove)")
		return nil
	}
	secretHex, err := EncSecretFromDir(filepath.Join(FreeholdHome(), "control-plane", "agent-ops"))
	if err != nil {
		return fmt.Errorf("no ops identity for DNS removal: %w", err)
	}
	secret, err := HexBytes(secretHex)
	if err != nil {
		return fmt.Errorf("ops identity enc secret: %w", err)
	}
	open := func(secret, aad, blob []byte) ([]byte, error) { return crypto.Open(secret, aad, blob) }
	provider, env, err := cert.LoadCreds(filepath.Join(FreeholdHome(), "control-plane", "dns-provider-relay.json"), open, secret)
	if err != nil {
		return fmt.Errorf("load relay DNS credential for removal: %w", err)
	}
	if provider != "cloudflare" {
		return fmt.Errorf("recorded DNS manager %q cannot remove records yet", provider)
	}
	man, err := dnsman.For(provider, env)
	if err != nil {
		return err
	}
	if h := cfg.RelayHost(); h != "" {
		if err := man.DeleteA(h); err != nil {
			return fmt.Errorf("remove DNS record %s: %w", h, err)
		}
	}
	return nil
}

// RunWholeWorldTeardown is the CP-preserving whole-world teardown: it asks the
// CP (console /api/world-teardown) to remove the WORLD through its co-located
// runner. The CP, its runner, the durable plane, and the cert mirror all stay.
func RunWholeWorldTeardown(cfg *config.Config, configPath string, removeDNS, yes bool) error {
	if !yes {
		fmt.Println("teardown scope: whole-world (CP-preserving — the CP, its runner, and the plane stay)")
		fmt.Println("keeps: control plane · co-located runner · durable plane · cert mirror · Cloudflare records")
		if err := ConfirmDestructive("teardown"); err != nil {
			return err
		}
	}
	if removeDNS {
		if err := RemoveManagedDNS(cfg); err != nil {
			fmt.Printf("  (warning: DNS removal skipped — %v)\n", err)
		}
	}
	secretHex, err := oplogin.SecretHex()
	if err != nil {
		return fmt.Errorf("no operator session on this box (%v) — run `freehold login` first", err)
	}
	secret, err := oplogin.NsecToSecret(secretHex)
	if err != nil {
		return err
	}
	// Reach the console over its LAN IP when this box knows it: teardown
	// destroys the k3s LXC, which hosts the Caddy edge fronting cfg.CPURL.
	loginURL := cfg.CPURL
	if ip := config.LxcIP(cfg.Lxc.Cp); ip != "" {
		loginURL = "http://" + ip + ":8080"
	}
	c, err := oplogin.Login(loginURL, secret)
	if err != nil {
		return fmt.Errorf("login to %s failed: %w", loginURL, err)
	}
	fmt.Printf("tearing down world %s through the CP (the CP + runner are preserved)…\n", cfg.TenantSlug())
	res, err := c.WorldTeardown()
	if err != nil {
		return fmt.Errorf("world-teardown (console /api/world-teardown): %w", err)
	}
	if res.Report != "" {
		fmt.Println(res.Report)
	}
	fmt.Printf("  world teardown: %d internal DNS record(s) cleared; the CP + runner are preserved\n", res.DnsRemoved)
	cfg.Lxc.Relay.Vmid, cfg.Lxc.Relay.Ip = nil, nil
	cfg.Lxc.K3s.Vmid, cfg.Lxc.K3s.Ip = nil, nil
	if err := cfg.Save(configPath); err != nil {
		return err
	}
	fmt.Println("cleared recorded relay/k3s coordinates (vmid + ip) — the next build re-creates them")
	return nil
}
