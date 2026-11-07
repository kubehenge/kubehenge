package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RemediationHeldCondition is true on a Cluster while Kubehenge keeps Cluster API from replacing
// machines whose Proxmox VE node is offline or in maintenance, or whose storage is unavailable.
const RemediationHeldCondition = "RemediationHeld"

// The reasons of RemediationHeldCondition, worst first.
const (
	HeldNodeOffline        = "NodeOffline"
	HeldNodeMaintenance    = "NodeMaintenance"
	HeldStorageUnavailable = "StorageUnavailable"
	// HeldNodeRecovering is a hold whose cause has gone but whose Kubernetes node is not Ready yet.
	HeldNodeRecovering = "NodeRecovering"
)

// StorageUnavailableCondition is true on a Cluster while its storage is not available on a
// Proxmox VE node that runs one of its VMs.
const StorageUnavailableCondition = "StorageUnavailable"

// StorageUnavailableReason is the reason of StorageUnavailableCondition.
const StorageUnavailableReason = "Unavailable"

const (
	// HoldAnnotation is on a machine Kubehenge keeps from replacement. Its value says why. It sits
	// beside Cluster API's skip-remediation annotation, which does the holding.
	HoldAnnotation = "kubehenge.dev/remediation-hold"
	// HoldSinceAnnotation is when the hold began, in RFC 3339. It outlives the hold when the ceiling
	// ends it, so that the same outage does not hold the machine a second time.
	HoldSinceAnnotation = "kubehenge.dev/remediation-hold-since"

	// HoldUntilAnnotation is when the hold ends whatever the host does, in RFC 3339. It is absent
	// for a cluster that holds for as long as the host is away.
	HoldUntilAnnotation = "kubehenge.dev/remediation-hold-until"

	nodeStatusOnline = "online"
	storageAvailable = "available"
	holdClockLayout  = "15:04 MST"
)

// holdReasons orders RemediationHeldCondition's reasons, worst first.
var holdReasons = []string{HeldNodeOffline, HeldNodeMaintenance, HeldStorageUnavailable, HeldNodeRecovering}

// machineHold is why a machine's VM is not replaced for now.
type machineHold struct {
	reason string
	text   string
}

// pveNodeUse is a node as Proxmox VE's resource list shows it.
type pveNodeUse struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

// haStatusPath is the HA manager's status. Proxmox VE's API module `src/PVE/API2/HA/Status.pm`
// (pve-ha-manager, master) registers it as `manager_status`, checks `Sys.Audit` on `/`, and returns
// `{manager_status => $status, quorum => {node, quorate}, lrm_status => {<node> => ...}}`.
const haStatusPath = "/cluster/ha/status/manager_status"

// pveHAStatus is the part of that answer that tells which nodes are in maintenance.
type pveHAStatus struct {
	ManagerStatus pveHAManager `json:"manager_status"`
}

// pveHAManager is the CRM's status file. `PVE::HA::Manager::flush_master_status` sets
// `$ms->{node_status} = $ns->{status}`, a map of node to one of the states of
// `PVE::HA::NodeStatus`: online, maintenance, unknown, fence and gone.
type pveHAManager struct {
	NodeStatus map[string]string `json:"node_status"`
}

// The node states of `PVE::HA::NodeStatus` that decide a hold. `maintenance` means the node is
// being emptied on purpose. `gone` means, in `$valid_node_states`, "node vanished from cluster
// members list, possibly deleted": `update()` sets it only for a node that was `unknown` and is no
// longer in the member list, so the host was removed from the cluster. `unknown` (out of the
// quorate partition) and `fence` are failures a rebooting host recovers from, and the node then
// shows offline in the resource list, which holds machines on its own.
const (
	haMaintenanceState = "maintenance"
	haGoneState        = "gone"
)

// haNodeSets lists the nodes the HA manager has in maintenance mode and those it has as gone.
func haNodeSets(ctx context.Context, r proxmoxReader) (map[string]bool, map[string]bool, error) {
	var status pveHAStatus
	if err := r.Get(ctx, haStatusPath, &status); err != nil {
		return nil, nil, fmt.Errorf("reading the HA manager's status: %w", err)
	}
	maintenance, gone := map[string]bool{}, map[string]bool{}
	for node, state := range status.ManagerStatus.NodeStatus {
		switch state {
		case haMaintenanceState:
			maintenance[node] = true
		case haGoneState:
			gone[node] = true
		}
	}
	return maintenance, gone, nil
}

