package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// Node is a Proxmox VE node that is online, with the x86-64 microarchitecture level its CPU
// reaches: a node OS built for a higher level stops at boot on it.
type Node struct {
	Name     string
	CPULevel int
}

// Nodes tells which Proxmox VE nodes are online; *Proxmox is the real one.
type Nodes interface {
	OnlineNodes(ctx context.Context) ([]Node, error)
	// TemplateTags are the tags of each Kubehenge template, the tags that record its hardware
	// included.
	TemplateTags(ctx context.Context) ([][]string, error)
}

// TemplateTags lists the tags of the Kubehenge templates the controller's token sees in the
// cluster's resources.
func (p *Proxmox) TemplateTags(ctx context.Context) ([][]string, error) {
	cluster, err := p.Client.Cluster(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the Proxmox VE cluster: %w", err)
	}
	resources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		return nil, fmt.Errorf("listing the cluster's VMs: %w", err)
	}
	var out [][]string
	for _, r := range resources {
		if r.Type != qemuType || r.Template != 1 {
			continue
		}
		if tags := templates.ParseAllTags(r.Tags); slices.Contains(tags, "kubehenge") {
			out = append(out, tags)
		}
	}
	return out, nil
}

// templateNeeds is what a cluster's templates ask of the Proxmox VE nodes and of its machines.
type templateNeeds struct {
	// Level is the x86-64 microarchitecture level the nodes must reach.
	Level int
	// CPUType names the CPU type behind Level when a template's own raised it above the node OS's.
	CPUType string
	// NUMA is set when a template gives its clones a NUMA topology.
	NUMA bool
}

// clusterNeeds are what the cluster's node OS or Talos needs, raised by the hardware of the
// templates its machines can be cloned from: those carrying its distribution's tag and, for a
// node OS, the node OS's.
func clusterNeeds(c *clusterv1.Cluster, templateTags [][]string) (templateNeeds, error) {
	level, err := clusterCPULevel(c)
	if err != nil {
		return templateNeeds{}, err
	}
	needs := templateNeeds{Level: level}
	line := []string{string(distributionOf(c))}
	if !isTalos(c) {
		o, err := ClusterNodeOS(c)
		if err != nil {
			return templateNeeds{}, err
		}
		line = append(line, o.Tag())
	}
	for _, tags := range templateTags {
		if !slices.ContainsFunc(line, func(t string) bool { return !slices.Contains(tags, t) }) {
			hw, recorded := templates.HardwareOf(tags)
			if !recorded {
				continue
			}
			if l := templates.CPUTypeLevel(hw.CPUType); l > needs.Level {
				needs.Level, needs.CPUType = l, hw.CPUType
			}
			needs.NUMA = needs.NUMA || hw.NUMA
		}
	}
	return needs, nil
}

// clusterTemplateNeeds are clusterNeeds with the templates Proxmox VE lists now. Where it cannot
// list them the node OS's own need stands, and the failure is logged: a cluster is never held up
// for a listing.
func (h *Hooks) clusterTemplateNeeds(ctx context.Context, c *clusterv1.Cluster) (templateNeeds, error) {
	var tags [][]string
	if h.Nodes != nil {
		var err error
		if tags, err = h.Nodes.TemplateTags(ctx); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "reading the templates' hardware", "cluster", client.ObjectKeyFromObject(c))
		}
	}
	return clusterNeeds(c, tags)
}

// OnlineNodes lists the nodes Proxmox VE reports online, sorted by name. A node's CPU is read once
// while it stays online: it changes only with the hardware, which takes the node down first.
func (p *Proxmox) OnlineNodes(ctx context.Context) ([]Node, error) {
	var nodes []pveNode
	if err := p.Client.Get(ctx, "/nodes", &nodes); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	var online []Node
	for _, n := range nodes {
		if n.Status != nodeOnline {
			continue
		}
		level, err := p.cpuLevel(ctx, n.Node)
		if err != nil {
			return nil, err
		}
		online = append(online, Node{Name: n.Node, CPULevel: level})
	}
	slices.SortFunc(online, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })
	p.mu.Lock()
	for name := range p.cpuLevels {
		if !slices.ContainsFunc(online, func(n Node) bool { return n.Name == name }) {
			delete(p.cpuLevels, name)
		}
	}
	p.mu.Unlock()
	return online, nil
}

// pveNodeStatus is the part of a node's status that names its CPU's features.
type pveNodeStatus struct {
	CPUInfo pveCPUInfo `json:"cpuinfo"`
}

type pveCPUInfo struct {
	Flags string `json:"flags"`
}

func (p *Proxmox) cpuLevel(ctx context.Context, node string) (int, error) {
	p.mu.Lock()
	level, ok := p.cpuLevels[node]
	p.mu.Unlock()
	if ok {
		return level, nil
	}
	var status pveNodeStatus
	if err := p.Client.Get(ctx, "/nodes/"+node+"/status", &status); err != nil {
		return 0, fmt.Errorf("reading node %s's CPU: %w", node, err)
	}
	level = templates.CPULevelOf(status.CPUInfo.Flags)
	p.mu.Lock()
	if p.cpuLevels == nil {
		p.cpuLevels = map[string]int{}
	}
	p.cpuLevels[node] = level
	p.mu.Unlock()
	return level, nil
}

