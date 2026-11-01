package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// MachinesStalledCondition on a Cluster says whether its machines stopped coming: True when a
// machine is not ready and none has become ready, or stopped being so, for stallAfter, with the
// worst its Machines and ProxmoxMachines report. A change, a creation or an upgrade that hangs then
// shows with kubectl as in the interface, rather than as a rollout that never ends.
const MachinesStalledCondition = "MachinesStalled"

// Reasons of MachinesStalledCondition.
const (
	StalledReason     = "NoProgress"
	ProgressingReason = "Progressing"
	SettledReason     = "Settled"
)

const (
	// stallAfter is how long a cluster's machines may go without one becoming ready before the
	// cluster counts as stalled. It is the ClusterClasses' nodeStartupTimeout: by then the machine
	// health check has given up on a node that never joined and replaces it, and the replacement
	// mostly fails the same way, so a rollout that has not moved for as long needs someone to look.
	// Machines being created and replaced do not count as progress for that reason.
	stallAfter = 20 * time.Minute
	// progressCheck is how often a cluster whose machines are on their way is looked at: Machines
	// are not watched, since the manager's cache holds only the control-plane ones.
	progressCheck = time.Minute
	// stalledEventReason is the Warning a cluster gets when its machines stall.
	stalledEventReason = "MachinesStalled"
)

// Outcomes of a cluster's machines on their way, as rolloutsTotal counts them.
const (
	outcomeSettled = "settled"
	outcomeStalled = "stalled"
)

// ProgressReconciler sets MachinesStalledCondition on the Clusters built from Kubehenge's
// ClusterClasses.
type ProgressReconciler struct {
	Client         client.Client
	ClassNamespace string
	// Events reports on a Cluster that its machines stalled. It may be nil.
	Events events.EventRecorder
	// Now is the time. Unset, the system's.
	Now func() time.Time

	mu sync.Mutex
	// seen is, per cluster on its way, its ready machines when last looked at and since when.
	seen map[types.NamespacedName]progressMark
}

// progressMark is what the reconciler remembers of a cluster whose machines are on their way. A
// controller that starts over estimates it from the objects (lastProgress).
type progressMark struct {
	ready string
	since time.Time
	// moving is when the machines were first seen on their way, for rolloutSeconds.
	moving time.Time
}

