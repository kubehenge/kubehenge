package provisioner

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// AddonMemoryAnnotation on a ClusterClass is the memory, in MiB, each add-on's pods use in one of its
// clusters, by add-on: headlamp, monitoring, logs, backups, kyverno, istio, clusterMesh,
// loadBalancer, certManager, rancher, rancherAgent, argocd and flux.
// The end-to-end tests measure the figures with every add-on on.
const AddonMemoryAnnotation = "kubehenge.dev/addon-memory"

// AddonControlPlaneMemoryAnnotation on a ClusterClass is the memory, in MiB, each add-on adds to
// what each control plane of one of its clusters uses itself, by the keys of AddonMemoryAnnotation:
// its objects in the API server and its pods on the control planes. The end-to-end tests measure the
// figures as each add-on is switched on.
const AddonControlPlaneMemoryAnnotation = "kubehenge.dev/addon-control-plane-memory"

// NodeMemoryAnnotation on a ClusterClass is the memory, in MiB, a node of one of its clusters uses
// besides the add-ons, by kind: controlPlane and worker, each before any add-on. The end-to-end
// tests measure them too. A kind it leaves out counts as the least its machines may have.
const NodeMemoryAnnotation = "kubehenge.dev/node-memory"

// AddonCPUAnnotation on a ClusterClass is the CPU, in millicores, each add-on's pods request in one
// of its clusters, by add-on, with the keys of AddonMemoryAnnotation: what its charts ask for as
// Kubehenge configures them, with one node's share of the pods that run on every node. A pod the
// scheduler finds no node with that much unrequested CPU for stays pending, however idle the nodes
// are.
const AddonCPUAnnotation = "kubehenge.dev/addon-cpu"

// NodeCPUAnnotation on a ClusterClass is the CPU, in millicores, a node of one of its clusters
// requests besides the add-ons' pods, by kind: controlPlane (without workers) or worker. It is what the
// kubelet keeps back and what the pods of the distribution, its CNI and Kubehenge's own ask for. A
// kind it leaves out counts as the least its machines may have.
const NodeCPUAnnotation = "kubehenge.dev/node-cpu"

// The keys in AddonMemoryAnnotation of what is not an add-on of its own.
const (
	certManagerFigure  = "certManager"
	rancherAgentFigure = "rancherAgent"
)

// addonNames are the add-ons as people know them, by their keys in AddonMemoryAnnotation.
var addonNames = map[string]string{
	headlampAddon: "Headlamp", monitoringAddon: "monitoring", logsAddon: "logs", backupsVariable: "backups", kyvernoAddon: "Kyverno", istioAddon: "Istio",
	clusterMeshVariable: "Cluster Mesh", loadBalancerVariable: "the load balancer", certManagerFigure: "cert-manager", rancherAddon: "Rancher", rancherAgentFigure: "Rancher's agent",
	argoCD: "Argo CD", flux: "Flux",
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
		{backupsVariable, a.Backups.Enabled},
		{kyvernoAddon, a.PodSecurity.kyvernoOn()},
		{istioAddon, a.Istio.Enabled},
		{clusterMeshVariable, a.ClusterMesh.Enabled},
		{loadBalancerVariable, a.LoadBalancer.Enabled},
		{certManagerFigure, a.certManager()},
		{rancherAddon, a.Rancher.Install},
		{rancherAgentFigure, !a.Rancher.Install && a.Rancher.registers()},
		{argoCD, a.GitOps.on(argoCD)},
		{flux, a.GitOps.on(flux)},
	} {
		if a.on {
			on = append(on, a.name)
		}
	}
	return on, nil
}

// addonResource is what the add-ons take from the nodes that carry them, memory or CPU, by the
// class's figures for the add-ons and the nodes.
type addonResource struct {
	addons, nodes string
	// growth names the figures of what each add-on adds to each control plane's own use, empty but
	// for memory: the add-ons' objects grow the API server, not what it requests.
	growth     string
	unit, name string
	// of is how much of it a node of this size has.
	of func(controlPlaneSize) int
}

