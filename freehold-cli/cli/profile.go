package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/cli/login"
)

// profilesCmd lists the registered tenant profiles (read-only). Each line shows
// the profile name, its config path, and the CP it logs into when recorded.
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

func init() {
	rootCmd.AddCommand(profilesCmd)
}

// negotiateProfile picks the active tenant for a config-touching command
// (build/bootstrap/teardown/world/exec). An explicit --config wins (a custom
// config path, state at the freehold base) and skips the picker. Otherwise,
// when the box has registered profiles the operator is asked to select one,
// which scopes both the config path and the freehold state dir via
// config.Current. Returns selected=false, err=nil when there are NO profiles —
// the caller decides (fail closed: "run `freehold login` first").
func negotiateProfile(cmd *cobra.Command, what string) (bool, error) {
	if cmd.Flags().Changed("config") {
		p, _ := cmd.Flags().GetString("config")
		config.SetCurrent(config.ProfileForConfigPath(p))
		return true, nil
	}
	if len(config.List()) == 0 {
		return false, nil
	}
	_, err := oplogin.SelectProfile(what)
	if err != nil {
		return false, err
	}
	return config.Current() != nil, nil
}

// profileConfigPath is the effective config path after negotiation: the active
// profile's when one is negotiated AND the operator didn't pin --config
// explicitly (the flag default is the default config path, fixed at
// registration), else the flag's own value.
func profileConfigPath(cmd *cobra.Command) string {
	if cmd.Flags().Changed("config") {
		p, _ := cmd.Flags().GetString("config")
		return p
	}
	return config.ConfigPath()
}
