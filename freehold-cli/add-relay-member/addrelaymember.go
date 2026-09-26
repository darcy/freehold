// Package addrelaymember implements `freehold add-relay-member` — adding a
// relay member through the relay-admin runner (buzz-admin).
package addrelaymember

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"freehold/freehold-cli/internal/common"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/box"
)

var addRelayMemberCmd = &cobra.Command{
	Use:   "add-relay-member",
	Short: "C2: add a relay member through the relay-admin runner (buzz-admin)",
	RunE: func(cmd *cobra.Command, args []string) error {
		commonArgs := common.ReadCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		pubkey, _ := cmd.Flags().GetString("pubkey")
		role, _ := cmd.Flags().GetString("role")
		lxcStr, _ := cmd.Flags().GetString("lxc")
		composeDir, _ := cmd.Flags().GetString("compose-dir")
		if pubkey == "" {
			return fmt.Errorf("add-relay-member needs --pubkey")
		}
		if !common.IsHex64(pubkey) {
			return fmt.Errorf("relay member pubkey must be a 64-hex Nostr pubkey (got %q)", pubkey)
		}
		if role != "" && role != "member" && role != "admin" {
			return fmt.Errorf("relay member role must be 'member' or 'admin' (got %q)", role)
		}
		// The compose dir is interpolated into a shell command on the host, so
		// reject metacharacters rather than let a path break out of the sh -c.
		if strings.ContainsAny(composeDir, " \t\n;|&$`'\"\\(){}[]<>*?!#~") {
			return fmt.Errorf("--compose-dir contains shell metacharacters (%q) — pass a plain path", composeDir)
		}
		c, err := common.Connect(commonArgs, target)
		if err != nil {
			return err
		}
		var lxc *uint32
		if lxcStr != "" {
			v, perr := strconv.ParseUint(lxcStr, 10, 32)
			if perr != nil || v == 0 {
				return fmt.Errorf("--lxc must be a positive LXC id (got %q)", lxcStr)
			}
			l := uint32(v)
			lxc = &l
		}
		roleSuffix := ""
		if role != "" {
			roleSuffix = " --role " + role
		}
		cmdLine := fmt.Sprintf("cd %s && docker compose exec -T relay buzz-admin add-member --pubkey %s%s", composeDir, pubkey, roleSuffix)
		full := cmdLine
		if lxc != nil {
			full = fmt.Sprintf("pct exec %d -- sh -c '%s'", *lxc, cmdLine)
		}
		out, err := bootstrap.ExecToOK(c, target, full, "buzz-admin add-member", 120)
		if err != nil {
			return err
		}
		roleNote := ""
		if role != "" {
			roleNote = " (" + role + ")"
		}
		fmt.Printf("relay member %s%s: %s\n", pubkey, roleNote, out.Stdout)
		return nil
	},
}

func init() {
	common.AddCommonFlags(addRelayMemberCmd, nil)
	addRelayMemberCmd.Flags().String("target", box.RunnerTarget, "Target runner (the box holding the relay host)")
	addRelayMemberCmd.Flags().String("pubkey", "", "Nostr pubkey (64-hex) to add as a relay member")
	addRelayMemberCmd.Flags().String("role", "", "Role: member (default) or admin (owner comes from RELAY_OWNER_PUBKEY)")
	addRelayMemberCmd.Flags().String("lxc", "", "The LXC on the box holding the relay compose stack")
	addRelayMemberCmd.Flags().String("compose-dir", "/srv/data/relay/deploy/compose", "Compose project dir on the relay host")
}

// Command returns the add-relay-member command for root registration.
func Command() *cobra.Command { return addRelayMemberCmd }
