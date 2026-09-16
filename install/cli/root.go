package cli

import "github.com/spf13/cobra"

// Command getters for the freehold-install cobra root (the commands are
// package-level for self-registration/flag wiring; the main wires them).

// InstallCommand is the interactive installer.
func InstallCommand() *cobra.Command { return installCmd }

// BootstrapCommand is the non-interactive CP bring-up.
func BootstrapCommand() *cobra.Command { return bootstrapCmd }

// ExecCommand self-stages signed exec against a runner.
func ExecCommand() *cobra.Command { return execCmd }

// ProvisionCommand self-stages the LXC/VPS bootstrap driver.
func ProvisionCommand() *cobra.Command { return provisionCmd }

// StorageCommand is the durable-volume-plane subcommand tree.
func StorageCommand() *cobra.Command { return storageCmd }

// DeployCpCommand self-stages the CP deploy.
func DeployCpCommand() *cobra.Command { return deployCpCmd }
