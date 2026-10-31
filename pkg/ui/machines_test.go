package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
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

func TestAWorkerIsDeletedAndItsPoolShrinks(t *testing.T) {
	t.Parallel()
	worker := clusterMachine("one", "one-md-0-a")
	delete(worker.Labels, clusterv1.MachineControlPlaneLabel)
	worker.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel] = "md-0"
	controlPlane := clusterMachine("one", "one-cp-1")
	scaled := cluster("default", "two")
	scaled.Spec.Topology.Workers.MachineDeployments[0].Metadata.Annotations = map[string]string{
		provisioner.AutoscalerMinAnnotation: "1", provisioner.AutoscalerMaxAnnotation: "3",
	}
	scaledWorker := clusterMachine("two", "two-md-0-a")
	delete(scaledWorker.Labels, clusterv1.MachineControlPlaneLabel)
	scaledWorker.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel] = "md-0"
	h := newHarness(t, cluster("default", "one"), scaled, worker, controlPlane, scaledWorker)
	const path = "/api/clusters/default/one/machines/one-md-0-a"

	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("DELETE", path, ""), http.StatusForbidden)

	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("DELETE", path, ""), http.StatusNoContent)
	got := &clusterv1.Machine{}
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(worker), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Annotations[clusterv1.DeleteMachineAnnotation]; !ok {
		t.Error("the worker is not marked to go first")
	}
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	before := *cluster("default", "one").Spec.Topology.Workers.MachineDeployments[0].Replicas
	if n := *c.Spec.Topology.Workers.MachineDeployments[0].Replicas; n != before-1 {
		t.Errorf("md-0 has %d workers, want %d", n, before-1)
	}

	// A control plane is replaced, not deleted, and an autoscaled pool's workers are the autoscaler's.
	expect(t, dev.do("DELETE", "/api/clusters/default/one/machines/one-cp-1", ""), http.StatusConflict)
	expect(t, dev.do("DELETE", "/api/clusters/default/two/machines/two-md-0-a", ""), http.StatusConflict)
	expect(t, dev.do("DELETE", "/api/clusters/default/one/machines/two-md-0-a", ""), http.StatusNotFound)
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(scaledWorker), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Annotations[clusterv1.DeleteMachineAnnotation]; ok {
		t.Error("an autoscaled pool's worker is marked")
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
