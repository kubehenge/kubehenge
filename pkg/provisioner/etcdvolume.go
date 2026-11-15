package provisioner

import (
	"context"
	"errors"
	"fmt"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// BackupVolume is the management cluster's backup volume, which keeps the etcd snapshots of every
// cluster whose etcdSnapshots name no target. The controller pulls each snapshot from the cluster's
// etcd and stores it there itself, so the management cluster opens nothing to workload networks.
type BackupVolume interface {
	// Open asks for the volume and returns its store once it is awake, or ErrVolumeAsleep.
	Open(ctx context.Context) (VolumeStore, error)
	// Done tells the volume that the controller no longer needs it awake.
	Done(ctx context.Context) error
}

// VolumeStore is the store on the backup volume.
type VolumeStore interface {
	ObjectStore
	// Room returns the bytes free on the volume and its size.
	Room(ctx context.Context) (free, size int64, err error)
}

// ErrVolumeAsleep answers while the backup volume sleeps on a node's own storage. The appliance
// wakes it once asked.
var ErrVolumeAsleep = errors.New("the backup volume sleeps until the appliance wakes it")

// VolumeTarget names the backup volume where a snapshot's target is asked for by name, as the
// empty name does in the etcdSnapshots variable and a restore request.
const VolumeTarget = ""

// volumeFolderPrefix holds the clusters' snapshots on the volume, apart from the management
// cluster's own.
const volumeFolderPrefix = "clusters"

// The reasons of the snapshot condition that are the volume's.
const (
	// EtcdSnapshotWaitingForVolume holds while the volume sleeps and the appliance has not woken it.
	EtcdSnapshotWaitingForVolume = "WaitingForVolume"
	// EtcdSnapshotVolumeFull refuses a snapshot the volume has no room for.
	EtcdSnapshotVolumeFull = "VolumeFull"
)

const (
	// volumeReserveShare is the share of the volume, in per cent, that cluster snapshots leave free,
	// for the management cluster's own backups.
	volumeReserveShare = 10
	// volumeFirstSnapshot is the room asked for a cluster's first snapshot, whose size is not known
	// yet.
	volumeFirstSnapshot = 64 << 20
)

// volumeWakingMessage says a snapshot or a restore waits for the volume.
const volumeWakingMessage = "Waiting for the appliance to wake the backup volume."

// ErrVolumeFull refuses a snapshot the volume has no room for.
var ErrVolumeFull = errors.New("the backup volume is nearly full")

// volumeRoom refuses a snapshot into folder when the volume would keep less than its reserve free
// once the snapshot is stored. The snapshot is taken as large as the largest kept in folder.
func volumeRoom(ctx context.Context, store VolumeStore, folder string) error {
	free, size, err := store.Room(ctx)
	if err != nil {
		return fmt.Errorf("reading the backup volume's free space: %w", err)
	}
	objects, err := store.List(ctx, folder+"/")
	if err != nil {
		return fmt.Errorf("listing the snapshots on the backup volume: %w", err)
	}
	var need int64 = volumeFirstSnapshot
	if len(objects) > 0 {
		need = 0
	}
	for _, o := range objects {
		need = max(need, o.Size)
	}
	reserve := size * volumeReserveShare / percent
	if free-need >= reserve {
		return nil
	}
	return fmt.Errorf("%w: %s free of %s, and the next snapshot needs about %s with %d%% kept free. Lower keep, or grow the volume", ErrVolumeFull, gibibytes(free), gibibytes(size), gibibytes(need), volumeReserveShare)
}

// gibibytes writes a size in GiB to one decimal.
func gibibytes(n int64) string {
	return fmt.Sprintf("%.1f GiB", float64(n)/gibibyte)
}

// openForSnapshot opens the cluster's target for a snapshot, and on the backup volume checks that
// the snapshot has room there.
func (s *EtcdSnapshotter) openForSnapshot(ctx context.Context, cluster *clusterv1.Cluster, target string) (*ClusterTarget, error) {
	t, err := s.Targets.Open(ctx, cluster, target, true)
	if err != nil || !t.OnVolume() {
		return t, err
	}
	if err := volumeRoom(ctx, t.volume, t.Folder); err != nil {
		s.release(ctx, t)
		return nil, err
	}
	return t, nil
}

// openVolume opens a cluster's folder on the backup volume with its keys.
func (ts *ClusterTargets) openVolume(ctx context.Context, cluster *clusterv1.Cluster, create bool) (*ClusterTarget, error) {
	if ts.Volume == nil {
		return nil, errors.New("the controller does not reach the backup volume")
	}
	keys, err := clusterKeys(ctx, ts.Client, cluster, create)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, errors.New("the cluster has no backup keys yet")
	}
	store, err := ts.Volume.Open(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // ErrVolumeAsleep is read as it is
	}
	return &ClusterTarget{
		Store: store, Folder: EtcdSnapshotPrefix(volumeFolderPrefix, cluster.Namespace, cluster.Name), Keys: keys,
		volume: store, done: ts.Volume.Done,
	}, nil
}

// OnVolume tells whether the target is the backup volume.
func (t *ClusterTarget) OnVolume() bool { return t.volume != nil }

// Release lets the backup volume sleep again once its target is done with. A bucket has nothing to
// release.
func (t *ClusterTarget) Release(ctx context.Context) error {
	if t.done == nil {
		return nil
	}
	return t.done(context.WithoutCancel(ctx))
}
