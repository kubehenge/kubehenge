package provisioner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"

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
	var skipped []string
	for _, n := range nodes {
		if n.Status != nodeOnline {
			continue
		}
		// A node that shows online but whose status cannot be read (a reboot, a token without
		// Sys.Audit on it) would otherwise leave every cluster unplaced.
		level, err := p.cpuLevel(ctx, n.Node)
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "leaving a node out of placement", "node", n.Node)
			skipped = append(skipped, n.Node)
			continue
		}
		online = append(online, Node{Name: n.Node, CPULevel: level})
	}
	slices.SortFunc(online, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(skipped)
	p.mu.Lock()
	p.skipped = skipped
	for name := range p.cpuLevels {
		if !slices.ContainsFunc(online, func(n Node) bool { return n.Name == name }) {
			delete(p.cpuLevels, name)
		}
	}
	p.mu.Unlock()
	return online, nil
}

// Unreadable are the nodes the last OnlineNodes left out because their status could not be read.
func (p *Proxmox) Unreadable() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.skipped)
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
func (h *Hooks) checkNodeOS(ctx context.Context, reads *pveReads, c *clusterv1.Cluster) error {
	if isTalos(c) {
		return nil
	}
	o, err := ClusterNodeOS(c)
	if err != nil {
		return err
	}
	if _, _, err = templates.Available(o); err != nil {
		return err //nolint:wrapcheck // says all a person needs
	}
	if h.Nodes == nil {
		return nil
	}
	online, sites, err := reads.allowedOnline(ctx, c)
	if err != nil {
		return err
	}
	need := templates.CPULevel(o)
	if sites != nil {
		need = cpuNeed(sites, c, need)
	}
	if need <= 1 || len(online) == 0 || len(nodesFor(online, need)) > 0 {
		return nil
	}
	names := make([]string, len(online))
	for i, n := range online {
		names[i] = n.Name
	}
	f, r, _ := templates.ReleaseOf(o)
	return fmt.Errorf("%s %s needs CPU type %s, which no allowed node (%s) offers. Choose another node OS",
		f.Title(), r.Name, templates.CPUTypeOf(r), strings.Join(names, ", "))
}

// pveReads is what one admission or hook call reads from Proxmox VE. Each read happens at most once
// per call, its failure included, so every check that needs it shares one look and one time budget.
// A check that needs another read of Proxmox VE adds a memoised method here.
type pveReads struct {
	h      *Hooks
	online memo[[]Node]
	sites  memo[*Sites]
}

// memo is one read, made on first use.
type memo[T any] struct {
	done  bool
	value T
	err   error
}

func (m *memo[T]) get(read func() (T, error)) (T, error) {
	if !m.done {
		m.value, m.err = read()
		m.done = true
	}
	return m.value, m.err
}

// reads starts a call's reads of Proxmox VE.
func (h *Hooks) reads() *pveReads { return &pveReads{h: h} }

// allowedOnline are the online nodes the cluster may use, with where the storages and templates
// are when the hooks can look at them.
func (r *pveReads) allowedOnline(ctx context.Context, c *clusterv1.Cluster) ([]Node, *Sites, error) {
	chosen, _, err := optionalVariable[[]string](c, allowedNodesVariable)
	if err != nil {
		return nil, nil, err
	}
	all, err := r.online.get(func() ([]Node, error) {
		nodes, readErr := r.h.Nodes.OnlineNodes(ctx)
		if readErr != nil {
			return nil, &pveReadError{fmt.Errorf("reading the Proxmox VE nodes: %w", readErr)}
		}
		return nodes, nil
	})
	if err != nil {
		return nil, nil, err
	}
	online := slices.Clone(all)
	if len(chosen) > 0 {
		online = slices.DeleteFunc(online, func(n Node) bool { return !slices.Contains(chosen, n.Name) })
	}
	if r.h.Sites == nil {
		return online, nil, nil
	}
	sites, err := r.sites.get(func() (*Sites, error) {
		s, readErr := r.h.Sites.Sites(ctx)
		if readErr != nil {
			return nil, &pveReadError{fmt.Errorf("reading the Proxmox VE storages and templates: %w", readErr)}
		}
		return s, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return online, sites, nil
}

// pveReadError is a failed or late read of Proxmox VE, as against a cluster that is wrong.
// Admission lets the cluster in then, and the creation hook, which retries, judges it.
type pveReadError struct{ err error }

func (e *pveReadError) Error() string { return e.err.Error() }
func (e *pveReadError) Unwrap() error { return e.err }

// defaultAdmissionBudget is how long admission reads Proxmox VE. The webhook itself times out at 5 s.
const defaultAdmissionBudget = 2 * time.Second

// admissionContext bounds what admission reads from Proxmox VE well inside the webhook's timeout.
func (h *Hooks) admissionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, cmp.Or(h.AdmissionBudget, defaultAdmissionBudget))
}

// admitOnReadFailure drops a failed read of Proxmox VE, logging it, and keeps every other error.
func admitOnReadFailure(ctx context.Context, err error) error {
	if _, ok := errors.AsType[*pveReadError](err); ok {
		ctrl.LoggerFrom(ctx).Error(err, "admitting a cluster without the Proxmox VE checks; the creation hook judges it")
		return nil
	}
	return err
}

// checkPlacement refuses a cluster whose storage is available on none of the online nodes it may
// use, or whose templates cannot be cloned on any of them. While none is online there is nothing
// to judge by.
func (h *Hooks) checkPlacement(ctx context.Context, r *pveReads, c *clusterv1.Cluster) error {
	if h.Nodes == nil || h.Sites == nil {
		return nil
	}
	online, sites, err := r.allowedOnline(ctx, c)
	if err != nil {
		return err
	}
	return sites.Refusal(c, online)
}
