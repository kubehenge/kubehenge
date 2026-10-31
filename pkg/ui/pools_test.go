package ui

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func poolsOf(t *testing.T, h *harness) []clusterv1.MachineDeploymentTopology {
	t.Helper()
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	return c.Spec.Topology.Workers.MachineDeployments
}

func TestPoolsAreAddedSizedAndRemoved(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	big := `{"cores":8,"memoryMiB":32768,"diskGiB":80}`

	rec := b.do("PATCH", "/api/clusters/default/one", `{"pools":{"gpu":{"replicas":2,"machine":`+big+`}}}`)
	expect(t, rec, http.StatusOK)
	var view clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.WorkerPools) != 2 || view.WorkerPools[1].Name != "gpu" || view.WorkerPools[1].Replicas != 2 || !sameJSON(view.WorkerPools[1].Machine, json.RawMessage(big)) {
		t.Fatalf("pools shown = %+v", view.WorkerPools)
	}
	pools := poolsOf(t, h)
	if pools[1].Class != workerClass || len(pools[1].Variables.Overrides) != 1 || pools[1].Variables.Overrides[0].Name != workerMachineVariable {
		t.Fatalf("the new pool = %+v", pools[1])
	}

	// Back to the cluster's size, then away.
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"pools":{"gpu":{"machine":null}}}`), http.StatusOK)
	if pools = poolsOf(t, h); len(pools[1].Variables.Overrides) != 0 || *pools[1].Replicas != 2 {
		t.Fatalf("the pool after taking the cluster's size = %+v", pools[1])
	}
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"pools":{"md-0":null}}`), http.StatusOK)
	if pools = poolsOf(t, h); len(pools) != 1 || pools[0].Name != "gpu" {
		t.Fatalf("pools after removing md-0 = %+v", pools)
	}
}

func TestPoolChangesAreChecked(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	for body, want := range map[string]string{
		`{"pools":{"gone":null}}`:                                      "no worker pool gone",
		`{"pools":{"gpu":{}}}`:                                         "Set the workers of the new pool gpu",
		`{"pools":{"GPU":{"replicas":1}}}`:                             "Pool names take",
		`{"pools":{"md-0":{"replicas":101}}}`:                          "in each pool",
		`{"pools":{"` + strings.Repeat("p", 42) + `":{"replicas":1}}}`: "names take 44 characters at most",
	} {
		rec := b.do("PATCH", "/api/clusters/default/one", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s, want %q", body, rec.Code, rec.Body, want)
		}
	}
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("PATCH", "/api/clusters/default/one", `{"pools":{"gpu":{"replicas":1}}}`), http.StatusForbidden)
}

func TestMachineSizesChangeAfterCreation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one",
		`{"variables":{"workerMachine":{"cores":4,"memoryMiB":8192,"diskGiB":40},"controlPlaneMachine":{"cores":4,"memoryMiB":8192,"diskGiB":40}}}`), http.StatusOK)
}

func TestPoolPlansSayWhatTheyReplace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	plan := func(body string) []plannedChange {
		t.Helper()
		rec := b.do("POST", "/api/clusters/default/one/plan", body)
		expect(t, rec, http.StatusOK)
		var p planView
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p.Changes
	}
	if got := plan(`{"pools":{"gpu":{"replicas":1}}}`); !slices.Equal(got, []plannedChange{{Setting: "pools", Effect: effectScales}}) {
		t.Errorf("adding a pool plans %+v", got)
	}
	if got := plan(`{"pools":{"md-0":{"machine":{"cores":8,"memoryMiB":16384,"diskGiB":40}}}}`); !slices.Equal(got, []plannedChange{{Setting: "pools", Effect: effectWorkers}}) {
		t.Errorf("resizing a pool plans %+v", got)
	}
	if got := plan(`{"pools":{"md-0":{"machine":null}}}`); !slices.Equal(got, []plannedChange{{Setting: "pools", Effect: effectScales}}) {
		t.Errorf("keeping a pool's size plans %+v", got)
	}
}
