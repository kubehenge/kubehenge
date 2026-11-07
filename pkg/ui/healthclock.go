package ui

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// clockProbeAsks is how many Talos nodes are asked about their clock at once.
const clockProbeAsks = 8

// readTalosClocks asks each node of a Talos cluster whether its clock is synced, as the cluster's
// talosconfig. Talos shows that on its own API alone: it sets no condition on the Node. A node that
// does not answer says nothing, so a cluster whose Talos API is out of reach shows no clock rows.
func (s *Server) readTalosClocks(ctx context.Context, kube client.Client, c *clusterv1.Cluster, wl *workloadRead) {
	if !provisioner.IsTalosCluster(c) || len(wl.nodes) == 0 {
		return
	}
	raw, err := provisioner.TalosconfigOf(ctx, kube, c.Namespace, c.Name)
	if err != nil {
		return
	}
	probe := s.TalosClock
	if probe == nil {
		probe = provisioner.ProbeTalosClock
	}
	var mu sync.Mutex
	wl.unsynced = map[string]bool{}
	sem := make(chan struct{}, clockProbeAsks)
	var wg sync.WaitGroup
	for i := range wl.nodes {
		address := internalAddress(&wl.nodes[i])
		name := wl.nodes[i].Name
		if address == "" {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, healthDeadline)
			defer cancel()
			if synced, err := probe(pctx, raw, address); err == nil && !synced {
				mu.Lock()
				wl.unsynced[name] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
}

func internalAddress(n *corev1.Node) string {
	i := slices.IndexFunc(n.Status.Addresses, func(a corev1.NodeAddress) bool { return a.Type == corev1.NodeInternalIP })
	if i < 0 {
		return ""
	}
	return n.Status.Addresses[i].Address
}

// clockSyncAttention warns about a Talos node whose time sync has not succeeded. Talos waits for it
// before it signs the cluster's certificates, so the node boots on after five minutes without.
func clockSyncAttention(a *attention, v *nodeView, unsynced map[string]bool) {
	if !unsynced[v.Name] {
		return
	}
	a.addMachine(v.Machine, severityWarning, roleWord(v.Role)+" "+v.Name+": clock not synced",
		"The node reaches no time server, so its clock may be wrong and certificates with it. Check that UDP 123 is open from the nodes, or set the cluster's time servers.", time.Time{})
}

// nodeClockWarn is how far a Proxmox VE node's clock may be from the controller's.
const nodeClockWarn = clockSkewLimit

// addNodeClocks adds a warning for each Proxmox VE node that runs one of the cluster's VMs and whose
// clock is over nodeClockWarn from this machine's. Proxmox VE shows a node's time to users with
// Sys.Audit on the node, so a user without it sees nothing, and a node that does not answer is left
// out. The nodes are asked with the user's own ticket.
func (s *Server) addNodeClocks(ctx context.Context, u user, v *healthView) {
	nodes := map[string]bool{}
	for i := range v.Nodes.Items {
		if vm := v.Nodes.Items[i].VM; vm != nil && vm.Node != "" {
			nodes[vm.Node] = true
		}
	}
	names := make([]string, 0, len(nodes))
	for n := range nodes {
		names = append(names, n)
	}
	slices.Sort(names)
	found, asked := s.nodeSkews(ctx, u, names)
	var a attention
	for i, name := range names {
		if asked[i] && found[i].Abs() > nodeClockWarn {
			direction := "ahead of"
			if found[i] < 0 {
				direction = "behind"
			}
			a.add(severityWarning, sectionNodes, "Proxmox VE node "+name+" clock is off by "+found[i].Abs().Round(time.Second).String(),
				"The node's clock is "+direction+" this machine's. VMs on it start with that clock, and Proxmox VE sign-ins through several nodes fail when two disagree by over "+strconv.Itoa(proxmoxTicketGraceMinutes)+" minutes. Check the node's time service.", time.Time{})
		}
	}
	if len(a) == 0 {
		return
	}
	v.Attention = append(v.Attention, a...)
	slices.SortStableFunc(v.Attention, func(x, y attentionView) int { return severityRank(x.Severity) - severityRank(y.Severity) })
	if v.Verdict == verdictHealthy || v.Verdict == verdictChanging {
		v.Verdict = verdictDegraded
	}
}

// nodeSkews asks each node for its time and returns how far it is ahead of this machine. A node that
// did not answer has false in asked.
func (s *Server) nodeSkews(ctx context.Context, u user, names []string) ([]time.Duration, []bool) {
	found := make([]time.Duration, len(names))
	asked := make([]bool, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			rctx, cancel := context.WithTimeout(ctx, healthDeadline)
			defer cancel()
			asked0 := s.now()
			theirs, err := s.Accounts.NodeTime(rctx, u.ticket, name)
			if err != nil {
				if !errors.Is(err, pve.ErrForbidden) {
					s.Log.DebugContext(ctx, "reading a Proxmox VE node's time", "node", name, "error", err)
				}
				return
			}
			found[i], asked[i] = theirs.Sub(asked0), true
		})
	}
	wg.Wait()
	return found, asked
}

// proxmoxTicketGraceMinutes is how far a ticket's age may be negative on a node whose clock is behind
// the node that signed it (auth_graceperiod in pve-access-control).
const proxmoxTicketGraceMinutes = 5
