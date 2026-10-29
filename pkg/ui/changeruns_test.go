package ui

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// machineAt is a machine of cluster one: a control-plane machine or a worker of md-0, ready or not.
func machineAt(name, role string, ready metav1.ConditionStatus, created time.Time) clusterv1.Machine {
	labels := map[string]string{clusterv1.ClusterNameLabel: "one", clusterv1.ClusterTopologyMachineDeploymentNameLabel: "md-0"}
	if role == controlPlaneRole {
		labels = map[string]string{clusterv1.ClusterNameLabel: "one", clusterv1.MachineControlPlaneLabel: ""}
	}
	m := clusterv1.Machine{Name: name, Labels: labels, CreationTimestamp: metav1.NewTime(created)}
	m.Status.Conditions = []metav1.Condition{{Type: clusterv1.MachineReadyCondition, Status: ready, LastTransitionTime: metav1.NewTime(created)}}
	return m
}

func settled(c *clusterv1.Cluster, at time.Time) {
	c.Status.ObservedGeneration = c.Generation
	c.Status.Conditions = []metav1.Condition{
		{Type: clusterv1.ClusterTopologyReconciledCondition, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)},
		{Type: clusterv1.ClusterAvailableCondition, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)},
	}
}

func stepStates(v *runView) []string {
	out := make([]string, 0, len(v.Steps))
	for _, s := range v.Steps {
		out = append(out, s.Name+" "+s.State+" "+s.Detail)
	}
	return out
}

func TestAChangeRunsUntilTheMachinesItReplacesAreNewAndTheClusterSettles(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	c := cluster("default", "one")
	c.Generation = 4
	rec := changeRecord{By: "dev@pve", At: at, Settings: []plannedChange{{Setting: "podSecurity", Effect: effectControlPlane}}}
	old := machineAt("one-cp-old", controlPlaneRole, metav1.ConditionTrue, at.Add(-time.Hour))
	workers := []clusterv1.Machine{machineAt("one-w-1", workerRole, metav1.ConditionTrue, at.Add(-time.Hour)), machineAt("one-w-2", workerRole, metav1.ConditionTrue, at.Add(-time.Hour))}

	settled(c, at.Add(-time.Hour))
	c.Status.ObservedGeneration = 3
	v := changeRun(c, append([]clusterv1.Machine{old}, workers...), rec)
	want := []string{"Accepted by the API server done ", "Replace the control-plane machines running 0 of 1", "Settle waiting "}
	if got := stepStates(&v); v.State != runRunning || v.Phase != "Replace the control-plane machines: 0 of 1" || !slices.Equal(got, want) {
		t.Fatalf("before the rollout: %s %q %q", v.State, v.Phase, got)
	}

	// A new machine counts once it is ready.
	booting := machineAt("one-cp-new", controlPlaneRole, metav1.ConditionFalse, at.Add(4*time.Minute))
	v = changeRun(c, append([]clusterv1.Machine{old, booting}, workers...), rec)
	if got := stepStates(&v); got[1] != "Replace the control-plane machines running 0 of 1" {
		t.Fatalf("with the new machine booting: %q", got)
	}

	// The new machine is ready, the old one still there: still running.
	fresh := machineAt("one-cp-new", controlPlaneRole, metav1.ConditionTrue, at.Add(4*time.Minute))
	v = changeRun(c, append([]clusterv1.Machine{old, fresh}, workers...), rec)
	if got := stepStates(&v); v.State != runRunning || got[1] != "Replace the control-plane machines running 1 of 1" {
		t.Fatalf("with the old machine left: %s %q", v.State, got)
	}

	// The old one gone, the topology not reconciled yet: it settles.
	v = changeRun(c, append([]clusterv1.Machine{fresh}, workers...), rec)
	if got := stepStates(&v); v.State != runRunning || got[2] != "Settle running " {
		t.Fatalf("before the topology is reconciled: %s %q", v.State, got)
	}

	settled(c, at.Add(2*time.Minute))
	v = changeRun(c, append([]clusterv1.Machine{fresh}, workers...), rec)
	if v.State != runSucceeded || v.Finished == nil || !v.Finished.Equal(at.Add(4*time.Minute)) || v.Phase != "" {
		t.Fatalf("once settled: %s %v %q", v.State, v.Finished, v.Phase)
	}
	if v.By != "dev@pve" || !slices.Equal(v.Settings, []string{"podSecurity"}) || v.ID != "change/default/one" {
		t.Errorf("run %+v", v)
	}
}

