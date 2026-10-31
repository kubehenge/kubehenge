package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func storedPool(t *testing.T, h *harness, name string) clusterv1.MachineDeploymentTopology {
	t.Helper()
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: name}, c); err != nil {
		t.Fatal(err)
	}
	return c.Spec.Topology.Workers.MachineDeployments[0]
}

func TestCreateAutoscaled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	body := strings.Replace(createBody, `"workerReplicas":2,`, `"workerReplicas":2,"workerAutoscaling":{"min":0,"max":3},`, 1)
	expect(t, b.do("POST", "/api/clusters", body), http.StatusCreated)
	pool := storedPool(t, h, "web")
	if pool.Replicas != nil || pool.Metadata.Annotations[provisioner.AutoscalerMinAnnotation] != "0" || pool.Metadata.Annotations[provisioner.AutoscalerMaxAnnotation] != "3" {
		t.Fatalf("pool = %+v", pool)
	}
	for _, bounds := range []string{`{"min":3,"max":2}`, `{"min":0,"max":0}`, `{"min":-1,"max":2}`, `{"min":0,"max":101}`} {
		bad := strings.Replace(strings.Replace(body, `{"min":0,"max":3}`, bounds, 1), `"web"`, `"bad"`, 1)
		if rec := b.do("POST", "/api/clusters", bad); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "autoscaling:") {
			t.Errorf("%s: %d %s", bounds, rec.Code, rec.Body.String())
		}
	}
}

// TestChangeAutoscaling switches a pool's autoscaling on and off again: switched on, its replicas
// are the autoscaler's and refused to anyone else. Switched off, the pool keeps the workers it runs.
func TestChangeAutoscaling(t *testing.T) {
	t.Parallel()
	running := &clusterv1.MachineDeployment{
		Namespace: "default", Name: "one-md-0-abcde", Labels: map[string]string{
			clusterv1.ClusterNameLabel: "one", clusterv1.ClusterTopologyMachineDeploymentNameLabel: "md-0",
		},
		Spec: clusterv1.MachineDeploymentSpec{ClusterName: "one", Replicas: new(int32(3))},
	}
	other := running.DeepCopy()
	other.Name, other.Labels[clusterv1.ClusterNameLabel] = "two-md-0-fghij", "two"
	other.Spec.Replicas = new(int32(9))
	h := newHarness(t, cluster("default", "one"), running, other)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"autoscaling":{"md-0":{"min":1,"max":4}}}`), http.StatusOK)
	if pool := storedPool(t, h, "one"); pool.Replicas != nil || pool.Metadata.Annotations[provisioner.AutoscalerMaxAnnotation] != "4" {
		t.Fatalf("pool = %+v", pool)
	}

	rec := b.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	var view clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if p := view.WorkerPools[0]; p.Replicas != 3 || p.Autoscaling == nil || *p.Autoscaling != (poolAutoscaling{Min: 1, Max: 4}) {
		t.Fatalf("pool view = %+v", p)
	}

	if rec := b.do("PATCH", "/api/clusters/default/one", `{"workerPools":{"md-0":2}}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "pool md-0 is autoscaled") {
		t.Fatalf("fixing an autoscaled pool: %d %s", rec.Code, rec.Body.String())
	}
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"autoscaling":{"md-0":{"min":5,"max":4}}}`), http.StatusUnprocessableEntity)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"autoscaling":{"md-9":{"min":0,"max":4}}}`), http.StatusUnprocessableEntity)

	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"autoscaling":{"md-0":null}}`), http.StatusOK)
	pool := storedPool(t, h, "one")
	if pool.Replicas == nil || *pool.Replicas != 3 || len(pool.Metadata.Annotations) != 0 {
		t.Fatalf("switched off, pool = %+v", pool)
	}

	// Switching off and fixing the workers in one change.
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"autoscaling":{"md-0":{"min":0,"max":2}}}`), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"autoscaling":{"md-0":null},"workerPools":{"md-0":1}}`), http.StatusOK)
	if pool := storedPool(t, h, "one"); pool.Replicas == nil || *pool.Replicas != 1 {
		t.Fatalf("switched off and fixed, pool = %+v", pool)
	}
}

func TestSwitchingOffKeepsToTheBounds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	// No MachineDeployment yet, the pool goes back to its least.
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"autoscaling":{"md-0":{"min":2,"max":4}}}`), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"autoscaling":{"md-0":null}}`), http.StatusOK)
	if pool := storedPool(t, h, "one"); pool.Replicas == nil || *pool.Replicas != 2 {
		t.Fatalf("pool = %+v", pool)
	}
}

// TestAutoscalerPlacementNeedsAudit holds running the autoscaler inside a cluster, where its token
// reads the machines of every cluster, to users who may see all of their VMs.
func TestAutoscalerPlacementNeedsAudit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	h.accounts.passwords["ops@pve"] = "secret"
	h.accounts.grants["ops@pve"] = map[string][]string{"/pool/kubehenge/default.one": {"VM.Audit", "VM.Allocate"}}
	ops := h.browser()
	ops.signIn("ops@pve")
	expect(t, ops.do("PATCH", "/api/clusters/default/one", `{"variables":{"autoscaler":{"placement":"workload"}}}`), http.StatusForbidden)
	expect(t, ops.do("PATCH", "/api/clusters/default/one", `{"variables":{"autoscaler":{"placement":"management","expander":"random"}}}`), http.StatusOK)

	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("PATCH", "/api/clusters/default/one", `{"variables":{"autoscaler":{"placement":"workload"}}}`), http.StatusOK)
	// Once inside, other settings may change without the right to put it there.
	expect(t, ops.do("PATCH", "/api/clusters/default/one", `{"variables":{"autoscaler":{"placement":"workload","expander":"random"}}}`), http.StatusOK)
}
