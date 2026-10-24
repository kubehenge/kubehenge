package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

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

// nodesFor are the names of the online nodes whose CPU runs the node OS.
func nodesFor(online []Node, o templates.NodeOS) []string {
	need := templates.CPULevel(o)
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
// to judge by, and the cluster waits for its nodes as any other does.
func (h *Hooks) checkNodeOS(ctx context.Context, c *clusterv1.Cluster) error {
	o, err := ClusterNodeOS(c)
	if err != nil {
		return err
	}
	if _, _, err = templates.Available(o); err != nil {
		return err //nolint:wrapcheck // says all a person needs
	}
	if h.Nodes == nil || templates.CPULevel(o) <= 1 {
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
	if len(online) == 0 || len(nodesFor(online, o)) > 0 {
		return nil
	}
	f, r, _ := templates.ReleaseOf(o)
	names := make([]string, len(online))
	for i, n := range online {
		names[i] = n.Name
	}
	return fmt.Errorf("%s %s needs a CPU of the %s level, and none of the Proxmox VE nodes the cluster may use (%s) has one: "+
		"choose another node OS, or nodes with a newer CPU", f.Title(), r.Name, templates.CPUTypeOf(r), strings.Join(names, ", "))
}
