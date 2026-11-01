// Package cli is the kubehenge command line.
package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// NewRoot returns the kubehenge command.
func NewRoot() *cobra.Command {
	var conn pve.Config
	root := &cobra.Command{
		Use:           "kubehenge",
		Short:         "Kubernetes for Proxmox VE",
		Version:       release.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	// The token comes only from the environment, so it never shows in process listings.
	conn.Token = os.Getenv("KUBEHENGE_PROXMOX_TOKEN")

	root.AddCommand(newTemplateCommand(&conn), newControllerCommand(&conn), newServeCommand(&conn),
		newUpdateCommand(), newReleaseCommand(), newWaitCommand(), newManifestsCommand(), newEtcdSnapshotCommand(),
		newApplianceCommand(), newDiagnoseCommand(), newTokenJobCommand(&conn), newCSIProxyCommand(&conn), newClusterCommand(&conn))
	// Cobra adds its completion command only once it executes. Added here, it refuses unknown
	// commands as the other groups do.
	root.InitDefaultCompletionCmd()
	refuseUnknownCommands(root)
	return root
}

// proxmoxFlags adds the flags of the commands that reach the Proxmox VE API.
func proxmoxFlags(f *pflag.FlagSet, conn *pve.Config) {
	f.StringVar(&conn.URL, "proxmox-url", os.Getenv("KUBEHENGE_PROXMOX_URL"),
		"Proxmox VE API, such as https://pve.example.org:8006 (env KUBEHENGE_PROXMOX_URL)")
	f.StringVar(&conn.CAFile, "proxmox-ca", os.Getenv("KUBEHENGE_PROXMOX_CA"),
		"PEM file with the CA of the Proxmox VE API's certificate, else the system trust store (env KUBEHENGE_PROXMOX_CA)")
	insecure, _ := strconv.ParseBool(os.Getenv("KUBEHENGE_PROXMOX_INSECURE"))
	f.BoolVar(&conn.Insecure, "proxmox-insecure", insecure,
		"do not verify the Proxmox VE API's certificate (env KUBEHENGE_PROXMOX_INSECURE)")
}

// suggestionDistance is cobra's own default, which it sets only where it makes the suggestions itself.
const suggestionDistance = 2

// refuseUnknownCommands makes every command group fail on a word that names none of its commands.
// Cobra checks that only for the root, and prints any other group's help and succeeds.
func refuseUnknownCommands(cmd *cobra.Command) {
	if cmd.HasSubCommands() && cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = suggestionDistance
	}
	if cmd.HasSubCommands() && cmd.Run == nil && cmd.RunE == nil {
		cmd.Args = cobra.ArbitraryArgs
		cmd.RunE = groupRun
	}
	for _, sub := range cmd.Commands() {
		refuseUnknownCommands(sub)
	}
}

// groupRun prints a group's help, or refuses the word given in place of one of its commands.
func groupRun(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		if err := cmd.Help(); err != nil {
			return fmt.Errorf("printing the help: %w", err)
		}
		return nil
	}
	return unknownCommand(cmd, args[0])
}

func unknownCommand(cmd *cobra.Command, name string) error {
	if near := cmd.SuggestionsFor(name); len(near) > 0 {
		return fmt.Errorf("unknown command %q for %q: did you mean %s?", name, cmd.CommandPath(), joinList(near, "or"))
	}
	var names []string
	for _, sub := range cmd.Commands() {
		if sub.IsAvailableCommand() {
			names = append(names, sub.Name())
		}
	}
	return fmt.Errorf("unknown command %q for %q: its commands are %s", name, cmd.CommandPath(), joinList(names, "and"))
}

// joinList joins names as "a, b and c", with conjunction in place of "and".
func joinList(names []string, conjunction string) string {
	last := len(names) - 1
	if last <= 0 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:last], ", ") + " " + conjunction + " " + names[last]
}

// noArgs refuses arguments to a command that takes none. Cobra's own check calls an argument an
// unknown command, which it is only for a command with commands of its own.
func noArgs(cmd *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return nil
	case cmd.HasAvailableSubCommands():
		return unknownCommand(cmd, args[0])
	}
	return fmt.Errorf("%s takes no arguments, not %s", cmd.CommandPath(), quoted(args))
}

// argsSay runs check and, when it refuses the arguments, says what the command takes in place of
// cobra's count of them.
func argsSay(check cobra.PositionalArgs, want string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if check(cmd, args) == nil {
			return nil
		}
		if len(args) == 0 {
			return errors.New(want)
		}
		return fmt.Errorf("%s, not %s", want, quoted(args))
	}
}

func quoted(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = strconv.Quote(a)
	}
	return strings.Join(q, " ")
}
