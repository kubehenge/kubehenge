package provisioner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubectl/pkg/drain"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// drainRetryDelay is the wait before the pods a disruption budget held are evicted again.
	drainRetryDelay = 5 * time.Second
	// listedPods is how many pods a message names before it counts the rest.
	listedPods = 3
	// listedReasons is how many different reasons a message gives before it stops.
	listedReasons = 5
)

// ErrDrainTimeout ends a drain that still had pods to evict when its timeout ran out.
var ErrDrainTimeout = errors.New("the drain timeout ran out")

// DrainOptions says how long a drain may take and where it reports what holds it.
type DrainOptions struct {
	// Timeout bounds the drain. Zero waits for ever, as a Cluster API Machine does without a
	// nodeDrainTimeoutSeconds.
	Timeout time.Duration
	// Blocked is told, after each round of evictions that left pods, which pods are left and why.
	// refused is true when the API server refused to evict some, as a disruption budget does.
	Blocked func(held string, refused bool)
}

// ShortStatefulSets implements NodeDrainer. A StatefulSet short of replicas elsewhere loses nothing
// more to the drain, and a broken one would make every node wait out its timeout.
func (d *RemoteDrainer) ShortStatefulSets(ctx context.Context, c *clusterv1.Cluster, node string) ([]string, error) {
	h, err := d.helper(ctx, c)
	if err != nil {
		return nil, err
	}
	pods, err := h.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing the pods on node %s: %w", node, err)
	}
	held := map[types.NamespacedName]bool{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if owner := metav1.GetControllerOf(p); owner != nil && owner.Kind == "StatefulSet" && podReady(p) {
			held[types.NamespacedName{Namespace: p.Namespace, Name: owner.Name}] = true
		}
	}
	if len(held) == 0 {
		return nil, nil
	}
	list, err := h.Client.AppsV1().StatefulSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the StatefulSets of cluster %s/%s: %w", c.Namespace, c.Name, err)
	}
	var short []string
	for i := range list.Items {
		s := &list.Items[i]
		if !held[types.NamespacedName{Namespace: s.Namespace, Name: s.Name}] {
			continue
		}
		want := int32(1)
		if s.Spec.Replicas != nil {
			want = *s.Spec.Replicas
		}
		if s.Status.ReadyReplicas < want {
			short = append(short, fmt.Sprintf("StatefulSet %s/%s has %d of %d replicas ready", s.Namespace, s.Name, s.Status.ReadyReplicas, want))
		}
	}
	slices.Sort(short)
	return short, nil
}

// evict evicts the node's pods, round after round, until none is left or the timeout runs out. A
// pod a disruption budget protects is asked again every retry delay, as kubectl drain does.
func (d *RemoteDrainer) evict(h *drain.Helper, node string, o DrainOptions) error {
	gv, err := drain.CheckEvictionSupport(h.Client)
	if err != nil {
		return fmt.Errorf("checking that the cluster supports eviction: %w", err)
	}
	if gv.Empty() {
		return errors.New("the cluster's API server does not support eviction")
	}
	// The deadline is checked between requests: a request given it would fail on the client's rate
	// limiter, which refuses a wait that would pass it, with an error that names no timeout.
	var deadline time.Time
	if o.Timeout > 0 {
		deadline = time.Now().Add(o.Timeout)
	}
	retry := cmp.Or(d.RetryDelay, drainRetryDelay)
	for {
		pods, err := podsToEvict(h, node)
		if err == nil && len(pods) == 0 {
			return nil
		}
		if stopped := drainStopped(h.Ctx, node, deadline); stopped != nil {
			return stopped
		}
		if err != nil {
			return err
		}
		held := evictAll(h, gv, pods)
		if stopped := drainStopped(h.Ctx, node, deadline); stopped != nil {
			return stopped
		}
		if o.Blocked != nil {
			o.Blocked(held.String(), len(held.refusals) > 0)
		}
		wait := retry
		if !deadline.IsZero() {
			wait = min(wait, time.Until(deadline))
		}
		select {
		case <-h.Ctx.Done():
		case <-time.After(wait):
		}
	}
}

