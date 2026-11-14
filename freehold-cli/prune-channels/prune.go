// Package prune-channels implements `freehold prune-channels` — the
// operator-authorized sweep of stale SYSTEM keys from visible agent channels.
// The default is a read-only preview (the member-list pull that confirms a
// world is affected); --apply removes members that are neither the current
// console key, an operator/admin, a human relay member, nor a known agent
// identity (live or removed). The preview's `allowed` list is the source of
// truth for exactly what a sweep will keep.
package prunechannels

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
)

var apply bool

var pruneCmd = &cobra.Command{
	Use:   "prune-channels",
	Short: "Preview or remove stale system keys from visible agent channels",
	Long: "Preview (default) the member lists of the world's visible agent channels and the\n" +
		"stale system keys --apply would remove. Human relay members and known agent\n" +
		"identities (live or removed) are kept; the preview's `allowed` list is explicit.\n" +
		"With --apply, remove those stale keys through the console (operator-authorized).\n" +
		"The scheduled-jobs channel is never swept.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := common.NegotiateProfile(cmd, "prune-channels")
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
		c, err := common.ConsoleLogin(cfg)
		if err != nil {
			return fmt.Errorf("console login: %w", err)
		}
		var raw json.RawMessage
		if apply {
			raw, err = c.ChannelsPruneApply()
		} else {
			raw, err = c.ChannelsPrunePreview()
		}
		if err != nil {
			return fmt.Errorf("channel prune: %w", err)
		}
		var pretty interface{}
		if err := json.Unmarshal(raw, &pretty); err != nil {
			return err
		}
		out, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(out))
		if !apply {
			fmt.Println("\npreview only — re-run with --apply to remove the stale keys")
		}
		return nil
	},
}

func init() {
	pruneCmd.Flags().String("config", common.DefaultConfigPath(), "Tenant config path to drive the world against")
	pruneCmd.Flags().BoolVar(&apply, "apply", false, "remove the stale keys (default: preview only)")
}

// Command returns the prune-channels command for root registration.
func Command() *cobra.Command { return pruneCmd }
