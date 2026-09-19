// Package cli holds the cobra command tree for freehold (the operator CLI —
// the freehold-orchestrator binary folded into it).
//
// The CLI contract (flag names, existence, defaults, help text) mirrors the
// old Rust `clap` surface so teardown, the TUI, and `freehold install` can
// invoke this binary by name + args unchanged. Behavior is wired to the
// internal/* packages.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	installcli "freehold/freehold-cli/install"
)

var rootCmd = &cobra.Command{
	Use:   "freehold",
	Short: "Freehold CLI: the scripted CPA stand-in that drives the engine room",
	Long: "Freehold CLI: the scripted CPA stand-in that drives the engine room\n" +
		"Freehold bootstrap, provisioning, and repair engine.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the CLI with the process args (the freehold binary entry
// point) and returns any error.
func Execute() error {
	return runErr(rootCmd.Execute())
}

// runErr optionally prints the error (SilenceErrors is on).
func runErr(err error) error {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return err
}

// Run executes the CLI with the given subcommand args (the freehold binary
// forwards them here) and returns the process exit code.
func Run(args []string) int {
	oldArgs := os.Args
	os.Args = append([]string{"freehold"}, args...)
	defer func() { os.Args = oldArgs }()
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func init() {
	rootCmd.AddCommand(
		execCmd,
		readinessCmd,
		demoCmd,
		consoleLoginCmd,
		relayMemberCmd,
		memoryCmd,
		delegateCmd,
		delegatePeerCmd,
		relayProfileCmd,
		teardownCmd,
		uninstallCmd,
		relayJoinCmd,
		relaySetupCmd,
		buildCmd,
		dnsCredCmd,
		// The install surface (folded in from the former freehold-install
		// binary): `install`/`bootstrap` + the box's self-staged stages
		// (provision/storage/deploy-cp). The operator `exec` above is shared.
		installcli.InstallCommand(),
		installcli.BootstrapCommand(),
		installcli.ProvisionCommand(),
		installcli.StorageCommand(),
		installcli.DeployCpCommand(),
	)
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.Version = "0.1.0"
	_ = fmt.Sprintf
	_ = os.Stdout
}
