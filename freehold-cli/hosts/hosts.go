// Package hosts implements `freehold hosts` — rename a world's public hosts
// onto its tree (the re-org): the relay to chat.<root>, the console to
// admin.<root>, the portal taking the world root. The edit is the CONFIG
// (the profile's relay/cp URLs); everything else — units, resolver records,
// certs, the edge render, world facts, public records — regenerates from
// those coords on the next `freehold update`. The world-domain invariant is
// enforced: the rename must not move the world's own domain, because every
// exposed app's name anchors on it.
package hosts

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
)

func newHostsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hosts [--relay <host>] [--cp <host>]",
		Short: "rename the world's public hosts onto its tree (relay -> chat.<root>, console -> admin.<root>)",
		Long: "Rename the world's public hosts onto its tree. With no flags: show the\n" +
			"current hosts and the tree targets. With --relay/--cp: rewrite the profile\n" +
			"config's relay/cp URLs — the next `freehold update` regenerates everything\n" +
			"from those coords (units, resolver records, certs, the edge render, world\n" +
			"facts, public records). The world's own domain must NOT move (it is what\n" +
			"every exposed app's name anchors on) — a --cp host that changes it is\n" +
			"refused. Nothing touches the world until the update runs.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := common.ResolveExecProfile(cmd); err != nil {
				return err
			}
			path := common.ProfileConfigPath(cmd)
			cfg, err := config.Load(path)
			if err != nil || cfg == nil {
				return fmt.Errorf("load config: %w", err)
			}
			oldRelay, oldCP := cfg.RelayHost(), cfg.CPHost()
			root := config.EffectiveWorldDomain(oldCP, cfg.WorldDomain)
			newRelay, _ := cmd.Flags().GetString("relay")
			newCP, _ := cmd.Flags().GetString("cp")
			if newRelay == "" && newCP == "" {
				fmt.Printf("world %q hosts today:\n  relay  %s\n  cp     %s\n  root   %s (the world's own domain)\n", cfg.Name, oldRelay, oldCP, root)
				fmt.Printf("tree targets: chat.%s (relay), admin.%s (console), %s (portal — the root itself)\n", root, root, root)
				fmt.Println("apply with: freehold hosts --relay chat." + root + " --cp admin." + root + " --config " + path)
				return nil
			}
			if newRelay == "" {
				newRelay = oldRelay
			}
			if newCP == "" {
				newCP = oldCP
			}
			if newRoot := config.EffectiveWorldDomain(newCP, cfg.WorldDomain); newRoot != root {
				return fmt.Errorf("the cp host %q would move the world's own domain (%s → %s) — refused: every exposed app's name anchors on it (rename within the tree, e.g. admin.%s)", newCP, root, newRoot, root)
			}
			if newRelay != oldRelay && !strings.HasSuffix(newRelay, "."+root) {
				return fmt.Errorf("the relay host %q is not a child of the world's own domain (%s) — the tree shape is chat.%s", newRelay, root, root)
			}
			cfg.RelayURL = "https://" + newRelay
			cfg.RelayWsURL = "wss://" + newRelay
			cfg.CPURL = "https://" + newCP
			// The login-seeded agent_tools_url rides the cp host's PUBLIC
			// form (https://<cp-host>/mcp) on a login'd box — a stale host
			// feeds the redeployed console (--agent-tools-url) and the
			// update's own migration dial, which would die post-rename (the
			// old cp vhost is gone). Swap the host when the field carries
			// it; a dial-LAN IP-form URL survives the rename untouched.
			if at := cfg.AgentToolsURL; strings.Contains(at, oldCP) {
				cfg.AgentToolsURL = strings.ReplaceAll(at, oldCP, newCP)
				fmt.Printf("  agent_tools_url  %s -> %s\n", at, cfg.AgentToolsURL)
			}
			if err := cfg.Save(path); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Printf("rewrote %s:\n  relay  %s -> %s\n  cp     %s -> %s\n  root   %s (unchanged — the portal takes it on the update)\n", path, oldRelay, newRelay, oldCP, newCP, root)
			if oldRelay != newRelay {
				fmt.Printf("\nAfter `freehold update` settles, remove the OLD public record (owner, via your CF dashboard/door):\n  %s\n", oldRelay)
			}
			if oldCP != newCP && oldCP != root {
				fmt.Printf("  %s\n", oldCP)
			}
			fmt.Printf("\nThen: `freehold update --config %s` — and update the Buzz desktop app's relay URL to %s\n", path, cfg.RelayURL)
			return nil
		},
	}
}

// Command returns the hosts command for root registration.
func Command() *cobra.Command {
	c := newHostsCmd()
	c.Flags().String("config", config.ConfigPath(), "Config path (default: the active profile's)")
	c.Flags().String("relay", "", "the relay's new public host (default target: chat.<world domain>)")
	c.Flags().String("cp", "", "the console's new public host (default target: admin.<world domain>)")
	return c
}
