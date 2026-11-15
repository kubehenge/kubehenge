package provisioner

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// deleted takes web's snapshots onto the volume, then deletes it with kubectl, with
// DeleteEtcdSnapshotsAnnotation set to answer unless that is empty.
func deleted(t *testing.T, answer string) (*etcdFixture, *fakeVolume, *events.FakeRecorder) {
	t.Helper()
	f := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true}`)))
	vol := newFakeVolume()
	f.targets.Volume = vol
	s := f.snapshotter(&fakeEtcd{data: etcdData(4096)}, etcdNow)
	recorder := events.NewFakeRecorder(8)
	s.Events = recorder
	reconcileEtcd(t, s)
	c := f.cluster()
	if !controllerutil.ContainsFinalizer(c, EtcdSnapshotsFinalizer) {
		t.Fatal("the cluster's deletion is not held for its snapshots")
	}
	if answer != "" {
		metav1.SetMetaDataAnnotation(&c.ObjectMeta, DeleteEtcdSnapshotsAnnotation, answer)
		if err := f.kube.Update(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.kube.Delete(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	reconcileEtcd(t, s)
	return f, vol, recorder
}

func clusterGone(t *testing.T, f *etcdFixture) bool {
	t.Helper()
	err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web"}, &clusterv1.Cluster{})
	return client.IgnoreNotFound(err) == nil && err != nil
}

// A cluster deleted without the annotation keeps its snapshots on the volume and its backup keys,
// and they show as a removed cluster's.
func TestADeletedClusterKeepsItsSnapshots(t *testing.T) {
	t.Parallel()
	f, vol, _ := deleted(t, "")
	if !clusterGone(t, f) {
		t.Fatal("the deletion was held")
	}
	if got := snapshotsIn(vol.store, volumeFolder); len(got) != 1 {
		t.Fatalf("the volume holds %q", got)
	}
	removed, err := RemovedClusters(context.Background(), f.kube, vol.store, "team-a")
	if err != nil || len(removed) != 1 || removed[0].Name != "web" || len(removed[0].Snapshots) != 1 || !removed[0].Keys {
		t.Fatalf("removed clusters %+v, %v", removed, err)
	}
	n, err := DeleteVolumeSnapshots(context.Background(), vol, "team-a", "web")
	if err != nil || n != 1 || len(vol.store.keys()) != 0 {
		t.Fatalf("deleted %d, left %q: %v", n, vol.store.keys(), err)
	}
}

// With the annotation, as kubectl or GitOps set it, the snapshots go with the cluster.
func TestADeletedClusterTakesItsSnapshotsWhenAsked(t *testing.T) {
	t.Parallel()
	f, vol, recorder := deleted(t, "true")
	if !clusterGone(t, f) {
		t.Fatal("the deletion was held")
	}
	if left := vol.store.keys(); len(left) != 0 {
		t.Fatalf("left %q on the volume", left)
	}
	found := false
	for len(recorder.Events) > 0 {
		found = found || strings.Contains(<-recorder.Events, "EtcdSnapshotsDeleted")
	}
	if !found {
		t.Error("no event says the snapshots were deleted")
	}
	keys := &corev1.Secret{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: BackupKeysSecretName("web")}, keys); err != nil {
		t.Errorf("the backup keys, which bucket backups open with, went: %v", err)
	}
}

// A sleeping volume holds the deletion until the appliance wakes it.
func TestADeletionWaitsForTheVolume(t *testing.T) {
	t.Parallel()
	f := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true}`)))
	vol := newFakeVolume()
	f.targets.Volume = vol
	s := f.snapshotter(&fakeEtcd{data: etcdData(4096)}, etcdNow)
	reconcileEtcd(t, s)
	c := f.cluster()
	metav1.SetMetaDataAnnotation(&c.ObjectMeta, DeleteEtcdSnapshotsAnnotation, "true")
	if err := f.kube.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	vol.asleep = true
	if res := reconcileEtcd(t, s); res.RequeueAfter != etcdSnapshotWait || clusterGone(t, f) {
		t.Fatalf("next look in %s, gone %v", res.RequeueAfter, clusterGone(t, f))
	}
	vol.asleep = false
	reconcileEtcd(t, s)
	if !clusterGone(t, f) || len(vol.store.keys()) != 0 {
		t.Fatalf("gone %v, left %q", clusterGone(t, f), vol.store.keys())
	}
}

// The annotation takes "true" alone, checked by the cluster webhook.
func TestValidateDeleteSnapshots(t *testing.T) {
	t.Parallel()
	for value, ok := range map[string]bool{"true": true, "yes": false, "": false} {
		c := etcdCluster(`{"enabled":true}`)
		metav1.SetMetaDataAnnotation(&c.ObjectMeta, DeleteEtcdSnapshotsAnnotation, value)
		if errs := validateDeleteSnapshots(c); (len(errs) == 0) != ok {
			t.Errorf("%q: %v", value, errs)
		}
	}
	if errs := validateDeleteSnapshots(etcdCluster(`{"enabled":true}`)); len(errs) != 0 {
		t.Errorf("without the annotation: %v", errs)
	}
}
