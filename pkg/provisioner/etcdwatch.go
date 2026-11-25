package provisioner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
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
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// QuorumLostCondition on a Cluster says that its API has not answered for DefaultQuorumSilence and
// that fewer than a majority of its control-plane machines have a running VM, so etcd cannot elect
// a leader again by starting what stopped. Cluster API does not remediate this: replacing a member
// without a quorum could lose the rest. The way out is a reset of etcd to a machine that survives,
// asked for as for a roll that stalled (EtcdRestoreRequest's Machine), which the interface offers
// as Recover.
const QuorumLostCondition = "ControlPlaneQuorumLost"

// QuorumLostReason is the reason of QuorumLostCondition.
const QuorumLostReason = "NoMajorityRunning"

// DefaultQuorumSilence is how long a cluster's API must stay silent before a missing majority of
// running control planes counts as lost, long enough for a host to reboot.
const DefaultQuorumSilence = 10 * time.Minute

// quorumRest starts what follows the first sentence of the condition's message, which the
// interface says with its own controls.
const quorumRest = " Start what stopped"

// NeedsRecovery tells whether a cluster's control plane has a condition that a reset of etcd to one
// machine answers: a roll that stalled, or a quorum that is lost.
func NeedsRecovery(c *clusterv1.Cluster) bool {
	return meta.IsStatusConditionTrue(c.Status.Conditions, RollStalledCondition) ||
		meta.IsStatusConditionTrue(c.Status.Conditions, QuorumLostCondition)
}

// QuorumLostHead is the first sentence of the condition's message: how many control-plane machines
// run, and how many etcd needs.
func QuorumLostHead(message string) string {
	head, _, _ := strings.Cut(message, quorumRest)
	return head
}

// EtcdWatch looks at the control planes of every cluster: whether a majority of them runs while the
// API is silent (QuorumLostCondition), and which etcd members are healthy, recorded in EtcdHealthName
// for a host reboot to wait on. It starts etcd again where Talos left it Failed.
type EtcdWatch struct {
	Client client.Client
	// Reader reads a cluster past the cache, after a write to it conflicted.
	Reader         client.Reader
	ClassNamespace string
	// Proxmox lists the VMs and nodes with the controller's token.
	Proxmox proxmoxReader
	// APIs tells whether a cluster's API still answers.
	APIs ClusterAPIs
	// Workload reads a workload cluster, for the etcd pods of kubeadm, RKE2 and k3s.
	Workload func(context.Context, *clusterv1.Cluster) (client.Reader, bool, error)
	// Talos reaches a Talos cluster's nodes, whose etcd is a service of theirs.
	Talos  TalosDialer
	Events events.EventRecorder
	// Interval is how often the clusters are looked at.
	Interval time.Duration
	// Silence is how long an API must be silent. It is DefaultQuorumSilence when 0.
	Silence time.Duration
	Now     func() time.Time

	mu sync.Mutex
	// silentSince is when each cluster's API first did not answer in the looks since it last did.
	silentSince map[string]time.Time
}

// maxEtcdRepairs is how often a machine's Failed etcd is started again before it is left to a
// person. A failure a reboot or a bootstrap cannot fix would otherwise cost a reboot every boot.
const maxEtcdRepairs = 3

// EtcdRepairsAnnotation on a control-plane Machine holds the starts of its Failed etcd since it last
// ran, as JSON. It lives on the Machine so that a controller restart or a new leader keeps the
// count, and goes with the Machine. Removing it by hand lets the controller start etcd again.
const EtcdRepairsAnnotation = "kubehenge.dev/etcd-repairs"

// etcdRepair is the value of EtcdRepairsAnnotation.
type etcdRepair struct {
	// Boot is the one the machine was last rebooted from, which still reads Failed while it shuts
	// down.
	Boot    string `json:"boot,omitempty"`
	Repairs int    `json:"repairs"`
	GaveUp  bool   `json:"gaveUp,omitempty"`
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*EtcdWatch) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (w *EtcdWatch) Start(ctx context.Context) error {
	return every(ctx, w.Interval, func(ctx context.Context) {
		if err := w.round(ctx); err != nil {
			ctrl.LoggerFrom(ctx).WithName("etcd-watch").Error(err, "looking at the control planes")
		}
	})
}

// pveLook is the VMs and nodes Proxmox VE lists.
type pveLook struct {
	guests map[int64]pveGuest
	nodes  []string
}

