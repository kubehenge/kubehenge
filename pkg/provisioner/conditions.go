package provisioner

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubehengeReadyCondition on a Cluster says whether Kubehenge's controller last reconciled it
// without an error, and when not, the error, so that what holds a cluster up shows with kubectl
// as in the interface rather than only in the controller's log.
const KubehengeReadyCondition = "KubehengeReady"

// Reasons of KubehengeReadyCondition.
const (
	ReconciledReason      = "Reconciled"
	ReconcileFailedReason = "ReconcileFailed"
)

const (
	// conditionMessageMax bounds a condition's message well below the API's own limit of 32 KiB,
	// which a Proxmox VE task's log could otherwise come near.
	conditionMessageMax = 4096
	// eventNoteMax is the events API's limit on an event's note, beyond which it refuses the event.
	eventNoteMax = 1024
	// reportTimeout bounds writing a reconcile's outcome, which has to happen also when the
	// reconcile ran out of time.
	reportTimeout = 30 * time.Second
)

// clip shortens s to at most n bytes, on a character boundary and marks that it was cut.
func clip(s string, n int) string {
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

// setClusterCondition sets a condition on a cluster, or takes the condition of condType away when
// cond is nil, beside the conditions Cluster API sets, which a conflict has it read again for. It
// returns the condition as it was before, nil when there was none.
//
// It reads through c first, a cache most often. After a conflict it reads through live, since the
// cache may not hold yet the write that conflicted, the caller's own just before most likely.
func setClusterCondition(ctx context.Context, c client.Client, live client.Reader, key client.ObjectKey, condType string, cond *metav1.Condition) (*metav1.Condition, error) {
	var before *metav1.Condition
	var read client.Reader = c
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cluster := &clusterv1.Cluster{}
		if err := read.Get(ctx, key, cluster); err != nil {
			return fmt.Errorf("reading the cluster: %w", err)
		}
		read = liveOr(live, c)
		if found := meta.FindStatusCondition(cluster.Status.Conditions, condType); found != nil {
			before = found.DeepCopy()
		} else {
			before = nil
		}
		unchanged := cluster.DeepCopy()
		var changed bool
		if cond == nil {
			changed = meta.RemoveStatusCondition(&cluster.Status.Conditions, condType)
		} else {
			set := *cond
			set.ObservedGeneration = cluster.Generation
			changed = meta.SetStatusCondition(&cluster.Status.Conditions, set)
		}
		if !changed {
			return nil
		}
		return c.Status().Patch(ctx, cluster, client.MergeFromWithOptions(unchanged, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		return before, fmt.Errorf("setting the cluster's %s condition: %w", condType, err)
	}
	return before, nil
}

// liveOr is live, or c where no live reader is set.
func liveOr(live client.Reader, c client.Client) client.Reader { //nolint:ireturn // either reader serves
	if live == nil {
		return c
	}
	return live
}

// report records how a reconcile of the cluster ended: KubehengeReadyCondition, and a Warning event
// for an error, which the event recorder folds into one series while the error repeats.
func (r *Reconciler) report(ctx context.Context, cluster *clusterv1.Cluster, reconcileErr error) {
	// A conflict is retried at once and says nothing about the cluster, and a cluster let go of
	// after its deletion is no longer there to report on.
	if apierrors.IsConflict(reconcileErr) || reconcileErr == nil && !cluster.DeletionTimestamp.IsZero() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
	defer cancel()
	cond := metav1.Condition{Type: KubehengeReadyCondition, Status: metav1.ConditionTrue, Reason: ReconciledReason}
	if reconcileErr != nil {
		message := reconcileErr.Error()
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, ReconcileFailedReason, clip(message, conditionMessageMax)
		if r.Events != nil {
			r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, ReconcileFailedReason, "Reconcile", "%s", clip(message, eventNoteMax))
		}
	}
	if _, err := setClusterCondition(ctx, r.Client, r.Reader, client.ObjectKeyFromObject(cluster), KubehengeReadyCondition, &cond); err != nil && !apierrors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).Error(err, "recording the outcome of the reconcile")
	}
}
