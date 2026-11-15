package provisioner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// fakeVolume stands in for the backup volume: a store with a size and free bytes, asleep until
// woken.
type fakeVolume struct {
	store       *memStore
	free, size  int64
	asleep      bool
	opened      int
	released    int
	roomErr     error
	openedAwake int
}

type volumeMem struct {
	*memStore

	v *fakeVolume
}

func (m volumeMem) Room(context.Context) (int64, int64, error) {
	return m.v.free, m.v.size, m.v.roomErr
}

func (v *fakeVolume) Open(context.Context, string) (VolumeStore, error) { //nolint:ireturn // BackupVolume's own signature
	v.opened++
	if v.asleep {
		return nil, ErrVolumeAsleep
	}
	v.openedAwake++
	return volumeMem{memStore: v.store, v: v}, nil
}

func (v *fakeVolume) Done(context.Context, string) error {
	v.released++
	return nil
}

func newFakeVolume() *fakeVolume {
	return &fakeVolume{store: newMemStore(), free: 18 << 30, size: 20 << 30}
}

// volumeFolder is web's folder of snapshots on the volume.
const volumeFolder = "clusters/team-a.web/etcd"

func snapshotsIn(store *memStore, folder string) []string {
	var names []string
	for _, k := range store.keys() {
		if rest, ok := strings.CutPrefix(k, folder+"/"); ok && strings.HasSuffix(rest, EtcdSnapshotSuffix) {
			names = append(names, rest)
		}
	}
	slices.Sort(names)
	return names
}

func classed(c *clusterv1.Cluster, distribution string) *clusterv1.Cluster {
	c.Spec.Topology.ClassRef.Name = ClassPrefix + distribution
	return c
}

// Without a bucket target every distribution's snapshots land on the backup volume, in a folder of
// the cluster's own, and the oldest beyond keep go. A cluster that leaves the variable unset gets
// them too, by default. The volume is let go after each snapshot.
func TestEtcdSnapshotsGoToTheVolumeWithoutABucket(t *testing.T) {
	t.Parallel()
	for _, d := range []string{"kubeadm", "rke2", "k3s", "talos"} {
		for name, raw := range map[string]string{"unset": "", "on": `{"enabled":true,"keep":2}`} {
			c := classed(etcdCluster(`{"enabled":true,"keep":2}`), d)
			if raw == "" {
				c.Spec.Topology.Variables = slices.DeleteFunc(c.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool { return v.Name == etcdSnapshotsVariable })
			}
			f := newEtcdFixture(t, c)
			vol := newFakeVolume()
			f.targets.Volume = vol
			etcd := &fakeEtcd{data: etcdData(4096)}
			for i := range 3 {
				at := etcdNow.Add(time.Hour + time.Duration(i)*13*time.Hour)
				reconcileEtcd(t, f.snapshotter(etcd, at))
			}
			want := 2
			if raw == "" {
				want = 3
			}
			if got := snapshotsIn(vol.store, volumeFolder); len(got) != want {
				t.Errorf("%s, %s: the volume holds %q, want %d", d, name, got, want)
			}
			if len(f.store.keys()) != 0 {
				t.Errorf("%s, %s: a bucket got %q", d, name, f.store.keys())
			}
			if vol.released != vol.openedAwake || vol.released != 3 {
				t.Errorf("%s, %s: the volume was opened %d times and let go %d", d, name, vol.openedAwake, vol.released)
			}
			cond := meta.FindStatusCondition(f.cluster().Status.Conditions, EtcdSnapshotCondition)
			if cond == nil || cond.Reason != "Taken" {
				t.Errorf("%s, %s: condition %+v", d, name, cond)
			}
		}
	}
}

