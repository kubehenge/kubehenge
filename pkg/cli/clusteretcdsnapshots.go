package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// etcdSnapshotsAnswer is what the interface says of a cluster's etcd snapshots.
type etcdSnapshotsAnswer struct {
	Enabled   bool                       `json:"enabled"`
	Target    string                     `json:"target"`
	Last      *provisioner.EtcdSnapshot  `json:"last"`
	Requested bool                       `json:"requested"`
	Snapshots []provisioner.EtcdSnapshot `json:"snapshots"`
	Message   string                     `json:"message"`
}

func (o *clusterOptions) etcdSnapshotsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "etcd-snapshots",
		Short: "List, take, download and restore a cluster's etcd snapshots",
	}
	o.namespaceFlag(cmd.PersistentFlags())
	list := &cobra.Command{
		Use:   "list NAME",
		Short: "Show where the cluster's etcd snapshots are kept, and list them newest first",
		Args:  argsSay(cobra.ExactArgs(1), clusterNameArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.listEtcdSnapshots(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	take := &cobra.Command{
		Use:   "take NAME",
		Short: "Have the controller take a snapshot of the cluster's etcd now",
		Args:  argsSay(cobra.ExactArgs(1), clusterNameArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := o.clusterPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if _, err := o.call(cmd.Context(), http.MethodPost, path+"/etcd-snapshots", nil); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cluster %s: etcd snapshot asked for\n", args[0])
			return nil
		},
	}
	var out string
	download := &cobra.Command{
		Use:   "download NAME SNAPSHOT --out FILE",
		Short: "Download a snapshot, decrypted, for a restore by hand",
		Long: `Download a snapshot of the cluster's etcd, decrypted. It holds every Secret of the cluster, so the
file is written for its owner alone, and an existing file is never overwritten.`,
		Args: argsSay(cobra.ExactArgs(2), "give the cluster and the snapshot, as list names it"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return errors.New("--out is required: the file to write the snapshot to")
			}
			return o.downloadEtcdSnapshot(cmd.Context(), cmd.OutOrStdout(), args[0], args[1], out)
		},
	}
	download.Flags().StringVar(&out, "out", "", "file to write the decrypted snapshot to")
	var yes bool
	restore := &cobra.Command{
		Use:   "restore NAME SNAPSHOT --yes",
		Short: "Take the cluster's etcd back to a snapshot",
		Long: `Have the controller take the cluster's etcd back to a snapshot. Every object of the cluster returns
to what the snapshot holds, and what changed since is lost.`,
		Args: argsSay(cobra.ExactArgs(2), "give the cluster and the snapshot, as list names it"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("a restore loses every change since the snapshot: add --yes")
			}
			path, err := o.clusterPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			req := map[string]string{"snapshot": args[1], "confirm": args[0]}
			if _, err := o.call(cmd.Context(), http.MethodPost, path+"/etcd-restores", req); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cluster %s: restore from %s asked for\n", args[0], args[1])
			return nil
		},
	}
	restore.Flags().BoolVar(&yes, "yes", false, "confirm the restore, which loses every change since the snapshot")
	cmd.AddCommand(list, take, download, restore)
	return cmd
}

// removedEtcdSnapshotsCommand reaches the snapshots the backup volume keeps of removed clusters, as
// the interface's Backups view does, through the same routes and the same typed-name rule.
func (o *clusterOptions) removedEtcdSnapshotsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "removed-etcd-snapshots",
		Short: "List and delete the etcd snapshots the backup volume keeps of removed clusters",
	}
	cmd.PersistentFlags().StringVarP(&o.namespace, "namespace", "n", "", "the namespace the removed clusters were in")
	list := &cobra.Command{
		Use:   "list --namespace NAMESPACE",
		Short: "List the removed clusters of a namespace whose snapshots the backup volume keeps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.listRemovedEtcdSnapshots(cmd.Context(), cmd.OutOrStdout())
		},
	}
	var yes bool
	del := &cobra.Command{
		Use:   "delete NAME --namespace NAMESPACE --yes",
		Short: "Delete a removed cluster's snapshots and its backup keys",
		Long: `Delete a removed cluster's etcd snapshots on the backup volume and its backup keys, which the
snapshots open with. They cannot be brought back.`,
		Args: argsSay(cobra.ExactArgs(1), "give the removed cluster, as list names it"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("deleting the snapshots and backup keys cannot be undone: add --yes")
			}
			if o.namespace == "" {
				return errNoRemovedNamespace
			}
			path := "/api/removed-etcd-snapshots/" + url.PathEscape(o.namespace) + "/" + url.PathEscape(args[0])
			body, err := o.call(cmd.Context(), http.MethodDelete, path, map[string]string{"confirm": args[0]})
			if err != nil {
				return err
			}
			var a struct {
				Deleted int `json:"deleted"`
			}
			if err := json.Unmarshal(body, &a); err != nil {
				return fmt.Errorf("reading the interface's answer: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed cluster %s: deleted %d snapshots and its backup keys\n", args[0], a.Deleted)
			return writeErr(err)
		},
	}
	del.Flags().BoolVar(&yes, "yes", false, "confirm the deletion, which cannot be undone")
	cmd.AddCommand(list, del)
	return cmd
}

