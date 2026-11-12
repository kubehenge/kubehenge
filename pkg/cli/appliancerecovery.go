package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

const (
	// maxExportFile bounds an export bundle read for an import, as the interface bounds it.
	maxExportFile = 64 << 20
	// privateFileMode is the mode of a file with a secret or a spec in it.
	privateFileMode os.FileMode = 0o600
	// passphraseField is the request field that carries the state passphrase.
	passphraseField = "passphrase"
)

const exportPassphraseUsage = "file holding the passphrase the export is encrypted with, else KUBEHENGE_EXPORT_PASSPHRASE holds it"

func (o *applianceOptions) exportCommand() *cobra.Command {
	var out, passphraseFile, exportFile string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Save the appliance's state, encrypted, for a new appliance to restore the management cluster",
		Long: `Save what a new appliance needs to restore the management cluster's etcd from the backup target: the
admin talosconfig, the state, the settings and the target's access key. The file is encrypted with the
export passphrase, which you set. The state passphrase comes from --passphrase-file or KUBEHENGE_PASSPHRASE.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := secretFrom(passphraseFile, "KUBEHENGE_PASSPHRASE")
			if err != nil {
				return err
			}
			export, err := secretFrom(exportFile, "KUBEHENGE_EXPORT_PASSPHRASE")
			if err != nil {
				return err
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				bundle, err := s.call(ctx, http.MethodPost, "/api/appliance/export", map[string]string{passphraseField: state, "exportPassphrase": export}, nil)
				if err != nil {
					return err
				}
				if err := os.WriteFile(out, bundle, privateFileMode); err != nil {
					return fmt.Errorf("saving the export: %w", err)
				}
				return say(cmd, "Exported to "+out)
			})
		},
	}
	f := cmd.Flags()
	f.StringVarP(&out, "output", "o", "kubehenge-export.age", "where to save the export")
	f.StringVar(&passphraseFile, "passphrase-file", "", passphraseFileUsage)
	f.StringVar(&exportFile, "export-passphrase-file", "", exportPassphraseUsage)
	o.signInFlags(cmd)
	return cmd
}

func (o *applianceOptions) importCommand() *cobra.Command {
	var exportFile string
	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Restore the appliance's state from an export, in a new appliance",
		Long: `Restore the appliance's state from an export made by "kubehenge appliance export". Run it in a new
appliance that has no management cluster. Then restore the management cluster's etcd from a snapshot
with "kubehenge appliance etcd-backups restore", and apply the settings once to sign the interface in.`,
		Args: argsSay(cobra.ExactArgs(1), "give the export file"),
		RunE: func(cmd *cobra.Command, args []string) error {
			export, err := secretFrom(exportFile, "KUBEHENGE_EXPORT_PASSPHRASE")
			if err != nil {
				return err
			}
			info, err := os.Stat(args[0])
			if err != nil || info.Size() > maxExportFile {
				return errors.New("the export file is missing or over 64 MiB")
			}
			bundle, err := os.ReadFile(filepath.Clean(args[0]))
			if err != nil {
				return fmt.Errorf("reading the export: %w", err)
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				req := map[string]any{"bundle": bundle, "exportPassphrase": export}
				if _, err := s.call(ctx, http.MethodPost, "/api/appliance/import", req, nil); err != nil {
					return err
				}
				return say(cmd, "Imported. Restore a snapshot next.")
			})
		},
	}
	cmd.Flags().StringVar(&exportFile, "export-passphrase-file", "", exportPassphraseUsage)
	o.signInFlags(cmd)
	return cmd
}

func (o *applianceOptions) recreateCommand() *cobra.Command {
	return o.snapshotRunCommand(&snapshotRun{
		use:   "recreate <snapshot>",
		short: "Make the management cluster's VMs again and restore a snapshot into them",
		long: `Recreate the management cluster after its VMs were lost. It makes the VMs and configures the nodes,
restores the snapshot into them, then applies the management cluster whole. Clusters changed since the
snapshot go back to how they were then. The state passphrase comes from --passphrase-file or
KUBEHENGE_PASSPHRASE.`,
		action: "recreate", started: "Recreating from ", done: "Recreated from ",
	})
}

// snapshotRun describes a command that starts a run of the appliance from one snapshot and follows
// it: restoring etcd, or recreating the management cluster.
type snapshotRun struct {
	use, short, long, action, started, done string
}

func (o *applianceOptions) snapshotRunCommand(spec *snapshotRun) *cobra.Command {
	wait := true
	var passphraseFile string
	cmd := &cobra.Command{
		Use:   spec.use,
		Short: spec.short,
		Long:  spec.long,
		Args:  argsSay(cobra.ExactArgs(1), "give the snapshot's name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			passphrase, err := secretFrom(passphraseFile, "KUBEHENGE_PASSPHRASE")
			if err != nil {
				return err
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				path := "/api/appliance/etcd-backups/snapshots/" + url.PathEscape(args[0]) + "/" + spec.action
				if _, err := s.call(ctx, http.MethodPost, path, map[string]string{passphraseField: passphrase}, nil); err != nil {
					return err
				}
				if !wait {
					return say(cmd, spec.started+args[0])
				}
				if err := s.untilJobEnds(ctx, cmd.OutOrStdout()); err != nil {
					return err
				}
				return say(cmd, spec.done+args[0])
			})
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", true, "wait until the run ends, printing its log")
	cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "", passphraseFileUsage)
	o.signInFlags(cmd)
	return cmd
}

func (o *applianceOptions) dismissWarningCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dismiss-warning",
		Short: "Silence the warning that the management cluster's etcd is not backed up, for 30 days",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				raw, err := s.call(ctx, http.MethodPost, "/api/appliance/etcd-backups/warning/dismissal", nil, nil)
				if err != nil {
					return err
				}
				return o.printBackupsAnswer(cmd, raw)
			})
		},
	}
	o.signInFlags(cmd)
	o.jsonFlag(cmd)
	return cmd
}
