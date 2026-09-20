// Package update implements `freehold update` — the future home for update
// behavior; today it runs the CP's world_migrate stage through the agent-tools
// toolset (the former `freehold world migrate`).
package update

import (
	"fmt"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update the world through the CP (runs the CP's world_migrate stage)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := common.NegotiateProfile(cmd, "update")
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
		mc, err := common.WorldMCP(cfg)
		if err != nil {
			return err
		}
		text, err := common.CallAgentToolsText(mc, "world_migrate", map[string]interface{}{})
		if err != nil {
			return fmt.Errorf("world_migrate: %w", err)
		}
		fmt.Println(text)
		return nil
	},
}

func init() {
	updateCmd.Flags().String("config", common.DefaultConfigPath(), "Tenant config path to drive the world against")
}

// Command returns the update command for root registration.
func Command() *cobra.Command { return updateCmd }