func TestAChangeWhoseMachinesStalledSaysWhyAndIsKept(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	c := cluster("default", "one")
	rec := changeRecord{By: "dev@pve", At: at, Settings: []plannedChange{{Setting: "version", Effect: effectAll}}}
	machines := []clusterv1.Machine{
		machineAt("one-cp-old", controlPlaneRole, metav1.ConditionTrue, at.Add(-time.Hour)),
		machineAt("one-cp-new", controlPlaneRole, metav1.ConditionFalse, at.Add(time.Minute)),
	}
	why := "no machine has become ready since 2026-10-13 09:01 UTC; one-cp-new: VirtualMachineProvisioned: VM template not found"
	c.Status.Conditions = []metav1.Condition{{Type: provisioner.MachinesStalledCondition, Status: metav1.ConditionTrue, Reason: provisioner.StalledReason, Message: why}}

	v := changeRun(c, machines, rec)
	if v.State != runStalled || v.Error != why || v.Finished != nil {
		t.Fatalf("stalled change: %s %q %v", v.State, v.Error, v.Finished)
	}
	kept := recentRuns([]runView{v}, at.Add(2*time.Hour))
	if len(kept) != 1 || kept[0].State != runStalled {
		t.Errorf("a stalled run left the list: %+v", kept)
	}

	c.Status.Conditions[0].Status = metav1.ConditionFalse
	if v = changeRun(c, machines, rec); v.State != runRunning || v.Error != "" {
		t.Errorf("once moving again: %s %q", v.State, v.Error)
	}
}

func TestAClusterShowsWhatHoldsItUp(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	c.Status.Conditions = []metav1.Condition{
		{Type: provisioner.KubehengeReadyCondition, Status: metav1.ConditionFalse, Reason: provisioner.ReconcileFailedReason, Message: "Proxmox VE task UPID:pve1 failed: no space"},
		{Type: provisioner.MachinesStalledCondition, Status: metav1.ConditionTrue, Reason: provisioner.StalledReason, Message: "one-cp-1: VM template not found"},
	}
	if got := controllerError(c); got != "Proxmox VE task UPID:pve1 failed: no space" {
		t.Errorf("controllerError = %q", got)
	}
	if got := stalled(c); got != "one-cp-1: VM template not found" {
		t.Errorf("stalled = %q", got)
	}
	c.Status.Conditions[0].Status, c.Status.Conditions[1].Status = metav1.ConditionTrue, metav1.ConditionFalse
	if controllerError(c) != "" || stalled(c) != "" {
		t.Error("a cluster that is well still shows an error")
	}
}

func TestAChangeIsRecordedAndListedAsARun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	class := &clusterv1.ClusterClass{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	class.Spec.Patches = planPatches()
	if err := h.kube.Update(t.Context(), class); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"workerPools":{"md-0":3},"variables":{"logs":{"collector":"node"}}}`), http.StatusOK)

	kept := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, kept); err != nil {
		t.Fatal(err)
	}
	rec, ok := changeOf(kept)
	want := []plannedChange{{Setting: "workerPools", Effect: effectScales}, {Setting: "logs", Effect: effectWorkers}}
	if !ok || rec.By != "dev@pve" || !rec.At.Equal(h.clock) || !slices.Equal(rec.Settings, want) {
		t.Fatalf("recorded %+v, %v", rec, ok)
	}

	got := b.do("GET", "/api/runs", "")
	expect(t, got, http.StatusOK)
	var runs []runView
	if err := json.Unmarshal(got.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(runs, func(v runView) bool { return v.ID == "change/default/one" })
	if i < 0 {
		t.Fatalf("no change in the runs: %+v", runs)
	}
	steps := []string{"Accepted by the API server done ", "Replace the workers running 0 of 3", "Add or remove machines running 0 of 4 ready", "Settle waiting "}
	if r := runs[i]; r.State != runRunning || !slices.Equal(stepStates(&r), steps) {
		t.Errorf("the change's run = %s %q", r.State, stepStates(&r))
	}

	// A day on it is no run any more.
	h.clock = h.clock.Add(25 * time.Hour)
	b = h.browser()
	b.signIn("dev@pve")
	got = b.do("GET", "/api/runs", "")
	expect(t, got, http.StatusOK)
	if err := json.Unmarshal(got.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(runs, func(v runView) bool { return v.Kind == runChange }) {
		t.Errorf("a day later the runs still hold the change: %+v", runs)
	}
}
