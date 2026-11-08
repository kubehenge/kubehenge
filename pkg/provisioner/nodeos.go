package provisioner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// Node is a Proxmox VE node that is online, with the x86-64 microarchitecture level its CPU
// reaches: a node OS built for a higher level stops at boot on it.
type Node struct {
	Name     string
	CPULevel int
	// FreeMiB is the memory Proxmox VE reports free on the node (its total less what is used), and
	// MemoryKnown tells whether it reported any. Without it the node is judged by its CPU alone.
	FreeMiB     int64
	MemoryKnown bool
	// Maintenance is set while the HA manager has the node in maintenance mode: it is being emptied.
	Maintenance bool
	// InactiveStorages are the storages that are defined for the node and not available on it.
	InactiveStorages []string
	// Version is the node's pve-manager version and Major its major number, 0 where it could not be
	// read.
	Version string
	Major   int
}

// Nodes tells which Proxmox VE nodes are online; *Proxmox is the real one.
type Nodes interface {
	OnlineNodes(ctx context.Context) ([]Node, error)
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

// needsOf are what the cluster's node OS or Talos needs, or, where Proxmox VE was looked at,
// what the newest builds of the cluster's template lines ask: their clones keep the template's CPU
// type, so it rules over the node OS's. The CPU type is named only where it asks more than the node
// OS or Talos does. Without a look, s is nil and the node OS's own need stands.
func needsOf(c *clusterv1.Cluster, s *Sites) (templateNeeds, error) {
	level, err := clusterCPULevel(c)
	if err != nil {
		return templateNeeds{}, err
	}
	needs := templateNeeds{Level: level}
	if s == nil {
		return needs, nil
	}
	if cpu, set := templateCPU(s, c); set {
		needs.Level = cpu.level
		if cpu.level > level {
			needs.CPUType = cpu.model
		}
	}
	needs.NUMA = templateNUMA(s, c)
	return needs, nil
}

// NodeFault is a node that Proxmox VE lists as online and that does not answer a call forwarded to
// it through the cluster, with why.
type NodeFault struct {
	Node string `json:"node"`
	// Reason is the status Proxmox VE answered with and its text, or that the node did not answer.
	Reason string `json:"reason"`
}

// faultList words faults as `pve2 (HTTP 596: certificate verify failed), pve3 (no answer in time)`.
func faultList(faults []NodeFault) string {
	parts := make([]string, len(faults))
	for i, f := range faults {
		parts[i] = f.Node + " (" + f.Reason + ")"
	}
	return strings.Join(parts, ", ")
}

// nodeProbeTimeout bounds the status call to one node, so that a node that hangs costs a read this
// long and no more.
const nodeProbeTimeout = 10 * time.Second

// nodeProbe is what a node's own status answered: its facts, or in fault why it did not answer.
type nodeProbe struct {
	status pveNodeStatus
	fault  string
	at     time.Time
}

func (p *Proxmox) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// probe asks a node for its own status through the node the controller is connected to, which is
// the call every VM operation on it takes too. Proxmox VE lists a node as online while its member of
// the cluster file system or its certificate is broken, and the call then fails with 596. The
// second result is why it failed, empty when the node answered. A cancelled ctx is not a fault of
// the node and is not kept.
func (p *Proxmox) probe(ctx context.Context, node string) (pveNodeStatus, string) {
	if p.ProbeTTL > 0 {
		p.mu.Lock()
		kept, ok := p.probes[node]
		p.mu.Unlock()
		if ok && p.now().Sub(kept.at) < p.ProbeTTL {
			return kept.status, kept.fault
		}
	}
	ask, cancel := context.WithTimeout(ctx, cmp.Or(p.ProbeTimeout, nodeProbeTimeout))
	defer cancel()
	var status pveNodeStatus
	err := p.Client.Get(ask, "/nodes/"+url.PathEscape(node)+"/status", &status)
	var fault string
	if err != nil {
		if ctx.Err() != nil {
			return status, pve.Reason(ctx.Err())
		}
		fault = pve.Reason(err)
	}
	if p.ProbeTTL > 0 {
		p.mu.Lock()
		if p.probes == nil {
			p.probes = map[string]nodeProbe{}
		}
		p.probes[node] = nodeProbe{status: status, fault: fault, at: p.now()}
		p.mu.Unlock()
	}
	return status, fault
}

// OnlineNodes lists the nodes Proxmox VE reports online that answer a call forwarded to them,
// sorted by name. Each read asks every online node for its status, all at once, which also gives
// its CPU and the memory it has free now. A node that does not answer is left out and named by
// Unreachable.
func (p *Proxmox) OnlineNodes(ctx context.Context) ([]Node, error) {
	var nodes []pveNode
	if err := p.Client.Get(ctx, "/nodes", &nodes); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	var storages []pveStorageUse
	if err := p.Client.Get(ctx, "/cluster/resources?type=storage", &storages); err != nil {
		return nil, fmt.Errorf("listing storages: %w", err)
	}
	maintenance, _ := haMaintenance(ctx, p.Client) // a refusal is ProxmoxReach's to report, once
	var listed []pveNode
	for _, n := range nodes {
		if n.Status == nodeOnline {
			listed = append(listed, n)
		}
	}
	probes := make([]nodeProbe, len(listed))
	var wg sync.WaitGroup
	for i, n := range listed {
		wg.Go(func() { probes[i].status, probes[i].fault = p.probe(ctx, n.Node) })
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading the nodes' status: %w", err)
	}
	var online []Node
	var faults []NodeFault
	for i, n := range listed {
		// A node that shows online but whose status does not answer (a broken certificate, a reboot,
		// a token without Sys.Audit on it) would otherwise be chosen for VMs that then cannot start,
		// or leave every cluster unplaced.
		if fault := probes[i].fault; fault != "" {
			ctrl.LoggerFrom(ctx).Info("leaving a node out of placement", "node", n.Node, "reason", fault)
			faults = append(faults, NodeFault{Node: n.Node, Reason: fault})
			continue
		}
		status := probes[i].status
		node := Node{Name: n.Node, CPULevel: templates.CPULevelOf(status.CPUInfo.Flags), Maintenance: maintenance[n.Node]}
		node.Version, node.Major = p.versions.read(ctx, p.Client, n.Node)
		node.FreeMiB, node.MemoryKnown = freeMiB(n, &status.Memory)
		for _, s := range storages {
			if s.Node == n.Node && s.Status != storageAvailable && !slices.Contains(node.InactiveStorages, s.Storage) {
				node.InactiveStorages = append(node.InactiveStorages, s.Storage)
			}
		}
		online = append(online, node)
	}
	slices.SortFunc(online, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(faults, func(a, b NodeFault) int { return strings.Compare(a.Node, b.Node) })
	names := make([]string, len(online))
	for i, n := range online {
		names[i] = n.Name
	}
	p.versions.keep(names)
	p.mu.Lock()
	p.faults = faults
	for name := range p.probes {
		if !slices.ContainsFunc(listed, func(n pveNode) bool { return n.Node == name }) {
			delete(p.probes, name)
		}
	}
	p.mu.Unlock()
	return online, nil
}

// Unreachable are the nodes the last OnlineNodes left out because Proxmox VE lists them online and
// their status does not answer, sorted by name.
func (p *Proxmox) Unreachable() []NodeFault {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.faults)
}

// pveNodeStatus is the part of a node's own status that names its CPU's features and its memory.
type pveNodeStatus struct {
	CPUInfo pveCPUInfo `json:"cpuinfo"`
	Memory  pveMemory  `json:"memory"`
}

type pveCPUInfo struct {
	Flags string `json:"flags"`
}

// pveMemory is a node's memory in bytes, as the node itself reads it.
type pveMemory struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
}

