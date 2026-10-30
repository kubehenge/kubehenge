package provisioner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VMsCondition is true on a Cluster while a VM of one of its machines does not run although its
// node joined: paused or stopped by hand, paused by QEMU after a disk error, or crashed.
const VMsCondition = "VMsNotRunning"

// The reasons of VMsCondition, worst first; the condition takes the worst a VM is in.
const (
	VMsIOError  = "IOError"
	VMsPanicked = "Panicked"
	VMsStopped  = "Stopped"
	VMsPaused   = "Paused"
)

// StorageCondition is true on a Cluster while its storage is nearly full on a node it runs on.
const StorageCondition = "StorageNearlyFull"

// The reasons of StorageCondition.
const (
	StorageFull       = "Full"
	StorageNearlyFull = "NearlyFull"
)

const (
	// A storage counts as nearly full from storageWarnPercent and as full from storageFullPercent;
	// QEMU pauses a VM whose disk write finds no space, which thin storage allows to happen.
	storageWarnPercent = 85
	storageFullPercent = 95
	percent            = 100
	gibibyte           = 1 << 30
	// vmStateGrace is how long a VM is out of the running state before it counts: a backup in
	// suspend mode, a migration or a reboot pause or stop it for a moment.
	vmStateGrace = 3 * time.Minute
)

// vmReasons orders VMsCondition's reasons, worst first.
var vmReasons = []string{VMsIOError, VMsPanicked, VMsStopped, VMsPaused}

// vmStateWords says what a VM in each state is, after its name.
var vmStateWords = map[string]string{
	VMsIOError:  "paused: disk error",
	VMsPanicked: "crashed",
	VMsStopped:  "stopped",
	VMsPaused:   "paused",
}

// vmAdvice is what to do about a VM in each state.
var vmAdvice = map[string]string{
	VMsIOError:  "Free space or fix the disk, then resume it in Proxmox VE.",
	VMsPanicked: "Reset it in Proxmox VE, or delete its Machine.",
	VMsStopped:  "Start it in Proxmox VE, or delete its Machine.",
	VMsPaused:   "Resume it in Proxmox VE.",
}

// transientLocks are the locks Proxmox VE holds on a VM while it pauses or stops it on purpose.
var transientLocks = []string{"backup", "migrate", "snapshot", "snapshot-delete", "rollback", "clone", "create", "suspending"}

// MessageHead is a watch's condition message up to its advice: the first sentence, which names
// what is wrong where. Names of machines, nodes and storages hold no ". ".
func MessageHead(message string) string {
	head, _, _ := strings.Cut(message, ". ")
	return strings.TrimSuffix(head, ".")
}

// storageWhy ends StorageCondition's message with why it matters, for kubectl readers.
const storageWhy = " A VM whose disk write finds no space is paused."

// StorageSummary is StorageCondition's message without why it matters: where, how full, what to do.
func StorageSummary(message string) string {
	summary, _, _ := strings.Cut(message, storageWhy)
	return summary
}

// proxmoxReader reads Proxmox VE's API; *proxmox.Client is one.
type proxmoxReader interface {
	Get(ctx context.Context, path string, v any) error
}

// VMWatch looks at the VMs of every cluster's machines and the storage each cluster's disks are
// on, with the controller's own token, and says on the Cluster what does not run and what is
// nearly full: VMsCondition and StorageCondition, with an event when each starts and ends.
type VMWatch struct {
	Client         client.Client
	ClassNamespace string
	Proxmox        proxmoxReader
	Events         events.EventRecorder
	Interval       time.Duration
	Now            func() time.Time

	// seen is when each VM was first found in the state it is in, while that is not running.
	seen map[int64]vmSeen
}

type vmSeen struct {
	reason string
	since  time.Time
}

