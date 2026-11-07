package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestCreateCarriesTheTimeoutsItIsGiven(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	body := strings.Replace(createBody, `"workerReplicas":2,`,
		`"workerReplicas":2,"controlPlaneTimeouts":{"nodeDrainTimeoutSeconds":1200,"nodeDeletionTimeoutSeconds":0},"workerTimeouts":{"nodeVolumeDetachTimeoutSeconds":300},`, 1)
	expect(t, b.do("POST", "/api/clusters", body), http.StatusCreated)
	c := storedCluster(t, h, "web")
	cp, pool := c.Spec.Topology.ControlPlane.Deletion, c.Spec.Topology.Workers.MachineDeployments[0].Deletion
	if cp.NodeDrainTimeoutSeconds == nil || *cp.NodeDrainTimeoutSeconds != 1200 || cp.NodeDeletionTimeoutSeconds == nil || *cp.NodeDeletionTimeoutSeconds != 0 || cp.NodeVolumeDetachTimeoutSeconds != nil {
		t.Errorf("control plane deletion = %+v", cp)
	}
	if pool.NodeVolumeDetachTimeoutSeconds == nil || *pool.NodeVolumeDetachTimeoutSeconds != 300 || pool.NodeDrainTimeoutSeconds != nil {
		t.Errorf("pool deletion = %+v", pool)
	}
}

func TestCreateRefusesTimeoutsOutOfRange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	for _, field := range []string{
		`"controlPlaneTimeouts":{"nodeDrainTimeoutSeconds":-1}`,
		`"workerTimeouts":{"nodeVolumeDetachTimeoutSeconds":86401}`,
		`"workerTimeouts":{"nodeDeletionTimeoutSeconds":-30}`,
	} {
		bad := strings.Replace(createBody, `"workerReplicas":2,`, `"workerReplicas":2,`+field+`,`, 1)
		if rec := b.do("POST", "/api/clusters", bad); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), provisioner.NodeTimeoutRule) {
			t.Errorf("%s: %d %s", field, rec.Code, rec.Body.String())
		}
	}
}

func TestChangeSetsAndClearsTimeouts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("PATCH", "/api/clusters/default/one",
		`{"controlPlaneTimeouts":{"nodeDrainTimeoutSeconds":1800},"pools":{"md-0":{"timeouts":{"nodeDrainTimeoutSeconds":600,"nodeVolumeDetachTimeoutSeconds":120}}}}`)
	expect(t, rec, http.StatusOK)
	var view clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if d := view.ControlPlaneTimeouts.Drain; d == nil || *d != 1800 {
		t.Errorf("control plane timeouts shown = %+v", view.ControlPlaneTimeouts)
	}
	if p := view.WorkerPools[0].Timeouts; p.Drain == nil || *p.Drain != 600 || p.VolumeDetach == nil || *p.VolumeDetach != 120 {
		t.Errorf("pool timeouts shown = %+v", p)
	}
	c := storedCluster(t, h, "one")
	if d := c.Spec.Topology.Workers.MachineDeployments[0].Deletion.NodeDrainTimeoutSeconds; d == nil || *d != 600 {
		t.Errorf("stored pool drain = %v", d)
	}

	// An empty object takes the class's timeouts again.
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"controlPlaneTimeouts":{},"pools":{"md-0":{"timeouts":{}}}}`), http.StatusOK)
	c = storedCluster(t, h, "one")
	if provisioner.NodeTimeoutsOf(&c.Spec.Topology.ControlPlane.Deletion) != (provisioner.NodeTimeouts{}) ||
		provisioner.NodeTimeoutsOfPool(&c.Spec.Topology.Workers.MachineDeployments[0].Deletion) != (provisioner.NodeTimeouts{}) {
		t.Errorf("after clearing: %+v", c.Spec.Topology)
	}

	for _, body := range []string{
		`{"controlPlaneTimeouts":{"nodeDrainTimeoutSeconds":-5}}`,
		`{"pools":{"md-0":{"timeouts":{"nodeVolumeDetachTimeoutSeconds":999999}}}}`,
	} {
		if rec := b.do("PATCH", "/api/clusters/default/one", body); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), provisioner.NodeTimeoutRule) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestATimeoutChangeReplacesNoMachine(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("POST", "/api/clusters/default/one/plan",
		`{"controlPlaneTimeouts":{"nodeDrainTimeoutSeconds":60},"pools":{"md-0":{"timeouts":{"nodeDrainTimeoutSeconds":60}}}}`)
	expect(t, rec, http.StatusOK)
	var plan planView
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 || plan.Changes[0].Effect != effectNone || plan.Changes[1].Effect != effectNone {
		t.Errorf("changes = %+v", plan.Changes)
	}
}

func TestClassesShowTheirTimeouts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/classes", "")
	expect(t, rec, http.StatusOK)
	var classes []classView
	if err := json.Unmarshal(rec.Body.Bytes(), &classes); err != nil {
		t.Fatal(err)
	}
	if len(classes) == 0 {
		t.Fatal("no classes")
	}
	c := classes[0]
	if d := c.ControlPlaneTimeouts.Drain; d == nil || *d != 3600 {
		t.Errorf("control plane timeouts = %+v", c.ControlPlaneTimeouts)
	}
	if d := c.WorkerTimeouts.VolumeDetach; d == nil || *d != 600 || c.WorkerTimeouts.Drain != nil {
		t.Errorf("worker timeouts = %+v", c.WorkerTimeouts)
	}
}
