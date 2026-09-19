package cli

import "github.com/spf13/cobra"

// Command getters for the install surface on the freehold cobra root (the
// commands are package-level for self-registration/flag wiring; the main
// wires them). The box self-staged stages live in internal/stages.

// InstallCommand is the interactive installer.
func InstallCommand() *cobra.Command { return installCmd }

// BootstrapCommand is the non-interactive CP bring-up.
func BootstrapCommand() *cobra.Command { return bootstrapCmd }
