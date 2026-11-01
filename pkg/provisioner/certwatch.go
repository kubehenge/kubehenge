package provisioner

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// CertificatesCondition on a Cluster says that certificates of the cluster run out within
// CertificateRenewWithin, and whether they are being renewed.
const CertificatesCondition = "CertificatesExpiring"

// The reasons of CertificatesCondition.
const (
	// CertificatesRenewing: the machines holding them are being replaced.
	CertificatesRenewing = "Renewing"
	// CertificatesRestartNeeded: an RKE2 control plane of fewer than three machines, which cannot
	// be replaced one by one without stopping the cluster. A restart of rke2-server renews them.
	CertificatesRestartNeeded = "RestartNeeded"
	// CertificatesNotRenewed: they run out within certificateWarnWithin and nothing renewed them.
	CertificatesNotRenewed = "NotRenewed"
)

const (
	certDay = 24 * time.Hour
	// CertificateRenewWithin is how long before its certificates run out a machine is replaced.
	// kubeadm's control plane does so itself (rollout.before in the kubeadm ClusterClass, which
	// must say the same); RKE2 and k3s report theirs from 120 days before, and renew them on a
	// restart, which a new machine is.
	CertificateRenewWithin = 60 * certDay
	// certificateWarnWithin is when a certificate still not renewed is a problem: a replacement
	// takes minutes, so one this late failed.
	certificateWarnWithin = 30 * certDay
	// quorumSafeReplicas is the fewest control-plane machines of which etcd keeps its quorum while
	// one is replaced by removing it first.
	quorumSafeReplicas = 3
	// certificateEventFresh is how recent a node's report must be: the monitor of RKE2 and k3s
	// repeats it every half hour, and stops once a restart renewed them.
	certificateEventFresh = 2 * time.Hour
	// certificatesRest starts what follows the condition's first sentence.
	certificatesRest = " To renew"
)

// The reasons of the events the certificate monitor of RKE2 and k3s records on a node.
const (
	nodeCertificatesEvent = "CertificateExpirationWarning"
	caCertificatesEvent   = "CACertificateExpirationWarning"
)

// certificateDate finds the dates in the monitor's message, one per certificate.
var certificateDate = regexp.MustCompile(`(?:expire within \d+ days at|expired at) (\d{4}-\d\d-\d\dT[0-9:]+(?:Z|[+-]\d\d:\d\d))`)

// restartWhy starts the reason, after what to do, that RestartNeeded's message gives kubectl readers.
const restartWhy = " A control plane of fewer than three machines"

// CertificatesHead is the first sentence of the condition's message: what runs out when.
func CertificatesHead(message string) string {
	head, _, _ := strings.Cut(message, certificatesRest)
	return head
}

// CertificatesSummary is the condition's message without the reason it gives kubectl readers: what
// runs out when, and what renews it.
func CertificatesSummary(message string) string {
	summary, _, _ := strings.Cut(message, restartWhy)
	return summary
}

// expiry is when certificates of a cluster run out, and the machine whose replacement renews them,
// nil for those no machine holds.
type expiry struct {
	what    string
	at      time.Time
	machine *clusterv1.Machine
}

// CertificateWatch looks at when the certificates of every cluster run out, renews those a new
// machine renews, and says so on the Cluster: CertificatesCondition, and an event when it starts.
type CertificateWatch struct {
	Client client.Client
	// Reader reads a cluster past the cache: before a roll of its workers, which follows the
	// watch's own roll of another pool, and after a write to it conflicted.
	Reader         client.Reader
	ClassNamespace string
	// Workload reads a workload cluster. found is false until it has a kubeconfig.
	Workload func(ctx context.Context, c *clusterv1.Cluster) (r client.Reader, found bool, err error)
	Events   events.EventRecorder
	// Interval is how often the clusters are looked at.
	Interval time.Duration
	Now      func() time.Time
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*CertificateWatch) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (w *CertificateWatch) Start(ctx context.Context) error {
	return every(ctx, w.Interval, func(ctx context.Context) {
		eachCluster(ctx, w.Client, w.ClassNamespace, "certificate-watch", w.watch)
	})
}

