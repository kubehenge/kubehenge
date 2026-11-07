package provisioner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// capmoxFinalizer keeps a ProxmoxMachine until capmox has destroyed its VM.
const capmoxFinalizer = "proxmoxmachine.infrastructure.cluster.x-k8s.io"

// MachineReleasedReason is the reason of the event that says a machine was let go without its VM
// being destroyed. The VM's Proxmox VE node has left the cluster, or an administrator forgot it.
const MachineReleasedReason = "NodeGone"

// MachineForgetRefusedReason is the reason of the event that says why a request to forget a machine
// was refused.
const MachineForgetRefusedReason = "ForgetRefused"

// MachineDeletionStalledCondition is set on a Machine that has been deleting for DeletionStallAfter,
// with the reason capmox cannot finish.
const MachineDeletionStalledCondition = "MachineDeletionStalled"

// ForgetAnnotation asks Kubehenge to let go of a machine being deleted without destroying its VM.
// Its value is the machine's name, so that a stray annotation cannot release another machine. The
// interface, the CLI and kubectl all set it, and the guard in forgetRefusal answers every one alike.
const ForgetAnnotation = "kubehenge.dev/forget-machine"

// ForgetRefusedAnnotation holds why the last request to forget the machine was refused.
const ForgetRefusedAnnotation = "kubehenge.dev/forget-refused"

// DeletionStallAfter is how long a machine may be deleting before its condition says it is stuck.
// It is also the least time an administrator's forget request waits for capmox to finish.
const DeletionStallAfter = 20 * time.Minute

// proxmoxClusterListGVK lists capmox's ProxmoxClusters, which record where each machine runs.
var proxmoxClusterListGVK = schema.GroupVersionKind{Group: infrastructureGroup, Version: proxmoxMachineVersion, Kind: "ProxmoxClusterList"}

// deletingMachine is a Machine being deleted with the ProxmoxMachine that still holds capmox's finalizer.
type deletingMachine struct {
	m    *clusterv1.Machine
	pm   *unstructured.Unstructured
	vmid int64
	// guest is the VM as Proxmox VE lists it, and listed whether it does.
	guest  pveGuest
	listed bool
}

// settleDeletingMachines lets go of machines being deleted whose VM can no longer be destroyed, and
// says why the others are stuck. capmox reads the VM's node before it destroys the VM, which a node
// that no longer answers refuses for ever, so the machine would stay deleting and Cluster API would
// neither scale up nor remediate while it does. A node that is only offline is not in look.gone: its
// VM may return, so its machine keeps waiting, unless an administrator asks to forget it.
func (w *VMWatch) settleDeletingMachines(ctx context.Context, c *clusterv1.Cluster, look *vmLook) error {
	stuck, err := w.deletingMachines(ctx, c, look)
	if err != nil {
		return err
	}
	var errs []error
	for i := range stuck {
		d := &stuck[i]
		switch {
		case look.goneGuest(d.vmid):
			errs = append(errs, w.forget(ctx, c, d, fmt.Sprintf("Proxmox VE node %s has left the cluster", d.guest.Node)))
		case d.m.Annotations[ForgetAnnotation] != "":
			errs = append(errs, w.answerForget(ctx, c, d, look))
		default:
			errs = append(errs, w.markStalled(ctx, d, look))
		}
	}
	return errors.Join(errs...)
}