var (
	addonMemory = &addonResource{
		addons: AddonMemoryAnnotation, nodes: NodeMemoryAnnotation, growth: AddonControlPlaneMemoryAnnotation,
		unit: "MiB", name: "memory", of: func(s controlPlaneSize) int { return s.MemoryMiB },
	}
	addonCPU = &addonResource{
		addons: AddonCPUAnnotation, nodes: NodeCPUAnnotation,
		unit: "millicores", name: "CPU", of: func(s controlPlaneSize) int { return s.Cores * millicores },
	}
)

const millicores = 1000

// addonRoom is how much of a resource a cluster's add-ons' pods take and the room its nodes leave
// them, on its workers or, without workers, on its control planes, beyond Base, what each of those
// takes itself with the add-ons. With workers and add-ons, ControlPlaneNeed is the memory each
// control plane uses itself with them, and ControlPlaneHave the memory each has.
type addonRoom struct {
	Addons         []string
	Need, Have     int
	Base           int
	WithoutWorkers bool
	// Autoscaled is a cluster whose workers all come and go with the autoscaler, down to none.
	Autoscaled                         bool
	ControlPlaneNeed, ControlPlaneHave int
}

func (r addonRoom) short() int { return r.Need - r.Have }

func (r addonRoom) controlPlaneShort() int { return r.ControlPlaneNeed - r.ControlPlaneHave }

// checkAddonRoom refuses a cluster whose workers, beyond what each takes itself, have less memory
// than its add-ons' pods use, or less CPU than they request. Without workers, the same holds for its
// control planes, beyond what each takes itself with the add-ons. So a node needs its distribution's least or that,
// whichever is more. With workers, each control plane of a cluster with add-ons needs the memory it
// uses itself with them. Updating a cluster that was already short is refused only when the update
// leaves it shorter.
func checkAddonRoom(old, cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	for _, r := range []*addonResource{addonMemory, addonCPU} {
		if err := checkRoomFor(r, old, cluster, class); err != nil {
			return err
		}
	}
	return nil
}

func checkRoomFor(r *addonResource, old, cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	room, err := addonRoomOf(r, cluster, class)
	if err != nil {
		return err
	}
	excused := func(short func(addonRoom) int) bool {
		if old == nil {
			return false
		}
		was, err := addonRoomOf(r, old, class)
		return err == nil && short(room) <= short(was)
	}
	names := make([]string, len(room.Addons))
	for i, a := range room.Addons {
		names[i] = addonNames[a]
	}
	addons := strings.Join(names, ", ")
	if room.short() > 0 && !excused(addonRoom.short) {
		if room.WithoutWorkers {
			return fmt.Errorf("add-ons (%s) need %d %s of %s on control planes without workers, which have %d %s",
				addons, room.Need, r.unit, r.name, room.Have, r.unit)
		}
		if room.Autoscaled {
			return fmt.Errorf("add-ons (%s) need a worker. Set an autoscaled pool's minimum to 1 or more", addons)
		}
		return fmt.Errorf("add-ons (%s) need %d %s of %s on workers, which have %d %s for them",
			addons, room.Need, r.unit, r.name, room.Have, r.unit)
	}
	if room.controlPlaneShort() > 0 && !excused(addonRoom.controlPlaneShort) {
		return fmt.Errorf("each control plane needs %d %s of %s with add-ons (%s), and has %d %s",
			room.ControlPlaneNeed, r.unit, r.name, addons, room.ControlPlaneHave, r.unit)
	}
	return nil
}

// classFigures reads one of a class's annotations of figures for the add-ons or the nodes.
func classFigures(class *clusterv1.ClusterClass, annotation string) (map[string]int, bool, error) {
	raw, ok := class.Annotations[annotation]
	if !ok {
		return nil, false, nil
	}
	figures := map[string]int{}
	if err := json.Unmarshal([]byte(raw), &figures); err != nil {
		return nil, false, fmt.Errorf("class %s, annotation %s: %w", class.Name, annotation, err)
	}
	return figures, true, nil
}

