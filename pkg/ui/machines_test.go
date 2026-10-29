package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func clusterMachine(cluster, name string) *clusterv1.Machine {
	return &clusterv1.Machine{
		Namespace: "default", Name: name,
		Labels: map[string]string{clusterv1.ClusterNameLabel: cluster, clusterv1.MachineControlPlaneLabel: ""},
	}
}

func TestMachinesAreMarkedForReplacementAndKept(t *testing.T) {
	t.Parallel()
	mine, theirs := clusterMachine("one", "one-cp-1"), clusterMachine("two", "two-cp-1")
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"), mine, theirs)
	marked := func(m *clusterv1.Machine) bool {
		t.Helper()
		got := &clusterv1.Machine{}
		if err := h.kube.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
			t.Fatal(err)
		}
		_, ok := got.Annotations[clusterv1.RemediateMachineAnnotation]
		return ok
	}
	const path = "/api/clusters/default/one/machines/one-cp-1/replacement"

	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("PUT", path, ""), http.StatusForbidden)

	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("PUT", path, ""), http.StatusNoContent)
	if !marked(mine) {
		t.Fatal("the machine is not marked for replacement")
	}
	rec := dev.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	var v struct {
		Machines []machineView `json:"machines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Machines) != 1 || !v.Machines[0].Replacing {
		t.Errorf("machines after marking %+v", v.Machines)
	}

	expect(t, dev.do("DELETE", path, ""), http.StatusNoContent)
	if marked(mine) {
		t.Error("the mark stays after taking it back")
	}

	// A machine is reached only through its own cluster.
	expect(t, dev.do("PUT", "/api/clusters/default/one/machines/two-cp-1/replacement", ""), http.StatusNotFound)
	expect(t, dev.do("PUT", "/api/clusters/default/one/machines/one-cp-9/replacement", ""), http.StatusNotFound)
	if marked(theirs) {
		t.Error("another cluster's machine is marked")
	}
}

func TestAMarkedMachineSaysWhatHoldsItsReplacementBack(t *testing.T) {
	t.Parallel()
	m := clusterMachine("one", "one-cp-1")
	m.Annotations = map[string]string{clusterv1.RemediateMachineAnnotation: ""}
	m.Status.Conditions = []metav1.Condition{
		{Type: clusterv1.MachineReadyCondition, Status: metav1.ConditionTrue},
		{
			Type: clusterv1.MachineOwnerRemediatedCondition, Status: metav1.ConditionFalse,
			Message: "KubeadmControlPlane can't remediate this machine because this could result in etcd loosing quorum",
		},
	}
	h := newHarness(t, cluster("default", "one"), m)
	b := h.browser()
	b.signIn("viewer@pve")
	rec := b.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	var v struct {
		Machines []machineView `json:"machines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Machines) != 1 || !v.Machines[0].Replacing || v.Machines[0].Waiting != m.Status.Conditions[1].Message {
		t.Errorf("machines %+v", v.Machines)
	}
}
