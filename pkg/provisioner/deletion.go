package provisioner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimev1 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// AbandonAnnotation on a Cluster being deleted, set to the cluster's name to confirm it, gives up on
// what the deletion waits for. Machines are deleted without draining their nodes, everything of the
// cluster left is deleted, and whatever has not finished deleting AbandonGrace later loses its
// finalizers: what it held outside the management cluster, such as a VM Proxmox VE would not
// delete, is left behind and named in a LeftBehind event.
const AbandonAnnotation = "kubehenge.dev/abandon-deletion"

// DeletionStatusAnnotation holds what a cluster being deleted waits for, as a JSON DeletionStatus,
// for kubectl and the interface alike.
const DeletionStatusAnnotation = "kubehenge.dev/deletion-status"

// DeletionStalledCondition on a Cluster says that its deletion has waited longer than it should.
const DeletionStalledCondition = "DeletionStalled"

// AbandonGrace is how long, once a deletion is abandoned, what is left of the cluster has to finish
// deleting by itself: long enough for a control plane's machines to be deleted one after another,
// each VM by capmox, wherever Proxmox VE answers for it.
const AbandonGrace = 5 * time.Minute

const (
	// deletionLook is how often a cluster being deleted is looked at, for what it waits on.
	deletionLook = 30 * time.Second
	// abandonRemoveTimeout bounds the last try at removing an abandoned cluster's Proxmox VE objects.
	abandonRemoveTimeout = time.Minute
	// deletionReasonMax bounds what each wait quotes of its object's conditions.
	deletionReasonMax = 300
	// stallWaitsNamed is how many waits a DeletionStalled message names.
	stallWaitsNamed = 3
)

// DeletionStatus is what a cluster's deletion waits on, and what an abandoned one leaves behind.
type DeletionStatus struct {
	// Waiting are the objects whose finalizers hold the deletion, with what each waits for.
	Waiting []DeletionWait `json:"waiting,omitempty"`
	// MayLeave is what abandoning the deletion would leave behind, should it not finish deleting.
	MayLeave []string `json:"mayLeave,omitempty"`
	// Abandoned is when the controller took up the abandonment.
	Abandoned *metav1.Time `json:"abandoned,omitempty"`
	// LeftBehind is what the abandonment left behind so far.
	LeftBehind []string `json:"leftBehind,omitempty"`
}

// DeletionWait is an object a deletion waits on.
type DeletionWait struct {
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	Finalizers []string `json:"finalizers,omitempty"`
	// Reason is what the object's own conditions say it waits for.
	Reason string `json:"reason,omitempty"`
}

// String names the wait and its reason in a few words.
func (w DeletionWait) String() string {
	if w.Reason == "" {
		return w.Kind + " " + w.Name
	}
	return fmt.Sprintf("%s %s (%s)", w.Kind, w.Name, w.Reason)
}

// Abandoned tells whether a cluster's deletion was abandoned: it is being deleted, and its
// AbandonAnnotation names it.
func Abandoned(c *clusterv1.Cluster) bool {
	return !c.DeletionTimestamp.IsZero() && c.Annotations[AbandonAnnotation] == c.Name
}

// DeletionStatusOf reads a cluster's DeletionStatusAnnotation; an unreadable one reads as empty.
func DeletionStatusOf(c *clusterv1.Cluster) DeletionStatus {
	var st DeletionStatus
	if raw := c.Annotations[DeletionStatusAnnotation]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &st)
	}
	return st
}

// validateAbandon holds AbandonAnnotation to a cluster being deleted, and to its own name.
func validateAbandon(old, c *clusterv1.Cluster) error {
	value, set := c.Annotations[AbandonAnnotation]
	if !set || (old != nil && old.Annotations[AbandonAnnotation] == value) {
		return nil
	}
	path := field.NewPath("metadata", "annotations").Key(AbandonAnnotation)
	var errs field.ErrorList
	switch {
	case c.DeletionTimestamp.IsZero():
		errs = append(errs, field.Forbidden(path, "only a cluster being deleted can have its deletion abandoned"))
	case value != c.Name:
		errs = append(errs, field.Invalid(path, value, "must be the cluster's name, to confirm"))
	default:
		return nil
	}
	return apierrors.NewInvalid(clusterv1.GroupVersion.WithKind("Cluster").GroupKind(), c.Name, errs) //nolint:wrapcheck // made here, not passed on
}

