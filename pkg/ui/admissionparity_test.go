package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// Each test here sends the interface a change it refuses, then puts the same change to the
// controller's admission validator, as kubectl or GitOps would reach it. Both have to refuse.

type twoNodes struct{}

func (twoNodes) OnlineNodes(context.Context) ([]provisioner.Node, error) {
	return []provisioner.Node{{Name: "pve1", CPULevel: 3}, {Name: "pve2", CPULevel: 3}}, nil
}

// admissionOver is the admission validator over the harness's objects, with two nodes online. It
// first proves itself on the harness's cluster: an unchanged cluster passes and four control planes
// do not.
func admissionOver(t *testing.T, h *harness, old *clusterv1.Cluster) *provisioner.ClusterValidator {
	t.Helper()
	v := &provisioner.ClusterValidator{Hooks: &provisioner.Hooks{Reader: h.kube, ClassNamespace: "kubehenge-system", Nodes: twoNodes{}}}
	if _, err := v.ValidateUpdate(t.Context(), old, old.DeepCopy()); err != nil {
		t.Fatalf("the harness's own cluster does not pass admission unchanged: %v", err)
	}
	four := int32(4)
	even := old.DeepCopy()
	even.Spec.Topology.ControlPlane.Replicas = &four
	if _, err := v.ValidateUpdate(t.Context(), old, even); err == nil {
		t.Fatal("the validator admits four control planes, so it proves nothing here")
	}
	return v
}

func TestInterfaceAndAdmissionBothRefuseAChangeToAClusterBeingDeleted(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	c.Finalizers = []string{"kubehenge.dev/parity"}
	h := newHarness(t, c.DeepCopy())
	stored := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(c), stored); err != nil {
		t.Fatal(err)
	}
	v := admissionOver(t, h, stored)
	if err := h.kube.Delete(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(c), stored); err != nil || stored.DeletionTimestamp.IsZero() {
		t.Fatalf("the cluster is not being deleted: %v", err)
	}
	b := h.browser()
	b.signIn("admin@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"controlPlaneReplicas":3}`), http.StatusConflict)

	scaled := stored.DeepCopy()
	three := int32(3)
	scaled.Spec.Topology.ControlPlane.Replicas = &three
	if _, err := v.ValidateUpdate(t.Context(), stored, scaled); err == nil {
		t.Error("admission admits three control planes on a cluster being deleted, which the interface refuses with 409")
	}
	unfinalized := stored.DeepCopy()
	unfinalized.Finalizers = nil
	if _, err := v.ValidateUpdate(t.Context(), stored, unfinalized); err != nil {
		t.Errorf("admission refuses the removal of a finalizer from a cluster being deleted: %v", err)
	}
}

func TestInterfaceAndAdmissionBothRefuseAChangedStorage(t *testing.T) {
	t.Parallel()
	old := cluster("default", "one")
	h := newHarness(t, old.DeepCopy())
	b := h.browser()
	b.signIn("admin@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"storage":"tank"}}`), http.StatusUnprocessableEntity)

	v := admissionOver(t, h, old)
	moved := old.DeepCopy()
	setVariable(moved, "storage", json.RawMessage(`"tank"`))
	if _, err := v.ValidateUpdate(t.Context(), old, moved); err == nil {
		t.Error("admission admits storage local-lvm changed to tank, which the interface refuses with 422")
	}
}
