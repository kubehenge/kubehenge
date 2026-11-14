package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// StorageLacksSpaceCondition on a Cluster says that its storage reserves each disk's whole size and
// has no room for the disks of the machines it has still to make, as the interface refuses such a
// cluster when it is created.
const StorageLacksSpaceCondition = "StorageLacksSpace"

// disksToMake is the disk, in GiB, of the machines a cluster has still to make: in each pool those
// beyond the machines that exist, and those whose VM is not cloned yet. A cloned VM's disk holds
// its space already, so the storage's free space leaves it out. vmids are the VM IDs capmox
// recorded on the cluster's ProxmoxMachines, by name.
func disksToMake(fit Fit, machines []clusterv1.Machine, vmids map[string]int) int64 {
	var need int64
	for _, pool := range fit.Pools {
		var exist, unmade int64
		for i := range machines {
			m := &machines[i]
			if !m.DeletionTimestamp.IsZero() || !inPool(m, pool.Name) {
				continue
			}
			exist++
			if !provisioned(m) && vmids[m.Spec.InfrastructureRef.Name] == 0 {
				unmade++
			}
		}
		need += (max(pool.Machines-exist, 0) + unmade) * pool.DiskGiB
	}
	return need
}

// provisioned tells whether Cluster API records a machine's infrastructure as made.
func provisioned(m *clusterv1.Machine) bool {
	p := m.Status.Initialization.InfrastructureProvisioned
	return p != nil && *p
}

// inPool tells whether a machine is one of the control planes (pool "") or of the worker pool named.
func inPool(m *clusterv1.Machine, pool string) bool {
	if _, cp := m.Labels[clusterv1.MachineControlPlaneLabel]; cp {
		return pool == ""
	}
	return pool != "" && m.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel] == pool
}

// diskRoom says why the storage has no room for the disks still to be made, or "" when it has, or
// when a figure is not known.
func diskRoom(fit Fit, machines []clusterv1.Machine, vmids map[string]int, storages []pve.Storage, nodes []string) string {
	need := disksToMake(fit, machines, vmids)
	if fit.Storage == "" || need == 0 {
		return ""
	}
	free, thick := pve.FreeOn(storages, fit.Storage, nodes)
	if !thick || free < 0 || need*bytesPerGiB <= free {
		return ""
	}
	where := "shared"
	if !slices.ContainsFunc(storages, func(s pve.Storage) bool { return s.Storage == fit.Storage && s.Shared }) {
		var on []string
		for _, s := range storages {
			if s.Storage == fit.Storage && s.Total > 0 && slices.Contains(nodes, s.Node) {
				on = append(on, s.Node)
			}
		}
		where = "on " + strings.Join(on, ", ")
	}
	return fmt.Sprintf("Storage %s (%s) reserves each disk's whole size and has %d GiB free. The machines still to be made need %d GiB",
		fit.Storage, where, free/bytesPerGiB, need)
}

// markDisks records on the cluster that its storage has no room for the disks still to be made,
// and clears the record once it has. nodes are those the cluster's new VMs may go to.
func (p *Placer) markDisks(ctx context.Context, c *clusterv1.Cluster, storages []pve.Storage, nodes []string) {
	log := ctrl.LoggerFrom(ctx).WithName("placer")
	class := &clusterv1.ClusterClass{}
	if err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.ClassNamespace, Name: c.Spec.Topology.ClassRef.Name}, class); err != nil {
		log.Error(err, "reading the cluster's class for its machines' disks", "cluster", client.ObjectKeyFromObject(c))
		return
	}
	fit, err := FitOf(c, class)
	if err != nil {
		log.Error(err, "sizing the cluster's disks", "cluster", client.ObjectKeyFromObject(c))
		return
	}
	machines := &clusterv1.MachineList{}
	if err := p.Client.List(ctx, machines, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		log.Error(err, "listing the cluster's machines", "cluster", client.ObjectKeyFromObject(c))
		return
	}
	// Only a machine Cluster API does not record as provisioned may lack its VM.
	var vmids map[string]int
	if slices.ContainsFunc(machines.Items, func(m clusterv1.Machine) bool { return !provisioned(&m) }) {
		if vmids, err = ProxmoxMachineVMIDs(ctx, p.Client, c.Namespace, c.Name); err != nil {
			log.Error(err, "reading which of the cluster's VMs are cloned", "cluster", client.ObjectKeyFromObject(c))
			return
		}
	}
	var cond *metav1.Condition
	have := meta.FindStatusCondition(c.Status.Conditions, StorageLacksSpaceCondition)
	if msg := diskRoom(fit, machines.Items, vmids, storages, nodes); msg != "" {
		cond = &metav1.Condition{Status: metav1.ConditionTrue, Reason: "NoSpace", Message: msg}
		if have != nil && have.Message == msg {
			return
		}
	} else if have == nil {
		return
	}
	if _, err := markCluster(ctx, p.Client, p.Client, client.ObjectKeyFromObject(c), StorageLacksSpaceCondition, cond); err != nil && !apierrors.IsNotFound(err) && ctx.Err() == nil {
		log.Error(err, "recording the lack of disk space", "cluster", client.ObjectKeyFromObject(c))
	}
}
