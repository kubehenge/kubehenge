package provisioner

import (
	"context"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The bounds of what the watches write on a Cluster.
const (
	// watchMessageMax bounds a condition's message well below the API's own limit of 32 KiB.
	watchMessageMax = 2048
	// watchEventMax is the events API's limit on an event's note.
	watchEventMax = 1024
	// watchReadTimeout bounds what a watch asks a workload cluster or Proxmox VE about one cluster,
	// so that one that does not answer holds up none of the others.
	watchReadTimeout = 15 * time.Second
)

// shorten cuts s to at most n bytes, on a character boundary, and marks that it was cut.
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "..."
	cut := n - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// markCluster sets one of the watches' conditions on a cluster, its message shortened, as
// setClusterCondition does.
func markCluster(ctx context.Context, c client.Client, live client.Reader, key client.ObjectKey, condType string, cond *metav1.Condition) (*metav1.Condition, error) {
	if cond != nil {
		set := *cond
		set.Type = condType
		set.Message = shorten(set.Message, watchMessageMax)
		cond = &set
	}
	return setClusterCondition(ctx, c, live, key, condType, cond)
}

// newlyMarked tells whether a condition set now differs from the one before in status or reason,
// which is when a watch records an event: the message alone moves with every look.
func newlyMarked(before, now *metav1.Condition) bool {
	switch {
	case now == nil:
		return false
	case before == nil:
		return true
	default:
		return before.Status != now.Status || before.Reason != now.Reason
	}
}

// eachCluster runs look on every cluster of Kubehenge's, logging what fails, until ctx ends.
func eachCluster(ctx context.Context, c client.Client, classNamespace, name string, look func(context.Context, *clusterv1.Cluster) error) {
	log := ctrl.LoggerFrom(ctx).WithName(name)
	clusters := &clusterv1.ClusterList{}
	if err := c.List(ctx, clusters); err != nil {
		log.Error(err, "listing clusters")
		return
	}
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if !Owns(cluster, classNamespace) {
			continue
		}
		if err := look(ctx, cluster); err != nil {
			log.Error(err, "watching a cluster", "cluster", cluster.Namespace+"/"+cluster.Name)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// every runs do at once and then every interval, until ctx ends.
func every(ctx context.Context, interval time.Duration, do func(context.Context)) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		do(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