func (w *EtcdWatch) readProxmox(ctx context.Context) (*pveLook, error) {
	ctx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	var guests []pveGuest
	if err := w.Proxmox.Get(ctx, "/cluster/resources?type=vm", &guests); err != nil {
		return nil, fmt.Errorf("listing the VMs: %w", err)
	}
	var nodes []pveNodeUse
	if err := w.Proxmox.Get(ctx, "/cluster/resources?type=node", &nodes); err != nil {
		return nil, fmt.Errorf("listing the nodes: %w", err)
	}
	look := &pveLook{guests: map[int64]pveGuest{}}
	for _, g := range guests {
		if g.Type == qemuType {
			look.guests[g.VMID] = g
		}
	}
	for _, n := range nodes {
		look.nodes = append(look.nodes, n.Node)
	}
	slices.Sort(look.nodes)
	return look, nil
}

// round looks at every cluster and records the members' health.
func (w *EtcdWatch) round(ctx context.Context) error {
	look, err := w.readProxmox(ctx)
	if err != nil {
		return err
	}
	record := EtcdHealth{Checked: w.now().UTC(), Nodes: look.nodes, Clusters: []EtcdSet{}}
	eachCluster(ctx, w.Client, w.ClassNamespace, "etcd-watch", func(ctx context.Context, c *clusterv1.Cluster) error {
		members, err := w.watch(ctx, c, look)
		if len(members) > 0 {
			record.Clusters = append(record.Clusters, EtcdSet{Cluster: c.Namespace + "/" + c.Name, Members: members})
		}
		return err
	})
	slices.SortFunc(record.Clusters, func(a, b EtcdSet) int { return strings.Compare(a.Cluster, b.Cluster) })
	return w.record(ctx, &record)
}

// cpMachine is a control-plane machine with its VM as Proxmox VE lists it.
type cpMachine struct {
	machine *clusterv1.Machine
	vmid    int64
	guest   pveGuest
	listed  bool
}

func (m *cpMachine) running() bool { return m.listed && m.guest.Status == "running" }

// controlPlaneVMs lists the cluster's control-plane machines that are not being deleted, with their VMs.
func (w *EtcdWatch) controlPlaneVMs(ctx context.Context, c *clusterv1.Cluster, look *pveLook) ([]cpMachine, error) {
	machines, err := clusterMachines(ctx, w.Client, c)
	if err != nil {
		return nil, err
	}
	pms := proxmoxMachineList()
	if err := w.Client.List(ctx, pms, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return nil, fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	ids := map[string]int64{}
	for i := range pms.Items {
		if vmid, found, _ := unstructured.NestedInt64(pms.Items[i].Object, "spec", "virtualMachineID"); found {
			ids[pms.Items[i].GetName()] = vmid
		}
	}
	var out []cpMachine
	for i := range machines {
		m := &machines[i]
		if _, cp := m.Labels[clusterv1.MachineControlPlaneLabel]; !cp {
			continue
		}
		vmid := ids[m.Spec.InfrastructureRef.Name]
		g, listed := look.guests[vmid]
		out = append(out, cpMachine{machine: m, vmid: vmid, guest: g, listed: listed && vmid != 0})
	}
	slices.SortFunc(out, func(a, b cpMachine) int { return strings.Compare(a.machine.Name, b.machine.Name) })
	return out, nil
}

// watch sets the quorum condition of one cluster and returns its etcd members, none where the
// cluster has no control-plane machine or is being deleted.
func (w *EtcdWatch) watch(ctx context.Context, c *clusterv1.Cluster, look *pveLook) ([]EtcdMemberHealth, error) {
	if !c.DeletionTimestamp.IsZero() {
		w.forgetSilence(c)
		return nil, nil
	}
	cps, err := w.controlPlaneVMs(ctx, c, look)
	if err != nil {
		return nil, err
	}
	actx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	apiUp := w.APIs == nil || w.APIs.Ready(actx, c) == nil
	var cond *metav1.Condition
	if apiUp || quorumNotApplicable(c) {
		w.forgetSilence(c)
	} else {
		cond = w.quorumLost(c, cps)
	}
	err = w.mark(ctx, c, cond)
	members := make([]EtcdMemberHealth, 0, len(cps))
	for i := range cps {
		members = append(members, w.member(actx, c, &cps[i], apiUp))
	}
	return members, err
}

// forgetSilence drops what is held of a cluster that is gone.
func (w *EtcdWatch) forgetSilence(c *clusterv1.Cluster) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.silentSince, remoteKey(c))
}

// quorumNotApplicable is true for a cluster whose silence says nothing: paused, being restored, or
// never up.
func quorumNotApplicable(c *clusterv1.Cluster) bool {
	return ptr.Deref(c.Spec.Paused, false) || RestoreInProgress(c) || !meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition)
}