// SetupWithManager registers the reconciler. A change of a Cluster's spec starts it at once. While
// machines are on their way it looks again every progressCheck.
func (r *ProgressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("machine-progress").
		Complete(r)
	if err != nil {
		return fmt.Errorf("setting up the machine progress reconciler: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler.
func (r *ProgressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	// A cluster being deleted reports its machines' removal through Cluster API's own Deleting.
	if !Owns(cluster, r.ClassNamespace) || !cluster.DeletionTimestamp.IsZero() {
		r.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	machines := &clusterv1.MachineList{}
	if err := r.Client.List(ctx, machines, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing the machines of cluster %s: %w", req.NamespacedName, err)
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	cond, mark, moving := r.progress(req.NamespacedName, cluster, machines.Items, now)
	if cond.Status == metav1.ConditionTrue {
		worst, err := r.worst(ctx, cluster, machines.Items)
		if err != nil {
			return ctrl.Result{}, err
		}
		cond.Message = clip(fmt.Sprintf("no machine has become ready since %s; %s", mark.since.UTC().Format("2006-01-02 15:04 UTC"), worst), conditionMessageMax)
	}
	before, err := setClusterCondition(ctx, r.Client, req.NamespacedName, MachinesStalledCondition, &cond)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.outcome(cluster, before, &cond, mark, now)
	if !moving {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: progressCheck}, nil
}

// progress is the condition a cluster's machines call for now, what is remembered of them, and
// whether they are on their way.
func (r *ProgressReconciler) progress(key types.NamespacedName, cluster *clusterv1.Cluster, machines []clusterv1.Machine, now time.Time) (metav1.Condition, progressMark, bool) {
	ready, total := readyMachines(machines)
	if !onTheirWay(cluster, machines) {
		mark := r.forget(key)
		return metav1.Condition{Type: MachinesStalledCondition, Status: metav1.ConditionFalse, Reason: SettledReason}, mark, false
	}
	fingerprint := strings.Join(ready, ",")
	r.mu.Lock()
	if r.seen == nil {
		r.seen = map[types.NamespacedName]progressMark{}
	}
	mark, known := r.seen[key]
	switch {
	case !known:
		since := lastProgress(cluster, machines)
		// A cluster found stalled before the controller started over stays so, rather than
		// taking a replacement made since for progress.
		if was := meta.FindStatusCondition(cluster.Status.Conditions, MachinesStalledCondition); was != nil && was.Status == metav1.ConditionTrue {
			if stalledFrom := was.LastTransitionTime.Add(-stallAfter); stalledFrom.Before(since) {
				since = stalledFrom
			}
		}
		mark = progressMark{ready: fingerprint, since: since, moving: since}
	case mark.ready != fingerprint:
		mark.ready, mark.since = fingerprint, now
	}
	r.seen[key] = mark
	r.mu.Unlock()
	cond := metav1.Condition{
		Type: MachinesStalledCondition, Status: metav1.ConditionFalse, Reason: ProgressingReason,
		Message: fmt.Sprintf("%d of %d machines ready", len(ready), total),
	}
	if now.Sub(mark.since) >= stallAfter {
		cond.Status, cond.Reason = metav1.ConditionTrue, StalledReason
	}
	return cond, mark, true
}

func (r *ProgressReconciler) forget(key types.NamespacedName) progressMark {
	r.mu.Lock()
	defer r.mu.Unlock()
	mark := r.seen[key]
	delete(r.seen, key)
	return mark
}

// outcome reports a change of where the machines stand: a Warning event and a count when they
// stall, a count and how long they took when they settle.
func (r *ProgressReconciler) outcome(cluster *clusterv1.Cluster, before, now *metav1.Condition, mark progressMark, at time.Time) {
	wasStalled := before != nil && before.Status == metav1.ConditionTrue
	wasMoving := before != nil && before.Reason != SettledReason
	switch {
	case now.Status == metav1.ConditionTrue && !wasStalled:
		rolloutsTotal.WithLabelValues(outcomeStalled).Inc()
		if r.Events != nil {
			r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, stalledEventReason, "Rollout", "%s", clip(now.Message, eventNoteMax))
		}
	case now.Reason == SettledReason && wasMoving:
		rolloutsTotal.WithLabelValues(outcomeSettled).Inc()
		if !mark.moving.IsZero() {
			rolloutSeconds.Observe(at.Sub(mark.moving).Seconds())
		}
	}
}

// onTheirWay tells whether a cluster's machines are changing: one not ready, or being deleted, or
// Cluster API scaling or rolling them out.
func onTheirWay(cluster *clusterv1.Cluster, machines []clusterv1.Machine) bool {
	for i := range machines {
		if !meta.IsStatusConditionTrue(machines[i].Status.Conditions, clusterv1.MachineReadyCondition) {
			return true
		}
	}
	for _, t := range []string{clusterv1.ClusterRollingOutCondition, clusterv1.ClusterScalingUpCondition, clusterv1.ClusterScalingDownCondition} {
		if meta.IsStatusConditionTrue(cluster.Status.Conditions, t) {
			return true
		}
	}
	return false
}

// readyMachines are the names of a cluster's ready machines, sorted, and how many it has.
func readyMachines(machines []clusterv1.Machine) ([]string, int) {
	var ready []string
	for i := range machines {
		if meta.IsStatusConditionTrue(machines[i].Status.Conditions, clusterv1.MachineReadyCondition) {
			ready = append(ready, machines[i].Name)
		}
	}
	slices.Sort(ready)
	return ready, len(machines)
}

// lastProgress estimates from the objects when a cluster's machines last made progress, for one the
// controller has not seen on its way before: when one became ready or stopped being so after it
// had joined, when Cluster API started moving them, and when the newest machine was made, since a
// controller that starts over cannot tell a replacement from a machine the rollout needs.
func lastProgress(cluster *clusterv1.Cluster, machines []clusterv1.Machine) time.Time {
	last := cluster.CreationTimestamp.Time
	later := func(t time.Time) {
		if t.After(last) {
			last = t
		}
	}
	for _, t := range []string{clusterv1.ClusterRollingOutCondition, clusterv1.ClusterScalingUpCondition, clusterv1.ClusterScalingDownCondition} {
		if c := meta.FindStatusCondition(cluster.Status.Conditions, t); c != nil && c.Status == metav1.ConditionTrue {
			later(c.LastTransitionTime.Time)
		}
	}
	for i := range machines {
		m := &machines[i]
		later(m.CreationTimestamp.Time)
		if c := meta.FindStatusCondition(m.Status.Conditions, clusterv1.MachineReadyCondition); c != nil && m.Status.NodeRef.Name != "" {
			later(c.LastTransitionTime.Time)
		}
	}
	return last
}

// worst is the worst a stalled cluster's waiting machines report, as <machine>: <message>: what
// capmox reports on their ProxmoxMachines first, by severity, then what Cluster API sums up on the
// Machines, of the oldest machine first.
func (r *ProgressReconciler) worst(ctx context.Context, cluster *clusterv1.Cluster, machines []clusterv1.Machine) (string, error) {
	infra, err := r.proxmoxMachines(ctx, cluster)
	if err != nil {
		return "", err
	}
	waiting := slices.DeleteFunc(slices.Clone(machines), func(m clusterv1.Machine) bool {
		return meta.IsStatusConditionTrue(m.Status.Conditions, clusterv1.MachineReadyCondition)
	})
	slices.SortStableFunc(waiting, func(a, b clusterv1.Machine) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	best, bestScore := "", -1
	for i := range waiting {
		m := &waiting[i]
		message, score := machineReport(m)
		if pm, ok := infra[m.Spec.InfrastructureRef.Name]; ok {
			if pmMessage, pmScore := proxmoxMachineReport(pm); pmScore > score {
				message, score = pmMessage, pmScore
			}
		}
		if score > bestScore && message != "" {
			best, bestScore = m.Name+": "+message, score
		}
	}
	if best == "" {
		return "no machine reports why", nil
	}
	return best, nil
}

// Scores of what a waiting machine reports: capmox's conditions carry a severity, Cluster API's
// summary on the Machine does not.
const (
	scoreMachine = 1
	scoreWarning = 2
	scoreError   = 3
)

// machineReport is what Cluster API reports on a waiting Machine: why its deletion is held up, or
// why it is not ready.
func machineReport(m *clusterv1.Machine) (string, int) {
	if !m.DeletionTimestamp.IsZero() {
		if c := meta.FindStatusCondition(m.Status.Conditions, clusterv1.MachineDeletingCondition); c != nil && c.Message != "" {
			return c.Message, scoreMachine
		}
	}
	if c := meta.FindStatusCondition(m.Status.Conditions, clusterv1.MachineReadyCondition); c != nil && c.Message != "" {
		return c.Message, scoreMachine
	}
	return "", 0
}

// proxmoxMachineReport is the worst condition capmox reports on a ProxmoxMachine as not met.
func proxmoxMachineReport(pm *unstructured.Unstructured) (string, int) {
	conditions, _, _ := unstructured.NestedSlice(pm.Object, "status", "conditions")
	message, score := "", 0
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok || cond["status"] != string(metav1.ConditionFalse) {
			continue
		}
		text, isText := cond["message"].(string)
		if !isText || text == "" {
			continue
		}
		s := scoreMachine
		switch cond["severity"] {
		case "Error":
			s = scoreError
		case "Warning":
			s = scoreWarning
		}
		if s > score {
			message, score = fmt.Sprintf("%v: %s", cond["type"], text), s
		}
	}
	return message, score
}

// proxmoxMachines are a cluster's ProxmoxMachines by name.
func (r *ProgressReconciler) proxmoxMachines(ctx context.Context, cluster *clusterv1.Cluster) (map[string]*unstructured.Unstructured, error) {
	list := proxmoxMachineList()
	if err := r.Client.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return nil, fmt.Errorf("listing the ProxmoxMachines of cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	byName := make(map[string]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		byName[list.Items[i].GetName()] = &list.Items[i]
	}
	return byName, nil
}
