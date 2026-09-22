package stages

import "github.com/spf13/cobra"

// Command getters for the box self-staged stages: the freehold binary
// re-invokes ITSELF for these low-level commands, exactly as the shared
// provisioning engine selfStages them. One home for the once-duplicated
// definitions (previously split between the local CLI and install).

// ProvisionCommand self-stages the LXC/VPS bootstrap driver.
func ProvisionCommand() *cobra.Command { return provisionCmd }

// StorageCommand is the durable-volume-plane subcommand tree.
func StorageCommand() *cobra.Command { return storageCmd }

// DeployCpCommand self-stages the CP deploy.
func DeployCpCommand() *cobra.Command { return deployCpCmd }
