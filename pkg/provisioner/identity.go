package provisioner

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The events the VM watch records on a Cluster for the identity of its VMs.
const (
	VMRenamedBack      = "VMRenamedBack"
	VMRenameFailed     = "VMRenameFailed"
	NodeZoneUpdated    = "NodeZoneUpdated"
	NodeZoneNotUpdated = "NodeZoneNotUpdated"
)

// The CSI plugin's node pods, which report their node's topology to the kubelet only when they start.
const (
	csiPluginName    = "proxmox-csi-plugin"
	csiNodeComponent = "node"
)

// workloadClients reaches the workload clusters; *Remotes does.
type workloadClients interface {
	Client(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, bool, error)
	forget(cluster *clusterv1.Cluster)
}

// reportKey tells one warning on a Cluster from another.
type reportKey struct {
	vmid   int64
	reason string
}

// keepIdentity sets back what the cloud controller manager and the CSI plugin take a VM and its node
// by, for the cluster's VMs in the cluster's own pool: the VM's name, which the cloud controller
// manager wants to start with its node's name or it reports the instance gone and Kubernetes
// deletes the Node, and the zone label of its node, which follows the Proxmox VE node the VM runs on.
func (w *VMWatch) keepIdentity(ctx context.Context, c *clusterv1.Cluster, vms []clusterVM, look *vmLook) error {
	names, err := NamesFor(w.WorkloadPool, c.Namespace, c.Name)
	if err != nil {
		return err
	}
	var own []clusterVM
	var errs []error
	for _, vm := range vms {
		g, ok := look.guests[vm.vmid]
		if !ok || g.Pool != names.Pool {
			continue
		}
		errs = append(errs, w.renameBack(ctx, c, vm, &g))
		own = append(own, vm)
	}
	return errors.Join(append(errs, w.followZones(ctx, c, own, look))...)
}

