package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// The remove dialog's choice reaches the controller as the annotation kubectl sets.
func TestDeleteClusterWithItsSnapshots(t *testing.T) {
	t.Parallel()
	h, _ := onTheVolume(t)
	c := h.cluster()
	controllerutil.AddFinalizer(c, provisioner.EtcdSnapshotsFinalizer)
	if err := h.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("DELETE", "/api/clusters/default/one", `{"confirm":"one","deleteEtcdSnapshots":true}`), http.StatusAccepted)
	if got := h.cluster().Annotations[provisioner.DeleteEtcdSnapshotsAnnotation]; got != "true" {
		t.Errorf("annotation %q", got)
	}
}

// A cluster removed with its snapshots kept shows in the Backups view, and they and its backup
// keys are deleted there on the typed name.
func TestRemovedClustersSnapshots(t *testing.T) {
	t.Parallel()
	h, vol := onTheVolume(t)
	b := h.browser()
	b.signIn("dev@pve")
	exists := b.do("DELETE", "/api/removed-etcd-snapshots/default/one", `{"confirm":"one"}`)
	expect(t, exists, http.StatusConflict)
	// The refusal names the cluster, the rule and the way to delete its snapshots.
	if msg := exists.Body.String(); !strings.Contains(msg, "cluster one still exists. Delete it with its etcd snapshots instead") {
		t.Errorf("refused with %s", msg)
	}
	rec := b.do("GET", "/api/removed-etcd-snapshots/default", "")
	expect(t, rec, http.StatusOK)
	if rec.Body.String() != "[]\n" {
		t.Fatalf("a cluster that exists is listed: %s", rec.Body)
	}
	if err := h.kube.Delete(t.Context(), h.cluster()); err != nil {
		t.Fatal(err)
	}
	rec = b.do("GET", "/api/removed-etcd-snapshots/default", "")
	expect(t, rec, http.StatusOK)
	var removed []provisioner.RemovedCluster
	if err := json.Unmarshal(rec.Body.Bytes(), &removed); err != nil || len(removed) != 1 || removed[0].Name != "one" || len(removed[0].Snapshots) != 3 || !removed[0].Keys {
		t.Fatalf("removed %+v, %v", removed, err)
	}
	vol.asleep = true
	expect(t, b.do("DELETE", "/api/removed-etcd-snapshots/default/one", `{"confirm":"one"}`), http.StatusConflict)
	vol.asleep = false
	expect(t, b.do("DELETE", "/api/removed-etcd-snapshots/default/one", `{"confirm":"two"}`), http.StatusUnprocessableEntity)
	expect(t, b.do("DELETE", "/api/removed-etcd-snapshots/default/one", `{"confirm":"one"}`), http.StatusOK)
	if len(h.store.objects) != 0 {
		t.Errorf("left %d objects on the volume", len(h.store.objects))
	}
	err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: provisioner.BackupKeysSecretName("one")}, &corev1.Secret{})
	if client.IgnoreNotFound(err) != nil || err == nil {
		t.Errorf("the backup keys: %v", err)
	}
}