// quorumLost is the condition a cluster whose API is silent gets, nil when it has not been silent
// long or a majority of its control-plane machines run.
func (w *EtcdWatch) quorumLost(c *clusterv1.Cluster, cps []cpMachine) *metav1.Condition {
	key := remoteKey(c)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.silentSince == nil {
		w.silentSince = map[string]time.Time{}
	}
	since, seen := w.silentSince[key]
	if !seen {
		since = w.now()
		w.silentSince[key] = since
	}
	silence := cmp.Or(w.Silence, DefaultQuorumSilence)
	total := max(len(cps), int(ptr.Deref(c.Spec.Topology.ControlPlane.Replicas, 0)))
	var alive []string
	for i := range cps {
		if cps[i].running() {
			alive = append(alive, cps[i].machine.Name)
		}
	}
	if w.now().Sub(since) < silence || len(alive) >= majority(total) {
		return nil
	}
	which := cmp.Or(strings.Join(alive, ", "), "none")
	return &metav1.Condition{
		Status: metav1.ConditionTrue, Reason: QuorumLostReason,
		Message: fmt.Sprintf("%d of %d control-plane machines have a running VM (%s), and etcd needs %d. The API has been silent for %s or more.%s if it can be. "+
			"Otherwise reset etcd to a machine that runs and replace the others, with Recover on the cluster's page or by annotating the cluster with %s. "+
			"What only the lost members held is gone.",
			len(alive), total, which, majority(total), silence, quorumRest, EtcdRestoreAnnotation),
	}
}

// mark sets the condition and says so in an event when it begins or ends.
func (w *EtcdWatch) mark(ctx context.Context, c *clusterv1.Cluster, cond *metav1.Condition) error {
	if cond == nil && meta.FindStatusCondition(c.Status.Conditions, QuorumLostCondition) == nil {
		return nil
	}
	before, err := markCluster(ctx, w.Client, w.Reader, client.ObjectKeyFromObject(c), QuorumLostCondition, cond)
	if err != nil || w.Events == nil {
		return err
	}
	switch {
	case newlyMarked(before, cond):
		w.Events.Eventf(c, nil, corev1.EventTypeWarning, QuorumLostCondition, "WatchQuorum", "%s", shorten(cond.Message, watchEventMax))
	case before != nil && cond == nil:
		w.Events.Eventf(c, nil, corev1.EventTypeNormal, "ControlPlaneQuorumBack", "WatchQuorum", "the control plane has a majority running again")
	}
	return nil
}

// member judges one control-plane machine's etcd member.
func (w *EtcdWatch) member(ctx context.Context, c *clusterv1.Cluster, m *cpMachine, apiUp bool) EtcdMemberHealth {
	out := EtcdMemberHealth{Machine: m.machine.Name, Node: m.guest.Node}
	switch {
	case !m.listed:
		out.Reason = "its VM does not exist in Proxmox VE"
	case !m.running():
		out.Reason = "its VM is " + m.guest.Status
	case isTalos(c):
		out.Reason = w.talosMember(ctx, c, m.machine)
	case !apiUp:
		out.Reason = "the cluster's API does not answer"
	default:
		out.Reason = w.podMember(ctx, c, m.machine)
	}
	out.Healthy = out.Reason == ""
	return out
}

// talosMember is "" when the machine's node says its etcd service is healthy, else why not.
func (w *EtcdWatch) talosMember(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine) string {
	address := machineAddress(m)
	if w.Talos == nil || address == "" {
		return "its Talos node has no address yet"
	}
	raw, err := readTalosconfig(ctx, w.Reader, c.Namespace, c.Name)
	if err != nil {
		return err.Error()
	}
	node, err := w.Talos(ctx, raw, address)
	if err != nil {
		return "its Talos API does not answer: " + err.Error()
	}
	defer func() { _ = node.Close() }()
	etcd, err := node.Service(ctx, talosServiceEtcd)
	switch {
	case err != nil:
	case etcd.State == talosEtcdFailed:
		w.startEtcd(ctx, c, m, node, etcd)
	case etcd.State == talosEtcdRunning && m.Annotations[EtcdRepairsAnnotation] != "":
		// etcd that fails again after running is started again, as often as the first time.
		if ferr := w.recordRepair(ctx, m.DeepCopy(), nil); ferr != nil {
			ctrl.LoggerFrom(ctx).WithName("etcd-watch").Error(ferr, "removing the etcd-repairs annotation of a machine whose etcd runs again")
		}
	}
	switch {
	case err != nil:
		return "its etcd service cannot be read: " + err.Error()
	case !etcd.Healthy:
		return "its etcd service is " + etcd.State + " and not healthy"
	}
	return ""
}