func (w *CertificateWatch) watch(ctx context.Context, c *clusterv1.Cluster) error {
	if !c.DeletionTimestamp.IsZero() || ptr.Deref(c.Spec.Paused, false) || RestoreInProgress(c) {
		return nil
	}
	machines, err := clusterMachines(ctx, w.Client, c)
	if err != nil {
		return err
	}
	found, readErr := w.expiries(ctx, c, machines)
	now := w.now()
	var due []expiry
	for _, e := range found {
		if e.at.Before(now.Add(CertificateRenewWithin)) {
			due = append(due, e)
		}
	}
	if len(due) == 0 {
		return errors.Join(readErr, w.mark(ctx, c, nil))
	}
	cond, renewErr := w.renew(ctx, c, due)
	return errors.Join(readErr, renewErr, w.mark(ctx, c, cond))
}

func (w *CertificateWatch) mark(ctx context.Context, c *clusterv1.Cluster, cond *metav1.Condition) error {
	if cond == nil && meta.FindStatusCondition(c.Status.Conditions, CertificatesCondition) == nil {
		return nil
	}
	before, err := markCluster(ctx, w.Client, w.Reader, client.ObjectKeyFromObject(c), CertificatesCondition, cond)
	if err != nil || w.Events == nil {
		return err
	}
	switch {
	case newlyMarked(before, cond) && cond.Reason != CertificatesRenewing:
		w.Events.Eventf(c, nil, corev1.EventTypeWarning, CertificatesCondition, "WatchCertificates", "%s", shorten(cond.Message, watchEventMax))
	case before != nil && cond == nil:
		w.Events.Eventf(c, nil, corev1.EventTypeNormal, "CertificatesRenewed", "WatchCertificates", "no certificate runs out within %d days", int(CertificateRenewWithin/certDay))
	}
	return nil
}

// clusterMachines lists a cluster's machines that are not being deleted.
func clusterMachines(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) ([]clusterv1.Machine, error) {
	var list clusterv1.MachineList
	if err := c.List(ctx, &list, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return nil, fmt.Errorf("listing the cluster's machines: %w", err)
	}
	return slices.DeleteFunc(list.Items, func(m clusterv1.Machine) bool { return !m.DeletionTimestamp.IsZero() }), nil
}

// expiries reads when the cluster's certificates run out, from every source there is for its
// distribution: what Cluster API records on a machine (kubeadm's control plane), what RKE2's and
// k3s' own monitor reports on each node, and the client certificates of the kubeconfig and
// talosconfig the management cluster keeps for it. Talos renews its nodes' certificates itself,
// and kubelets of kubeadm their own.
func (w *CertificateWatch) expiries(ctx context.Context, c *clusterv1.Cluster, machines []clusterv1.Machine) ([]expiry, error) {
	var out []expiry
	for i := range machines {
		if at := machines[i].Status.CertificatesExpiryDate; !at.IsZero() {
			out = append(out, expiry{what: "machine " + machines[i].Name, at: at.Time, machine: &machines[i]})
		}
	}
	var errs []error
	if d := distributionOf(c); d == templates.RKE2 || d == templates.K3s {
		reported, err := w.reported(ctx, c, machines)
		out = append(out, reported...)
		errs = append(errs, err)
	}
	clientCerts := []func(context.Context, client.Reader, *clusterv1.Cluster) (expiry, bool, error){kubeconfigExpiry}
	if distributionOf(c) == templates.Talos {
		clientCerts = append(clientCerts, talosconfigExpiry)
	}
	for _, read := range clientCerts {
		e, ok, err := read(ctx, w.Client, c)
		if ok {
			out = append(out, e)
		}
		errs = append(errs, err)
	}
	return earliest(out), errors.Join(errs...)
}

// earliest keeps the first expiry of each thing, in order of when they run out.
func earliest(all []expiry) []expiry {
	slices.SortStableFunc(all, func(a, b expiry) int { return a.at.Compare(b.at) })
	var out []expiry
	for _, e := range all {
		if !slices.ContainsFunc(out, func(o expiry) bool { return o.what == e.what }) {
			out = append(out, e)
		}
	}
	return out
}