// haMaintenance lists the nodes the HA manager has in maintenance mode.
func haMaintenance(ctx context.Context, r proxmoxReader) (map[string]bool, error) {
	maintenance, _, err := haNodeSets(ctx, r)
	return maintenance, err
}

// clusterNodesPath is the cluster's own list of members. Proxmox VE's `src/PVE/API2/ClusterConfig.pm`
// registers it as `nodes`, checks `Sys.Audit` on `/`, and builds it from the nodelist of
// `corosync.conf`. `pvecm delnode` takes a node out of that list, while its VMs stay in the
// resource list until their configuration files are removed.
const clusterNodesPath = "/cluster/config/nodes"

// removedNodes lists the nodes that run guests of the resource list and are not in the cluster's
// membership. An empty membership proves nothing, as on a node outside a cluster, so then no node
// is called removed. A refused or failed read is the caller's to handle.
func removedNodes(ctx context.Context, r proxmoxReader, guests map[int64]pveGuest) (map[string]bool, error) {
	var members []pveNodeUse
	if err := r.Get(ctx, clusterNodesPath, &members); err != nil {
		return nil, fmt.Errorf("reading the cluster's nodes: %w", err)
	}
	if len(members) == 0 {
		return map[string]bool{}, nil
	}
	listed := make(map[string]bool, len(members))
	for _, m := range members {
		listed[m.Node] = true
	}
	removed := map[string]bool{}
	for _, g := range guests {
		if !listed[g.Node] {
			removed[g.Node] = true
		}
	}
	return removed, nil
}

// hostFactsOf reads the nodes in maintenance and the nodes that will not come back: those the HA
// manager has as gone and those the cluster no longer lists. A token without Sys.Audit on / is
// refused, which ProxmoxReach reports once on the management cluster's health, so the refusal is
// silent here. Any other failure is logged when its text changes.
func hostFactsOf(ctx context.Context, r proxmoxReader, guests map[int64]pveGuest, lastErr *string) (map[string]bool, map[string]bool) {
	maintenance, gone, err := haNodeSets(ctx, r)
	if err == nil {
		var removed map[string]bool
		removed, err = removedNodes(ctx, r, guests)
		maps.Copy(gone, removed)
	}
	switch {
	case err == nil:
		*lastErr = ""
	case errors.Is(err, proxmox.ErrNotAuthorized):
		*lastErr = err.Error()
	case err.Error() != *lastErr:
		*lastErr = err.Error()
		ctrl.LoggerFrom(ctx).WithName("ha-status").Error(err, "the HA manager's status or the cluster's nodes are not read")
	}
	return maintenance, gone
}

// downNodes lists the nodes, by name, where the storage is listed and not available.
func downNodes(storage string, storages []pveStorageUse) map[string]bool {
	down := map[string]bool{}
	for _, s := range storages {
		if s.Storage == storage && s.Status != storageAvailable {
			down[s.Node] = true
		}
	}
	return down
}

// holdOf says why the machine's VM is not replaced for now, nil when it may be. A node that is not
// listed is none of the cluster's concern.
func holdOf(vm clusterVM, storage string, look *vmLook) *machineHold {
	g, ok := look.guests[vm.vmid]
	if !ok {
		return nil
	}
	switch status, listed := look.nodes[g.Node]; {
	case listed && status != nodeStatusOnline:
		return &machineHold{HeldNodeOffline, fmt.Sprintf("Proxmox VE node %s is offline", g.Node)}
	case look.maintenance[g.Node]:
		return &machineHold{HeldNodeMaintenance, fmt.Sprintf("Proxmox VE node %s is in maintenance", g.Node)}
	case storage != "" && look.storageDown(storage)[g.Node]:
		return &machineHold{HeldStorageUnavailable, fmt.Sprintf("Storage %s is unavailable on %s", storage, g.Node)}
	}
	return nil
}

func (l *vmLook) storageDown(storage string) map[string]bool {
	return downNodes(storage, l.storages)
}

