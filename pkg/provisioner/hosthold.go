package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

	// HoldCeiling is how long a machine is held. A host that stays gone longer than a reboot or a
	// maintenance takes needs its machines replaced elsewhere.
	HoldCeiling = 30 * time.Minute

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

// pveHAStatus is the part of the HA manager's status that tells which nodes are in maintenance.
// LAB: node_status as read from /cluster/ha/status/manager_status on a real host.
type pveHAStatus struct {
	ManagerStatus pveHAManager `json:"manager_status"`
}

type pveHAManager struct {
	NodeStatus map[string]string `json:"node_status"`
}

// haMaintenance lists the nodes the HA manager has in maintenance mode.
func haMaintenance(ctx context.Context, r proxmoxReader) map[string]bool {
	var status pveHAStatus
	if err := r.Get(ctx, "/cluster/ha/status/manager_status", &status); err != nil {
		ctrl.LoggerFrom(ctx).WithName("ha-status").Error(err, "reading the HA manager's status")
		return nil
	}
	maintenance := map[string]bool{}
	for node, state := range status.ManagerStatus.NodeStatus {
		if state == "maintenance" {
			maintenance[node] = true
		}
	}
	return maintenance
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
func storageUnavailableCondition(storage string, vms []clusterVM, look *vmLook) *metav1.Condition {
	down := look.storageDown(storage)
	var nodes []string
	for _, vm := range vms {
		g, ok := look.guests[vm.vmid]
		if !ok || !down[g.Node] || slices.Contains(nodes, g.Node) {
			continue
		}
		if status, listed := look.nodes[g.Node]; listed && status != nodeStatusOnline {
			continue
		}
		nodes = append(nodes, g.Node)
	}
	if len(nodes) == 0 {
		return nil
	}
	slices.Sort(nodes)
	return &metav1.Condition{
		Status: metav1.ConditionTrue, Reason: StorageUnavailableReason,
		Message: fmt.Sprintf("Storage %s is unavailable on %s. Its machines are not replaced for %d minutes. Bring the storage back in Proxmox VE.",
			storage, strings.Join(nodes, ", "), int(HoldCeiling/time.Minute)),
	}
}

// holdMachines places, keeps and lifts the hold on each of the cluster's machines, and says which
// are held. A hold lifts when its cause has gone and the machine's node is Ready, or at HoldCeiling.
func (w *VMWatch) holdMachines(ctx context.Context, vms []clusterVM, storage string, look *vmLook) (*metav1.Condition, error) {
	now := w.now()
	var held []string
	found := map[string]bool{}
	var errs []error
	for _, vm := range vms {
		kept, reason, err := w.holdMachine(ctx, vm, holdOf(vm, storage, look), now)
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

// holdMachine moves one machine to the state its cause calls for. It returns what the condition
// says of a machine that stays held, and its reason, both empty otherwise.
func (w *VMWatch) holdMachine(ctx context.Context, vm clusterVM, cause *machineHold, now time.Time) (string, string, error) {
	m := vm.m
	_, ours := m.Annotations[HoldAnnotation]
	sinceText, hasSince := m.Annotations[HoldSinceAnnotation]
	since, _ := time.Parse(time.RFC3339, sinceText)
	_, skipping := m.Annotations[clusterv1.MachineSkipRemediationAnnotation]
	expired := hasSince && now.Sub(since) >= HoldCeiling
	switch {
	case cause != nil && skipping && !ours && !hasSince:
		return "", "", nil // an administrator's own skip stands
	case cause != nil && expired:
		if !skipping && !ours {
			return "", "", nil
		}
		return "", "", w.patchHold(ctx, m, map[string]*string{clusterv1.MachineSkipRemediationAnnotation: nil, HoldAnnotation: nil})
	case cause != nil:
		if !hasSince {
			since = now
		}
		want := map[string]*string{
			clusterv1.MachineSkipRemediationAnnotation: new(""),
			HoldAnnotation:      &cause.text,
			HoldSinceAnnotation: new(since.UTC().Format(time.RFC3339)),
		}
		if m.Annotations[HoldAnnotation] != cause.text || !skipping || !hasSince {
			if err := w.patchHold(ctx, m, want); err != nil {
				return "", "", err
			}
		}
		return holdLine(vm.machine, cause.text, since), cause.reason, nil
	case ours && !expired && !meta.IsStatusConditionTrue(m.Status.Conditions, clusterv1.MachineNodeReadyCondition):
		return holdLine(vm.machine, "its node is not Ready yet", since), HeldNodeRecovering, nil
	case ours || hasSince:
		return "", "", w.patchHold(ctx, m, map[string]*string{
			clusterv1.MachineSkipRemediationAnnotation: nil, HoldAnnotation: nil, HoldSinceAnnotation: nil,
		})
	}
	return "", "", nil
}

// holdLine is what the condition says of one held machine.
func holdLine(machine, why string, since time.Time) string {
	return fmt.Sprintf("%s: %s, replacement waits until %s", machine, why, since.Add(HoldCeiling).UTC().Format(holdClockLayout))
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
