package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
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
// recorded on the cluster's ProxmoxMachines, by name. target is how many machines a pool runs.
func disksToMake(fit *Fit, machines []clusterv1.Machine, vmids map[string]int, target func(*PoolFit) int64) int64 {
	var need int64
	for p := range fit.Pools {
		pool := &fit.Pools[p]
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
		need += (max(target(pool)-exist, 0) + unmade) * pool.DiskGiB
	}
	return need
}

// asked is the machines a pool runs without growing: its replicas, or an autoscaled pool's minimum.
// The machines the autoscaler adds exist as Machines, and count once they do.
func asked(p *PoolFit) int64 { return p.Machines }

// atMost is the machines a pool runs once the autoscaler has grown it as far as it may.
func atMost(p *PoolFit) int64 { return p.Most }

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
// when a figure is not known. The second message says the same of the autoscaled pools at their
// maximum, where only they lack room.
func diskRoom(fit *Fit, machines []clusterv1.Machine, vmids map[string]int, storages []pve.Storage, nodes []string) (string, string) {
	need, grown := disksToMake(fit, machines, vmids, asked), disksToMake(fit, machines, vmids, atMost)
	if fit.Storage == "" || grown == 0 {
		return "", ""
	}
	free, thick := pve.FreeOn(storages, fit.Storage, nodes)
	if !thick || free < 0 || grown*bytesPerGiB <= free {
		return "", ""
	}
	where := storageWhere(fit.Storage, storages, nodes)
	if need*bytesPerGiB > free {
		return fmt.Sprintf("Storage %s (%s) reserves each disk's whole size and has %d GiB free. The machines still to be made need %d GiB",
			fit.Storage, where, free/bytesPerGiB, need), ""
	}
	return "", fmt.Sprintf("Storage %s (%s) reserves each disk's whole size and has %d GiB free. At the autoscaler's maximum, the machines still to be made need %d GiB. A worker past that room waits, and the autoscaler gives it up after 30 minutes",
		fit.Storage, where, free/bytesPerGiB, grown)
}

// storageWhere words where a storage is: shared, or on which of the nodes.
func storageWhere(storage string, storages []pve.Storage, nodes []string) string {
	if slices.ContainsFunc(storages, func(s pve.Storage) bool { return s.Storage == storage && s.Shared }) {
		return "shared"
	}
	var on []string
	for i := range storages {
		if s := &storages[i]; s.Storage == storage && s.Total > 0 && slices.Contains(nodes, s.Node) {
			on = append(on, s.Node)
		}
	}
	return "on " + strings.Join(on, ", ")
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
	if err = p.Client.List(ctx, machines, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
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
	msg, growth := diskRoom(&fit, machines.Items, vmids, storages, nodes)
	p.sayGrowth(c, growth)
	var cond *metav1.Condition
	have := meta.FindStatusCondition(c.Status.Conditions, StorageLacksSpaceCondition)
	if msg != "" {
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

// sayGrowth warns in an event, once each time it starts or changes, that the storage lacks room for
// the autoscaled pools at their maximum. The autoscaler may never grow that far, so the cluster
// carries no condition for it.
func (p *Placer) sayGrowth(c *clusterv1.Cluster, msg string) {
	key := client.ObjectKeyFromObject(c).String()
	if msg == "" {
		delete(p.growth, key)
		return
	}
	if p.growth[key] == msg {
		return
	}
	if p.growth == nil {
		p.growth = map[string]string{}
	}
	p.growth[key] = msg
	if p.Events != nil {
		p.Events.Eventf(c, nil, corev1.EventTypeWarning, "AutoscalerNoRoom", "AddWorker", "%s", msg)
	}
}
