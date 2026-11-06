package provisioner

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// TalosUpgradeWaitingAnnotation on a Cluster says why the upgrade of a control-plane node waits: the
// node's reboot would cost etcd its quorum. It is gone once the node goes on.
const TalosUpgradeWaitingAnnotation = "kubehenge.dev/talos-upgrade-waiting"

// talosQuorumTimeout bounds the reads of one control-plane node's etcd.
const talosQuorumTimeout = 30 * time.Second

func isControlPlane(m *clusterv1.Machine) bool {
	_, ok := m.Labels[clusterv1.MachineControlPlaneLabel]
	return ok
}

// quorumBlock says why a control-plane node cannot reboot without costing etcd its quorum, or is ""
// when it can: it reads, through each of the other control planes' Talos API, their etcd member and the
// member list, and wants every other member to be a healthy voter. A worker never waits.
func (u *TalosUpgrader) quorumBlock(ctx context.Context, c *clusterv1.Cluster, machines []clusterv1.Machine, target *clusterv1.Machine) (string, error) {
	if !isControlPlane(target) {
		return "", nil
	}
	var others []*clusterv1.Machine
	total := 0
	for i := range machines {
		m := &machines[i]
		if !isControlPlane(m) || !m.DeletionTimestamp.IsZero() {
			continue
		}
		total++
		if m.Name != target.Name {
			others = append(others, m)
		}
	}
	if len(others) == 0 {
		return "", nil
	}
	raw, err := readTalosconfig(ctx, u.Reader, c.Namespace, c.Name)
	if err != nil {
		return "", err
	}
	var reasons []string
	var listed []EtcdListed
	healthy := map[uint64]bool{}
	for _, o := range others {
		member, list, err := u.readEtcd(ctx, raw, o)
		switch {
		case err != nil:
			reasons = append(reasons, fmt.Sprintf("etcd on %s cannot be read: %v", o.Name, err))
			continue
		case member.Learner:
			reasons = append(reasons, fmt.Sprintf("etcd member %s is still joining as a learner", o.Name))
		case member.Alarmed():
			reasons = append(reasons, fmt.Sprintf("etcd member %s refuses writes, its database is full", o.Name))
		case len(member.Errors) > 0:
			reasons = append(reasons, fmt.Sprintf("etcd member %s is unhealthy: %s", o.Name, strings.Join(member.Errors, ", ")))
		default:
			healthy[member.ID] = true
		}
		if listed == nil {
			listed = list
		}
	}
	return strings.Join(append(reasons, unaccounted(listed, healthy, target, total)...), "; "), nil
}

// unaccounted are the reasons the member list gives that the other control planes' own reads did not:
// a learner, a voter that none of them answered for and that is not the target's, and fewer voters
// than control planes.
func unaccounted(listed []EtcdListed, healthy map[uint64]bool, target *clusterv1.Machine, total int) []string {
	var reasons []string
	voters := 0
	for _, l := range listed {
		if l.Learner {
			reasons = append(reasons, fmt.Sprintf("etcd member %s is a learner", l.Name))
			continue
		}
		voters++
		if !healthy[l.ID] && l.Name != target.Name && l.Name != target.Status.NodeRef.Name {
			reasons = append(reasons, fmt.Sprintf("etcd member %s is not healthy", l.Name))
		}
	}
	if listed != nil && voters < total {
		reasons = append(reasons, fmt.Sprintf("etcd has %d voting members for %d control planes", voters, total))
	}
	return reasons
}

// readEtcd reads the etcd member on a control-plane node and the member list it has.
func (u *TalosUpgrader) readEtcd(ctx context.Context, raw []byte, m *clusterv1.Machine) (EtcdMember, []EtcdListed, error) {
	ctx, cancel := context.WithTimeout(ctx, talosQuorumTimeout)
	defer cancel()
	node, err := u.Dial(ctx, raw, machineAddress(m))
	if err != nil {
		return EtcdMember{}, nil, err
	}
	defer func() { _ = node.Close() }()
	member, err := node.EtcdMember(ctx)
	if err != nil {
		return EtcdMember{}, nil, err
	}
	list, err := node.EtcdMembers(ctx)
	return member, list, err
}

// stillHoldsQuorum asks holdForQuorum again for a node whose installer is written, just before it is
// drained and rebooted.
func (u *TalosUpgrader) stillHoldsQuorum(ctx context.Context, c *clusterv1.Cluster, s *talosStep) (bool, error) {
	machines, err := u.machines(ctx, c)
	if err != nil {
		return true, err
	}
	m := nodeMachine(machines, s.node)
	if m == nil {
		return false, nil
	}
	return u.holdForQuorum(ctx, c, machines, m)
}

// holdForQuorum tells whether a control-plane node's upgrade must wait for etcd, and says why on the
// cluster, with an event naming the member, while it does. The note goes once the node goes on.
func (u *TalosUpgrader) holdForQuorum(ctx context.Context, c *clusterv1.Cluster, machines []clusterv1.Machine, m *clusterv1.Machine) (bool, error) {
	reason, err := u.quorumBlock(ctx, c, machines, m)
	if err != nil {
		return true, err
	}
	waiting, noted := c.Annotations[TalosUpgradeWaitingAnnotation]
	if reason == "" {
		if noted {
			return false, u.annotate(ctx, c, map[string]any{TalosUpgradeWaitingAnnotation: nil})
		}
		return false, nil
	}
	note := fmt.Sprintf("node %s waits: %s", m.Name, reason)
	if note != waiting {
		u.eventf(c, corev1.EventTypeWarning, "TalosUpgradeWaiting", "the upgrade of node %s waits for etcd: %s", m.Name, reason)
		if err := u.annotate(ctx, c, map[string]any{TalosUpgradeWaitingAnnotation: note}); err != nil {
			return true, err
		}
	}
	return true, nil
}