// storageUnavailableCondition says on which nodes the cluster's VMs run without its storage, nil
// when it is available on all of them. A node that is offline is the node's problem, said there.
func storageUnavailableCondition(storage string, vms []clusterVM, look *vmLook, ceiling time.Duration) *metav1.Condition {
	down := look.storageDown(storage)
	var nodes []string
	for _, vm := range vms {
		g, ok := look.guests[vm.vmid]
		if !ok || !down[g.Node] || slices.Contains(nodes, g.Node) {
			continue
		}
		if status, listed := look.nodes[g.Node]; listed && status != nodeStatusOnline || look.gone[g.Node] {
			continue
		}
		nodes = append(nodes, g.Node)
	}
	if len(nodes) == 0 {
		return nil
	}
	slices.Sort(nodes)
	waits := "while it is gone"
	if ceiling > 0 {
		waits = fmt.Sprintf("for %d minutes", int(ceiling/time.Minute))
	}
	return &metav1.Condition{
		Status: metav1.ConditionTrue, Reason: StorageUnavailableReason,
		Message: fmt.Sprintf("Storage %s is unavailable on %s. Its machines are not replaced %s. Bring the storage back in Proxmox VE.",
			storage, strings.Join(nodes, ", "), waits),
	}
}

// holdMachines places, keeps and lifts the hold on each of the cluster's machines, and says which
// are held. A hold lifts when its cause has gone and the machine's node is Ready, or at HoldCeiling.
func (w *VMWatch) holdMachines(ctx context.Context, vms []clusterVM, storage string, look *vmLook, ceiling time.Duration) (*metav1.Condition, error) {
	now := w.now()
	var held []string
	found := map[string]bool{}
	var errs []error
	for _, vm := range vms {
		if look.goneGuest(vm.vmid) {
			errs = append(errs, w.releaseMachine(ctx, vm.m))
			continue
		}
		kept, reason, err := w.holdMachine(ctx, vm, holdOf(vm, storage, look), now, ceiling)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if kept == "" {
			continue
		}
		found[reason] = true
		held = append(held, kept)
	}
	if len(held) == 0 {
		return nil, errors.Join(errs...)
	}
	var worst string
	for _, r := range holdReasons {
		if found[r] {
			worst = r
			break
		}
	}
	return &metav1.Condition{
		Status: metav1.ConditionTrue, Reason: worst,
		Message: strings.Join(held, "; "),
	}, errors.Join(errs...)
}

// goneGuest is true when the guest's node has left the cluster for good, so that waiting for it
// would only delay the replacement.
func (l *vmLook) goneGuest(vmid int64) bool {
	g, ok := l.guests[vmid]
	return ok && l.gone[g.Node]
}

// releaseMachine ends Kubehenge's hold on a machine at once, and clears the record of the outage.
// An administrator's own skip annotation stays.
func (w *VMWatch) releaseMachine(ctx context.Context, m *clusterv1.Machine) error {
	_, ours := m.Annotations[HoldAnnotation]
	if _, hasSince := m.Annotations[HoldSinceAnnotation]; !ours && !hasSince {
		return nil
	}
	return w.patchHold(ctx, m, map[string]*string{
		clusterv1.MachineSkipRemediationAnnotation: nil, HoldAnnotation: nil, HoldSinceAnnotation: nil, HoldUntilAnnotation: nil,
	})
}