// With a bucket target the snapshots go to the bucket alone.
func TestEtcdSnapshotsGoToTheBucketWhenOneIsSet(t *testing.T) {
	t.Parallel()
	f := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true,"target":"store"}`)))
	vol := newFakeVolume()
	f.targets.Volume = vol
	reconcileEtcd(t, f.snapshotter(&fakeEtcd{data: etcdData(4096)}, etcdNow))
	if got := snapshotsIn(f.store, snapshotFolder); len(got) != 1 {
		t.Errorf("the bucket holds %q", got)
	}
	if vol.opened != 0 || len(vol.store.keys()) != 0 {
		t.Errorf("the volume was opened %d times and holds %q", vol.opened, vol.store.keys())
	}
}

// A volume that would keep less than a tenth free once the next snapshot is in refuses it, before
// etcd is read, and says so in the condition and an event.
func TestEtcdSnapshotsRefuseANearlyFullVolume(t *testing.T) {
	t.Parallel()
	f := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true}`)))
	vol := newFakeVolume()
	vol.free = 2<<30 + volumeFirstSnapshot - 1
	f.targets.Volume = vol
	etcd := &fakeEtcd{data: etcdData(4096)}
	s := f.snapshotter(etcd, etcdNow)
	recorder := events.NewFakeRecorder(4)
	s.Events = recorder
	if res := reconcileEtcd(t, s); res.RequeueAfter != etcdSnapshotRetry {
		t.Errorf("next look in %s", res.RequeueAfter)
	}
	cond := meta.FindStatusCondition(f.cluster().Status.Conditions, EtcdSnapshotCondition)
	if cond == nil || cond.Reason != EtcdSnapshotVolumeFull || !strings.Contains(cond.Message, "grow the volume") {
		t.Errorf("condition %+v", cond)
	}
	if etcd.calls.Load() != 0 || len(vol.store.keys()) != 0 {
		t.Errorf("etcd was read %d times into a full volume, which holds %q", etcd.calls.Load(), vol.store.keys())
	}
	if got := <-recorder.Events; !strings.Contains(got, "EtcdSnapshotFailed") || !strings.Contains(got, "nearly full") {
		t.Errorf("event %q", got)
	}
	if vol.released != 1 {
		t.Errorf("the full volume was let go %d times", vol.released)
	}

	// With one more byte free it takes the snapshot.
	vol.free++
	f2 := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true}`)))
	f2.targets.Volume = vol
	reconcileEtcd(t, f2.snapshotter(etcd, etcdNow))
	if got := snapshotsIn(vol.store, volumeFolder); len(got) != 1 {
		t.Errorf("with room the volume holds %q", got)
	}
}

// The next snapshot is taken as large as the largest kept.
func TestVolumeRoomCountsTheLargestSnapshot(t *testing.T) {
	t.Parallel()
	vol := newFakeVolume()
	vol.free, vol.size = 3<<30, 20<<30
	store := volumeMem{memStore: vol.store, v: vol}
	if _, err := store.Put(t.Context(), volumeFolder+"/a"+EtcdSnapshotSuffix, strings.NewReader(strings.Repeat("x", 1<<20)), nil); err != nil {
		t.Fatal(err)
	}
	if err := volumeRoom(t.Context(), store, volumeFolder); err != nil {
		t.Fatalf("room for a snapshot of 1 MiB: %v", err)
	}
	vol.free = 2<<30 + 1<<20 - 1
	if err := volumeRoom(t.Context(), store, volumeFolder); !errors.Is(err, ErrVolumeFull) {
		t.Fatalf("no room for a snapshot of 1 MiB: %v", err)
	}
}

// On a node's own storage the volume sleeps. The snapshot waits for the appliance to wake it, says
// so, and is taken at the next look once it is awake.
func TestEtcdSnapshotsWaitForASleepingVolume(t *testing.T) {
	t.Parallel()
	f := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true}`)))
	vol := newFakeVolume()
	vol.asleep = true
	f.targets.Volume = vol
	etcd := &fakeEtcd{data: etcdData(4096)}
	if res := reconcileEtcd(t, f.snapshotter(etcd, etcdNow)); res.RequeueAfter != etcdSnapshotWait {
		t.Errorf("next look in %s", res.RequeueAfter)
	}
	cond := meta.FindStatusCondition(f.cluster().Status.Conditions, EtcdSnapshotCondition)
	if cond == nil || cond.Reason != EtcdSnapshotWaitingForVolume || cond.Message != volumeWakingMessage || etcd.calls.Load() != 0 {
		t.Errorf("condition %+v, %d snapshots", cond, etcd.calls.Load())
	}
	if vol.released != 0 {
		t.Error("the request for the sleeping volume was taken away before the appliance woke it")
	}
	vol.asleep = false
	reconcileEtcd(t, f.snapshotter(etcd, etcdNow.Add(time.Minute)))
	if got := snapshotsIn(vol.store, volumeFolder); len(got) != 1 || vol.released != 1 {
		t.Errorf("once awake the volume holds %q and was let go %d times", got, vol.released)
	}
}

