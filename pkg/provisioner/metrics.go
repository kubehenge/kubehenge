package provisioner

import (
	"context"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// hookLabel names the hook a hook metric counts.
	hookLabel = "hook"
	// A hook's answer takes from hookFastest, a cached read, to hookFastest*hookGrowth^(hookBuckets-1),
	// about 80 s, well past the 10 s Cluster API waits for one by default.
	hookFastest = 0.005
	hookGrowth  = 4
	hookBuckets = 8
)

// Kubehenge's own metrics, beside controller-runtime's, served on the controller's metrics endpoint
// from controller-runtime's registry.
var (
	registry = promauto.With(metrics.Registry)

	// hookWaits counts the answers of a lifecycle hook that hold Cluster API back to ask again: a
	// cluster held for long shows as a hook that keeps waiting.
	hookWaits = registry.NewCounterVec(prom.CounterOpts{
		Name: "kubehenge_hook_waits_total",
		Help: "Answers of a Runtime Extension hook that had Cluster API wait and call it again.",
	}, []string{hookLabel})
	// hookRefusals counts the answers that refused what Cluster API asked.
	hookRefusals = registry.NewCounterVec(prom.CounterOpts{
		Name: "kubehenge_hook_refusals_total",
		Help: "Answers of a Runtime Extension hook that refused the request.",
	}, []string{hookLabel})
	hookDuration = registry.NewHistogramVec(prom.HistogramOpts{
		Name:    "kubehenge_hook_duration_seconds",
		Help:    "How long a Runtime Extension hook took to answer.",
		Buckets: prom.ExponentialBuckets(hookFastest, hookGrowth, hookBuckets),
	}, []string{hookLabel})

	// rolloutsTotal counts clusters whose machines settled or stalled (MachinesStalledCondition).
	rolloutsTotal = registry.NewCounterVec(prom.CounterOpts{
		Name: "kubehenge_machine_rollouts_total",
		Help: "Clusters whose machines settled, every one ready, or stalled on their way, by outcome.",
	}, []string{"outcome"})
	rolloutSeconds = registry.NewHistogram(prom.HistogramOpts{
		Name: "kubehenge_machine_rollout_duration_seconds",
		Help: "How long a cluster's machines took to settle, from when the controller first saw them on their way.",
		// From a single node replaced in a few minutes to a large cluster's upgrade.
		Buckets: []float64{60, 180, 300, 600, 900, 1200, 1800, 2700, 3600, 5400, 7200, 14400},
	})
)

// Measured is a lifecycle hook that counts, under name, how long it takes, how often it has
// Cluster API wait and how often it refuses.
func Measured[Req any, Resp runtimehooksv1.ResponseObject](name string, hook func(context.Context, Req, Resp)) func(context.Context, Req, Resp) {
	return func(ctx context.Context, req Req, resp Resp) {
		start := time.Now()
		hook(ctx, req, resp)
		hookDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
		if resp.GetStatus() == runtimehooksv1.ResponseStatusFailure {
			hookRefusals.WithLabelValues(name).Inc()
		}
		if retry, ok := any(resp).(runtimehooksv1.RetryResponseObject); ok && retry.GetRetryAfterSeconds() > 0 {
			hookWaits.WithLabelValues(name).Inc()
		}
	}
}