// deletionKinds are the kinds of what Cluster API and the providers Kubehenge installs keep of a
// cluster in its namespace, labelled with its name: whatever of them holds a finalizer can hold the
// cluster's deletion up. A kind the management cluster does not have is passed over.
var deletionKinds = func() []schema.GroupKind {
	var kinds []schema.GroupKind
	for group, names := range map[string][]string{
		clusterv1.GroupVersion.Group:    {"MachineDeployment", "MachineSet", "Machine"},
		"controlplane.cluster.x-k8s.io": {"KubeadmControlPlane", "RKE2ControlPlane", "KThreesControlPlane", "TalosControlPlane"},
		"bootstrap.cluster.x-k8s.io":    {"KubeadmConfig", "RKE2Config", "KThreesConfig", "TalosConfig"},
		infrastructureGroup:             {"ProxmoxMachine", "ProxmoxCluster"},
		"ipam.cluster.x-k8s.io":         {"IPAddressClaim", "IPAddress"},
		"addons.cluster.x-k8s.io":       {"HelmReleaseProxy"},
	} {
		for _, kind := range names {
			kinds = append(kinds, schema.GroupKind{Group: group, Kind: kind})
		}
	}
	return kinds
}()

// clusterObjects lists what of a cluster is kept in its namespace, of every kind of deletionKinds
// the management cluster has.
func clusterObjects(ctx context.Context, c client.Client, cluster *clusterv1.Cluster) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured
	for _, gk := range deletionKinds {
		mapping, err := c.RESTMapper().RESTMapping(gk)
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("looking up %s: %w", gk.Kind, err)
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(mapping.GroupVersionKind.GroupVersion().WithKind(gk.Kind + "List"))
		err = c.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name})
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("listing the cluster's %ss: %w", gk.Kind, err)
		}
		for i := range list.Items {
			objects = append(objects, &list.Items[i])
		}
	}
	return objects, nil
}

// waitsOf are the objects being deleted whose finalizers hold them, with what each waits for.
func waitsOf(objects []*unstructured.Unstructured) []DeletionWait {
	var waits []DeletionWait
	for _, o := range objects {
		if o.GetDeletionTimestamp() == nil || len(o.GetFinalizers()) == 0 {
			continue
		}
		waits = append(waits, DeletionWait{Kind: o.GetKind(), Name: o.GetName(), Finalizers: o.GetFinalizers(), Reason: deletingReason(o)})
	}
	return waits
}

// deletingReason is what an object's conditions say its deletion waits for: Cluster API's Deleting
// condition, or else why it is not ready, in the conditions of either contract.
func deletingReason(o *unstructured.Unstructured) string {
	var conditions []any
	for _, at := range [][]string{{}, {"v1beta2"}, {"deprecated", "v1beta1"}} {
		found, _, _ := unstructured.NestedSlice(o.Object, slices.Concat([]string{"status"}, at, []string{"conditions"})...)
		conditions = append(conditions, found...)
	}
	for _, want := range []struct{ kind, status string }{{clusterv1.DeletingCondition, "True"}, {clusterv1.ReadyCondition, "False"}} {
		for _, c := range conditions {
			cond, ok := c.(map[string]any)
			if !ok || cond["type"] != want.kind || cond["status"] != want.status {
				continue
			}
			if message, ok := cond["message"].(string); ok && message != "" {
				return shorten(strings.Join(strings.Fields(message), " "), deletionReasonMax)
			}
		}
	}
	return ""
}

// leftOf is what an object holds outside the management cluster that goes with it only when its
// deletion finishes: a ProxmoxMachine's VM. Empty for anything else.
func leftOf(o *unstructured.Unstructured) string {
	if o.GetKind() != "ProxmoxMachine" {
		return ""
	}
	vmid, _, _ := unstructured.NestedInt64(o.Object, "spec", "virtualMachineID")
	if vmid == 0 {
		return ""
	}
	if node, _, _ := unstructured.NestedString(o.Object, "status", "proxmoxNode"); node != "" {
		return fmt.Sprintf("VM %d on %s", vmid, node)
	}
	return fmt.Sprintf("VM %d", vmid)
}