// reported reads what the certificate monitor of RKE2 and k3s reported on the cluster's nodes in
// the last certificateEventFresh.
func (w *CertificateWatch) reported(ctx context.Context, c *clusterv1.Cluster, machines []clusterv1.Machine) ([]expiry, error) {
	if w.Workload == nil {
		return nil, nil
	}
	rctx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	workload, found, err := w.Workload(rctx, c)
	if err != nil || !found {
		return nil, err
	}
	var list corev1.EventList
	if err := workload.List(rctx, &list, client.InNamespace(metav1.NamespaceDefault)); err != nil {
		return nil, fmt.Errorf("reading the nodes' certificate reports: %w", err)
	}
	var out []expiry
	for i := range list.Items {
		e := &list.Items[i]
		if e.InvolvedObject.Kind != "Node" || eventSeen(e).Before(w.now().Add(-certificateEventFresh)) {
			continue
		}
		at, ok := firstDate(e.Message)
		if !ok {
			continue
		}
		switch e.Reason {
		case caCertificatesEvent:
			out = append(out, expiry{what: "the cluster's certificate authorities", at: at})
		case nodeCertificatesEvent:
			if m := machineOfNode(machines, e.InvolvedObject.Name); m != nil {
				out = append(out, expiry{what: "machine " + m.Name, at: at, machine: m})
			}
		}
	}
	return out, nil
}

// eventSeen is when an event was last recorded.
func eventSeen(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil:
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	default:
		return e.EventTime.Time
	}
}

// firstDate is the earliest date in a certificate report.
func firstDate(message string) (time.Time, bool) {
	var first time.Time
	for _, m := range certificateDate.FindAllStringSubmatch(message, -1) {
		at, err := time.Parse(time.RFC3339, m[1])
		if err == nil && (first.IsZero() || at.Before(first)) {
			first = at
		}
	}
	return first, !first.IsZero()
}

func machineOfNode(machines []clusterv1.Machine, node string) *clusterv1.Machine {
	for i := range machines {
		if machines[i].Status.NodeRef.Name == node {
			return &machines[i]
		}
	}
	return nil
}

// kubeconfigExpiry is when the client certificate of the kubeconfig Cluster API keeps for the
// cluster runs out. ok is false for one without a certificate, as before Cluster API made it.
func kubeconfigExpiry(ctx context.Context, r client.Reader, c *clusterv1.Cluster) (expiry, bool, error) {
	config, found, err := remoteConfig(ctx, r, client.ObjectKeyFromObject(c))
	if err != nil || !found {
		return expiry{}, false, err
	}
	at, ok := notAfter(config.CertData)
	return expiry{what: "the kubeconfig " + c.Name + "-kubeconfig", at: at}, ok, nil
}

// talosconfigExpiry is when the client certificate of a Talos cluster's talosconfig runs out.
func talosconfigExpiry(ctx context.Context, r client.Reader, c *clusterv1.Cluster) (expiry, bool, error) {
	raw, err := readTalosconfig(ctx, r, c.Namespace, c.Name)
	if apierrors.IsNotFound(err) {
		return expiry{}, false, nil
	}
	if err != nil {
		return expiry{}, false, err
	}
	cfg, err := talosconfig.FromBytes(raw)
	if err != nil {
		return expiry{}, false, fmt.Errorf("reading the talosconfig: %w", err)
	}
	var first time.Time
	for _, tc := range cfg.Contexts {
		crt, err := base64.StdEncoding.DecodeString(tc.Crt)
		if at, ok := notAfter(crt); err == nil && ok && (first.IsZero() || at.Before(first)) {
			first = at
		}
	}
	return expiry{what: "the talosconfig " + TalosconfigSecret(c.Name), at: first}, !first.IsZero(), nil
}

// notAfter is when the first certificate in PEM data runs out.
func notAfter(data []byte) (time.Time, bool) {
	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, false
	}
	return cert.NotAfter, true
}

// renewal is what is done about the certificates due for renewal.
type renewal struct {
	renewing, restart, late []expiry
	started                 []string
}

