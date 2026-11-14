package cli

import (
	"io"
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
			in, interactive, closeIn := questionInput(cmd.InOrStdin(), term.IsTerminal(os.Stdin.Fd()), term.IsTerminal(os.Stdout.Fd()),
				func() (io.ReadCloser, error) { return os.Open("/dev/tty") })
			defer closeIn()
			return installer.Execute(cmd.Context(), &cfg, in, cmd.OutOrStdout(), interactive)
		},
	}
	installer.BindFlags(cmd.Flags(), &cfg)
	return cmd
}

// questionInput returns where the installer's questions are asked and whether it can ask them.
// With the script piped into sh, standard input is the pipe, so the questions go to the
// controlling terminal when the output is one.
func questionInput(stdin io.Reader, stdinTerminal, stdoutTerminal bool, openTerminal func() (io.ReadCloser, error)) (io.Reader, bool, func()) {
	switch {
	case !stdoutTerminal:
		return stdin, false, func() {}
	case stdinTerminal:
		return stdin, true, func() {}
	}
	tty, err := openTerminal()
	if err != nil {
		return stdin, false, func() {}
	}
	return tty, true, func() { _ = tty.Close() }
}
