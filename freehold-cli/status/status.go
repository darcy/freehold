// Package status implements `freehold status` — the single world inventory
// read, served by the CP's public /api/world console route.
package status

import (
	"fmt"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/freehold-cli/internal/common"
	oplogin "freehold/freehold-cli/login"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the world inventory (agents + runners + DNS) from the CP",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := common.NegotiateProfile(cmd, "status")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
		}
		cfg, err := config.Load(common.ConfigPath())
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		return statusFromConsole(cfg)
	},
}

func init() {
	statusCmd.Flags().String("config", common.DefaultConfigPath(), "Tenant config path to drive the world against")
}

// statusFromConsole renders the CP's single inventory off the public
// /api/world console route (session authed as the operator), so a remote box's
// `freehold status` works.
func statusFromConsole(cfg *config.Config) error {
	sec, err := oplogin.SecretHex()
	if err != nil {
		return fmt.Errorf("no operator identity (run `freehold login`): %v", err)
	}
	key, err := oplogin.NsecToSecret(sec)
	if err != nil {
		return fmt.Errorf("operator secret not valid: %v", err)
	}
	c, err := oplogin.Login(cfg.CPURL, key)
	if err != nil {
		return fmt.Errorf("console login: %v", err)
	}
	w, err := c.World()
	if err != nil {
		return fmt.Errorf("world status: %v", err)
	}
	return printWorldSummary(w)
}

func printWorldSummary(w *console.WorldSummary) error {
	fmt.Printf("CP pubkey: %s\n", w.CPPubkey)
	fmt.Printf("agents (%d):\n", len(w.Agents))
	for _, a := range w.Agents {
		fmt.Printf("  %-20s %s", a.Name, a.Pubkey)
		if a.Purpose != "" {
			fmt.Printf("  (%s)", a.Purpose)
		}
		fmt.Println()
	}
	runners := w.RunnersList()
	fmt.Printf("runners (%d):\n", len(runners))
	for _, r := range runners {
		fmt.Printf("  %-20s %s\n", r.Name, r.NostrPubkey)
	}
	dns := w.DNSRecords()
	fmt.Printf("dns (%d):\n", len(dns))
	for _, d := range dns {
		fmt.Printf("  %s -> %s\n", d.Name, d.IP)
	}
	return nil
}

// Command returns the status command for root registration.
func Command() *cobra.Command { return statusCmd }