// renew replaces the machines whose certificates are due, as each distribution allows, and returns
// the cluster's condition, nil when nothing is due.
func (w *CertificateWatch) renew(ctx context.Context, c *clusterv1.Cluster, due []expiry) (*metav1.Condition, error) {
	var r renewal
	var errs []error
	for _, e := range due {
		if e.machine == nil || !e.at.After(w.now().Add(certificateWarnWithin)) {
			r.late = append(r.late, e)
			continue
		}
		how, err := w.replace(ctx, c, e.machine)
		errs = append(errs, err)
		switch how {
		case replaceRestart:
			r.restart = append(r.restart, e)
		case replaceStarted:
			r.started = append(r.started, e.machine.Name)
			r.renewing = append(r.renewing, e)
		default:
			r.renewing = append(r.renewing, e)
		}
	}
	if len(r.started) > 0 && w.Events != nil {
		w.Events.Eventf(c, nil, corev1.EventTypeNormal, "RenewingCertificates", "WatchCertificates",
			"replacing %s, whose certificates run out within %d days", strings.Join(r.started, ", "), int(CertificateRenewWithin/certDay))
	}
	return r.condition(c), errors.Join(errs...)
}

// What replace did about a machine.
const (
	// replaceUnderWay: its replacement was asked for before, or its control plane does it itself.
	replaceUnderWay = iota
	// replaceStarted: its replacement was asked for now.
	replaceStarted
	// replaceRestart: it cannot be replaced. A restart renews its certificates.
	replaceRestart
)

// replace has a machine replaced, as its distribution and role allow.
func (w *CertificateWatch) replace(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine) (int, error) {
	_, controlPlane := m.Labels[clusterv1.MachineControlPlaneLabel]
	switch {
	case !controlPlane:
		return w.rollWorkers(ctx, c, m)
	case distributionOf(c) == templates.K3s:
		return w.rollK3s(ctx, c, m)
	case distributionOf(c) == templates.RKE2:
		return w.remediate(ctx, c, m)
	default:
		return replaceUnderWay, nil
	}
}

// rollWorkers rolls the machine's deployment, through the cluster's topology, unless a roll that
// replaces the machine was asked for already.
func (w *CertificateWatch) rollWorkers(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine) (int, error) {
	name := m.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel]
	how := replaceUnderWay
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		how = replaceUnderWay
		cluster := &clusterv1.Cluster{}
		if err := liveOr(w.Reader, w.Client).Get(ctx, client.ObjectKeyFromObject(c), cluster); err != nil {
			return fmt.Errorf("reading the cluster: %w", err)
		}
		before := cluster.DeepCopy()
		i := slices.IndexFunc(cluster.Spec.Topology.Workers.MachineDeployments, func(md clusterv1.MachineDeploymentTopology) bool { return md.Name == name })
		if i < 0 {
			return nil
		}
		rollout := &cluster.Spec.Topology.Workers.MachineDeployments[i].Rollout
		if rollout.After.After(m.CreationTimestamp.Time) {
			return nil
		}
		rollout.After = metav1.NewTime(w.now().Truncate(time.Second))
		how = replaceStarted
		return w.Client.Patch(ctx, cluster, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		return replaceUnderWay, fmt.Errorf("rolling the workers of %s: %w", name, err)
	}
	return how, nil
}

// rollK3s sets the k3s control plane's rolloutAfter, which Cluster API's topology leaves alone: k3s
// does not take the contract's spec.rollout.after.
func (w *CertificateWatch) rollK3s(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine) (int, error) {
	ref := c.Spec.ControlPlaneRef
	mapping, err := w.Client.RESTMapper().RESTMapping(schema.GroupKind{Group: ref.APIGroup, Kind: ref.Kind})
	if err != nil {
		return replaceUnderWay, fmt.Errorf("finding the control plane's kind: %w", err)
	}
	cp := &unstructured.Unstructured{}
	cp.SetGroupVersionKind(mapping.GroupVersionKind)
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: ref.Name}, cp); err != nil {
		return replaceUnderWay, fmt.Errorf("reading the control plane: %w", err)
	}
	raw, _, _ := unstructured.NestedString(cp.Object, "spec", "rolloutAfter")
	if after, err := time.Parse(time.RFC3339, raw); err == nil && after.After(m.CreationTimestamp.Time) {
		return replaceUnderWay, nil
	}
	patch := fmt.Appendf(nil, `{"spec":{"rolloutAfter":%q}}`, w.now().UTC().Truncate(time.Second).Format(time.RFC3339))
	if err := w.Client.Patch(ctx, cp, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return replaceUnderWay, fmt.Errorf("rolling the control plane: %w", err)
	}
	return replaceStarted, nil
}