// errNoRemovedNamespace asks for the namespace, which a removed cluster no longer shows.
var errNoRemovedNamespace = errors.New("--namespace is required: the namespace the removed cluster was in")

func (o *clusterOptions) listRemovedEtcdSnapshots(ctx context.Context, out io.Writer) error {
	if o.namespace == "" {
		return errNoRemovedNamespace
	}
	body, err := o.call(ctx, http.MethodGet, "/api/removed-etcd-snapshots/"+url.PathEscape(o.namespace), nil)
	if err != nil {
		return err
	}
	var removed []provisioner.RemovedCluster
	if err := json.Unmarshal(body, &removed); err != nil {
		return fmt.Errorf("reading the interface's answer: %w", err)
	}
	if len(removed) == 0 {
		_, err := fmt.Fprintf(out, "the backup volume keeps no snapshots of removed clusters in %s\n", o.namespace)
		return writeErr(err)
	}
	w := tabwriter.NewWriter(out, 0, 0, columnGap, ' ', 0)
	_, _ = fmt.Fprintln(w, "CLUSTER\tSNAPSHOTS\tNEWEST\tKEYS")
	for i := range removed {
		r := &removed[i]
		newest := "-"
		if len(r.Snapshots) > 0 {
			newest = r.Snapshots[0].Time.UTC().Format(time.RFC3339)
		}
		keys := "kept"
		if !r.Keys {
			keys = "gone"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", r.Name, len(r.Snapshots), newest, keys)
	}
	return writeErr(w.Flush())
}

func (o *clusterOptions) listEtcdSnapshots(ctx context.Context, out io.Writer, name string) error {
	path, err := o.clusterPath(ctx, name)
	if err != nil {
		return err
	}
	body, err := o.call(ctx, http.MethodGet, path+"/etcd-snapshots", nil)
	if err != nil {
		return err
	}
	var a etcdSnapshotsAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		return fmt.Errorf("reading the interface's answer: %w", err)
	}
	if !a.Enabled {
		_, err := fmt.Fprintf(out, "cluster %s takes no etcd snapshots\n", name)
		return writeErr(err)
	}
	kept := "the backup volume"
	if a.Target != "" {
		kept = "target " + a.Target
	}
	last := "none yet"
	if a.Last != nil {
		last = a.Last.Time.UTC().Format(time.RFC3339)
	}
	w := tabwriter.NewWriter(out, 0, 0, columnGap, ' ', 0)
	_, _ = fmt.Fprintf(w, "Kept on:\t%s\nLast taken:\t%s\n", kept, last)
	if a.Requested {
		_, _ = fmt.Fprintln(w, "Asked for:\ta snapshot, not taken yet")
	}
	if a.Message != "" {
		_, _ = fmt.Fprintf(w, "Snapshots:\t%s\n", a.Message)
	}
	if len(a.Snapshots) > 0 {
		_, _ = fmt.Fprintln(w, "\nSNAPSHOT\tTAKEN\tSIZE")
	}
	for i := range a.Snapshots {
		s := &a.Snapshots[i]
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\n", s.Name, s.Time.UTC().Format(time.RFC3339), s.Size)
	}
	return writeErr(w.Flush())
}

func (o *clusterOptions) downloadEtcdSnapshot(ctx context.Context, out io.Writer, name, snapshot, file string) error {
	path, err := o.clusterPath(ctx, name)
	if err != nil {
		return err
	}
	res, err := o.open(ctx, http.MethodGet, path+"/etcd-snapshots/"+url.PathEscape(snapshot), nil)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, snapshotMode) //nolint:gosec // the file the user names
	if err != nil {
		return fmt.Errorf("the snapshot's file: %w", err)
	}
	n, err := io.Copy(f, res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(file)
		return fmt.Errorf("downloading the snapshot: %w", err)
	}
	_, err = fmt.Fprintf(out, "wrote %d bytes to %s\n", n, file)
	return writeErr(err)
}

// writeErr names a failed write to the terminal.
func writeErr(err error) error {
	if err != nil {
		return fmt.Errorf("writing the answer: %w", err)
	}
	return nil
}
