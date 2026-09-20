// Package dnscred implements `freehold dns-cred` — storing (and pre-verifying)
// a DNS provider credential for a cert slot, one-time seeding a build reuses.
package dnscred

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/freehold-cli/internal/certcred"
	"freehold/platform/provisioning/box"
	cert "freehold/platform/services/certificates/letsencrypt"
)

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
		zero := &box.Engine{Out: os.Stdout, In: os.Stdin}
		cc := &certcred.Engine{Out: os.Stdout, Prompt: zero.Prompt}

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
			_, _, pub, err := cc.CertIdent()
			if err != nil {
				return err
			}
			seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
			if err := cert.SaveCreds(cc.CertCredPath(slot), provider, env, seal, pub, "cert-dns-"+slot); err != nil {
				return err
			}
			fmt.Fprintf(cc.Out, "  ✓ %s DNS provider credential saved (%s) — builds will reuse it\n", slot, provider)
			fmt.Fprintf(cc.Out, "  stored sealed at %s\n", cc.CertCredPath(slot))
			return nil
		}
		// Interactive: provider picker + env collection.
		if slot == "cp" {
			provider, _, err := cc.PromptDNSCred(slot, domain, "relay")
			if err != nil {
				return err
			}
			fmt.Fprintf(cc.Out, "  ✓ %s DNS provider credential saved (%s) — builds will reuse it\n", slot, provider)
			return nil
		}
		provider, _, err := cc.PromptDNSCred("relay", domain, "")
		if err != nil {
			return err
		}
		fmt.Fprintf(cc.Out, "  ✓ relay DNS provider credential saved (%s) — builds will reuse it\n", provider)
		fmt.Fprintf(cc.Out, "  stored sealed at %s\n", cc.CertCredPath("relay"))
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

// Command returns the dns-cred command for root registration.
func Command() *cobra.Command { return dnsCredCmd }