// remediate has Cluster API's health check replace one RKE2 control-plane machine at a time, which
// keeps etcd's quorum with three or more. RKE2's control plane has no roll of its own to ask for.
func (w *CertificateWatch) remediate(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine) (int, error) {
	if ptr.Deref(c.Spec.Topology.ControlPlane.Replicas, 1) < quorumSafeReplicas {
		return replaceRestart, nil
	}
	var all clusterv1.MachineList
	err := w.Client.List(ctx, &all, client.InNamespace(c.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		return replaceUnderWay, fmt.Errorf("listing the control-plane machines: %w", err)
	}
	// One at a time, and only with every other member up, so that etcd keeps its quorum.
	busy := slices.ContainsFunc(all.Items, func(o clusterv1.Machine) bool {
		_, asked := o.Annotations[clusterv1.RemediateMachineAnnotation]
		return asked || !o.DeletionTimestamp.IsZero() || !machineReady(&o)
	})
	if busy {
		return replaceUnderWay, nil
	}
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:""}}}`, clusterv1.RemediateMachineAnnotation)
	if err := w.Client.Patch(ctx, m, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return replaceUnderWay, fmt.Errorf("asking for machine %s to be replaced: %w", m.Name, err)
	}
	return replaceStarted, nil
}

func (r *renewal) condition(c *clusterv1.Cluster) *metav1.Condition {
	all := slices.Concat(r.late, r.restart, r.renewing)
	slices.SortStableFunc(all, func(a, b expiry) int { return a.at.Compare(b.at) })
	items := make([]string, len(all))
	for i, e := range all {
		items[i] = e.what + " on " + e.at.UTC().Format(time.DateOnly)
	}
	msg := "Certificates run out: " + strings.Join(items, ", ") + "."
	switch {
	case len(r.restart) > 0:
		nodes := make([]string, len(r.restart))
		for i, e := range r.restart {
			nodes[i] = cmp.Or(e.machine.Status.NodeRef.Name, e.machine.Name)
		}
		return &metav1.Condition{Status: metav1.ConditionTrue, Reason: CertificatesRestartNeeded, Message: msg + certificatesRest +
			" them, restart rke2-server on " + strings.Join(nodes, ", ") + "." + restartWhy + " cannot be replaced one at a time without stopping the cluster."}
	case len(r.late) > 0:
		return &metav1.Condition{Status: metav1.ConditionTrue, Reason: CertificatesNotRenewed, Message: msg + certificatesRest + " " + lateAdvice(c, r.late)}
	default:
		return &metav1.Condition{Status: metav1.ConditionTrue, Reason: CertificatesRenewing, Message: msg + certificatesRest +
			" them, Kubehenge has the machines holding them replaced."}
	}
}

// lateAdvice says how to renew what was not renewed in time.
func lateAdvice(c *clusterv1.Cluster, late []expiry) string {
	var advice []string
	for _, e := range late {
		switch {
		case e.machine != nil:
			advice = append(advice, fmt.Sprintf("replace %s: kubectl --namespace %s delete machine %s", e.what, c.Namespace, e.machine.Name))
		case strings.HasPrefix(e.what, "the kubeconfig"), strings.HasPrefix(e.what, "the talosconfig"):
			advice = append(advice, "check the control-plane provider's log for why it did not renew "+e.what)
		default:
			advice = append(advice, "rotate "+e.what+" as "+string(distributionOf(c))+" documents")
		}
	}
	return "them, " + strings.Join(advice, "; ") + "."
}

func (w *CertificateWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