// drainStopped is why a drain ended before it finished, or nil while it may go on.
func drainStopped(ctx context.Context, node string, deadline time.Time) error {
	if ctx.Err() != nil {
		return fmt.Errorf("draining node %s: %w", node, context.Cause(ctx))
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return fmt.Errorf("draining node %s: %w", node, ErrDrainTimeout)
	}
	return nil
}

func podsToEvict(h *drain.Helper, node string) ([]corev1.Pod, error) {
	list, errs := h.GetPodsForDeletion(node)
	if len(errs) > 0 {
		return nil, fmt.Errorf("listing the pods of node %s: %w", node, errors.Join(errs...))
	}
	return list.Pods(), nil
}

// heldPods are the pods one round of evictions left on a node.
type heldPods struct {
	// refusals are the pods the API server refused to evict, by its reason.
	refusals map[string][]string
	// waiting are the pods whose eviction went through and that have not gone yet.
	waiting []string
}

func evictAll(h *drain.Helper, gv schema.GroupVersion, pods []corev1.Pod) heldPods {
	held := heldPods{refusals: map[string][]string{}}
	for i := range pods {
		name := pods[i].Namespace + "/" + pods[i].Name
		switch err := h.EvictPod(pods[i], gv); {
		case err == nil:
			held.waiting = append(held.waiting, name)
		case apierrors.IsNotFound(err):
		default:
			reason := evictionRefusal(err)
			held.refusals[reason] = append(held.refusals[reason], name)
		}
	}
	return held
}

// evictionRefusal is why an eviction failed, worded as Cluster API words it for a Machine's
// drain: a disruption budget's refusal names the budget.
func evictionRefusal(err error) string {
	status, ok := errors.AsType[*apierrors.StatusError](err)
	if !ok || !apierrors.IsTooManyRequests(err) {
		return "failed to evict Pod, " + err.Error()
	}
	msg := status.Status().Message
	if details := status.Status().Details; details != nil {
		causes := make([]string, 0, len(details.Causes))
		for _, cause := range details.Causes {
			causes = append(causes, cause.Message)
		}
		if len(causes) > 0 {
			msg += " " + strings.Join(causes, ",")
		}
	}
	return strings.Replace(msg, "Cannot evict pod", "cannot evict pod", 1)
}

func (h heldPods) String() string {
	reasons := slices.Sorted(maps.Keys(h.refusals))
	parts := make([]string, 0, len(reasons)+1)
	for _, reason := range reasons[:min(len(reasons), listedReasons)] {
		parts = append(parts, podsNamed(h.refusals[reason])+": "+reason)
	}
	if len(reasons) > listedReasons {
		parts = append(parts, fmt.Sprintf("%d more reasons", len(reasons)-listedReasons))
	}
	if len(h.waiting) > 0 {
		parts = append(parts, podsNamed(h.waiting)+": deletionTimestamp set, but still not removed from the Node")
	}
	return strings.Join(parts, "; ")
}

// podsNamed is "Pod ns/a" or "Pods ns/a, ns/b, ns/c and 2 more".
func podsNamed(names []string) string {
	if len(names) == 1 {
		return "Pod " + names[0]
	}
	return "Pods " + listed(names)
}

// listed joins the first few names, and counts the rest.
func listed(names []string) string {
	if len(names) <= listedPods {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:listedPods], ", "), len(names)-listedPods)
}