// beforeDeleteWaits tells whether Cluster API still waits for the BeforeClusterDelete hook, through
// which the cluster leaves Rancher, before it deletes anything of the cluster.
func beforeDeleteWaits(c *clusterv1.Cluster) bool {
	_, ok := c.Annotations[runtimev1.OkToDeleteAnnotation]
	return !ok
}

// deletionStatus is what a cluster's deletion waits on and what abandoning it would leave, beside
// what an abandonment already recorded. removeErr is why Kubehenge's own removal of the cluster's
// Proxmox VE objects failed, if it did.
func (r *Reconciler) deletionStatus(cluster *clusterv1.Cluster, objects []*unstructured.Unstructured, removeErr error) DeletionStatus {
	before := DeletionStatusOf(cluster)
	st := DeletionStatus{Waiting: waitsOf(objects), Abandoned: before.Abandoned, LeftBehind: before.LeftBehind}
	if beforeDeleteWaits(cluster) {
		// Kubehenge's hook is the one that holds a deletion: until the cluster has left Rancher.
		reason := "leaving Rancher"
		if c := meta.FindStatusCondition(cluster.Status.Conditions, clusterv1.ClusterTopologyReconciledCondition); c != nil &&
			c.Status == metav1.ConditionFalse && strings.Contains(c.Message, "BeforeClusterDelete") {
			reason = shorten(strings.Join(strings.Fields(c.Message), " "), deletionReasonMax)
		}
		st.Waiting = append([]DeletionWait{{Kind: "Hook", Name: "BeforeClusterDelete", Reason: reason}}, st.Waiting...)
		st.MayLeave = append(st.MayLeave, "Rancher registration")
	}
	if removeErr != nil {
		st.Waiting = append(st.Waiting, DeletionWait{
			Kind: clusterv1.ClusterKind, Name: cluster.Name, Finalizers: []string{Finalizer},
			Reason: shorten("removing the Proxmox VE objects: "+removeErr.Error(), deletionReasonMax),
		})
	}
	for _, o := range objects {
		if left := leftOf(o); left != "" {
			st.MayLeave = append(st.MayLeave, left)
		}
	}
	if controllerutil.ContainsFinalizer(cluster, Finalizer) {
		if names, err := NamesFor(r.WorkloadPool, cluster.Namespace, cluster.Name); err == nil {
			st.MayLeave = append(st.MayLeave, "Pool "+names.Pool)
		}
	}
	return st
}

// recordDeletion writes a deletion's status on the Cluster when it changed.
func (r *Reconciler) recordDeletion(ctx context.Context, cluster *clusterv1.Cluster, st *DeletionStatus) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encoding the deletion's status: %w", err)
	}
	if cluster.Annotations[DeletionStatusAnnotation] == string(raw) {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{DeletionStatusAnnotation: string(raw)}}})
	if err != nil {
		return fmt.Errorf("encoding the deletion's status: %w", err)
	}
	if err := r.Client.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("recording the deletion's status: %w", err)
	}
	return nil
}

// reportStall sets DeletionStalledCondition on a cluster whose deletion has waited longer than
// StallAfter, with a Warning event when it starts, and takes it away when nothing waits any more.
func (r *Reconciler) reportStall(ctx context.Context, cluster *clusterv1.Cluster, st *DeletionStatus) error {
	var cond *metav1.Condition
	waited := r.now().Sub(cluster.DeletionTimestamp.Time)
	if len(st.Waiting) > 0 && waited >= r.stallAfter() {
		named := st.Waiting[:min(len(st.Waiting), stallWaitsNamed)]
		parts := make([]string, len(named))
		for i, w := range named {
			parts[i] = w.String()
		}
		more := ""
		if len(st.Waiting) > len(named) {
			more = fmt.Sprintf(" and %d more", len(st.Waiting)-len(named))
		}
		cond = &metav1.Condition{
			Status: metav1.ConditionTrue, Reason: "Waiting",
			Message: fmt.Sprintf("the deletion has waited %s on %s%s."+stallRest+", abandon it with Abandon on the cluster's page or: "+
				"kubectl --namespace %s annotate cluster %s %s=%s",
				waited.Truncate(time.Minute), strings.Join(parts, ", "), more, cluster.Namespace, cluster.Name, AbandonAnnotation, cluster.Name),
		}
	}
	if cond == nil && meta.FindStatusCondition(cluster.Status.Conditions, DeletionStalledCondition) == nil {
		return nil
	}
	before, err := markCluster(ctx, r.Client, client.ObjectKeyFromObject(cluster), DeletionStalledCondition, cond)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if newlyMarked(before, cond) && r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, DeletionStalledCondition, "WatchDeletion", "%s", shorten(cond.Message, watchEventMax))
	}
	return nil
}