// startEtcd starts etcd again on a node Talos left it Failed on, which Talos never does by itself,
// as when the image pull of etcd's pre stage could not fetch the image's signature. The first etcd
// of a cluster not yet initialised is bootstrapped again: CACPPT bootstraps once, and a rebooted
// node would wait for it. Any other node is rebooted, at most once per boot, since a bootstrap
// would make it a cluster of its own, empty. After maxEtcdRepairs the node is left Failed. A
// restore or a Talos upgrade owns the nodes while it runs.
//
// Each start is counted on the Machine before it is made, so that a controller that stops between
// the two undercounts rather than starts etcd twice.
func (w *EtcdWatch) startEtcd(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine, node TalosNode, etcd TalosService) {
	log := ctrl.LoggerFrom(ctx).WithName("etcd-watch").WithValues("cluster", c.Namespace+"/"+c.Name, "machine", m.Name)
	boot, err := node.BootID(ctx)
	if err != nil {
		log.Error(err, "reading the boot of a node whose etcd Talos left Failed")
		return
	}
	// The round listed the cluster a while ago. An upgrade the upgrader recorded since then, or a
	// start another controller counted, shows only past the cache.
	live := liveOr(w.Reader, w.Client)
	fresh, machine := &clusterv1.Cluster{}, &clusterv1.Machine{}
	if err = live.Get(ctx, client.ObjectKeyFromObject(c), fresh); err == nil {
		err = live.Get(ctx, client.ObjectKeyFromObject(m), machine)
	}
	if err != nil {
		log.Error(err, "reading the cluster and the machine again")
		return
	}
	r := repairsOf(machine)
	if leftToOthers(fresh) || r.GaveUp || r.Boot == boot {
		return
	}
	note := "etcd on " + m.Name + " is Failed"
	if etcd.Event != "" {
		note += `, Talos says "` + etcd.Event + `"`
	}
	if r.Repairs >= maxEtcdRepairs {
		r.GaveUp = true
		if err = w.recordRepair(ctx, machine, &r); err != nil {
			log.Error(err, "giving up on etcd")
			return
		}
		note += fmt.Sprintf(". Kubehenge started it %d times without etcd running and has stopped trying.", r.Repairs)
		log.Info(note)
		w.event(c, "TalosEtcdLeftFailed", note)
		return
	}
	only, err := w.onlyEtcd(ctx, fresh, machine)
	if err != nil {
		log.Error(err, "telling whether etcd on the node is the cluster's only one")
		return
	}
	var done string
	if only {
		done, err = w.bootstrapEtcd(ctx, machine, node, boot, r)
	} else {
		done, err = w.rebootEtcd(ctx, machine, node, boot, r)
	}
	if err != nil {
		log.Error(err, "starting etcd again")
		return
	}
	note += done
	log.Info(note)
	w.event(c, "TalosEtcdFailed", note)
}

// bootstrapEtcd bootstraps the node's etcd, counting the start on the machine first, and says what
// it did. After a bootstrap Talos took, a node whose etcd fails once more in the same boot is
// bootstrapped again, as a restore does. Talos refuses one while etcd has data, which a reboot
// starts from instead.
func (w *EtcdWatch) bootstrapEtcd(ctx context.Context, machine *clusterv1.Machine, node TalosNode, boot string, r etcdRepair) (string, error) {
	counted := r
	counted.Repairs++
	if err := w.recordRepair(ctx, machine, &counted); err != nil {
		return "", err
	}
	err := node.Bootstrap(ctx)
	switch {
	case err == nil:
		return ". Kubehenge bootstrapped it again.", nil
	case errors.Is(err, errTalosNotReady):
		// Not a start, so it does not count.
		return "", errors.Join(err, w.recordRepair(ctx, machine, &r))
	}
	refused := fmt.Sprintf(". Talos did not bootstrap it again: %v", err)
	done, err := w.rebootEtcd(ctx, machine, node, boot, r)
	return refused + done, err
}

// rebootEtcd reboots the node, counting the start and the boot on the machine first, and says what
// it did.
func (w *EtcdWatch) rebootEtcd(ctx context.Context, machine *clusterv1.Machine, node TalosNode, boot string, r etcdRepair) (string, error) {
	r.Repairs++
	r.Boot = boot
	if err := w.recordRepair(ctx, machine, &r); err != nil {
		return "", err
	}
	if err := node.Reboot(ctx); err != nil {
		return "", err //nolint:wrapcheck // the node's error names the node and the call
	}
	return ". Kubehenge rebooted the node to start etcd again.", nil
}

