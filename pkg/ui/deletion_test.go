package ui

import (
	"encoding/json"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// deleting has cluster one deleted, held by a finalizer, with what the controller recorded it waits on.
func (h *etcdHarness) deleting() {
	h.t.Helper()
	c := h.cluster()
	c.Finalizers = append(c.Finalizers, provisioner.Finalizer)
	raw, err := json.Marshal(provisioner.DeletionStatus{
		Waiting:  []provisioner.DeletionWait{{Kind: "Machine", Name: "one-md-0-a", Reason: "Drain not completed yet"}},
		MayLeave: []string{"VM 101 on pve1"},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	metav1.SetMetaDataAnnotation(&c.ObjectMeta, provisioner.DeletionStatusAnnotation, string(raw))
	if err := h.kube.Update(h.t.Context(), c); err != nil {
		h.t.Fatal(err)
	}
	if err := h.kube.Delete(h.t.Context(), c); err != nil {
		h.t.Fatal(err)
	}
}

func TestAbandonDeletion(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	b := h.browser()
	b.signIn("dev@pve")
	body := `{"confirm":"one"}`

	expect(t, b.do("POST", "/api/clusters/default/one/abandon-deletion", body), http.StatusConflict)
	h.deleting()

	rec := b.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	var view clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Deletion == nil || len(view.Deletion.Waiting) != 1 || view.Deletion.MayLeave[0] != "VM 101 on pve1" {
		t.Errorf("deletion %+v", view.Deletion)
	}

	expect(t, b.do("POST", "/api/clusters/default/one/abandon-deletion", `{"confirm":"One"}`), http.StatusUnprocessableEntity)
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("POST", "/api/clusters/default/one/abandon-deletion", body), http.StatusForbidden)
	if provisioner.Abandoned(h.cluster()) {
		t.Fatal("abandoned before it was asked to be")
	}
	expect(t, b.do("POST", "/api/clusters/default/one/abandon-deletion", body), http.StatusAccepted)
	if c := h.cluster(); !provisioner.Abandoned(c) || c.Annotations[provisioner.AbandonAnnotation] != "one" {
		t.Errorf("annotations %v", c.Annotations)
	}
	expect(t, b.do("POST", "/api/clusters/default/one/abandon-deletion", body), http.StatusAccepted)
}

func TestHealthSaysTheDeletionStalled(t *testing.T) {
	t.Parallel()
	c := &clusterv1.Cluster{Name: "web", Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
		Type: provisioner.DeletionStalledCondition, Status: metav1.ConditionTrue, Reason: "Waiting",
		Message: "the deletion has waited 25m0s on Machine web-md-0-a. If it cannot finish, abandon it with Abandon on the cluster's page or: kubectl annotate",
	}}}}
	var a attention
	watchAttention(c, &a)
	if len(a) != 1 || a[0].Section != sectionClusterAPI || a[0].Message != "the deletion has waited 25m0s on Machine web-md-0-a." {
		t.Errorf("%+v", a)
	}
}
