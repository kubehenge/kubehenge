package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// snapshotMode keeps a decrypted snapshot, which holds every Secret of its cluster, to its owner.
const snapshotMode = 0o600

func newEtcdSnapshotCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "etcd-snapshot",
		Short: "Decrypt the etcd snapshots Kubehenge stores, or receive one on a node",
	}
	cmd.AddCommand(newEtcdSnapshotDecryptCommand(), newEtcdSnapshotReceiveCommand())
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

func newEtcdSnapshotReceiveCommand() *cobra.Command {
	var out string
	var need int64
	cmd := &cobra.Command{
		Use:   "receive --out <file> --need <bytes>",
		Short: "Receive a snapshot on a node for a restore, from stdin",
		Long: `Receive an etcd snapshot the controller streams to stdin, for a restore from the backup volume.
It runs in a pod on the node restored on. It refuses before reading when the disk holding the file
has less than --need bytes free, and exits 0 only once the stream's SHA-256 matches.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return receiveSnapshot(cmd, out, need)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "file to write the snapshot to")
	cmd.Flags().Int64Var(&need, "need", 0, "bytes the restore needs free on the file's disk")
	return cmd
}

func receiveSnapshot(cmd *cobra.Command, out string, need int64) error {
	if out == "" {
		return errors.New("--out is required: the file to write")
	}
	dir := filepath.Dir(out)
	_, free, err := backupstore.DiskUse(dir)
	if err != nil {
		return err //nolint:wrapcheck // DiskUse names the path
	}
	if err = provisioner.ReceiveRoom(dir, need, free); err != nil {
		return err //nolint:wrapcheck // the refusal is the whole message
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, snapshotMode) //nolint:gosec // the file the controller names
	if err != nil {
		return fmt.Errorf("the received snapshot: %w", err)
	}
	n, err := provisioner.ReceiveEtcdSnapshot(cmd.InOrStdin(), f)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("the received snapshot: %w", cerr)
	}
	if err != nil {
		_ = os.Remove(out)
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "received %d bytes into %s\n", n, out)
	return nil
}