// leftToOthers says whether the cluster's nodes are someone else's to restart: it is paused, being
// deleted, restored or upgraded.
func leftToOthers(c *clusterv1.Cluster) bool {
	_, upgrading := c.Annotations[TalosUpgradingAnnotation]
	return ptr.Deref(c.Spec.Paused, false) || !c.DeletionTimestamp.IsZero() || RestoreInProgress(c) || upgrading
}

// repairsOf reads EtcdRepairsAnnotation. A value that does not read, as after a hand edit, counts as
// none.
func repairsOf(m *clusterv1.Machine) etcdRepair {
	var r etcdRepair
	_ = json.Unmarshal([]byte(m.Annotations[EtcdRepairsAnnotation]), &r)
	return r
}

// recordRepair writes r on the machine as read, or removes the annotation for nil. A machine another
// writer changed since is refused, so that two controllers never act on one count.
func (w *EtcdWatch) recordRepair(ctx context.Context, m *clusterv1.Machine, r *etcdRepair) error {
	before := m.DeepCopy()
	if r == nil {
		delete(m.Annotations, EtcdRepairsAnnotation)
	} else {
		raw, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("encoding etcd's starts: %w", err)
		}
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		m.Annotations[EtcdRepairsAnnotation] = string(raw)
	}
	if err := w.Client.Patch(ctx, m, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("recording etcd's starts on machine %s: %w", m.Name, err)
	}
	return nil
}

func (w *EtcdWatch) event(c *clusterv1.Cluster, reason, note string) {
	if w.Events != nil {
		w.Events.Eventf(c, nil, corev1.EventTypeWarning, reason, "WatchEtcd", "%s", shorten(note, watchEventMax))
	}
}

// onlyEtcd says whether etcd on the machine is the first of a cluster that has none yet: the
// control plane is not initialised, CACPPT recorded it as bootstrapped, and no other control-plane
// machine exists, not even one being deleted, which may still run a member. The machine's own etcd
// reads Failed, so none runs. Once initialised, a cluster's etcd had data, which a new empty etcd
// would replace, so its node is rebooted instead.
func (w *EtcdWatch) onlyEtcd(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine) (bool, error) {
	if meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
		return false, nil
	}
	var machines clusterv1.MachineList
	err := w.Client.List(ctx, &machines, client.InNamespace(c.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		return false, fmt.Errorf("listing the control-plane machines: %w", err)
	}
	if len(machines.Items) != 1 || machines.Items[0].Name != m.Name {
		return false, nil
	}
	cp, err := controlPlaneOf(ctx, w.Client, w.Client.RESTMapper(), c)
	if err != nil || cp == nil {
		return false, err
	}
	bootstrapped, _, _ := unstructured.NestedBool(cp.Object, "status", "bootstrapped")
	return bootstrapped, nil
}

// podMember is "" when the etcd pod on the machine's node is ready, else why not.
func (w *EtcdWatch) podMember(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine) string {
	node := m.Status.NodeRef.Name
	if node == "" {
		return "its node has not joined yet"
	}
	if w.Workload == nil {
		return "the controller does not reach the cluster"
	}
	reader, found, err := w.Workload(ctx, c)
	switch {
	case err != nil:
		return err.Error()
	case !found:
		return "the cluster has no kubeconfig yet"
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(metav1.NamespaceSystem), etcdPodLabels(distributionOf(c))); err != nil {
		return "the cluster's etcd pods cannot be listed: " + err.Error()
	}
	i := slices.IndexFunc(pods.Items, func(p corev1.Pod) bool { return p.Spec.NodeName == node })
	switch {
	case i < 0:
		return "no etcd pod runs on " + node
	case !podReady(&pods.Items[i]):
		return "its etcd pod on " + node + " is not ready"
	}
	return ""
}

// record writes the members' health for the interface and the CLI to read.
func (w *EtcdWatch) record(ctx context.Context, h *EtcdHealth) error {
	raw, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encoding the etcd health: %w", err)
	}
	cm := &corev1.ConfigMap{}
	err = liveOr(w.Reader, w.Client).Get(ctx, client.ObjectKey{Namespace: w.ClassNamespace, Name: EtcdHealthName}, cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = &corev1.ConfigMap{Data: map[string]string{EtcdHealthKey: string(raw)}}
		cm.Namespace, cm.Name = w.ClassNamespace, EtcdHealthName
		err = w.Client.Create(ctx, cm)
	case err != nil:
	default:
		cm.Data = map[string]string{EtcdHealthKey: string(raw)}
		err = w.Client.Update(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("recording the etcd health in %s: %w", EtcdHealthName, err)
	}
	return nil
}

func (w *EtcdWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
