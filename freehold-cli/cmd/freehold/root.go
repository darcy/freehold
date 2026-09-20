// Command freehold is ONE binary, two surfaces:
//
//   - no args                -> the interactive TUI dashboard
//   - --config <path>        -> the TUI with an explicit config
//   - freehold <subcommand>  -> the CLI (the operator surface)
//
// This file holds the root cobra command and wires every verb package onto it.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"freehold/contract/version"
	addrelaymember "freehold/freehold-cli/add-relay-member"
	"freehold/freehold-cli/build"
	"freehold/freehold-cli/channel"
	dnscred "freehold/freehold-cli/dns-cred"
	"freehold/freehold-cli/door"
	"freehold/freehold-cli/exec"
	"freehold/freehold-cli/install"
	"freehold/freehold-cli/internal/stages"
	"freehold/freehold-cli/profiles"
	"freehold/freehold-cli/status"
	"freehold/freehold-cli/teardown"
	"freehold/freehold-cli/uninstall"
	"freehold/freehold-cli/update"
)

var rootCmd = &cobra.Command{
	Use:   "freehold",
	Short: "Freehold CLI: the scripted CPA stand-in that drives the engine room",
	Long: "Freehold CLI: the scripted CPA stand-in that drives the engine room\n" +
		"Freehold bootstrap, provisioning, and repair engine.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.AddCommand(
		exec.Command(),
		profiles.Command(),
		door.Command(),
		dnscred.Command(),
		addrelaymember.Command(),
		status.Command(),
		update.Command(),
		channel.Command(),
		build.Command(),
		teardown.Command(),
		uninstall.Command(),
		install.InstallCommand(),
		// The box's self-staged stages: the freehold binary re-invokes ITSELF
		// for these low-level commands (provision/storage/deploy-cp).
		stages.ProvisionCommand(),
		stages.StorageCommand(),
		stages.DeployCpCommand(),
		stages.StampVersionCommand(),
	)
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.Version = version.Version + " (" + version.Commit + ")"
}

// run executes the CLI with the given subcommand args and returns the process
// exit code.
func run(args []string) int {
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
