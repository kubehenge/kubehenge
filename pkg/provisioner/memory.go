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
)

// NodesLackMemoryCondition on a Cluster says that no Proxmox VE node it may use has the free memory
// for its largest machine, so that its placement was left as it was.
const NodesLackMemoryCondition = "NodesLackMemory"

const (
	bytesPerMiB = 1 << 20
	// memoryEntryMarginPercent is how far above the largest machine a node's free memory has to be
	// for the node to join a placement, while a node already in it stays down to the machine's size.
	// Free memory moves with every guest, so one threshold would have the placement flap between
	// two values on each look.
	memoryEntryMarginPercent = 10
)

// largestMachineMiB is the memory of the cluster's largest machine, the one a placement has to
// have room for.
func largestMachineMiB(c *clusterv1.Cluster, class *clusterv1.ClusterClass) (int64, error) {
	topology := &c.Spec.Topology
	_, most, err := nodeSize(c, class, "controlPlaneMachine", topology.ControlPlane.Variables.Overrides)
	if err != nil {
		return 0, err
	}
	for i := range topology.Workers.MachineDeployments {
		_, memory, err := nodeSize(c, class, "workerMachine", topology.Workers.MachineDeployments[i].Variables.Overrides)
		if err != nil {
			return 0, err
		}
		most = max(most, memory)
	}
	return int64(most), nil
}

// roomOn is the outcome of looking for the nodes with room for a cluster's largest machine.
type roomOn struct {
	// Nodes are the candidates with room, in their order.
	Nodes []string
	// Need is the memory asked for, in MiB, and Best the candidate with the most free memory.
	Need int64
	Best Node
}

// short tells whether there were candidates and none had room.
func (r *roomOn) short(candidates []string) bool { return len(candidates) > 0 && len(r.Nodes) == 0 }

// withRoom keeps of the candidates the nodes whose free memory takes a machine of need MiB. A node
// outside current, the placement as recorded, has to clear the entry margin as well. A node whose
// memory was not reported is kept.
func withRoom(online []Node, candidates, current []string, need int64) roomOn {
	out := roomOn{Need: need}
	entry := need + need*memoryEntryMarginPercent/percent
	for _, name := range candidates {
		i := slices.IndexFunc(online, func(n Node) bool { return n.Name == name })
		if i < 0 {
			continue
		}
		n := online[i]
		if !n.MemoryKnown {
			out.Nodes = append(out.Nodes, name)
			continue
		}
		if n.FreeMiB > out.Best.FreeMiB || out.Best.Name == "" {
			out.Best = n
		}
		if n.FreeMiB >= need && (slices.Contains(current, name) || n.FreeMiB >= entry) {
			out.Nodes = append(out.Nodes, name)
		}
	}
	return out
}

// recordedNodes are the nodes OnlineNodesAnnotation holds, and whether it is set.
func recordedNodes(c *clusterv1.Cluster) ([]string, bool) {
	value, ok := c.Annotations[OnlineNodesAnnotation]
	if !ok || value == "" {
		return nil, ok
	}
	return strings.Split(value, ","), true
}

// fitMemory narrows the hosts a cluster could run on to those with room for its largest machine.
// When none has, the placement stays as it was, narrowed to the hosts that can still run the
// cluster at all. A cluster with no placement yet takes the hosts as they are. Where the cluster's
// machines cannot be sized, nothing is narrowed.
func fitMemory(ctx context.Context, reader client.Reader, classNamespace string, c *clusterv1.Cluster, online []Node, hosts []string) ([]string, *roomOn) {
	if reader == nil {
		return hosts, nil
	}
	class := &clusterv1.ClusterClass{}
	key := client.ObjectKey{Namespace: classNamespace, Name: c.Spec.Topology.ClassRef.Name}
	if err := reader.Get(ctx, key, class); err != nil {
		ctrl.LoggerFrom(ctx).WithName("placer").Error(err, "reading the cluster's class for its machines' memory", "cluster", client.ObjectKeyFromObject(c))
		return hosts, nil
	}
	need, err := largestMachineMiB(c, class)
	if err != nil {
		ctrl.LoggerFrom(ctx).WithName("placer").Error(err, "sizing the cluster's machines", "cluster", client.ObjectKeyFromObject(c))
		return hosts, nil
	}
	recorded, tracked := recordedNodes(c)
	// Without a placement there is nothing to keep steady, so every host is judged by the size alone.
	current := recorded
	if !tracked {
		current = hosts
	}
	room := withRoom(online, hosts, current, need)
	if !room.short(hosts) {
		return room.Nodes, nil
	}
	held := slices.DeleteFunc(slices.Clone(recorded), func(n string) bool { return !slices.Contains(hosts, n) })
	if !tracked || len(held) == 0 {
		return hosts, &room
	}
	return held, &room
}

// markShort records on the cluster that no node had room for it, as room says, and clears the
// record once room is nil.
func (p *Placer) markShort(ctx context.Context, c *clusterv1.Cluster, room *roomOn) {
	var cond *metav1.Condition
	have := meta.FindStatusCondition(c.Status.Conditions, NodesLackMemoryCondition)
	if room != nil {
		cond = &metav1.Condition{
			Status: metav1.ConditionTrue, Reason: "NoRoom",
			// Whole GiB keep the message, and so the status write, from changing with every look.
			Message: fmt.Sprintf("No Proxmox VE node this cluster may use has %d GiB of free memory for its largest machine. The best, %s, has %d GiB",
				(room.Need+mebibytesPerGibibyte-1)/mebibytesPerGibibyte, room.Best.Name, room.Best.FreeMiB/mebibytesPerGibibyte),
		}
		if have != nil && have.Message == cond.Message {
			return
		}
	} else if have == nil {
		return
	}
	if _, err := markCluster(ctx, p.Client, p.Client, client.ObjectKeyFromObject(c), NodesLackMemoryCondition, cond); err != nil && !apierrors.IsNotFound(err) && ctx.Err() == nil {
		ctrl.LoggerFrom(ctx).WithName("placer").Error(err, "recording the lack of memory", "cluster", client.ObjectKeyFromObject(c))
	}
}