// resourceFigures are a class's figures for one resource: its add-ons' pods', what they add to each
// control plane's own use, and its nodes' own.
type resourceFigures struct {
	pods, growth, nodes map[string]int
}

// addonFigures reads a class's figures for the resource. A class without figures for the add-ons'
// pods has none.
func addonFigures(r *addonResource, class *clusterv1.ClusterClass) (resourceFigures, bool, error) {
	var f resourceFigures
	var ok bool
	var err error
	if f.pods, ok, err = classFigures(class, r.addons); err != nil || !ok {
		return f, false, err
	}
	if f.nodes, _, err = classFigures(class, r.nodes); err != nil {
		return f, false, err
	}
	if f.growth, _, err = classFigures(class, r.growth); err != nil {
		return f, false, err
	}
	return f, true, nil
}

func addonRoomOf(r *addonResource, cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) (addonRoom, error) {
	var room addonRoom
	f, ok, err := addonFigures(r, class)
	if err != nil || !ok {
		return room, err
	}
	figures, growth, nodes := f.pods, f.growth, f.nodes
	on, err := addonsOn(cluster)
	if err != nil {
		return room, err
	}
	topology := cluster.Spec.Topology
	room.Base = nodes["worker"]
	if room.Base == 0 {
		room.Base = r.of(classMinimumSize(class, "workerMachine"))
	}
	// An autoscaled pool counts with its least workers, the room the add-ons always have. The control
	// planes stay tainted while any pool is autoscaled (TaintReconciler), so the add-ons never move
	// there.
	workers, most := int32(0), int32(0)
	for i := range topology.Workers.MachineDeployments {
		md := &topology.Workers.MachineDeployments[i]
		pool := workerRange(md)
		workers += pool.least
		most += pool.most
		cores, mem, sizeErr := nodeSize(cluster, class, "workerMachine", md.Variables.Overrides)
		if sizeErr != nil {
			return room, sizeErr
		}
		room.Have += int(pool.least) * max(r.of(controlPlaneSize{Cores: cores, MemoryMiB: mem})-room.Base, 0)
	}
	if workers == 0 && most > 0 {
		// Rancher's agent alone holds no worker up: it runs only once there is a Rancher to register
		// with, and its pod waiting for room brings a worker up from none.
		on = slices.DeleteFunc(on, func(a string) bool { return a == rancherAgentFigure })
	}
	grown := 0
	for _, a := range on {
		room.Need += figures[a]
		grown += growth[a]
	}
	room.Addons = slices.DeleteFunc(on, func(a string) bool { return figures[a] == 0 && growth[a] == 0 })
	if workers > 0 || most > 0 {
		room.Autoscaled = workers == 0
		if len(room.Addons) == 0 || r.growth == "" || nodes["controlPlane"] == 0 {
			return room, nil
		}
		room.ControlPlaneNeed = nodes["controlPlane"] + grown
		cores, mem, sizeErr := nodeSize(cluster, class, "controlPlaneMachine", topology.ControlPlane.Variables.Overrides)
		room.ControlPlaneHave = r.of(controlPlaneSize{Cores: cores, MemoryMiB: mem})
		return room, sizeErr
	}
	room.WithoutWorkers = true
	need, have, err := controlPlaneFit(cluster, class)
	if err != nil {
		return room, err
	}
	room.Base = nodes["controlPlane"]
	if room.Base == 0 {
		room.Base = r.of(need)
	}
	room.Base += grown
	replicas := int32(1)
	if topology.ControlPlane.Replicas != nil {
		replicas = *topology.ControlPlane.Replicas
	}
	room.Have = int(replicas) * max(r.of(have)-room.Base, 0)
	return room, nil
}
