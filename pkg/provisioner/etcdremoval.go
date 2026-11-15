package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// DeleteEtcdSnapshotsAnnotation on a Cluster, "true", has its etcd snapshots on the backup volume
// deleted with it. Without it they stay, with the cluster's backup keys, until deleted in the
// Backups view or with the CLI.
const DeleteEtcdSnapshotsAnnotation = "kubehenge.dev/delete-etcd-snapshots"

// EtcdSnapshotsFinalizer holds a cluster's deletion until the snapshotter has done what
// DeleteEtcdSnapshotsAnnotation asks. Finalizers cannot be added to a cluster being deleted, so
// every cluster carries it.
const EtcdSnapshotsFinalizer = "kubehenge.dev/etcd-snapshots"

// validateDeleteSnapshots holds the annotation to the one value it takes.
func validateDeleteSnapshots(c *clusterv1.Cluster) field.ErrorList {
	v, ok := c.Annotations[DeleteEtcdSnapshotsAnnotation]
	if !ok || v == "true" {
		return nil
	}
	path := field.NewPath("metadata", "annotations").Key(DeleteEtcdSnapshotsAnnotation)
	return field.ErrorList{field.Invalid(path, v, `set it to "true" to delete the cluster's etcd snapshots on the backup volume with it, or leave it out to keep them`)}
}

// removed does what a cluster being deleted asks of its snapshots on the volume, then lets the
// deletion go on.
func (s *EtcdSnapshotter) removed(ctx context.Context, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, EtcdSnapshotsFinalizer) {
		return ctrl.Result{}, nil
	}
	if cluster.Annotations[DeleteEtcdSnapshotsAnnotation] == "true" {
		n, err := DeleteVolumeSnapshots(ctx, s.Targets.Volume, cluster.Namespace, cluster.Name)
		if errors.Is(err, ErrVolumeAsleep) {
			return ctrl.Result{RequeueAfter: etcdSnapshotWait}, nil
		}
		if err != nil {
			s.eventf(cluster, corev1.EventTypeWarning, "EtcdSnapshotsNotDeleted", "deleting the etcd snapshots on the backup volume: %v", err)
			return ctrl.Result{RequeueAfter: etcdSnapshotRetry}, nil
		}
		s.eventf(cluster, corev1.EventTypeNormal, "EtcdSnapshotsDeleted", "deleted %d etcd snapshots on the backup volume", n)
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	controllerutil.RemoveFinalizer(cluster, EtcdSnapshotsFinalizer)
	if err := s.Client.Patch(ctx, cluster, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("letting the deletion go on: %w", err)
	}
	return ctrl.Result{}, nil
}

// holdDeletion puts EtcdSnapshotsFinalizer on the cluster.
func (s *EtcdSnapshotter) holdDeletion(ctx context.Context, cluster *clusterv1.Cluster) error {
	if controllerutil.ContainsFinalizer(cluster, EtcdSnapshotsFinalizer) {
		return nil
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	controllerutil.AddFinalizer(cluster, EtcdSnapshotsFinalizer)
	if err := s.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("holding the cluster's deletion for its snapshots: %w", err)
	}
	return nil
}

// VolumeFolder is the folder of a cluster's snapshots on the backup volume.
func VolumeFolder(namespace, cluster string) string {
	return EtcdSnapshotPrefix(volumeFolderPrefix, namespace, cluster)
}

// DeleteVolumeSnapshots deletes every object in a cluster's folder on the backup volume, and tells
// how many snapshots went. It takes the volume's request away again.
func DeleteVolumeSnapshots(ctx context.Context, volume BackupVolume, namespace, cluster string) (int, error) {
	if volume == nil {
		return 0, errors.New("the backup volume is not reached here")
	}
	store, err := volume.Open(ctx)
	if err != nil {
		return 0, err //nolint:wrapcheck // ErrVolumeAsleep is read as it is
	}
	defer func() { _ = volume.Done(context.WithoutCancel(ctx)) }()
	folder := VolumeFolder(namespace, cluster)
	objects, err := store.List(ctx, folder+"/")
	if err != nil {
		return 0, fmt.Errorf("listing the snapshots: %w", err)
	}
	n := 0
	for _, o := range objects {
		if err := store.Remove(ctx, o.Key); err != nil {
			return n, fmt.Errorf("deleting %s: %w", o.Key, err)
		}
		if _, ok := snapshotOf(o, folder); ok {
			n++
		}
	}
	return n, nil
}

// RemovedCluster is a removed cluster whose snapshots the backup volume still holds.
type RemovedCluster struct {
	Namespace string         `json:"namespace"`
	Name      string         `json:"name"`
	Snapshots []EtcdSnapshot `json:"snapshots"`
	// Keys is set while the cluster's backup keys, which the snapshots open with, are kept.
	Keys bool `json:"keys"`
}

// RemovedClusters lists the folders on the backup volume of clusters in namespace that no longer
// exist.
func RemovedClusters(ctx context.Context, kube client.Reader, store ObjectStore, namespace string) ([]RemovedCluster, error) {
	objects, err := store.List(ctx, volumeFolderPrefix+"/"+namespace+".")
	if err != nil {
		return nil, fmt.Errorf("listing the backup volume: %w", err)
	}
	seen := map[string]bool{}
	var out []RemovedCluster
	for _, o := range objects {
		name, _, ok := strings.Cut(o.Key[len(volumeFolderPrefix)+len(namespace)+2:], "/")
		if !ok || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &clusterv1.Cluster{})
		if err == nil {
			continue
		}
		if client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("reading cluster %s: %w", name, err)
		}
		snapshots, err := ListEtcdSnapshots(ctx, store, VolumeFolder(namespace, name))
		if err != nil {
			return nil, err
		}
		keys := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: BackupKeysSecretName(name)}, &corev1.Secret{}) == nil
		out = append(out, RemovedCluster{Namespace: namespace, Name: name, Snapshots: snapshots, Keys: keys})
	}
	return out, nil
}
