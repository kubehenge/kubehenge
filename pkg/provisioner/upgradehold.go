package provisioner

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UpgradeHeldCondition on a Cluster says that BeforeClusterUpgrade holds back the Kubernetes
// upgrade its topology asks for. Its reason says why and its message says what to do. Cluster
// API's TopologyReconciled condition carries the hook's message too, but only inside its own prose.
const UpgradeHeldCondition = "UpgradeHeld"

// The reasons of UpgradeHeldCondition.
const (
	// UpgradeHeldRancherKubernetes: the Rancher release Kubehenge installs does not run on the version.
	UpgradeHeldRancherKubernetes = "RancherKubernetes"
	// UpgradeHeldRancherElsewhere: the cluster asks for Rancher, which another cluster runs already.
	UpgradeHeldRancherElsewhere = "RancherElsewhere"
	// UpgradeHeldTalosUpgrade: a node is moving to another Talos release.
	UpgradeHeldTalosUpgrade = "TalosUpgrade"
)

// How long Cluster API waits before it asks again, in seconds. A newer Rancher comes only with a
// Kubehenge release, and a Talos node upgrade takes minutes.
const (
	rancherHoldRetry = 300
	talosHoldRetry   = 30
)

// upgradeHoldRetry is how long Cluster API waits before it asks again, by reason.
var upgradeHoldRetry = map[string]int32{
	UpgradeHeldRancherKubernetes: rancherHoldRetry,
	UpgradeHeldRancherElsewhere:  rancherHoldRetry,
	UpgradeHeldTalosUpgrade:      talosHoldRetry,
}

// upgradeHolds decides what holds a cluster's Kubernetes upgrade back. The hook holds the upgrade
// for it and the controller clears what no longer holds, so both share it.
type upgradeHolds struct {
	reader         client.Reader
	classNamespace string
	// rancher is what the Rancher release Kubehenge installs runs on. Unset allows any.
	rancher KubernetesMinors
}

// hold is why the cluster's upgrade to version waits, as a true UpgradeHeld condition, and
// whether anything holds it.
func (u upgradeHolds) hold(ctx context.Context, cluster *clusterv1.Cluster, version string) (metav1.Condition, bool, error) {
	reason, message, err := u.rancher.hold(ctx, u.reader, u.classNamespace, cluster, version)
	if err != nil {
		return metav1.Condition{}, false, err
	}
	if reason == "" {
		if upgrading := cluster.Annotations[TalosUpgradingAnnotation]; upgrading != "" {
			reason, message = UpgradeHeldTalosUpgrade, "waiting for the Talos upgrade of "+upgrading
		}
	}
	cond := metav1.Condition{Type: UpgradeHeldCondition, Status: metav1.ConditionTrue, Reason: reason, Message: message}
	return cond, reason != "", nil
}

// hold is why Rancher refuses the cluster at version: it does not run there, or another cluster
// runs it already, since every cluster registers with the one. The reason is empty where Rancher
// takes it.
func (m KubernetesMinors) hold(ctx context.Context, reader client.Reader, classNamespace string, cluster *clusterv1.Cluster, version string) (string, string, error) {
	v, err := rancherOf(cluster)
	if err != nil || !v.Install {
		return "", "", err
	}
	if m != (KubernetesMinors{}) && !m.Allows(version) {
		return UpgradeHeldRancherKubernetes, m.refusal(version), nil
	}
	clusters := &clusterv1.ClusterList{}
	if err := reader.List(ctx, clusters); err != nil {
		return "", "", fmt.Errorf("listing clusters: %w", err)
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if c.Namespace == cluster.Namespace && c.Name == cluster.Name || !Owns(c, classNamespace) {
			continue
		}
		if other, err := rancherOf(c); err == nil && other.Install {
			return UpgradeHeldRancherElsewhere, fmt.Sprintf("cluster %s/%s already runs Rancher. Only one cluster may run it", c.Namespace, c.Name), nil
		}
	}
	return "", "", nil
}

// refusal says which versions to choose instead of one outside the range.
func (m KubernetesMinors) refusal(version string) string {
	return fmt.Sprintf("for Rancher, choose Kubernetes 1.%d to 1.%d, not %s", m.Min, m.Max, version)
}

// markUpgradeHeld sets the cluster's UpgradeHeld condition to cond, or removes it where cond is
// nil. It writes only what changes, since the hook is asked again and again while it holds.
func markUpgradeHeld(ctx context.Context, c client.Client, live client.Reader, cluster *clusterv1.Cluster, cond *metav1.Condition) error {
	found := meta.FindStatusCondition(cluster.Status.Conditions, UpgradeHeldCondition)
	switch {
	case cond == nil && found == nil:
		return nil
	case cond != nil && found != nil && found.Status == cond.Status && found.Reason == cond.Reason && found.Message == cond.Message:
		return nil
	}
	_, err := markCluster(ctx, c, live, client.ObjectKeyFromObject(cluster), UpgradeHeldCondition, cond)
	return err
}

// clearUpgradeHeld removes a cluster's UpgradeHeld condition once nothing holds its upgrade any
// more. The hook is no longer asked once the Cluster names the version it runs again, so without
// this a hold would outlast the upgrade it held.
func (r *Reconciler) clearUpgradeHeld(ctx context.Context, cluster *clusterv1.Cluster) error {
	if meta.FindStatusCondition(cluster.Status.Conditions, UpgradeHeldCondition) == nil {
		return nil
	}
	holds := upgradeHolds{reader: r.Client, classNamespace: r.ClassNamespace, rancher: r.RancherKubernetes}
	_, held, err := holds.hold(ctx, cluster, cluster.Spec.Topology.Version)
	if err != nil || held {
		return err
	}
	return markUpgradeHeld(ctx, r.Client, r.Reader, cluster, nil)
}
