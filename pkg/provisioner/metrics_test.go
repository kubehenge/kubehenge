package provisioner

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

func TestMeasuredCountsWaitsAndRefusals(t *testing.T) {
	t.Parallel()
	const name = "test-measured"
	wait := Measured(name, func(_ context.Context, _ *runtimehooksv1.BeforeClusterCreateRequest, resp *runtimehooksv1.BeforeClusterCreateResponse) {
		resp.Status = runtimehooksv1.ResponseStatusSuccess
		resp.RetryAfterSeconds = 5
	})
	refuse := Measured(name, func(_ context.Context, _ *runtimehooksv1.BeforeClusterCreateRequest, resp *runtimehooksv1.BeforeClusterCreateResponse) {
		resp.Status = runtimehooksv1.ResponseStatusFailure
	})
	pass := Measured(name, func(_ context.Context, _ *runtimehooksv1.BeforeClusterCreateRequest, resp *runtimehooksv1.BeforeClusterCreateResponse) {
		resp.Status = runtimehooksv1.ResponseStatusSuccess
	})
	for _, hook := range []func(context.Context, *runtimehooksv1.BeforeClusterCreateRequest, *runtimehooksv1.BeforeClusterCreateResponse){wait, wait, refuse, pass} {
		hook(t.Context(), &runtimehooksv1.BeforeClusterCreateRequest{}, &runtimehooksv1.BeforeClusterCreateResponse{})
	}
	if got := testutil.ToFloat64(hookWaits.WithLabelValues(name)); got != 2 {
		t.Fatalf("waits = %v, want 2", got)
	}
	if got := testutil.ToFloat64(hookRefusals.WithLabelValues(name)); got != 1 {
		t.Fatalf("refusals = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(hookDuration, "kubehenge_hook_duration_seconds"); got < 1 {
		t.Fatal("no duration recorded")
	}
}

func TestMeasuredCountsRefusedPatches(t *testing.T) {
	t.Parallel()
	const name = "test-measured-patches"
	refuse := Measured(name, func(_ context.Context, _ *runtimehooksv1.GeneratePatchesRequest, resp *runtimehooksv1.GeneratePatchesResponse) {
		resp.Status = runtimehooksv1.ResponseStatusFailure
	})
	refuse(t.Context(), &runtimehooksv1.GeneratePatchesRequest{}, &runtimehooksv1.GeneratePatchesResponse{})
	if got := testutil.ToFloat64(hookRefusals.WithLabelValues(name)); got != 1 {
		t.Fatalf("refusals = %v, want 1", got)
	}
	if got := testutil.ToFloat64(hookWaits.WithLabelValues(name)); got != 0 {
		t.Fatalf("waits = %v for a hook that cannot wait", got)
	}
}
