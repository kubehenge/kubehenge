package ui

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

type uiVolume struct {
	store  *uiStore
	asleep bool
}

type uiVolumeStore struct{ *uiStore }

func (uiVolumeStore) Room(context.Context) (int64, int64, error) { return 10 << 30, 20 << 30, nil }

func (v *uiVolume) Open(context.Context, string) (provisioner.VolumeStore, error) { //nolint:ireturn // BackupVolume's own signature
	if v.asleep {
		return nil, provisioner.ErrVolumeAsleep
	}
	return uiVolumeStore{v.store}, nil
}

func (*uiVolume) Done(context.Context, string) error { return nil }

// onTheVolume moves cluster one's snapshots from the bucket onto the backup volume, with no target
// set.
func onTheVolume(t *testing.T) (*etcdHarness, *uiVolume) {
	t.Helper()
	h := etcdSnapshotting(t)
	c := h.cluster()
	for i := range c.Spec.Topology.Variables {
		if c.Spec.Topology.Variables[i].Name == "etcdSnapshots" {
			c.Spec.Topology.Variables[i].Value.Raw = []byte(`{"enabled":true,"keep":5}`)
		}
	}
	if err := h.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	vol := &uiVolume{store: h.store}
	for _, key := range slices.Collect(maps.Keys(h.store.objects)) {
		o := h.store.objects[key]
		moved := "clusters/default.one/etcd/" + strings.TrimPrefix(key, uiSnapshotFolder+"/")
		o.Key = moved
		h.store.objects[moved], h.store.data[moved] = o, h.store.data[key]
		delete(h.store.objects, key)
		delete(h.store.data, key)
	}
	h.server.OpenStore = func(*provisioner.BackupTarget, map[string][]byte) (provisioner.ObjectStore, error) {
		t.Error("a bucket was opened for snapshots on the backup volume")
		return h.store, nil
	}
	h.server.Volume = vol
	return h, vol
}

// The Backups view lists the snapshots on the backup volume, downloads one for a restore by hand,
// and asks the controller to restore from the volume.
func TestEtcdSnapshotsOnTheVolume(t *testing.T) {
	t.Parallel()
	h, _ := onTheVolume(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/etcd-snapshots", "")
	expect(t, rec, http.StatusOK)
	v := etcdViewOf(t, rec.Body.Bytes())
	if !v.Enabled || v.Target != "" || len(v.Snapshots) != 3 || v.Message != "" {
		t.Fatalf("view %+v", v)
	}
	rec = b.do("GET", "/api/clusters/default/one/etcd-snapshots/"+h.name, "")
	expect(t, rec, http.StatusOK)
	if !bytes.Equal(rec.Body.Bytes(), h.plain) {
		t.Errorf("downloaded %q", rec.Body)
	}
	h.setClass(map[string]string{provisioner.AutomaticRestoreAnnotation: "true"})
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-restores", `{"snapshot":"`+h.name+`","confirm":"one"}`), http.StatusAccepted)
	req, ok, err := provisioner.EtcdRestoreRequestOf(h.cluster())
	if !ok || err != nil || req.Snapshot != h.name || req.Target != provisioner.VolumeTarget {
		t.Errorf("request %+v, %v", req, err)
	}
}

// While the volume sleeps the view says it is waking, and a restore waits for it.
func TestEtcdSnapshotsOnASleepingVolume(t *testing.T) {
	t.Parallel()
	h, vol := onTheVolume(t)
	vol.asleep = true
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/etcd-snapshots", "")
	expect(t, rec, http.StatusOK)
	if v := etcdViewOf(t, rec.Body.Bytes()); len(v.Snapshots) != 0 || !strings.Contains(v.Message, "waking") {
		t.Errorf("view %+v", v)
	}
	h.setClass(map[string]string{provisioner.AutomaticRestoreAnnotation: "true"})
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-restores", `{"snapshot":"`+h.name+`","confirm":"one"}`), http.StatusConflict)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := provisioner.EtcdRestoreRequestOf(c); ok {
		t.Error("a restore was asked for from a volume that was not read")
	}
}
