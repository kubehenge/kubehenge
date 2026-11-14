package ui

import (
	"net/http"
	"testing"
)

func TestPoolLabelNeverReadsLikeAUsersPool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		pools []string
		want  string
	}{
		{workerPool, []string{workerPool, "gpu"}, defaultPoolLabel},
		{"gpu", []string{workerPool, "gpu"}, "gpu"},
		// A user's pool named workers would read like the default pool's label, so both keep their names.
		{workerPool, []string{workerPool, "workers"}, workerPool},
		{"workers", []string{workerPool, "workers"}, "workers"},
	} {
		if got := poolLabel(tc.name, tc.pools); got != tc.want {
			t.Errorf("poolLabel(%q, %v) = %q, want %q", tc.name, tc.pools, got, tc.want)
		}
	}
}

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