// pveGuest is a VM as Proxmox VE's resource list shows it.
type pveGuest struct {
	VMID   int64  `json:"vmid"`
	Node   string `json:"node"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Lock   string `json:"lock"`
}

// pveStorageUse is a storage on one node as Proxmox VE's resource list shows it.
type pveStorageUse struct {
	Storage string `json:"storage"`
	Node    string `json:"node"`
	Status  string `json:"status"`
	Shared  int    `json:"shared"`
	Disk    int64  `json:"disk"`
	MaxDisk int64  `json:"maxdisk"`
}

// vmLook is what one look at Proxmox VE found, shared by every cluster's watch.
type vmLook struct {
	guests   map[int64]pveGuest
	down     map[int64]string
	storages []pveStorageUse
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*VMWatch) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (w *VMWatch) Start(ctx context.Context) error {
	return every(ctx, w.Interval, func(ctx context.Context) {
		look, err := w.read(ctx)
		if err != nil {
			ctrl.LoggerFrom(ctx).WithName("vm-watch").Error(err, "reading Proxmox VE")
			return
		}
		eachCluster(ctx, w.Client, w.ClassNamespace, "vm-watch", func(ctx context.Context, c *clusterv1.Cluster) error {
			return w.watch(ctx, c, look)
		})
	})
}

// read lists the VMs and storages the token sees.
func (w *VMWatch) read(ctx context.Context) (*vmLook, error) {
	ctx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	var guests []pveGuest
	if err := w.Proxmox.Get(ctx, "/cluster/resources?type=vm", &guests); err != nil {
		return nil, fmt.Errorf("listing the VMs: %w", err)
	}
	look := &vmLook{guests: map[int64]pveGuest{}, down: map[int64]string{}}
	if err := w.Proxmox.Get(ctx, "/cluster/resources?type=storage", &look.storages); err != nil {
		return nil, fmt.Errorf("listing the storages: %w", err)
	}
	for _, g := range guests {
		if g.Type == qemuType {
			look.guests[g.VMID] = g
		}
	}
	w.settle(look)
	return look, nil
}

// settle keeps the states VMs have been in for vmStateGrace in look.down.
func (w *VMWatch) settle(look *vmLook) {
	if w.seen == nil {
		w.seen = map[int64]vmSeen{}
	}
	now := w.now()
	for vmid := range w.seen {
		if _, ok := look.guests[vmid]; !ok {
			delete(w.seen, vmid)
		}
	}
	for vmid, g := range look.guests {
		reason := vmStateOf(g)
		if reason == "" {
			delete(w.seen, vmid)
			continue
		}
		s, ok := w.seen[vmid]
		if !ok || s.reason != reason {
			s = vmSeen{reason: reason, since: now}
			w.seen[vmid] = s
		}
		if now.Sub(s.since) >= vmStateGrace {
			look.down[vmid] = reason
		}
	}
}

// vmStateOf is the reason a VM counts as not running, "" while it runs or is only passing
// through a state on purpose. The resource list gives a started VM's state as QEMU reports it,
// "paused" or "io-error" rather than "running"; "unknown" is a VM whose node is offline, which
// Cluster API sees from the node.
func vmStateOf(g pveGuest) string {
	if slices.Contains(transientLocks, g.Lock) {
		return ""
	}
	switch g.Status {
	case guestStopped:
		return VMsStopped
	case "io-error":
		return VMsIOError
	case "guest-panicked", "internal-error":
		return VMsPanicked
	case "paused", "suspended":
		return VMsPaused
	default:
		return ""
	}
}

func (w *VMWatch) watch(ctx context.Context, c *clusterv1.Cluster, look *vmLook) error {
	if !c.DeletionTimestamp.IsZero() || RestoreInProgress(c) {
		return nil
	}
	vms, err := w.clusterVMs(ctx, c)
	if err != nil {
		return err
	}
	vmsErr := w.mark(ctx, c, VMsCondition, vmsCondition(vms, look), "VMsRunning", "every VM of the cluster runs")
	storage, set, err := optionalVariable[string](c, "storage")
	if err != nil || !set {
		return errors.Join(vmsErr, err)
	}
	return errors.Join(vmsErr, w.mark(ctx, c, StorageCondition, storageCondition(storage, vms, look), "StorageFreed",
		fmt.Sprintf("storage %s is below %d%% full", storage, storageWarnPercent)))
}

// clusterVM is the VM of a machine whose node joined, so that its VM ran once.
type clusterVM struct {
	machine string
	vmid    int64
}

// clusterVMs lists the VMs of the cluster's machines that joined and are not being deleted.
func (w *VMWatch) clusterVMs(ctx context.Context, c *clusterv1.Cluster) ([]clusterVM, error) {
	machines, err := clusterMachines(ctx, w.Client, c)
	if err != nil {
		return nil, err
	}
	pms := &unstructured.UnstructuredList{}
	pms.SetGroupVersionKind(schema.GroupVersionKind{Group: infrastructureGroup, Version: proxmoxMachineVersion, Kind: "ProxmoxMachineList"})
	if err := w.Client.List(ctx, pms, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return nil, fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	ids := map[string]int64{}
	for i := range pms.Items {
		if vmid, found, _ := unstructured.NestedInt64(pms.Items[i].Object, "spec", "virtualMachineID"); found {
			ids[pms.Items[i].GetName()] = vmid
		}
	}
	var vms []clusterVM
	for i := range machines {
		m := &machines[i]
		vmid, ok := ids[m.Spec.InfrastructureRef.Name]
		if !ok || m.Status.NodeRef.Name == "" {
			continue
		}
		vms = append(vms, clusterVM{machine: m.Name, vmid: vmid})
	}
	slices.SortFunc(vms, func(a, b clusterVM) int { return int(a.vmid - b.vmid) })
	return vms, nil
}

// vmsCondition says which of the cluster's VMs do not run, nil when all do.
func vmsCondition(vms []clusterVM, look *vmLook) *metav1.Condition {
	var down []string
	found := map[string]bool{}
	for _, vm := range vms {
		reason, ok := look.down[vm.vmid]
		if !ok {
			continue
		}
		found[reason] = true
		down = append(down, fmt.Sprintf("%s (VM %d on %s) %s", vm.machine, vm.vmid, look.guests[vm.vmid].Node, vmStateWords[reason]))
	}
	if len(down) == 0 {
		return nil
	}
	var worst string
	advice := make([]string, 0, len(found))
	for _, reason := range vmReasons {
		if found[reason] {
			if worst == "" {
				worst = reason
			}
			advice = append(advice, vmAdvice[reason])
		}
	}
	return &metav1.Condition{
		Status: metav1.ConditionTrue, Reason: worst,
		Message: strings.Join(down, "; ") + ". " + strings.Join(advice, " "),
	}
}

// storageCondition says on which nodes the cluster's storage is nearly full: those its VMs run on,
// or every node when none runs yet. A shared storage is named once.
func storageCondition(storage string, vms []clusterVM, look *vmLook) *metav1.Condition {
	var nodes []string
	for _, vm := range vms {
		if g, ok := look.guests[vm.vmid]; ok && !slices.Contains(nodes, g.Node) {
			nodes = append(nodes, g.Node)
		}
	}
	var full []string
	worst := 0
	for _, s := range look.storages {
		if s.Storage != storage || s.Status != "available" || s.MaxDisk <= 0 || len(nodes) > 0 && !slices.Contains(nodes, s.Node) {
			continue
		}
		used := int(s.Disk * percent / s.MaxDisk)
		if used < storageWarnPercent {
			continue
		}
		worst = max(worst, used)
		where := " on " + s.Node
		if s.Shared != 0 {
			where = ""
		}
		entry := fmt.Sprintf("%d%% full%s (%d of %d GiB)", used, where, s.Disk/gibibyte, s.MaxDisk/gibibyte)
		if s.Shared != 0 && len(full) > 0 {
			continue
		}
		full = append(full, entry)
	}
	if len(full) == 0 {
		return nil
	}
	reason := StorageNearlyFull
	if worst >= storageFullPercent {
		reason = StorageFull
	}
	return &metav1.Condition{
		Status: metav1.ConditionTrue, Reason: reason,
		Message: fmt.Sprintf("Storage %s is %s. Free space or add capacity.%s", storage, strings.Join(full, ", "), storageWhy),
	}
}

// mark sets one of the watch's conditions, with an event when it starts, worsens or ends.
func (w *VMWatch) mark(ctx context.Context, c *clusterv1.Cluster, condType string, cond *metav1.Condition, endReason, endNote string) error {
	if cond == nil && meta.FindStatusCondition(c.Status.Conditions, condType) == nil {
		return nil
	}
	before, err := markCluster(ctx, w.Client, client.ObjectKeyFromObject(c), condType, cond)
	if err != nil || w.Events == nil {
		return err
	}
	switch {
	case newlyMarked(before, cond):
		w.Events.Eventf(c, nil, corev1.EventTypeWarning, condType, "WatchVMs", "%s", shorten(cond.Message, watchEventMax))
	case before != nil && cond == nil:
		w.Events.Eventf(c, nil, corev1.EventTypeNormal, endReason, "WatchVMs", "%s", endNote)
	}
	return nil
}

func (w *VMWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
