// Package cli holds the cobra command tree for freehold-orchestrator.
//
// The CLI contract (flag names, existence, defaults, help text) mirrors the
// Rust `clap` surface in orchestrator/src/cli.rs byte-for-byte so that
// installer, teardown, and the TUI can invoke this binary by name + args
// unchanged. Subcommands are wired here; behavior lives in the internal/*
// packages and is filled in progressively.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "freehold-orchestrator",
	Short: "Freehold bootstrap, provisioning, and repair engine",
	Long: "freehold-orchestrator is the permanent bootstrap/repair engine for the\n" +
		"freehold appliance. It provisions environments through a Rust runner over\n" +
		"HTTP MCP. Subcommands mirror the original Rust CLI surface.",
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

// placeholder registers a subcommand that prints "not yet implemented" and
// exits non-zero. Real implementations replace these as the migration lands.
func placeholder(use, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("%s: not yet implemented", cmd.Name())
		},
	}
	return cmd
}

func init() {
	rootCmd.AddCommand(
		placeholder("onboard", ""),
		placeholder("exec", ""),
		placeholder("readiness", ""),
		placeholder("demo", ""),
		placeholder("bootstrap", ""),
		placeholder("deploy-relay", ""),
		placeholder("deploy-cp", ""),
		placeholder("console-login", ""),
		placeholder("relay-member", ""),
		placeholder("memory", ""),
		placeholder("delegate", ""),
		placeholder("delegate-peer", ""),
		placeholder("relay-profile", ""),
		placeholder("teardown", ""),
		placeholder("relay-join", ""),
		placeholder("relay-setup", ""),
		placeholder("storage", ""),
	)
}