// stallRest starts what follows the first sentence of a DeletionStalled condition's message: the way
// out, which the interface offers with its own controls.
const stallRest = " If it cannot finish"

// DeletionStallHead is the first sentence of a DeletionStalled condition's message: how long the
// deletion waited, and on what.
func DeletionStallHead(message string) string {
	head, _, _ := strings.Cut(message, stallRest)
	return head
}

// abandon carries out an abandoned deletion: it deletes what of the cluster is not being deleted
// yet, lets machines go without draining their nodes, and once AbandonGrace has passed, takes the
// finalizers off whatever has not finished, the Cluster's last, recording what each leaves behind.
func (r *Reconciler) abandon(ctx context.Context, cluster *clusterv1.Cluster, objects []*unstructured.Unstructured) (ctrl.Result, error) {
	st := r.deletionStatus(cluster, objects, nil)
	now := r.now()
	if st.Abandoned == nil {
		st.Abandoned = &metav1.Time{Time: now}
		ctrl.LoggerFrom(ctx).Info("abandoning the deletion", "grace", AbandonGrace)
		r.event(cluster, corev1.EventTypeWarning, "AbandoningDeletion",
			"abandoning the deletion: what has not finished deleting by %s is left behind", now.Add(AbandonGrace).UTC().Format(time.TimeOnly+" MST"))
	}
	due := !now.Before(st.Abandoned.Add(AbandonGrace))
	var errs []error
	if beforeDeleteWaits(cluster) {
		errs = append(errs, r.markOkToDelete(ctx, cluster))
		st.LeftBehind = r.leave(cluster, st.LeftBehind, "Rancher registration")
	}
	for _, o := range objects {
		errs = append(errs, r.hurry(ctx, o))
		if !due {
			continue
		}
		left, err := r.release(ctx, o)
		errs = append(errs, err)
		if left != "" {
			st.LeftBehind = r.leave(cluster, st.LeftBehind, left)
		}
	}
	if len(objects) > 0 || !due {
		errs = append(errs, r.recordDeletion(ctx, cluster, &st))
		if len(objects) == 0 {
			// Kubehenge's own part ends as it would have, now that nothing of the cluster is left; what
			// it cannot remove is tried once more when the grace is up.
			if _, err := r.removeProxmox(ctx, cluster); err != nil {
				ctrl.LoggerFrom(ctx).Info("removing the abandoned cluster's Proxmox VE objects", "error", err.Error())
			}
		}
		return ctrl.Result{RequeueAfter: waitInterval}, errors.Join(errs...)
	}
	return ctrl.Result{}, r.releaseAbandoned(ctx, cluster, &st)
}

