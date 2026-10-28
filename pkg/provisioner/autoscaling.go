package provisioner

import (
	"encoding/binary"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// The annotations that have the cluster autoscaler scale a pool, Cluster API's own. Set on a pool of
// the Cluster's topology, Cluster API copies them to the pool's MachineDeployment.
const (
	AutoscalerMinAnnotation = "cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size"
	AutoscalerMaxAnnotation = "cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size"
	// MaxAutoscaledWorkers is the most workers an autoscaled pool may grow to, as many as the
	// interface lets a pool have.
	MaxAutoscaledWorkers = 100
)

// capacityPrefix starts the annotations that tell the cluster autoscaler what a node of a pool
// holds, so that it scales a pool up from no nodes, where it has none to look at.
const capacityPrefix = "capacity.cluster-autoscaler.kubernetes.io/"

// capacityShare is the part of a VM's memory and disk the capacity annotations give its node: the
// kernel, the kubelet's eviction threshold and the disk's own partitions take the rest. Saying less
// than a node holds costs a pod that would just have fitted a scale-up; saying more has the
// autoscaler add nodes the pod then does not fit on, again and again.
const capacityShare = 0.9

// mebibytesPerGibibyte converts a machine's disk size to the unit of the capacity annotations.
const mebibytesPerGibibyte = 1024

// workerBounds are the least and the most workers a pool runs.
type workerBounds struct{ least, most int32 }

// poolScale reads the autoscaler's bounds on a pool, and whether it is autoscaled at all.
func poolScale(md *clusterv1.MachineDeploymentTopology) (workerBounds, bool, error) {
	minValue, hasMin := md.Metadata.Annotations[AutoscalerMinAnnotation]
	maxValue, hasMax := md.Metadata.Annotations[AutoscalerMaxAnnotation]
	if !hasMin && !hasMax {
		return workerBounds{}, false, nil
	}
	if !hasMin || !hasMax {
		return workerBounds{}, true, fmt.Errorf("pool %s: an autoscaled pool sets both %s and %s", md.Name, AutoscalerMinAnnotation, AutoscalerMaxAnnotation)
	}
	lo, err1 := strconv.ParseInt(minValue, 10, 32)
	hi, err2 := strconv.ParseInt(maxValue, 10, 32)
	if err1 != nil || err2 != nil {
		return workerBounds{}, true, fmt.Errorf("pool %s: the autoscaler's least and most workers are whole numbers", md.Name)
	}
	return workerBounds{least: int32(lo), most: int32(hi)}, true, nil
}

// workerRange is the least and the most workers a pool runs: its replicas, Cluster API's default of
// one when unset, or the autoscaler's bounds. A pool whose bounds do not read counts by its
// replicas; the validator refuses it.
func workerRange(md *clusterv1.MachineDeploymentTopology) workerBounds {
	if bounds, ok, err := poolScale(md); ok && err == nil {
		return bounds
	}
	if md.Replicas != nil {
		return workerBounds{least: *md.Replicas, most: *md.Replicas}
	}
	return workerBounds{least: 1, most: 1}
}

// autoscaled tells whether any of the cluster's pools is autoscaled.
func autoscaled(cluster *clusterv1.Cluster) bool {
	return slices.ContainsFunc(cluster.Spec.Topology.Workers.MachineDeployments, func(md clusterv1.MachineDeploymentTopology) bool {
		_, ok, _ := poolScale(&md)
		return ok
	})
}

// validateAutoscaling holds each autoscaled pool to bounds the autoscaler can work with, and to
// leaving its replicas unset: Cluster API would otherwise set them back on every change of the
// cluster, undoing what the autoscaler did.
func validateAutoscaling(c *clusterv1.Cluster) field.ErrorList {
	var errs field.ErrorList
	pools := field.NewPath("spec", "topology", "workers", "machineDeployments")
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		md := &c.Spec.Topology.Workers.MachineDeployments[i]
		path := pools.Index(i)
		bounds, ok, err := poolScale(md)
		lo, hi := bounds.least, bounds.most
		switch {
		case !ok:
			continue
		case err != nil:
			errs = append(errs, field.Invalid(path.Child("metadata", "annotations"), md.Metadata.Annotations, err.Error()))
			continue
		case lo < 0 || hi < 1 || lo > hi || hi > MaxAutoscaledWorkers:
			errs = append(errs, field.Invalid(path.Child("metadata", "annotations"), fmt.Sprintf("%d to %d", lo, hi),
				fmt.Sprintf("an autoscaled pool's least is 0 or more, and its most from 1 to %d and never below its least", MaxAutoscaledWorkers)))
		}
		if md.Replicas != nil {
			errs = append(errs, field.Forbidden(path.Child("replicas"), "an autoscaled pool leaves its number of workers to the cluster autoscaler"))
		}
	}
	return errs
}