// deletingMachines lists the cluster's machines that wait on capmox to destroy their VM.
func (w *VMWatch) deletingMachines(ctx context.Context, c *clusterv1.Cluster, look *vmLook) ([]deletingMachine, error) {
	var machines clusterv1.MachineList
	if err := w.Client.List(ctx, &machines, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return nil, fmt.Errorf("listing the cluster's machines: %w", err)
	}
	pms := proxmoxMachineList()
	if err := w.Client.List(ctx, pms, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return nil, fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	byName := make(map[string]*unstructured.Unstructured, len(pms.Items))
	for i := range pms.Items {
		byName[pms.Items[i].GetName()] = &pms.Items[i]
	}
	var out []deletingMachine
	for i := range machines.Items {
		m := &machines.Items[i]
		pm := byName[m.Spec.InfrastructureRef.Name]
		if m.DeletionTimestamp.IsZero() || pm == nil || pm.GetDeletionTimestamp().IsZero() || !slices.Contains(pm.GetFinalizers(), capmoxFinalizer) {
			continue
		}
		vmid, found, _ := unstructured.NestedInt64(pm.Object, "spec", "virtualMachineID")
		if !found {
			continue
		}
		g, listed := look.guests[vmid]
		out = append(out, deletingMachine{m: m, pm: pm, vmid: vmid, guest: g, listed: listed})
	}
	return out, nil
}

// forgetRefusal says why a machine may not be forgotten, or "" when it may. The one guard answers
// the interface, the CLI and kubectl alike. A VM that still runs on a node that answers is never
// forgotten, since letting go of it would leave it running with nothing to manage it.
func forgetRefusal(d *deletingMachine, look *vmLook, now time.Time) string {
	if d.m.DeletionTimestamp.IsZero() {
		return "the machine is not being deleted"
	}
	if age := now.Sub(d.m.DeletionTimestamp.Time); age < DeletionStallAfter {
		return fmt.Sprintf("the machine has been deleting for only %s, and capmox may still finish", age.Round(time.Second))
	}
	if d.listed && look.nodes[d.guest.Node] == nodeOnline && d.guest.Status != guestStopped {
		return fmt.Sprintf("VM %d is %s on node %s, which answers. Stop it first", d.vmid, d.guest.Status, d.guest.Node)
	}
	return ""
}

// stalledWhy says what keeps capmox from finishing a machine's deletion.
func stalledWhy(d *deletingMachine, look *vmLook) string {
	switch {
	case !d.listed:
		return fmt.Sprintf("VM %d is not listed in Proxmox VE, or not by this token. capmox cannot find it to destroy it", d.vmid)
	case look.maintenance[d.guest.Node]:
		return fmt.Sprintf("Proxmox VE node %s is in maintenance, so capmox waits for it", d.guest.Node)
	case look.nodes[d.guest.Node] != nodeOnline:
		return fmt.Sprintf("Proxmox VE node %s is %s, so capmox waits for it to return", d.guest.Node, look.nodes[d.guest.Node])
	default:
		return fmt.Sprintf("capmox does not destroy VM %d though node %s answers (VM %s). See the ProxmoxMachine's conditions and capmox's log", d.vmid, d.guest.Node, d.guest.Status)
	}
}

// markStalled sets MachineDeletionStalled on a machine that has been deleting for long, and clears it
// when the machine is no longer stuck.
func (w *VMWatch) markStalled(ctx context.Context, d *deletingMachine, look *vmLook) error {
	if w.now().Sub(d.m.DeletionTimestamp.Time) < DeletionStallAfter {
		return nil
	}
	before := d.m.DeepCopy()
	meta.SetStatusCondition(&d.m.Status.Conditions, metav1.Condition{
		Type: MachineDeletionStalledCondition, Status: metav1.ConditionTrue, Reason: "WaitingOnProxmoxVE", Message: stalledWhy(d, look),
	})
	if equalConditions(before.Status.Conditions, d.m.Status.Conditions) {
		return nil
	}
	if err := w.Client.Status().Patch(ctx, d.m, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("saying why machine %s is stuck deleting: %w", d.m.Name, err)
	}
	return nil
}

func equalConditions(a, b []metav1.Condition) bool {
	return slices.EqualFunc(a, b, func(x, y metav1.Condition) bool {
		return x.Type == y.Type && x.Status == y.Status && x.Reason == y.Reason && x.Message == y.Message
	})
}

// answerForget applies an administrator's request to forget a machine, or refuses it with the reason.
func (w *VMWatch) answerForget(ctx context.Context, c *clusterv1.Cluster, d *deletingMachine, look *vmLook) error {
	refusal := ""
	if d.m.Annotations[ForgetAnnotation] != d.m.Name {
		refusal = fmt.Sprintf("the request names %q, not the machine %q", d.m.Annotations[ForgetAnnotation], d.m.Name)
	} else {
		refusal = forgetRefusal(d, look, w.now())
	}
	if refusal == "" {
		return w.forget(ctx, c, d, "an administrator asked to forget it")
	}
	before := d.m.DeepCopy()
	delete(d.m.Annotations, ForgetAnnotation)
	d.m.Annotations[ForgetRefusedAnnotation] = refusal
	if err := w.Client.Patch(ctx, d.m, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("refusing to forget machine %s: %w", d.m.Name, err)
	}
	if w.Events != nil {
		w.Events.Eventf(c, nil, corev1.EventTypeWarning, MachineForgetRefusedReason, "WatchVMs", "Machine %s was not forgotten: %s.", d.m.Name, refusal)
	}
	return nil
}

// forget removes capmox's finalizer from the machine's ProxmoxMachine, so that Cluster API finishes
// deleting the machine, and removes the machine from the ProxmoxCluster's record of where machines run.
func (w *VMWatch) forget(ctx context.Context, c *clusterv1.Cluster, d *deletingMachine, why string) error {
	before := d.pm.DeepCopy()
	controllerutil.RemoveFinalizer(d.pm, capmoxFinalizer)
	if err := w.Client.Patch(ctx, d.pm, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("letting go of machine %s: %w", d.m.Name, err)
	}
	if w.Events != nil {
		w.Events.Eventf(c, nil, corev1.EventTypeWarning, MachineReleasedReason, "WatchVMs",
			"Machine %s was deleted without destroying VM %d, because %s. Remove the VM's configuration from Proxmox VE if it is still listed.", d.m.Name, d.vmid, why)
	}
	return w.forgetLocation(ctx, c, d.m.Name)
}

// forgetLocation removes a machine from the ProxmoxCluster's node locations, which capmox keeps for
// every machine it placed and would otherwise keep pointing at the released one.
func (w *VMWatch) forgetLocation(ctx context.Context, c *clusterv1.Cluster, machine string) error {
	pcs := &unstructured.UnstructuredList{}
	pcs.SetGroupVersionKind(proxmoxClusterListGVK)
	if err := w.Client.List(ctx, pcs, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return fmt.Errorf("listing the cluster's ProxmoxClusters: %w", err)
	}
	var errs []error
	for i := range pcs.Items {
		pc := &pcs.Items[i]
		before := pc.DeepCopy()
		changed := false
		for _, role := range []string{"controlPlane", "workers"} {
			path := []string{"status", "nodeLocations", role}
			locations, found, _ := unstructured.NestedSlice(pc.Object, path...)
			if !found {
				continue
			}
			kept := make([]any, 0, len(locations))
			for _, l := range locations {
				entry, _ := l.(map[string]any)
				if name, _, _ := unstructured.NestedString(entry, "machine", "name"); name != machine {
					kept = append(kept, l)
				}
			}
			if len(kept) != len(locations) {
				changed = true
				_ = unstructured.SetNestedSlice(pc.Object, kept, path...)
			}
		}
		if !changed {
			continue
		}
		if err := w.Client.Status().Patch(ctx, pc, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing machine %s from the node locations of %s: %w", machine, pc.GetName(), err))
		}
	}
	return errors.Join(errs...)
}
