package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// snapshotMode keeps a decrypted snapshot, which holds every Secret of its cluster, to its owner.
const snapshotMode = 0o600

func newEtcdSnapshotCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "etcd-snapshot",
		Short: "Decrypt the etcd snapshots Kubehenge stores in clusters' backup targets",
	}
	cmd.AddCommand(newEtcdSnapshotDecryptCommand())
	return cmd
}

func newEtcdSnapshotDecryptCommand() *cobra.Command {
	var identityFile string
	cmd := &cobra.Command{
		Use:   "decrypt --identity <file> <snapshot.db.age> <snapshot.db>",
		Short: "Decrypt a snapshot and check etcd's checksum in it",
		Long: `Decrypt an etcd snapshot downloaded from a cluster's backup target, and check etcd's own checksum
at its end. The identity file holds the etcd-snapshot-identity key of the cluster's
<cluster>-backup-keys Secret in the management cluster. Any age client decrypts the snapshots too.`,
		Args: argsSay(cobra.ExactArgs(2), "give the encrypted snapshot and the file to write"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if identityFile == "" {
				return errors.New("--identity is required: the file with the cluster's etcd snapshot key")
			}
			identity, err := os.ReadFile(identityFile) //nolint:gosec // the file the user names
			if err != nil {
				return fmt.Errorf("the identity file: %w", err)
			}
			in, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("the snapshot: %w", err)
			}
			defer func() { _ = in.Close() }()
			out, err := os.OpenFile(args[1], os.O_WRONLY|os.O_CREATE|os.O_EXCL, snapshotMode)
			if err != nil {
				return fmt.Errorf("the decrypted snapshot: %w", err)
			}
			sum, size, err := provisioner.DecryptEtcdSnapshot(in, string(identity), out)
			if cerr := out.Close(); err == nil && cerr != nil {
				err = fmt.Errorf("the decrypted snapshot: %w", cerr)
			}
			if err != nil {
				_ = os.Remove(args[1])
				return err
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %s\n%d bytes, etcd's checksum matches\n", sum, args[1], size); err != nil {
				return fmt.Errorf("writing the checksum: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&identityFile, "identity", "", "file with the cluster's etcd snapshot key")
	return cmd
}
