// Command freehold-install is the CP installer/bootstrapper — the day-0 CLI
// that brings a CONTROL PLANE up in an environment (Proxmox today; Vultr/
// Hetzner providers come later) and provisions the box's door to it. After
// bootstrap, world bring-up is `freehold build` from ANY box via the CP — the
// environment no longer matters.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"freehold/install/cli"
)

func main() {
	root := &cobra.Command{
		Use:   "freehold-install",
		Short: "freehold-install — bring up a control plane in an environment + the box's door; then `freehold build` runs the world from anywhere",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		cli.InstallCommand(),
		cli.BootstrapCommand(),
		cli.ExecCommand(),
		cli.ProvisionCommand(),
		cli.StorageCommand(),
		cli.DeployCpCommand(),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
