// Package door implements `freehold door` — authorize/revoke this box's
// DOOR_SPEC key on the host through the CP's co-located runner.
package door

import (
	"fmt"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
)

var doorCmd = &cobra.Command{
	Use:   "door <authorize|revoke>",
	Short: "authorize/revoke this box's door key on the host (DOOR_SPEC)",
	Long: "door authorizes or revokes THIS box's public door key on the host door,\n" +
		"through the CP's co-located runner. The box derives the door key from its\n" +
		"agent-ops identity seed (the private half never leaves the box; only the\n" +
		"public line is presented) — the ACTIVE profile's seed, so pin --config on\n" +
		"a multi-profile box. authorize = a fresh box can run CP-lifecycle\n" +
		"verbs (bootstrap-cp/teardown-cp); revoke removes it.",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("door needs a subcommand: authorize|revoke")
		}
		return common.DoorAction(cmd, args[0])
	},
}

// Command returns the door command for root registration.
func Command() *cobra.Command {
	doorCmd.Flags().String("config", config.ConfigPath(), "Config path (default: the active profile's)")
	return doorCmd
}
