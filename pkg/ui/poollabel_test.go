package ui

import (
	"net/http"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestChangeTakesTheDefaultPoolsLabel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"workerPools":{"workers":4}}`), http.StatusOK)
	if pool := storedPool(t, h, "one"); pool.Replicas == nil || *pool.Replicas != 4 {
		t.Fatalf("pool = %+v, want the default pool at 4 workers", pool)
	}
	// kubectl users know the pool by its own name, which keeps working.
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"workerPools":{"md-0":5}}`), http.StatusOK)
	if pool := storedPool(t, h, "one"); pool.Replicas == nil || *pool.Replicas != 5 {
		t.Fatalf("pool = %+v, want the default pool at 5 workers", pool)
	}
}

// poolsByName are the stored pools of cluster one by name.
func poolsByName(t *testing.T, h *harness) map[string]clusterv1.MachineDeploymentTopology {
	t.Helper()
	pools := map[string]clusterv1.MachineDeploymentTopology{}
	mds := poolsOf(t, h)
	for i := range mds {
		pools[mds[i].Name] = mds[i]
	}
	return pools
}

// What the CLI's pool, timeouts and autoscale commands send all reach the default pool by its label.
func TestEveryPoolOfAChangeTakesTheDefaultPoolsLabel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	for _, tc := range []struct {
		body string
		want func(md clusterv1.MachineDeploymentTopology) bool
	}{
		{`{"pools":{"workers":{"replicas":3}}}`, func(md clusterv1.MachineDeploymentTopology) bool {
			return md.Replicas != nil && *md.Replicas == 3
		}},
		{`{"pools":{"Workers":{"timeouts":{"nodeDrainTimeoutSeconds":3600}}}}`, func(md clusterv1.MachineDeploymentTopology) bool {
			return provisioner.NodeTimeoutsOfPool(&md.Deletion).Drain != nil && *provisioner.NodeTimeoutsOfPool(&md.Deletion).Drain == 3600
		}},
		{`{"autoscaling":{"workers":{"min":1,"max":4}}}`, func(md clusterv1.MachineDeploymentTopology) bool {
			return md.Metadata.Annotations[provisioner.AutoscalerMaxAnnotation] == "4"
		}},
	} {
		expect(t, b.do("PATCH", "/api/clusters/default/one", tc.body), http.StatusOK)
		pools := poolsByName(t, h)
		if len(pools) != 1 || !tc.want(pools[workerPool]) {
			t.Errorf("after %s: pools %+v, want md-0 changed and no other pool", tc.body, pools)
		}
	}
}

func TestAPoolNamedWorkersKeepsItsName(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	c.Spec.Topology.Workers.MachineDeployments = append(c.Spec.Topology.Workers.MachineDeployments,
		clusterv1.MachineDeploymentTopology{Class: workerClass, Name: "workers", Replicas: new(int32(1))})
	h := newHarness(t, c)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"pools":{"workers":{"replicas":3}},"workerPools":{"md-0":4}}`), http.StatusOK)
	pools := poolsByName(t, h)
	if got := pools["workers"].Replicas; len(pools) != 2 || got == nil || *got != 3 {
		t.Errorf("pool workers = %v of %d pools, want 3 of 2", got, len(pools))
	}
	if got := pools[workerPool].Replicas; got == nil || *got != 4 {
		t.Errorf("pool md-0 = %v, want 4", got)
	}
}

// One change giving the default pool two values, under its name and its label, has no value that
// was surely meant.
func TestAChangeNamingTheDefaultPoolTwiceIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	for body, names := range map[string]string{
		`{"workerPools":{"workers":4,"md-0":5}}`:                         "md-0 and workers",
		`{"workerPools":{"md-0":4},"pools":{"Workers":{"timeouts":{}}}}`: "md-0 and Workers",
	} {
		rec := b.do("PATCH", "/api/clusters/default/one", body)
		expect(t, rec, http.StatusUnprocessableEntity)
		if !strings.Contains(rec.Body.String(), "pool md-0 as both "+names+".") {
			t.Errorf("%s: %s, want both names, %s", body, rec.Body.String(), names)
		}
	}
	if pool := storedPool(t, h, "one"); pool.Replicas == nil || *pool.Replicas != 2 {
		t.Errorf("pool = %+v, want it as it was", pool)
	}
}

// The Add pool dialog's change: a pool added under the default pool's label would change the
// default pool instead.
func TestAPoolAddedUnderTheDefaultPoolsLabelIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	const size = `"replicas":1,"machine":{"cores":2,"memoryMiB":4096,"diskGiB":30}`
	for _, name := range []string{"workers", "Workers"} {
		rec := b.do("PATCH", "/api/clusters/default/one", `{"pools":{"`+name+`":{"add":true,`+size+`}}}`)
		expect(t, rec, http.StatusUnprocessableEntity)
		if !strings.Contains(rec.Body.String(), name+" is the default pool's name") {
			t.Errorf("adding %s: %s", name, rec.Body.String())
		}
	}
	rec := b.do("PATCH", "/api/clusters/default/one", `{"pools":{"md-0":{"add":true,`+size+`}}}`)
	expect(t, rec, http.StatusUnprocessableEntity)
	pools := poolsByName(t, h)
	if md := pools[workerPool]; len(pools) != 1 || md.Replicas == nil || *md.Replicas != 2 || len(md.Variables.Overrides) != 0 {
		t.Errorf("pools %+v, want md-0 alone and as it was", pools)
	}
	// Any other name is added as before.
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"pools":{"gpu":{"add":true,`+size+`}}}`), http.StatusOK)
	if pools = poolsByName(t, h); len(pools) != 2 {
		t.Errorf("pools %+v, want md-0 and gpu", pools)
	}
}

func TestAnAutoscaledPoolIsNamedByItsLabel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"autoscaling":{"md-0":{"min":1,"max":4}}}`), http.StatusOK)
	for _, body := range []string{`{"workerPools":{"workers":3}}`, `{"pools":{"workers":{"replicas":3}}}`} {
		rec := b.do("PATCH", "/api/clusters/default/one", body)
		expect(t, rec, http.StatusUnprocessableEntity)
		if !strings.Contains(rec.Body.String(), "pool Workers is autoscaled") {
			t.Errorf("%s: %s, want the pool by its label", body, rec.Body.String())
		}
	}
}