// holdMachine moves one machine to the state its cause calls for. It returns what the condition
// says of a machine that stays held, and its reason, both empty otherwise.
func (w *VMWatch) holdMachine(ctx context.Context, vm clusterVM, cause *machineHold, now time.Time, ceiling time.Duration) (string, string, error) {
	m := vm.m
	_, ours := m.Annotations[HoldAnnotation]
	sinceText, hasSince := m.Annotations[HoldSinceAnnotation]
	since, _ := time.Parse(time.RFC3339, sinceText)
	_, skipping := m.Annotations[clusterv1.MachineSkipRemediationAnnotation]
	expired := hasSince && ceiling > 0 && now.Sub(since) >= ceiling
	switch {
	case cause != nil && skipping && !ours && !hasSince:
		return "", "", nil // an administrator's own skip stands
	case cause != nil && expired:
		if !skipping && !ours {
			return "", "", nil
		}
		return "", "", w.patchHold(ctx, m, map[string]*string{
			clusterv1.MachineSkipRemediationAnnotation: nil, HoldAnnotation: nil, HoldUntilAnnotation: nil,
		})
	case cause != nil:
		if !hasSince {
			since = now
		}
		var until *string
		if ceiling > 0 {
			until = new(since.Add(ceiling).UTC().Format(time.RFC3339))
		}
		current, hasUntil := m.Annotations[HoldUntilAnnotation]
		if m.Annotations[HoldAnnotation] != cause.text || !skipping || !hasSince || hasUntil != (until != nil) || until != nil && current != *until {
			err := w.patchHold(ctx, m, map[string]*string{
				clusterv1.MachineSkipRemediationAnnotation: new(""),
				HoldAnnotation:      &cause.text,
				HoldSinceAnnotation: new(since.UTC().Format(time.RFC3339)),
				HoldUntilAnnotation: until,
			})
			if err != nil {
				return "", "", err
			}
		}
		return holdLine(vm.machine, cause.text, since, ceiling), cause.reason, nil
	case ours && !expired && !meta.IsStatusConditionTrue(m.Status.Conditions, clusterv1.MachineNodeReadyCondition):
		return holdLine(vm.machine, "its node is not Ready yet", since, ceiling), HeldNodeRecovering, nil
	case ours || hasSince:
		return "", "", w.patchHold(ctx, m, map[string]*string{
			clusterv1.MachineSkipRemediationAnnotation: nil, HoldAnnotation: nil, HoldSinceAnnotation: nil, HoldUntilAnnotation: nil,
		})
	}
	return "", "", nil
}

// holdLine is what the condition says of one held machine.
func holdLine(machine, why string, since time.Time, ceiling time.Duration) string {
	if ceiling == 0 {
		return fmt.Sprintf("%s: %s, replacement waits for the host", machine, why)
	}
	return fmt.Sprintf("%s: %s, replacement waits until %s", machine, why, since.Add(ceiling).UTC().Format(holdClockLayout))
}

// HostOutageGraceVariable is the ClusterClass variable giving the minutes a machine whose host is
// away is kept from replacement. Zero keeps it for as long as the host is away.
const HostOutageGraceVariable = "hostOutageGraceMinutes"

// DefaultHostOutageGraceMinutes is the variable's default, which the schema gives too.
const DefaultHostOutageGraceMinutes = 30

// MinHostOutageGraceMinutes is the least a set ceiling may be, a reboot's firmware alone taking minutes.
const MinHostOutageGraceMinutes = 5

// MaxHostOutageGraceMinutes is a week, the most a hold may last.
const MaxHostOutageGraceMinutes = 7 * 24 * 60

// validateHostOutageGrace holds the variable to the schema's rule at admission too, so that a
// cluster the schema's defaulting never reached is refused as well: zero, or 5 minutes to a week.
func validateHostOutageGrace(c *clusterv1.Cluster) field.ErrorList {
	minutes, set, err := optionalVariable[int](c, HostOutageGraceVariable)
	path := field.NewPath("spec", "topology", "variables").Key(HostOutageGraceVariable)
	switch {
	case err != nil:
		return field.ErrorList{field.Invalid(path, HostOutageGraceVariable, "a whole number of minutes")}
	case !set, minutes == 0, minutes >= MinHostOutageGraceMinutes && minutes <= MaxHostOutageGraceMinutes:
		return nil
	}
	return field.ErrorList{field.Invalid(path, minutes, fmt.Sprintf("0 for no ceiling, or %d to %d minutes", MinHostOutageGraceMinutes, MaxHostOutageGraceMinutes))}
}

// hostOutageGrace is the cluster's ceiling on a hold, zero for none.
func hostOutageGrace(c *clusterv1.Cluster) (time.Duration, error) {
	minutes, set, err := optionalVariable[int](c, HostOutageGraceVariable)
	if err != nil {
		return 0, err
	}
	if !set {
		minutes = DefaultHostOutageGraceMinutes
	}
	return time.Duration(minutes) * time.Minute, nil
}

// patchHold sets the annotations given, and takes away those given as nil.
func (w *VMWatch) patchHold(ctx context.Context, m *clusterv1.Machine, annotations map[string]*string) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{annotationsKey: annotations}})
	if err != nil {
		return fmt.Errorf("encoding the hold on machine %s: %w", m.Name, err)
	}
	target := &clusterv1.Machine{}
	target.Namespace, target.Name = m.Namespace, m.Name
	if err := w.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, body)); err != nil {
		return fmt.Errorf("holding machine %s: %w", m.Name, err)
	}
	return nil
}
