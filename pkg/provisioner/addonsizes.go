package provisioner

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// AddonMemoryAnnotation on a ClusterClass is the memory, in MiB, each add-on's pods use in one of its
// clusters, by add-on: headlamp, monitoring, logs, kyverno, istio, clusterMesh, loadBalancer, certManager,
// rancher and rancherAgent.
// The end-to-end tests measure the figures with every add-on on.
const AddonMemoryAnnotation = "kubehenge.dev/addon-memory"

// NodeMemoryAnnotation on a ClusterClass is the memory, in MiB, a node of one of its clusters uses
// besides the add-ons' pods, by kind: controlPlane, with every add-on on, as their objects grow the
// API server; worker, idle. The end-to-end tests measure them too. A kind it leaves out counts as
// the least its machines may have.
const NodeMemoryAnnotation = "kubehenge.dev/node-memory"

// The keys in AddonMemoryAnnotation of what is not an add-on of its own.
const (
	certManagerFigure  = "certManager"
	rancherAgentFigure = "rancherAgent"
)

// addonNames are the add-ons as people know them, by their keys in AddonMemoryAnnotation.
var addonNames = map[string]string{
	headlampAddon: "Headlamp", monitoringAddon: "monitoring", logsAddon: "logs", kyvernoAddon: "Kyverno", istioAddon: "Istio",
	clusterMeshVariable: "Cluster Mesh", loadBalancerVariable: "the load balancer", certManagerFigure: "cert-manager", rancherAddon: "Rancher", rancherAgentFigure: "Rancher's agent",
}

// addonsOn names the add-ons the cluster runs, in a fixed order. Rancher's agent counts wherever the
// cluster would register: once a cluster installs Rancher, every other one registers with it.
func addonsOn(cluster *clusterv1.Cluster) ([]string, error) {
	a, err := addonsOf(cluster)
	if err != nil {
		return nil, err
	}
	var on []string
	for _, a := range []struct {
		name string
		on   bool
	}{
		{headlampAddon, a.Headlamp.Enabled},
		{monitoringAddon, a.Monitoring.Enabled},
		{logsAddon, a.logsOn()},
		{kyvernoAddon, a.PodSecurity.kyvernoOn()},
		{istioAddon, a.Istio.Enabled},
		{clusterMeshVariable, a.ClusterMesh.Enabled},
		{loadBalancerVariable, a.LoadBalancer.Enabled},
		{certManagerFigure, a.Monitoring.Enabled || a.Rancher.Install},
		{rancherAddon, a.Rancher.Install},
		{rancherAgentFigure, !a.Rancher.Install && a.Rancher.registers()},
	} {
		if a.on {
			on = append(on, a.name)
		}
	}
	return on, nil
}

// addonRoom is the memory a cluster's add-ons use and the room its nodes leave them, on its workers
// or, without workers, on its control planes, beyond Base, what each of those uses itself.
type addonRoom struct {
	Addons         []string
	Need, Have     int
	Base           int
	WithoutWorkers bool
}

func (r addonRoom) short() int { return r.Need - r.Have }

// checkAddonRoom refuses a cluster whose workers, beyond what each uses itself, have less memory than
// its add-ons use; without workers, its control planes. So a node needs its distribution's least or
// that, whichever is more. Updating a cluster that was already short is refused only when the
// update leaves it shorter.
func checkAddonRoom(old, cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	room, err := addonRoomOf(cluster, class)
	if err != nil || room.short() <= 0 {
		return err
	}
	if old != nil {
		was, err := addonRoomOf(old, class)
		if err == nil && room.short() <= was.short() {
			return nil
		}
	}
	names := make([]string, len(room.Addons))
	for i, a := range room.Addons {
		names[i] = addonNames[a]
	}
	addons := strings.Join(names, ", ")
	if room.WithoutWorkers {
		return fmt.Errorf("the cluster's add-ons (%s) need %d MiB of memory on its control planes, which run them without workers, beyond the %d MiB a control plane uses itself, and the control planes have %d MiB for them",
			addons, room.Need, room.Base, room.Have)
	}
	return fmt.Errorf("the cluster's add-ons (%s) need %d MiB of memory on its workers, beyond the %d MiB a worker uses itself, and the workers have %d MiB for them",
		addons, room.Need, room.Base, room.Have)
}

// classMemory reads one of a class's memory figures annotations.
func classMemory(class *clusterv1.ClusterClass, annotation string) (map[string]int, bool, error) {
	figures, ok := class.Annotations[annotation]
	if !ok {
		return nil, false, nil
	}
	memory := map[string]int{}
	if err := json.Unmarshal([]byte(figures), &memory); err != nil {
		return nil, false, fmt.Errorf("class %s, annotation %s: %w", class.Name, annotation, err)
	}
	return memory, true, nil
}

func addonRoomOf(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) (addonRoom, error) {
	var room addonRoom
	memory, ok, err := classMemory(class, AddonMemoryAnnotation)
	if err != nil || !ok {
		return room, err
	}
	nodes, _, err := classMemory(class, NodeMemoryAnnotation)
	if err != nil {
		return room, err
	}
	on, err := addonsOn(cluster)
	if err != nil {
		return room, err
	}
	for _, a := range on {
		room.Need += memory[a]
	}
	room.Addons = slices.DeleteFunc(on, func(a string) bool { return memory[a] == 0 })
	topology := cluster.Spec.Topology
	room.Base = nodes["worker"]
	if room.Base == 0 {
		room.Base = classMinimumSize(class, "workerMachine").MemoryMiB
	}
	workers := int32(0)
	for i := range topology.Workers.MachineDeployments {
		md := &topology.Workers.MachineDeployments[i]
		replicas := int32(1)
		if md.Replicas != nil {
			replicas = *md.Replicas
		}
		workers += replicas
		_, mem, sizeErr := nodeSize(cluster, class, "workerMachine", md.Variables.Overrides)
		if sizeErr != nil {
			return room, sizeErr
		}
		room.Have += int(replicas) * max(mem-room.Base, 0)
	}
	if workers > 0 {
		return room, nil
	}
	room.WithoutWorkers = true
	need, have, err := controlPlaneFit(cluster, class)
	if err != nil {
		return room, err
	}
	room.Base = nodes["controlPlane"]
	if room.Base == 0 {
		room.Base = need.MemoryMiB
	}
	replicas := int32(1)
	if topology.ControlPlane.Replicas != nil {
		replicas = *topology.ControlPlane.Replicas
	}
	room.Have = int(replicas) * max(have.MemoryMiB-room.Base, 0)
	return room, nil
}