// releaseAbandoned tries once more to remove an abandoned cluster's Proxmox VE objects, then takes
// every finalizer off the Cluster, with an event that names everything left behind.
func (r *Reconciler) releaseAbandoned(ctx context.Context, cluster *clusterv1.Cluster, st *DeletionStatus) error {
	log := ctrl.LoggerFrom(ctx)
	if controllerutil.ContainsFinalizer(cluster, Finalizer) {
		names, held, err := r.cloudObjects(ctx, cluster)
		if err == nil {
			rctx, cancel := context.WithTimeout(ctx, abandonRemoveTimeout)
			err = r.Cloud.Remove(rctx, names, held)
			cancel()
		}
		if err != nil {
			log.Error(err, "removing the abandoned cluster's Proxmox VE objects")
			if names, nerr := NamesFor(r.WorkloadPool, cluster.Namespace, cluster.Name); nerr == nil {
				st.LeftBehind = r.leave(cluster, st.LeftBehind, fmt.Sprintf("Pool %s and user %s", names.Pool, names.User))
			}
		}
	}
	if controllerutil.ContainsFinalizer(cluster, AutoscalerFinalizer) {
		st.LeftBehind = r.leave(cluster, st.LeftBehind, "Autoscaler in the management cluster")
	}
	left := "nothing"
	if len(st.LeftBehind) > 0 {
		left = strings.Join(st.LeftBehind, ", ")
	}
	log.Info("abandoned the deletion", "leftBehind", st.LeftBehind)
	r.event(cluster, corev1.EventTypeWarning, "DeletionAbandoned", "deletion abandoned; left behind: %s", left)
	if err := r.Client.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("taking the finalizers off the cluster: %w", err)
	}
	return nil
}

// leave adds what an abandonment leaves behind to those recorded, with a LeftBehind event for it.
func (r *Reconciler) leave(cluster *clusterv1.Cluster, recorded []string, left string) []string {
	if slices.Contains(recorded, left) {
		return recorded
	}
	r.event(cluster, corev1.EventTypeWarning, "LeftBehind", "left behind by the abandoned deletion: %s", left)
	return append(recorded, left)
}

// hurry deletes an object of an abandoned cluster that is not being deleted yet. A Machine is told
// first not to drain its node or wait for its volumes, which a cluster being given up may never let
// happen.
func (r *Reconciler) hurry(ctx context.Context, o *unstructured.Unstructured) error {
	if o.GetKind() == "Machine" && o.GetObjectKind().GroupVersionKind().Group == clusterv1.GroupVersion.Group {
		if _, ok := o.GetAnnotations()[clusterv1.ExcludeNodeDrainingAnnotation]; !ok {
			patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:"abandoned",%q:"abandoned"}}}`,
				clusterv1.ExcludeNodeDrainingAnnotation, clusterv1.ExcludeWaitForNodeVolumeDetachAnnotation)
			if err := r.Client.Patch(ctx, o, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("letting machine %s go without draining: %w", o.GetName(), err)
			}
		}
	}
	if o.GetDeletionTimestamp() != nil {
		return nil
	}
	if err := r.Client.Delete(ctx, o, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting %s %s: %w", o.GetKind(), o.GetName(), err)
	}
	return nil
}

// release takes the finalizers off an object of an abandoned cluster that is being deleted, once
// the grace is up, and returns what it leaves behind.
func (r *Reconciler) release(ctx context.Context, o *unstructured.Unstructured) (string, error) {
	if o.GetDeletionTimestamp() == nil || len(o.GetFinalizers()) == 0 {
		return "", nil
	}
	left := leftOf(o)
	ctrl.LoggerFrom(ctx).Info("taking the finalizers off", "kind", o.GetKind(), "name", o.GetName(), "finalizers", o.GetFinalizers())
	if err := r.Client.Patch(ctx, o, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("taking the finalizers off %s %s: %w", o.GetKind(), o.GetName(), err)
	}
	return left, nil
}

// markOkToDelete lets Cluster API go on with an abandoned cluster's deletion without waiting for the
// BeforeClusterDelete hook, which BeforeClusterDelete itself answers at once for one abandoned.
func (r *Reconciler) markOkToDelete(ctx context.Context, cluster *clusterv1.Cluster) error {
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:""}}}`, runtimev1.OkToDeleteAnnotation)
	if err := r.Client.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("letting Cluster API delete the cluster: %w", err)
	}
	return nil
}

func (r *Reconciler) event(cluster *clusterv1.Cluster, kind, reason, note string, args ...any) {
	if r.Events != nil {
		r.Events.Eventf(cluster, nil, kind, reason, "AbandonDeletion", note, args...)
	}
}

func (r *Reconciler) stallAfter() time.Duration {
	return cmp.Or(r.StallAfter, DefaultStallAfter)
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
