// Package exec implements `freehold exec` — signed exec against a running
// runner, or (on a thin box) through the CP's co-located runner.
package exec

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"freehold/freehold-cli/internal/common"
)

var execCmd = &cobra.Command{
	Use:   "exec <TARGET> <CMD>",
	Short: "Signed exec against a running runner",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			return fmt.Errorf("exec needs <TARGET> <CMD>")
		}
		if err := common.ResolveExecProfile(cmd); err != nil {
			return err
		}
		target, cmds := args[0], args[1]
		commonArgs := common.ReadCommonFlags(cmd)
		// The signing identity dir must follow the NEGOTIATED profile, not the
		// flag's init-time default, unless the operator pinned --agent-dir.
		if !cmd.Flags().Changed("agent-dir") {
			commonArgs.AgentDir = common.DefaultAgentDir()
		}
		secrets, _ := cmd.Flags().GetStringSlice("secret")
		timeoutS, _ := cmd.Flags().GetUint64("timeout")
		if common.NoLocalRunner() {
			return common.WorldExecThroughCP(target, cmds, secrets, timeoutS)
		}
		c, err := common.Connect(commonArgs, target)
		if err != nil {
			return err
		}
		refs := secrets
		if len(refs) == 0 {
			refs = []string{target}
		}
		out, err := c.Exec(target, cmds, refs, timeoutS)
		if err != nil {
			return err
		}
		if out.Stdout != "" {
			fmt.Print(out.Stdout)
		}
		if out.Stderr != "" {
			fmt.Fprint(os.Stderr, out.Stderr)
		}
		if out.TimedOut {
			fmt.Fprintln(os.Stderr, "TIMED OUT")
			return fmt.Errorf("exec timed out")
		}
		if out.ExitCode != nil && *out.ExitCode != 0 {
			return fmt.Errorf("exec exit %d", *out.ExitCode)
		}
		return nil
	},
}

func init() {
	common.AddCommonFlags(execCmd, nil)
	execCmd.Flags().String("config", common.DefaultConfigPath(), "Tenant config path to resolve the runner from")
	execCmd.Flags().StringSliceP("secret", "s", nil, "Secret names to request (must be the target's own credential; defaults to the target name — the provision convention)")
	execCmd.Flags().Uint64("timeout", 60, "Runner-side watchdog in seconds (client deadline sits above it)")
}

// Command returns the exec command for root registration.
func Command() *cobra.Command { return execCmd }
