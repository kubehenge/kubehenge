package ui

import (
	"encoding/json"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// proxmoxMachineWithVM is a ProxmoxMachine of cluster "one" with the guest ID capmox recorded.
func proxmoxMachineWithVM(name string, vmid int64) client.Object {
	pm := &unstructured.Unstructured{}
	pm.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1alpha2")
	pm.SetKind("ProxmoxMachine")
	pm.SetNamespace("default")
	pm.SetName(name)
	pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "one"})
	_ = unstructured.SetNestedField(pm.Object, vmid, "spec", "virtualMachineID")
	return pm
}

func joinedMachine(name, infra, node string, controlPlane bool) *clusterv1.Machine {
	m := &clusterv1.Machine{}
	m.Namespace, m.Name = "default", name
	m.Labels = map[string]string{clusterv1.ClusterNameLabel: "one"}
	if controlPlane {
		m.Labels[clusterv1.MachineControlPlaneLabel] = ""
	}
	m.Spec.ClusterName = "one"
	m.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{Kind: "ProxmoxMachine", APIGroup: "infrastructure.cluster.x-k8s.io", Name: infra}
	m.Status.NodeRef.Name = node
	return m
}

// A VM that Proxmox VE lists without a name is found by the guest ID on its machine's
// ProxmoxMachine. Two of them stay two VMs, and a machine nothing matches shows as not reporting.
func TestNamelessVMsAreFoundByTheirGuestID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"),
		joinedMachine("one-cp-a", "one-cp-a-x1", "kube-cp-a", true),
		joinedMachine("one-md-a", "one-md-a-y2", "kube-md-a", false),
		joinedMachine("one-md-b", "one-md-b-z3", "kube-md-b", false),
		joinedMachine("one-md-c", "one-md-c-w4", "", false),
		proxmoxMachineWithVM("one-cp-a-x1", 201), proxmoxMachineWithVM("one-md-a-y2", 202), proxmoxMachineWithVM("one-md-b-z3", 203),
	)
	h.accounts.members = []pve.Member{
		{VMID: 201, Node: "pve2", Type: "qemu", Status: "unknown"},
		{VMID: 202, Node: "pve1", Type: "qemu", Status: "unknown"},
		{VMID: 90, Node: "pve3", Type: "qemu", Name: "other", Status: "running"},
	}
	h.server.ProxmoxURL = "https://pve.example.org:8006"
	b := h.browser()
	b.signIn("admin@pve")
	rec := b.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	var v clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	byName := map[string]machineView{}
	for i := range v.Machines {
		byName[v.Machines[i].Name] = v.Machines[i]
	}
	if cp := byName["one-cp-a"]; cp.ProxmoxNode != "pve2" || cp.NotReporting || !cp.Reboot ||
		cp.Console != "https://pve.example.org:8006/?cmd=&console=kvm&node=pve2&novnc=1&resize=off&vmid=201&vmname=one-cp-a-x1" {
		t.Errorf("control plane = %+v", cp)
	}
	if a := byName["one-md-a"]; a.ProxmoxNode != "pve1" || a.NotReporting {
		t.Errorf("first worker = %+v", a)
	}
	// Its VM is not in the pool at all, and its node joined: nothing matches it.
	if m := byName["one-md-b"]; m.ProxmoxNode != "" || m.NotReporting || m.Reboot {
		t.Errorf("worker with no VM listed = %+v", m)
	}
	if m := byName["one-md-c"]; m.ProxmoxNode != "" || m.NotReporting {
		t.Errorf("worker still being created = %+v", m)
	}
	expect(t, b.do("POST", "/api/clusters/default/one/machines/one-md-a/reboot", ""), http.StatusAccepted)
	if len(h.accounts.rebooted) != 1 || h.accounts.rebooted[0] != 202 {
		t.Errorf("rebooted %v", h.accounts.rebooted)
	}
}

// A listed VM that no machine claims means a machine with nothing to match is the one that does not
// report.
func TestMachineWithNothingToMatchIsNotReporting(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"),
		joinedMachine("one-cp-a", "one-cp-a-x1", "kube-cp-a", true),
		joinedMachine("one-md-a", "one-md-a-y2", "kube-md-a", false),
		proxmoxMachineWithVM("one-cp-a-x1", 201),
	)
	h.accounts.members = []pve.Member{
		{VMID: 201, Node: "pve2", Type: "qemu", Status: "unknown"},
		{VMID: 202, Node: "pve1", Type: "qemu", Status: "unknown"},
	}
	b := h.browser()
	b.signIn("admin@pve")
	rec := b.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	var v clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	byName := map[string]machineView{}
	for i := range v.Machines {
		byName[v.Machines[i].Name] = v.Machines[i]
	}
	if m := byName["one-cp-a"]; m.ProxmoxNode != "pve2" || m.NotReporting {
		t.Errorf("matched machine = %+v", m)
	}
	if m := byName["one-md-a"]; m.ProxmoxNode != "" || !m.NotReporting || m.Reboot || m.Console != "" {
		t.Errorf("unmatched machine = %+v", m)
	}
}

// The health page gives a node the VM found by its guest ID, with its status as Proxmox VE says it:
// unknown, not stopped.
func TestHealthFindsNamelessVMs(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t, proxmoxMachineWithVM("one-cp-a-vm", 201), proxmoxMachineWithVM("one-md-0-a-vm", 202))
	h.accounts.members = []pve.Member{
		{VMID: 201, Node: "pve1", Type: "qemu", Status: "unknown"},
		{VMID: 202, Node: "pve2", Type: "qemu", Status: "unknown"},
	}
	b := h.browser()
	b.signIn("viewer@pve")
	v, _ := readHealthView(t, b)
	if len(v.Nodes.Items) != 2 {
		t.Fatalf("nodes = %+v", v.Nodes.Items)
	}
	cp, worker := v.Nodes.Items[0], v.Nodes.Items[1]
	if cp.VM == nil || cp.VM.ID != 201 || cp.VM.Node != "pve1" || cp.VM.Status != "unknown" {
		t.Errorf("control plane's VM = %+v", cp.VM)
	}
	if worker.VM == nil || worker.VM.ID != 202 || worker.VM.Node != "pve2" || worker.VM.Status != "unknown" {
		t.Errorf("worker's VM = %+v", worker.VM)
	}
}
