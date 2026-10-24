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

// addonRoom is the memory a cluster's add-ons' pods use and the room its nodes leave them, on its
// workers or, without workers, on its control planes, beyond Base, what each of those uses itself
// with the add-ons. With workers and add-ons, ControlPlaneNeed is what each control plane uses
// itself with them, and ControlPlaneHave the memory each has.
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

// checkAddonRoom refuses a cluster whose workers, beyond what each uses itself, have less memory than
// its add-ons' pods use; without workers, its control planes, beyond what each uses itself with the
// add-ons. So a node needs its distribution's least or that, whichever is more. With workers, each
// control plane of a cluster with add-ons needs what it uses itself with them. Updating a cluster
// that was already short is refused only when the update leaves it shorter.
func checkAddonRoom(old, cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	room, err := addonRoomOf(cluster, class)
	if err != nil {
		return err
	}
	excused := func(short func(addonRoom) int) bool {
		if old == nil {
			return false
		}
		was, err := addonRoomOf(old, class)
		return err == nil && short(room) <= short(was)
	}
	names := make([]string, len(room.Addons))
	for i, a := range room.Addons {
		names[i] = addonNames[a]
	}
	addons := strings.Join(names, ", ")
	if room.short() > 0 && !excused(addonRoom.short) {
		if room.WithoutWorkers {
			return fmt.Errorf("the cluster's add-ons (%s) need %d MiB of memory on its control planes, which run them without workers, beyond the %d MiB a control plane uses itself with them, and the control planes have %d MiB for them",
				addons, room.Need, room.Base, room.Have)
		}
		if room.Autoscaled {
			return fmt.Errorf("the cluster's add-ons (%s) need %d MiB of memory on its workers, beyond the %d MiB a worker uses itself, and its autoscaled pools may go down to no workers; at least one of them has to keep one",
				addons, room.Need, room.Base)
		}
		return fmt.Errorf("the cluster's add-ons (%s) need %d MiB of memory on its workers, beyond the %d MiB a worker uses itself, and the workers have %d MiB for them",
			addons, room.Need, room.Base, room.Have)
	}
	if room.controlPlaneShort() > 0 && !excused(addonRoom.controlPlaneShort) {
		return fmt.Errorf("with the cluster's add-ons (%s), whose objects grow the API server, each control plane needs %d MiB of memory, and has %d MiB",
			addons, room.ControlPlaneNeed, room.ControlPlaneHave)
	}
	return nil
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

// memoryFigures are a class's memory figures: its add-ons' pods', what they add to each control
// plane's own use, and its nodes' own.
type memoryFigures struct {
	pods, growth, nodes map[string]int
}

// addonFigures reads a class's memory figures. A class without figures for the add-ons' pods has
// none.
func addonFigures(class *clusterv1.ClusterClass) (memoryFigures, bool, error) {
	var f memoryFigures
	var ok bool
	var err error
	if f.pods, ok, err = classMemory(class, AddonMemoryAnnotation); err != nil || !ok {
		return f, false, err
	}
	if f.nodes, _, err = classMemory(class, NodeMemoryAnnotation); err != nil {
		return f, false, err
	}
	if f.growth, _, err = classMemory(class, AddonControlPlaneMemoryAnnotation); err != nil {
		return f, false, err
	}
	return f, true, nil
}

func addonRoomOf(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) (addonRoom, error) {
	var room addonRoom
	f, ok, err := addonFigures(class)
	if err != nil || !ok {
		return room, err
	}
	memory, growth, nodes := f.pods, f.growth, f.nodes
	on, err := addonsOn(cluster)
	if err != nil {
		return room, err
	}
	topology := cluster.Spec.Topology
	room.Base = nodes["worker"]
	if room.Base == 0 {
		room.Base = classMinimumSize(class, "workerMachine").MemoryMiB
	}
	// An autoscaled pool counts with its least workers, the room the add-ons always have; the control
	// planes stay tainted while any pool is autoscaled (TaintReconciler), so the add-ons never move
	// there.
	workers, most := int32(0), int32(0)
	for i := range topology.Workers.MachineDeployments {
		md := &topology.Workers.MachineDeployments[i]
		r := workerRange(md)
		workers += r.least
		most += r.most
		_, mem, sizeErr := nodeSize(cluster, class, "workerMachine", md.Variables.Overrides)
		if sizeErr != nil {
			return room, sizeErr
		}
		room.Have += int(r.least) * max(mem-room.Base, 0)
	}
	if workers == 0 && most > 0 {
		// Rancher's agent alone holds no worker up: it runs only once there is a Rancher to register
		// with, and its pod waiting for room brings a worker up from none.
		on = slices.DeleteFunc(on, func(a string) bool { return a == rancherAgentFigure })
	}
	grown := 0
	for _, a := range on {
		room.Need += memory[a]
		grown += growth[a]
	}
	room.Addons = slices.DeleteFunc(on, func(a string) bool { return memory[a] == 0 && growth[a] == 0 })
	if workers > 0 || most > 0 {
		room.Autoscaled = workers == 0
		if len(room.Addons) == 0 || nodes["controlPlane"] == 0 {
			return room, nil
		}
		room.ControlPlaneNeed = nodes["controlPlane"] + grown
		_, room.ControlPlaneHave, err = nodeSize(cluster, class, "controlPlaneMachine", topology.ControlPlane.Variables.Overrides)
		return room, err
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
	room.Base += grown
	replicas := int32(1)
	if topology.ControlPlane.Replicas != nil {
		replicas = *topology.ControlPlane.Replicas
	}
	room.Have = int(replicas) * max(have.MemoryMiB-room.Base, 0)
	return room, nil
}