// freeMiB is the node's free memory, and whether it is known at all. The node's own figures rule
// over the list's: Proxmox VE lists a node with the last figures it sent for five minutes after it
// stops sending, so a node back online is listed with the memory it had before it left.
func freeMiB(n pveNode, live *pveMemory) (int64, bool) {
	total, used := n.MaxMem, n.Mem
	if live != nil && live.Total > 0 {
		total, used = live.Total, live.Used
	}
	if total <= 0 {
		return 0, false
	}
	return max(0, total-used) / bytesPerMiB, true
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

// placeable are the online nodes that can take a new VM now: not in maintenance, and with the
// cluster's storage active.
func placeable(online []Node, storage string) []Node {
	return slices.DeleteFunc(slices.Clone(online), func(n Node) bool {
		return n.Maintenance || storage != "" && slices.Contains(n.InactiveStorages, storage)
	})
}

// nodesToPlace are the nodes capmox may put a cluster's new VMs on: the chosen ones that are
// online, run the cluster's node OS, are not in maintenance and have the cluster's storage active.
// When nodes the cluster could use are online but every one is left out for those last two, there is
// nowhere to put a VM, and blocked says why.
func nodesToPlace(online []Node, level int, chosen []string, storage string) ([]string, string) {
	capable := slices.DeleteFunc(slices.Clone(online), func(n Node) bool {
		return n.CPULevel < level || len(chosen) > 0 && !slices.Contains(chosen, n.Name)
	})
	if len(capable) == 0 {
		return placement(chosen, nodesFor(online, level)), ""
	}
	var names, why []string
	for _, n := range capable {
		switch {
		case n.Maintenance:
			why = append(why, n.Name+" is in maintenance")
		case storage != "" && slices.Contains(n.InactiveStorages, storage):
			why = append(why, "storage "+storage+" is not active on "+n.Name)
		default:
			names = append(names, n.Name)
		}
	}
	if len(names) == 0 {
		return nil, "No Proxmox VE node can take a new VM: " + strings.Join(why, ", ")
	}
	return placement(chosen, names), ""
}

// fipsCNI is the only CNI RKE2 builds for FIPS mode.
const fipsCNI = "canal"

// checkFIPS refuses FIPS mode to a distribution without it, and to an RKE2 cluster on a CNI that
// RKE2 does not build for FIPS mode.
func checkFIPS(c *clusterv1.Cluster) error {
	dist := templates.Distribution(strings.TrimPrefix(c.Spec.Topology.ClassRef.Name, ClassPrefix))
	if err := templates.FIPSDistribution(dist); err != nil {
		return err //nolint:wrapcheck // says all a person needs
	}
	cni, set, err := optionalVariable[string](c, "cni")
	if err != nil {
		return err
	}
	if dist == templates.RKE2 && set && cni != fipsCNI {
		return fmt.Errorf("FIPS mode needs Canal, RKE2's only FIPS pod network, not %s", cni)
	}
	return nil
}

// checkNodeOS refuses a node OS Kubehenge builds no templates of, and one whose CPU type none of the
// online Proxmox VE nodes the cluster may use offers. While none of those is online there is nothing
// to judge by, and the cluster waits for its nodes as any other does. A Talos cluster has no node
// OS: Talos is its own.
func (h *Hooks) checkNodeOS(ctx context.Context, reads *pveReads, c *clusterv1.Cluster) error {
	o, err := buildableNodeOS(c)
	if err != nil {
		return err
	}
	if h.Nodes == nil {
		return nil
	}
	online, sites, err := reads.allowedOnline(ctx, c)
	if err != nil {
		return err
	}
	needs, err := needsOf(c, sites)
	if err != nil {
		return err
	}
	// Talos's own level is no reason to refuse: only a template's CPU type above it is.
	if needs.Level <= 1 || (isTalos(c) && needs.CPUType == "") || len(online) == 0 || len(nodesFor(online, needs.Level)) > 0 {
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

// buildableNodeOS is the cluster's node OS once Kubehenge builds templates of it and its FIPS
// choices hold. A Talos cluster has none.
func buildableNodeOS(c *clusterv1.Cluster) (templates.NodeOS, error) {
	if isTalos(c) {
		return templates.NodeOS{}, nil
	}
	o, err := ClusterNodeOS(c)
	if err != nil {
		return o, err
	}
	if _, _, err = templates.Available(o); err != nil {
		return o, err //nolint:wrapcheck // says all a person needs
	}
	if o.FIPS {
		return o, checkFIPS(c)
	}
	return o, nil
}

// pveReads is what one admission or hook call reads from Proxmox VE. Each read happens at most once
// per call, its failure included, so every check that needs it shares one look and one time budget.
// A check that needs another read of Proxmox VE adds a memoised method here.
type pveReads struct {
	h      *Hooks
	online memo[[]Node]
	sites  memo[*Sites]
}

// lookAtSites is where the storages and templates are. Callers check that the hooks have Sites.
func (r *pveReads) lookAtSites(ctx context.Context) (*Sites, error) {
	return r.sites.get(func() (*Sites, error) {
		s, err := r.h.Sites.Sites(ctx)
		if err != nil {
			return nil, &pveReadError{fmt.Errorf("reading the Proxmox VE storages and templates: %w", err)}
		}
		return s, nil
	})
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
	sites, err := r.lookAtSites(ctx)
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
func (h *Hooks) checkNUMA(ctx context.Context, reads *pveReads, c *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	if h.Sites == nil || class == nil {
		return nil
	}
	sites, err := reads.lookAtSites(ctx)
	if err != nil {
		return err
	}
	needs, err := needsOf(c, sites)
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
