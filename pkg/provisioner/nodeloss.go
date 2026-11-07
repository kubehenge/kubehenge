package provisioner

import (
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// capmoxFinalizer keeps a ProxmoxMachine until capmox has destroyed its VM.
const capmoxFinalizer = "proxmoxmachine.infrastructure.cluster.x-k8s.io"

// MachineReleasedReason is the reason of the event that says a machine was let go without its VM
// being destroyed, because the VM's Proxmox VE node has left the cluster.
const MachineReleasedReason = "NodeGone"

// releaseLostMachines lets go of machines being deleted whose VM is on a Proxmox VE node that has
// left the cluster for good. capmox reads the VM's node before it destroys the VM, which a node that
// no longer answers refuses for ever, so the machine would stay deleting and Cluster API would
// neither scale up nor remediate while it does. A node that is only offline is not in look.gone:
// its VM may return, so its machine keeps waiting.
func (w *VMWatch) releaseLostMachines(ctx context.Context, c *clusterv1.Cluster, look *vmLook) error {
	if len(look.gone) == 0 {
		return nil
	}
	var machines clusterv1.MachineList
	if err := w.Client.List(ctx, &machines, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return fmt.Errorf("listing the cluster's machines: %w", err)
	}
	pms := proxmoxMachineList()
	if err := w.Client.List(ctx, pms, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	byName := make(map[string]*unstructured.Unstructured, len(pms.Items))
	for i := range pms.Items {
		byName[pms.Items[i].GetName()] = &pms.Items[i]
	}
	var errs []error
	for i := range machines.Items {
		m := &machines.Items[i]
		pm := byName[m.Spec.InfrastructureRef.Name]
		if m.DeletionTimestamp.IsZero() || pm == nil || pm.GetDeletionTimestamp().IsZero() || !slices.Contains(pm.GetFinalizers(), capmoxFinalizer) {
			continue
		}
		vmid, found, _ := unstructured.NestedInt64(pm.Object, "spec", "virtualMachineID")
		if !found || !look.goneGuest(vmid) {
			continue
		}
		node := look.guests[vmid].Node
		before := pm.DeepCopy()
		controllerutil.RemoveFinalizer(pm, capmoxFinalizer)
		if err := w.Client.Patch(ctx, pm, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("letting go of machine %s: %w", m.Name, err))
			}
			continue
		}
		if w.Events != nil {
			w.Events.Eventf(c, nil, corev1.EventTypeWarning, MachineReleasedReason, "WatchVMs",
				"Machine %s was deleted without destroying VM %d, because Proxmox VE node %s has left the cluster. Remove the VM's configuration from the cluster if it is still listed.", m.Name, vmid, node)
		}
	}
	return errors.Join(errs...)
}