// checkNodeAddresses refuses a cluster whose nodes' addresses are fewer than the machines it may run
// at once: its control planes, every pool at its most and one more for a rollout, which makes each
// new node before it removes an old one. Short of that, an upgrade or a node image refresh waits for
// ever for an address, and an autoscaled pool grows into a machine without one. A change to a
// cluster that was already short is refused only when it leaves it shorter.
func checkNodeAddresses(old, c *clusterv1.Cluster) error {
	now, ok := addressNeed(c)
	if !ok || now.short() <= 0 {
		return nil
	}
	if old != nil {
		if was, ok := addressNeed(old); ok && was.short() >= now.short() {
			return nil
		}
	}
	return fmt.Errorf("the nodes' addresses give %d, and the cluster may run %d machines at once: its control planes, every pool at its most and one more for a rollout", now.have, now.need)
}

// addresses are the most machines a cluster runs at once and the addresses its nodes' network gives
// them.
type addresses struct{ need, have int }

func (a addresses) short() int { return a.need - a.have }

// addressNeed is what the cluster needs of its nodes' addresses; false where the variables do not
// read, which Cluster API refuses itself.
func addressNeed(c *clusterv1.Cluster) (addresses, bool) {
	network, err := variable[nodeNetwork](c, "network")
	if err != nil {
		return addresses{}, false
	}
	have, err := addressCount(network.Addrs)
	if err != nil {
		return addresses{}, false
	}
	need := 1
	if r := c.Spec.Topology.ControlPlane.Replicas; r != nil {
		need += int(*r)
	} else {
		need++
	}
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		need += int(workerRange(&c.Spec.Topology.Workers.MachineDeployments[i]).most)
	}
	return addresses{need: need, have: have}, true
}

// addressCount counts the addresses entries give nodes as the network variable writes them:
// ranges, networks without their own and broadcast address, and single addresses.
func addressCount(entries []string) (int, error) {
	n := 0
	for _, e := range entries {
		e = strings.TrimSpace(e)
		s, err := parseSpan(e)
		if err != nil {
			return 0, err
		}
		size := int(addressNumber(s.last)-addressNumber(s.first)) + 1
		if p, err := netip.ParsePrefix(e); err == nil && p.Bits() < pointToPointBits {
			size -= 2
		}
		n += size
	}
	return n, nil
}

func addressNumber(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

// capacityAnnotations tell the autoscaler what a node of a pool holds, from its machine's size, and
// the labels Cluster API gives its Node from the machines' own. The autoscaler's reading of those
// labels from the MachineDeployment looks for them where a MachineDeployment has none, so a pool at
// zero would otherwise seem to offer no pod with a node selector a node of its own.
func capacityAnnotations(size machineSize, machineLabels map[string]string) map[string]string {
	a := map[string]string{
		capacityPrefix + "cpu":    strconv.Itoa(*size.Cores),
		capacityPrefix + "memory": fmt.Sprintf("%dMi", int(float64(*size.MemoryMiB)*capacityShare)),
	}
	if size.DiskGiB != nil && *size.DiskGiB > 0 {
		a[capacityPrefix+"ephemeral-disk"] = fmt.Sprintf("%dMi", int(float64(*size.DiskGiB*mebibytesPerGibibyte)*capacityShare))
	}
	var labels []string
	for _, k := range slices.Sorted(maps.Keys(machineLabels)) {
		if nodeLabel(k) {
			labels = append(labels, k+"="+machineLabels[k])
		}
	}
	if len(labels) > 0 {
		a[capacityPrefix+"labels"] = strings.Join(labels, ",")
	}
	return a
}

// nodeLabel tells whether Cluster API copies a Machine's label of this key to its Node: those of
// its own domain for nodes and of the node-role and node-restriction ones, subdomains included.
func nodeLabel(key string) bool {
	domain, _, found := strings.Cut(key, "/")
	if !found {
		return false
	}
	for _, d := range []string{"node.cluster.x-k8s.io", "node-role.kubernetes.io", "node-restriction.kubernetes.io"} {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}
