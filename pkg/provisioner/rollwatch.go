package provisioner

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RollStalledCondition on a Cluster says that a roll of its only control plane stalled: the new
// control-plane machine has not become ready long after it was made, while the old one waits for
// it. A cluster with one control plane runs on both machines' etcd members from the moment the new
// one joins until the old one is removed, each needing the other, so a successor that never comes
// up can hold the cluster's API down for good, which nothing in Cluster API undoes.
const RollStalledCondition = "ControlPlaneRollStalled"

// The reasons of RollStalledCondition.
const (
	// RollStalledAnswering: the successor is not ready, and the cluster's API still answers.
	RollStalledAnswering = "SuccessorNotReady"
	// RollStalledUnreachable: the successor is not ready, and the cluster's API does not answer.
	RollStalledUnreachable = "APIUnreachable"
)

// rollStallRest starts what follows the first sentence of the condition's message: what it means
// and how to recover, which kubectl readers need and the interface says with its own controls.
const rollStallRest = " The cluster"

// RollStallHead is the first sentence of the condition's message: which machine holds the roll up,
// for how long and why.
func RollStallHead(message string) string {
	head, _, _ := strings.Cut(message, rollStallRest)
	return head
}

// RollStallAfter is how long a successor may take to become ready before its roll counts as
// stalled: the health checks' own startup timeout, after which they would have replaced it if they
// could.
const RollStallAfter = 20 * time.Minute

// RollWatch looks at every cluster with one control plane for a roll that stalled, and says so on
// the Cluster: RollStalledCondition, and a Warning event when it starts. The way out is a reset of
// etcd to the machine kept, asked for as an etcd restore that names it (EtcdRestoreRequest's
// Machine), which the interface offers as Recover.
type RollWatch struct {
	Client         client.Client
	ClassNamespace string
	// APIs tells whether the cluster's API still answers.
	APIs   ClusterAPIs
	Events events.EventRecorder
	// Interval is how often the clusters are looked at.
	Interval time.Duration
	// StallAfter is how long a successor may take; RollStallAfter when 0.
	StallAfter time.Duration
	Now        func() time.Time
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*RollWatch) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (w *RollWatch) Start(ctx context.Context) error {
	return every(ctx, w.Interval, func(ctx context.Context) {
		eachCluster(ctx, w.Client, w.ClassNamespace, "roll-watch", w.watch)
	})
}

func (w *RollWatch) watch(ctx context.Context, c *clusterv1.Cluster) error {
	cond, err := w.stalled(ctx, c)
	if err != nil {
		return err
	}
	if cond == nil && meta.FindStatusCondition(c.Status.Conditions, RollStalledCondition) == nil {
		return nil
	}
	before, err := markCluster(ctx, w.Client, client.ObjectKeyFromObject(c), RollStalledCondition, cond)
	if err != nil || w.Events == nil {
		return err
	}
	switch {
	case newlyMarked(before, cond):
		w.Events.Eventf(c, nil, corev1.EventTypeWarning, RollStalledCondition, "WatchRoll", "%s", shorten(cond.Message, watchEventMax))
	case before != nil && cond == nil:
		w.Events.Eventf(c, nil, corev1.EventTypeNormal, "ControlPlaneRollResumed", "WatchRoll", "the control plane's roll is no longer stalled")
	}
	return nil
}

// stallReasonMax bounds Cluster API's reason a successor is not ready, as the message quotes it.
const stallReasonMax = 400

// roll is the two machines of a roll of one control plane: the successor, which replaces kept.
type roll struct {
	successor, kept *clusterv1.Machine
}

// stalled is the condition a cluster's control plane gets, nil when its roll has not stalled.
func (w *RollWatch) stalled(ctx context.Context, c *clusterv1.Cluster) (*metav1.Condition, error) {
	r, ok, err := w.stalledRoll(ctx, c)
	if err != nil || !ok {
		return nil, err
	}
	waited := w.now().Sub(r.successor.CreationTimestamp.Time)
	why := "Cluster API gives no reason"
	if ready := meta.FindStatusCondition(r.successor.Status.Conditions, clusterv1.MachineReadyCondition); ready != nil && ready.Message != "" {
		why = shorten(ready.Message, stallReasonMax)
	}
	command := fmt.Sprintf(`kubectl --namespace %s annotate cluster %s %s="{\"machine\":\"%s\",\"at\":\"$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)\"}"`,
		c.Namespace, c.Name, EtcdRestoreAnnotation, r.kept.Name)
	head := fmt.Sprintf("control-plane machine %s, which replaces %s, has not become ready in %s: %s.",
		r.successor.Name, r.kept.Name, waited.Truncate(time.Minute), why)
	actx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	if w.APIs != nil && w.APIs.Ready(actx, c) != nil {
		return &metav1.Condition{
			Status: metav1.ConditionTrue, Reason: RollStalledUnreachable,
			Message: head + fmt.Sprintf(rollStallRest+" runs on the etcd members of both until it is, and its API does not answer, so one of them does not run. "+
				"Start what stopped if it can be; otherwise reset etcd to %s's own and replace %s, with Recover on the cluster's page or: %s",
				r.kept.Name, r.successor.Name, command),
		}, nil
	}
	return &metav1.Condition{
		Status: metav1.ConditionTrue, Reason: RollStalledAnswering,
		Message: head + rollStallRest + "'s API still answers. Correct what holds the machine up, or replace it, with Recover on the cluster's page or: " +
			command,
	}, nil
}

// stalledRoll finds the roll of a cluster's only control plane whose successor has not become
// ready in StallAfter; ok is false when there is none.
func (w *RollWatch) stalledRoll(ctx context.Context, c *clusterv1.Cluster) (roll, bool, error) {
	if !c.DeletionTimestamp.IsZero() || ptr.Deref(c.Spec.Paused, false) || RestoreInProgress(c) {
		return roll{}, false, nil
	}
	if r := c.Spec.Topology.ControlPlane.Replicas; r == nil || *r != 1 {
		return roll{}, false, nil
	}
	var machines clusterv1.MachineList
	err := w.Client.List(ctx, &machines, client.InNamespace(c.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		return roll{}, false, fmt.Errorf("listing the control-plane machines: %w", err)
	}
	r, ok := rollPair(machines.Items)
	if !ok || machineReady(r.successor) || w.now().Sub(r.successor.CreationTimestamp.Time) < cmp.Or(w.StallAfter, RollStallAfter) {
		return roll{}, false, nil
	}
	return r, true, nil
}

// rollPair finds the two machines of a roll of one control plane: the newest, which replaces the
// oldest of the others that is not being deleted. ok is false when no such roll is under way.
func rollPair(machines []clusterv1.Machine) (roll, bool) {
	sorted := make([]*clusterv1.Machine, len(machines))
	for i := range machines {
		sorted[i] = &machines[i]
	}
	slices.SortFunc(sorted, func(a, b *clusterv1.Machine) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.Name, b.Name))
	})
	if len(sorted) == 0 || !sorted[len(sorted)-1].DeletionTimestamp.IsZero() {
		return roll{}, false
	}
	successor := sorted[len(sorted)-1]
	for _, m := range sorted[:len(sorted)-1] {
		if m.DeletionTimestamp.IsZero() {
			return roll{successor: successor, kept: m}, true
		}
	}
	return roll{}, false
}

func machineReady(m *clusterv1.Machine) bool {
	return meta.IsStatusConditionTrue(m.Status.Conditions, clusterv1.MachineReadyCondition)
}

func (w *RollWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
