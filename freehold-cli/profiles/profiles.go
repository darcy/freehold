// Package profiles implements `freehold profiles` — the read-only list of
// registered tenant profiles.
package profiles

import (
	"fmt"

	"github.com/spf13/cobra"

	"freehold/contract/config"
)

var profilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "List the registered tenant profiles (owned + config paths)",
	RunE: func(cmd *cobra.Command, args []string) error {
		list := config.List()
		if len(list) == 0 {
			fmt.Println("no profiles registered — run `freehold login` to add one")
			return nil
		}
		for _, p := range list {
			desc := ""
			if cfg, err := config.Load(p.ConfigPath); err == nil && cfg != nil {
				if cfg.CPURL != "" {
					desc = " -> " + cfg.CPURL
				} else {
					desc = " (not logged in)"
				}
			}
			fmt.Printf("%s\t%s%s\n", p.Name, p.ConfigPath, desc)
		}
		return nil
	},
}

// Command returns the profiles command for root registration.
func Command() *cobra.Command { return profilesCmd }