// renameBack gives a VM the name capmox made it with. A VM under a lock is left to the next look,
// as Proxmox VE refuses to configure it.
func (w *VMWatch) renameBack(ctx context.Context, c *clusterv1.Cluster, vm clusterVM, g *pveGuest) error {
	if g.Name == "" || g.Name == vm.vmName || g.Lock != "" {
		if g.Name == vm.vmName {
			delete(w.reported, reportKey{vm.vmid, VMRenameFailed})
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(g.Node), vm.vmid)
	if err := w.Proxmox.Put(ctx, path, map[string]any{"name": vm.vmName}, nil); err != nil {
		w.warn(c, vm.vmid, VMRenameFailed, fmt.Sprintf(
			"VM %d is named %q, not %q, and renaming it back failed: %v. The cloud controller manager deletes the node of a VM whose name does not start with the node's name.",
			vm.vmid, g.Name, vm.vmName, err))
		return fmt.Errorf("renaming VM %d back to %s: %w", vm.vmid, vm.vmName, err)
	}
	delete(w.reported, reportKey{vm.vmid, VMRenameFailed})
	if w.Events != nil {
		w.Events.Eventf(c, nil, corev1.EventTypeNormal, VMRenamedBack, "WatchVMs",
			"VM %d was renamed from %q back to %q. The cloud controller manager deletes the node of a VM whose name does not start with the node's name, and Cluster API then replaces the machine.",
			vm.vmid, g.Name, vm.vmName)
	}
	return nil
}

// followZones points the zone label of the nodes whose VM runs on another Proxmox VE node than the
// one the label was last found to name. The workload cluster is asked only for those, so that a
// cluster whose VMs stay put costs it nothing.
func (w *VMWatch) followZones(ctx context.Context, c *clusterv1.Cluster, vms []clusterVM, look *vmLook) error {
	var due []clusterVM
	for _, vm := range vms {
		if w.zoned[vm.vmid] != look.guests[vm.vmid].Node || w.restartDue[vm.vmid] {
			due = append(due, vm)
		}
	}
	if len(due) == 0 || w.Workloads == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	workload, found, err := w.Workloads.Client(ctx, c)
	if err != nil {
		return fmt.Errorf("reaching the workload cluster: %w", err)
	}
	if !found {
		return nil
	}
	for _, vm := range due {
		if err := w.followZone(ctx, c, workload, vm, look.guests[vm.vmid].Node); err != nil {
			w.Workloads.forget(c)
			return err
		}
	}
	return nil
}

// followZone sets the zone label of vm's node to pveNode and restarts the CSI node plugin on it.
// A node without the label has not been initialised by the cloud controller manager, which sets it
// then.
func (w *VMWatch) followZone(ctx context.Context, c *clusterv1.Cluster, workload client.Client, vm clusterVM, pveNode string) error {
	node := &corev1.Node{}
	if err := workload.Get(ctx, client.ObjectKey{Name: vm.node}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading node %s: %w", vm.node, err)
	}
	if have, labelled := node.Labels[corev1.LabelTopologyZone]; labelled && have != pveNode {
		if err := setZone(ctx, workload, node, pveNode); err != nil {
			w.warn(c, vm.vmid, NodeZoneNotUpdated, fmt.Sprintf("Node %s runs on Proxmox VE node %s but is labelled zone %s, and relabelling it failed: %v", vm.node, pveNode, have, err))
			return err
		}
		if w.restartDue == nil {
			w.restartDue = map[int64]bool{}
		}
		w.restartDue[vm.vmid] = true
		delete(w.reported, reportKey{vm.vmid, NodeZoneNotUpdated})
		if w.Events != nil {
			w.Events.Eventf(c, nil, corev1.EventTypeNormal, NodeZoneUpdated, "WatchVMs",
				"Node %s moved from Proxmox VE node %s to %s. Its zone label follows, and the CSI plugin on it restarts to report the new zone.", vm.node, have, pveNode)
		}
	}
	if w.restartDue[vm.vmid] {
		if err := restartCSINode(ctx, workload, vm.node); err != nil {
			w.warn(c, vm.vmid, NodeZoneNotUpdated, fmt.Sprintf("Node %s is labelled zone %s, but restarting its CSI node plugin failed: %v", vm.node, pveNode, err))
			return err
		}
		delete(w.restartDue, vm.vmid)
		delete(w.reported, reportKey{vm.vmid, NodeZoneNotUpdated})
	}
	if w.zoned == nil {
		w.zoned = map[int64]string{}
	}
	w.zoned[vm.vmid] = pveNode
	return nil
}

// setZone labels the node with its Proxmox VE node, as the cloud controller manager did at the start.
func setZone(ctx context.Context, workload client.Client, node *corev1.Node, pveNode string) error {
	before := node.DeepCopy()
	node.Labels[corev1.LabelTopologyZone] = pveNode
	if _, ok := node.Labels[corev1.LabelFailureDomainBetaZone]; ok {
		node.Labels[corev1.LabelFailureDomainBetaZone] = pveNode
	}
	if err := workload.Patch(ctx, node, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("labelling node %s: %w", node.Name, err)
	}
	return nil
}

// restartCSINode deletes the CSI plugin's node pod on the node, which its DaemonSet makes again.
// The plugin tells the kubelet its node's topology from the node's labels when it starts.
func restartCSINode(ctx context.Context, workload client.Client, node string) error {
	pods := &corev1.PodList{}
	err := workload.List(ctx, pods, client.InNamespace(metav1.NamespaceSystem), client.MatchingLabels{
		"app.kubernetes.io/name":      csiPluginName,
		"app.kubernetes.io/component": csiNodeComponent,
	})
	if err != nil {
		return fmt.Errorf("listing the CSI node pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName != node {
			continue
		}
		err := workload.Delete(ctx, &pods.Items[i])
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("restarting the CSI node pod %s: %w", pods.Items[i].Name, err)
		}
	}
	return nil
}

// warn tells a Cluster of a failure that the watch retries, once for each way a VM fails.
func (w *VMWatch) warn(c *clusterv1.Cluster, vmid int64, reason, note string) {
	if w.Events == nil {
		return
	}
	key := reportKey{vmid, reason}
	if w.reported[key] == note {
		return
	}
	if w.reported == nil {
		w.reported = map[reportKey]string{}
	}
	w.reported[key] = note
	w.Events.Eventf(c, nil, corev1.EventTypeWarning, reason, "WatchVMs", "%s", shorten(note, watchEventMax))
}

// forgetGone drops what the watch kept about VMs no longer in the resource list.
func (w *VMWatch) forgetGone(look *vmLook) {
	for vmid := range w.zoned {
		if _, ok := look.guests[vmid]; !ok {
			delete(w.zoned, vmid)
		}
	}
	for vmid := range w.restartDue {
		if _, ok := look.guests[vmid]; !ok {
			delete(w.restartDue, vmid)
		}
	}
	for key := range w.reported {
		if _, ok := look.guests[key.vmid]; !ok {
			delete(w.reported, key)
		}
	}
}