// A cluster whose etcd does not answer in its slot fails that snapshot with a condition and an
// event, lets the volume go, and is tried again later.
func TestEtcdSnapshotsOfAnUnreachableClusterTryAgain(t *testing.T) {
	t.Parallel()
	f := newEtcdFixture(t, etcdCluster(`{"enabled":true}`))
	vol := newFakeVolume()
	f.targets.Volume = vol
	etcd := &fakeEtcd{err: errors.New("dial tcp 10.0.0.5:6443: i/o timeout")}
	s := f.snapshotter(etcd, etcdNow.Add(12*time.Hour))
	recorder := events.NewFakeRecorder(4)
	s.Events = recorder
	if res := reconcileEtcd(t, s); res.RequeueAfter != etcdSnapshotRetry {
		t.Errorf("next look in %s", res.RequeueAfter)
	}
	cond := meta.FindStatusCondition(f.cluster().Status.Conditions, EtcdSnapshotCondition)
	if cond == nil || cond.Reason != "Failed" || !strings.Contains(cond.Message, "i/o timeout") {
		t.Errorf("condition %+v", cond)
	}
	if got := <-recorder.Events; !strings.Contains(got, "EtcdSnapshotFailed") {
		t.Errorf("event %q", got)
	}
	if vol.released != 1 {
		t.Errorf("the volume was let go %d times", vol.released)
	}
	etcd.err = nil
	etcd.data = etcdData(4096)
	reconcileEtcd(t, f.snapshotter(etcd, etcdNow.Add(12*time.Hour+etcdSnapshotRetry)))
	if got := snapshotsIn(vol.store, volumeFolder); len(got) != 1 {
		t.Errorf("the next try left %q", got)
	}
}

// The volume needs no target and no key. The distributions' own upload still needs a bucket.
func TestCheckEtcdSnapshotsOnTheVolume(t *testing.T) {
	t.Parallel()
	if err := checkEtcdSnapshots(nil, etcdCluster(`{"enabled":true}`), &clusterv1.ClusterClass{}); err != nil {
		t.Errorf("snapshots to the volume: %v", err)
	}
	if err := checkEtcdSnapshots(nil, etcdCluster(`{"enabled":true,"distributionUpload":true}`), &clusterv1.ClusterClass{}); err == nil || !strings.Contains(err.Error(), "needs a target") {
		t.Errorf("the distribution's upload to the volume: %v", err)
	}
	if missing, err := etcdCredentialMissing(t.Context(), newEtcdFixture(t).kube, etcdCluster(`{"enabled":true}`)); missing != "" || err != nil {
		t.Errorf("the volume waits for a key: %q %v", missing, err)
	}
}

// A restore names the volume with no target.
func TestARestoreRequestMayNameTheVolume(t *testing.T) {
	t.Parallel()
	c := etcdCluster(`{"enabled":true}`)
	c.Annotations = map[string]string{EtcdRestoreAnnotation: `{"snapshot":"20261021T120000Z-v1.36.5-rke2r1.db.age","at":"2026-10-21T12:00:00Z"}`}
	r, ok, err := EtcdRestoreRequestOf(c)
	if !ok || err != nil || r.Target != VolumeTarget {
		t.Errorf("request %+v, %v, %v", r, ok, err)
	}
}