// drainTimeoutSeconds is how long the drain of a node of pool waits, 0 for ever: the Cluster's own
// value for the node's control plane or pool, else its ClusterClass's, else the one the shipped
// classes set. It is the value Cluster API gives a Machine of the same place.
func (u *TalosUpgrader) drainTimeoutSeconds(ctx context.Context, c *clusterv1.Cluster, pool string) (int32, error) {
	own, class := topologyDrainTimeout(c, pool)
	if own != nil {
		return *own, nil
	}
	cc := &clusterv1.ClusterClass{}
	err := u.Client.Get(ctx, client.ObjectKey{Namespace: u.ClassNamespace, Name: c.Spec.Topology.ClassRef.Name}, cc)
	switch {
	case apierrors.IsNotFound(err):
		return DefaultNodeDrainTimeoutSeconds, nil
	case err != nil:
		return 0, fmt.Errorf("reading the class of cluster %s/%s: %w", c.Namespace, c.Name, err)
	}
	if v := classDrainTimeout(cc, class); v != nil {
		return *v, nil
	}
	return DefaultNodeDrainTimeoutSeconds, nil
}

// topologyDrainTimeout is the drain timeout a Cluster's topology sets for its control plane, or for
// the pool named, with the pool's class.
func topologyDrainTimeout(c *clusterv1.Cluster, pool string) (*int32, string) {
	t := &c.Spec.Topology
	if pool == "" {
		return t.ControlPlane.Deletion.NodeDrainTimeoutSeconds, ""
	}
	for i := range t.Workers.MachineDeployments {
		if md := &t.Workers.MachineDeployments[i]; md.Name == pool {
			return md.Deletion.NodeDrainTimeoutSeconds, md.Class
		}
	}
	return nil, ""
}

// classDrainTimeout is the drain timeout a class gives its control plane, or the workers of class.
func classDrainTimeout(cc *clusterv1.ClusterClass, class string) *int32 {
	if class == "" {
		return cc.Spec.ControlPlane.Deletion.NodeDrainTimeoutSeconds
	}
	for i := range cc.Spec.Workers.MachineDeployments {
		if md := &cc.Spec.Workers.MachineDeployments[i]; md.Class == class {
			return md.Deletion.NodeDrainTimeoutSeconds
		}
	}
	return nil
}

// nodeWait is what a node waits for before its reboot: StatefulSets short of replicas, then its
// pods to leave, each for at most the node's drain timeout.
type nodeWait struct {
	u       *TalosUpgrader
	c       *clusterv1.Cluster
	node    string
	timeout time.Duration
	started time.Time
	// shown is what the Cluster says the node waits for now.
	shown string
	// held is what held the pods back last.
	held string
	// told is whether the wait has had its event.
	told bool
	// failed is the last drain error an event reported.
	failed string
}

// drainNode waits until no StatefulSet of the cluster is short of ready replicas, so that the node's
// reboot does not take a second replica of a quorum, then drains the node. At the drain timeout it
// goes on, as a Cluster API Machine's deletion does: the pods left stop with the node.
func (u *TalosUpgrader) drainNode(ctx context.Context, c *clusterv1.Cluster, s *talosStep) error {
	seconds, err := u.drainTimeoutSeconds(ctx, c, s.pool)
	if err != nil {
		return err
	}
	w := &nodeWait{u: u, c: c, node: s.node, timeout: time.Duration(seconds) * time.Second}
	defer w.clear(ctx)
	w.begin()
	if err := w.statefulSets(ctx); err != nil {
		return err
	}
	w.say(ctx, "")
	w.begin()
	return w.drain(ctx)
}

func (w *nodeWait) begin() {
	w.started, w.told = w.u.Now(), false
}

func (w *nodeWait) expired() bool {
	return w.timeout > 0 && !w.u.Now().Before(w.started.Add(w.timeout))
}

// remaining is what is left of the timeout, zero for a wait with none.
func (w *nodeWait) remaining() time.Duration {
	if w.timeout <= 0 {
		return 0
	}
	return w.started.Add(w.timeout).Sub(w.u.Now())
}

