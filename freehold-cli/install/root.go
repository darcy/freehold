package install

import "github.com/spf13/cobra"

// Command getters for the install surface on the freehold cobra root. The box
// self-staged stages live in internal/stages.

// InstallCommand is the interactive/headless installer.
func InstallCommand() *cobra.Command { return installCmd }