// clusterCPULevel is the x86-64 microarchitecture level a cluster's nodes need of the Proxmox VE
// nodes: their node OS's, or Talos's, which is its own OS.
func clusterCPULevel(c *clusterv1.Cluster) (int, error) {
	if isTalos(c) {
		return templates.TalosCPULevel(), nil
	}
	o, err := ClusterNodeOS(c)
	if err != nil {
		return 0, err
	}
	return templates.CPULevel(o), nil
}

// nodesFor are the names of the online nodes whose CPU reaches the level.
func nodesFor(online []Node, need int) []string {
	var names []string
	for _, n := range online {
		if n.CPULevel >= need {
			names = append(names, n.Name)
		}
	}
	return names
}

// checkNodeOS refuses a node OS Kubehenge builds no templates of, and one whose CPU type none of the
// online Proxmox VE nodes the cluster may use offers. While none of those is online there is nothing
// to judge by, and the cluster waits for its nodes as any other does. A Talos cluster has no node
// OS: Talos is its own.
func (h *Hooks) checkNodeOS(ctx context.Context, c *clusterv1.Cluster) error {
	var o templates.NodeOS
	if !isTalos(c) {
		var err error
		if o, err = ClusterNodeOS(c); err != nil {
			return err
		}
		if _, _, err = templates.Available(o); err != nil {
			return err //nolint:wrapcheck // says all a person needs
		}
	}
	if h.Nodes == nil {
		return nil
	}
	needs, err := h.clusterTemplateNeeds(ctx, c)
	if err != nil {
		return err
	}
	// Talos's own level is no reason to refuse: only a template's CPU type above it is.
	if needs.Level <= 1 || (isTalos(c) && needs.CPUType == "") {
		return nil
	}
	chosen, _, err := optionalVariable[[]string](c, allowedNodesVariable)
	if err != nil {
		return err
	}
	online, err := h.Nodes.OnlineNodes(ctx)
	if err != nil {
		return fmt.Errorf("reading the Proxmox VE nodes: %w", err)
	}
	if len(chosen) > 0 {
		online = slices.DeleteFunc(online, func(n Node) bool { return !slices.Contains(chosen, n.Name) })
	}
	if len(online) == 0 || len(nodesFor(online, needs.Level)) > 0 {
		return nil
	}
	names := make([]string, len(online))
	for i, n := range online {
		names[i] = n.Name
	}
	if needs.CPUType != "" {
		return fmt.Errorf("the templates of %s use CPU type %s, which no allowed node (%s) offers. Build them with another CPU type",
			strings.Join(distributionTags(c, o), " on "), needs.CPUType, strings.Join(names, ", "))
	}
	f, r, _ := templates.ReleaseOf(o)
	return fmt.Errorf("%s %s needs CPU type %s, which no allowed node (%s) offers. Choose another node OS",
		f.Title(), r.Name, templates.CPUTypeOf(r), strings.Join(names, ", "))
}

// distributionTags names a cluster's template line for a person: its distribution, then its node OS
// where it has one.
func distributionTags(c *clusterv1.Cluster, o templates.NodeOS) []string {
	if isTalos(c) {
		return []string{"Talos"}
	}
	f, r, _ := templates.ReleaseOf(o)
	return []string{strings.ToUpper(string(distributionOf(c))), f.Title() + " " + r.Name}
}

// checkNUMA refuses a cluster whose templates give the guest a NUMA topology but whose machines
// have fewer cores than that is worth, on the control plane or in any machine deployment.
func (h *Hooks) checkNUMA(ctx context.Context, c *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	if h.Nodes == nil || class == nil {
		return nil
	}
	needs, err := h.clusterTemplateNeeds(ctx, c)
	if err != nil || !needs.NUMA {
		return err
	}
	topology := c.Spec.Topology
	if cores, _, err := nodeSize(c, class, "controlPlaneMachine", topology.ControlPlane.Variables.Overrides); err != nil {
		return err
	} else if cores < templates.NUMAMinCores {
		return fmt.Errorf("the templates use NUMA, which needs at least %d cores per machine, and the control plane has %d", templates.NUMAMinCores, cores)
	}
	for i := range topology.Workers.MachineDeployments {
		md := &topology.Workers.MachineDeployments[i]
		cores, _, err := nodeSize(c, class, "workerMachine", md.Variables.Overrides)
		if err != nil {
			return err
		}
		if cores < templates.NUMAMinCores {
			return fmt.Errorf("the templates use NUMA, which needs at least %d cores per machine, and worker pool %s has %d", templates.NUMAMinCores, md.Name, cores)
		}
	}
	return nil
}
