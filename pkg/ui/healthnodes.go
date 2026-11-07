package ui

import (
	"slices"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// clockSkewLimit is how far a node's clock may be from the controller's before the page says so.
// Certificates, tokens and OpenID sign-ins all trust the clock.
const clockSkewLimit = 30 * time.Second

// leaseRenewals are the times the nodes' kubelets last renewed their leases, by node name.
func leaseRenewals(leases []coordinationv1.Lease) map[string]time.Time {
	out := make(map[string]time.Time, len(leases))
	for i := range leases {
		if t := leases[i].Spec.RenewTime; t != nil {
			out[leases[i].Name] = t.Time
		}
	}
	return out
}

// clockSkewAttention warns about a ready node whose lease was renewed, by its own clock, more than
// clockSkewLimit from now. A ready node renews it every ten seconds, so a gap is the clock's and
// not the node's. The Ready condition's heartbeat cannot say: the kubelet writes it only every few
// minutes while nothing changes.
func clockSkewAttention(a *attention, v *nodeView, renewed, now time.Time) {
	if !v.Ready || renewed.IsZero() {
		return
	}
	skew := renewed.Sub(now)
	if skew.Abs() <= clockSkewLimit {
		return
	}
	direction := "ahead of"
	if skew < 0 {
		direction = "behind"
	}
	a.addMachine(v.Machine, severityWarning, roleWord(v.Role)+" "+v.Name+" clock is off by "+skew.Abs().Round(time.Second).String(),
		"The node's clock is "+direction+" the controller's. Tokens and certificates fail on a clock this far out. Check the time daemon on the node and the Proxmox VE host's time.", time.Time{})
}

// ignoredTaint tells whether a taint on a node comes from the cluster itself and not from a person:
// the scheduler's and the cloud controller manager's, Cluster API's, the autoscaler's and the
// control-plane taint Kubehenge's controller keeps.
func ignoredTaint(t corev1.Taint) bool {
	return t.Key == controlPlaneTaint || strings.HasPrefix(t.Key, "node.kubernetes.io/") ||
		strings.HasPrefix(t.Key, "node.cloudprovider.kubernetes.io/") || strings.HasPrefix(t.Key, "node.cluster.x-k8s.io/") ||
		t.Key == "ToBeDeletedByClusterAutoscaler" || t.Key == "DeletionCandidateOfClusterAutoscaler"
}

// taintDriftAttention warns about a worker whose taints are not those of its pool's workerNodes
// setting: a taint put on by hand goes with the node at its next replacement, and a taint of the
// setting that is missing was taken off by hand.
func taintDriftAttention(a *attention, c *clusterv1.Cluster, v *nodeView, n *corev1.Node) {
	if v.Role != workerRole || v.Pool == "" {
		return
	}
	var overrides []clusterv1.ClusterVariable
	if i := slices.IndexFunc(c.Spec.Topology.Workers.MachineDeployments, func(md clusterv1.MachineDeploymentTopology) bool { return md.Name == v.Pool }); i >= 0 {
		overrides = c.Spec.Topology.Workers.MachineDeployments[i].Variables.Overrides
	}
	want, err := provisioner.PoolNodeSettings(c, overrides)
	if err != nil {
		return
	}
	wanted := want.CoreTaints()
	var extra, missing []string
	for _, t := range n.Spec.Taints {
		if !ignoredTaint(t) && !slices.ContainsFunc(wanted, func(w corev1.Taint) bool { return w.MatchTaint(&t) }) {
			extra = append(extra, taintText(t))
		}
	}
	for _, w := range wanted {
		if !slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return w.MatchTaint(&t) }) {
			missing = append(missing, taintText(w))
		}
	}
	if len(extra) == 0 && len(missing) == 0 {
		return
	}
	var parts []string
	if len(extra) > 0 {
		parts = append(parts, "Not in the pool's taints: "+strings.Join(extra, ", ")+". A node replacement removes them. Add them to the pool's labels and taints to keep them.")
	}
	if len(missing) > 0 {
		parts = append(parts, "Missing from the node: "+strings.Join(missing, ", ")+".")
	}
	a.addMachine(v.Machine, severityWarning, roleWord(v.Role)+" "+v.Name+": taints differ from pool "+v.Pool, strings.Join(parts, " "), time.Time{})
}

// taintText writes a taint as key=value:effect, or key:effect without a value.
func taintText(t corev1.Taint) string {
	if t.Value == "" {
		return t.Key + ":" + string(t.Effect)
	}
	return t.Key + "=" + t.Value + ":" + string(t.Effect)
}
