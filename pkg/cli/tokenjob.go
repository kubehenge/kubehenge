package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/luthermonson/go-proxmox"
	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// newTokenJobCommand runs in the Jobs the controller starts for what its own token may not do.
func newTokenJobCommand(conn *pve.Config) *cobra.Command {
	var input, result string
	cmd := &cobra.Command{
		Use:    "token-job <ha|guest-agent>",
		Short:  "Run the request the controller left for this Job, with this Job's Proxmox VE token",
		Hidden: true,
		Long: `Run the request the controller left for this Job in its input Secret, signed in with the token in
KUBEHENGE_PROXMOX_TOKEN, and write the result as the pod's termination message: an HA write to one
of Kubehenge's HA resources or rules (ha), or a command through a VM's guest agent (guest-agent).
KUBEHENGE_JOB names the Job, from the pod's batch.kubernetes.io/job-name label.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"ha", "guest-agent"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var serve func(context.Context, *proxmox.Client, []byte) (string, error)
			switch args[0] {
			case "ha":
				serve = provisioner.ServeHARequest
			case "guest-agent":
				serve = provisioner.ServeGuestRequest
			default:
				return fmt.Errorf("no token Job %q: ha or guest-agent", args[0])
			}
			// A missing token or CA is reported as the request's result, which the controller shows.
			c, connErr := pve.Connect(*conn)
			return provisioner.ServeTokenJob(cmd.Context(), input, os.Getenv("KUBEHENGE_JOB"), result,
				func(ctx context.Context, request []byte) (string, error) {
					if connErr != nil {
						return "", fmt.Errorf("connecting to Proxmox VE: %w", connErr)
					}
					return serve(ctx, c, request)
				})
		},
	}
	cmd.Flags().StringVar(&input, "input", "/etc/kubehenge/input", "directory of the input Secret")
	cmd.Flags().StringVar(&result, "result", "/dev/termination-log", "file the result is written to")
	return cmd
}