func (w *nodeWait) statefulSets(ctx context.Context) error {
	for {
		short, err := w.u.Nodes.ShortStatefulSets(ctx, w.c, w.node)
		if err != nil {
			short = []string{"the StatefulSets cannot be read: " + err.Error()}
		}
		if len(short) == 0 {
			return nil
		}
		text := listed(short)
		if w.expired() {
			w.u.eventf(w.c, corev1.EventTypeWarning, "TalosStatefulSetsNotReady",
				"node %s drains after waiting %s, with %s", w.node, w.timeout, text)
			return nil
		}
		if !w.told {
			w.told = true
			w.u.eventf(w.c, corev1.EventTypeNormal, "TalosStatefulSetsWaiting", "node %s waits to drain: %s", w.node, text)
		}
		w.say(ctx, fmt.Sprintf("Waiting to drain %s: %s", w.node, text))
		if err := w.sleep(ctx); err != nil {
			return err
		}
	}
}

func (w *nodeWait) drain(ctx context.Context) error {
	for !w.expired() {
		err := w.u.Nodes.Drain(ctx, w.c, w.node, DrainOptions{Timeout: w.remaining(), Blocked: w.blocked(ctx)})
		switch {
		case err == nil:
			w.u.eventf(w.c, corev1.EventTypeNormal, "TalosDrained", "node %s drained before its reboot", w.node)
			return nil
		case errors.Is(err, ErrDrainTimeout):
			w.timedOut()
			return nil
		case ctx.Err() != nil:
			return fmt.Errorf("draining node %s: %w", w.node, context.Cause(ctx))
		}
		if msg := err.Error(); msg != w.failed {
			w.failed = msg
			w.u.eventf(w.c, corev1.EventTypeWarning, "TalosDrain", "node %s is not drained yet, and the drain goes on: %v", w.node, err)
		}
		if err := w.sleep(ctx); err != nil {
			return err
		}
	}
	w.timedOut()
	return nil
}

// timedOut records that the node reboots with pods on it, as Cluster API's machine deletion goes
// on once its drain timeout has run out.
func (w *nodeWait) timedOut() {
	left := cmp.Or(w.held, w.failed, "pods left")
	w.u.eventf(w.c, corev1.EventTypeWarning, "TalosDrain",
		"node %s reboots without all its pods moved: the drain timeout of %s ran out, with %s", w.node, w.timeout, left)
}

// blocked is the drain's report of the pods that are left.
func (w *nodeWait) blocked(ctx context.Context) func(string, bool) {
	return func(held string, refused bool) {
		w.held = held
		w.say(ctx, fmt.Sprintf("Draining %s: %s", w.node, held))
		if refused && !w.told {
			w.told = true
			w.u.eventf(w.c, corev1.EventTypeWarning, "TalosDrainBlocked", "node %s is held while it drains: %s", w.node, held)
		}
	}
}

// say puts on the Cluster what the node waits for. The upgrade goes on when that cannot be written.
func (w *nodeWait) say(ctx context.Context, text string) {
	if text == w.shown {
		return
	}
	w.shown = text
	var value any = text
	if text == "" {
		value = nil
	}
	if err := w.u.annotate(ctx, w.c, map[string]any{TalosWaitingAnnotation: value}); err != nil {
		ctrl.LoggerFrom(ctx).WithName("talos-upgrader").Error(err, "saying what the node waits for", "cluster", client.ObjectKeyFromObject(w.c), "node", w.node)
	}
}

// clear takes what the node waited for off the Cluster, also when the upgrade was stopped.
func (w *nodeWait) clear(ctx context.Context) {
	if w.shown == "" {
		return
	}
	if err := w.u.annotate(context.WithoutCancel(ctx), w.c, map[string]any{TalosWaitingAnnotation: nil}); err != nil {
		ctrl.LoggerFrom(ctx).WithName("talos-upgrader").Error(err, "clearing what the node waited for", "cluster", client.ObjectKeyFromObject(w.c), "node", w.node)
	}
}

func (w *nodeWait) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("waiting to drain node %s: %w", w.node, context.Cause(ctx))
	case <-time.After(cmp.Or(w.u.WaitInterval, talosWaitInterval)):
		return nil
	}
}
