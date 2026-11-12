package cli

import (
	"os"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/installer"
)

// newInstallCommand installs the appliance on this Proxmox VE node, with a full-screen installer
// on a terminal and one line per step without.
func newInstallCommand() *cobra.Command {
	var cfg installer.Config
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the appliance on this Proxmox VE node",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			interactive := term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
			return installer.Execute(cmd.Context(), &cfg, cmd.InOrStdin(), cmd.OutOrStdout(), interactive)
		},
	}
	installer.BindFlags(cmd.Flags(), &cfg)
	return cmd
}
