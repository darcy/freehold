// Package cli holds the cobra command tree for freehold-orchestrator.
//
// The CLI contract (flag names, existence, defaults, help text) mirrors the
// Rust `clap` surface in orchestrator/src/cli.rs so installer, teardown, and
// the TUI can invoke this binary by name + args unchanged. Behavior is wired
// to the internal/* packages.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "freehold-orchestrator",
	Short: "Freehold CLI: the scripted CPA stand-in that drives the engine room",
	Long: "Freehold CLI: the scripted CPA stand-in that drives the engine room\n" +
		"Freehold bootstrap, provisioning, and repair engine.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command tree and returns the exit-relevant error.
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return err
	}
	return nil
}

func init() {
	rootCmd.AddCommand(
		onboardCmd,
		execCmd,
		readinessCmd,
		demoCmd,
		bootstrapCmd,
		deployRelayCmd,
		deployCpCmd,
		consoleLoginCmd,
		relayMemberCmd,
		memoryCmd,
		delegateCmd,
		delegatePeerCmd,
		relayProfileCmd,
		teardownCmd,
		relayJoinCmd,
		relaySetupCmd,
		storageCmd,
	)
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.Version = "0.1.0"
	_ = fmt.Sprintf
	_ = os.Stdout
}
