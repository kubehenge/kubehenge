package provisioner

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// AutoUpgradeSkippedCondition on a Cluster says that an automatic upgrade is due and waits because
// the cluster would run into something, which the message names. A person can still upgrade it by
// hand, past the warning. A version that Rancher or the Talos release does not run, and a Talos node
// upgrade under way, hold it too, with the reasons of UpgradeHeldCondition.
const AutoUpgradeSkippedCondition = "AutoUpgradeSkipped"

// The reasons of AutoUpgradeSkippedCondition.
const (
	// AutoUpgradeSkippedRemovedAPIs: APIs the new release removes are still in use.
	AutoUpgradeSkippedRemovedAPIs = "RemovedAPIs"
	// AutoUpgradeSkippedWebhooks: a webhook that fails closed has a backend one drained node can take.
	AutoUpgradeSkippedWebhooks = "FailClosedWebhooks"
	// AutoUpgradeSkippedUnchecked: the cluster could not be read for either.
	AutoUpgradeSkippedUnchecked = "ChecksUnavailable"
	// AutoUpgradeSkippedTalosRelease: the cluster's Talos release does not run the version. The
	// Talos release moves first, by hand.
	AutoUpgradeSkippedTalosRelease = "TalosRelease"
)

// KubernetesOutOfSupportCondition on a Cluster says that its Kubernetes minor version is older
// than the oldest Kubehenge offers for new clusters.
const KubernetesOutOfSupportCondition = "KubernetesOutOfSupport"

// KubernetesBehind is the reason of KubernetesOutOfSupportCondition.
const KubernetesBehind = "Behind"

// supportEvery is how often the clusters' minors are compared with what Kubehenge offers. The
// offer moves with a release of Kubernetes, a few times a year.
const supportEvery = time.Hour

// supportDue tells whether this round compares the clusters' minors with the offer.
func (u *Upgrader) supportDue() bool {
	return u.supportAt.IsZero() || u.Now().Sub(u.supportAt) >= supportEvery
}

// markSupport sets KubernetesOutOfSupportCondition on a cluster whose minor is older than the
// oldest one offered for a new cluster, and takes it away once it is not.
func (u *Upgrader) markSupport(ctx context.Context, c *clusterv1.Cluster, channels map[string]Channels) {
	if !Owns(c, u.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
		return
	}
	log := ctrl.LoggerFrom(ctx).WithName("upgrader")
	running, ok := parseKubeVersion(c.Spec.Topology.Version)
	if !ok {
		return
	}
	ch, err := u.channelsOf(ctx, c, channels)
	if err != nil {
		log.Error(err, "comparing a cluster's Kubernetes version with the offer", "cluster", client.ObjectKeyFromObject(c))
		return
	}
	var cond *metav1.Condition
	if oldest, ok := ch.OldestOffered(); ok && running.Minor < oldest {
		cond = &metav1.Condition{
			Status: metav1.ConditionTrue, Reason: KubernetesBehind,
			Message: fmt.Sprintf("Kubernetes 1.%d is older than 1.%d, the oldest minor version Kubehenge offers for new clusters, and gets no more fixes. Upgrade one minor version at a time.", running.Minor, oldest),
		}
	}
	before, err := markCluster(ctx, u.Client, nil, client.ObjectKeyFromObject(c), KubernetesOutOfSupportCondition, cond)
	if err != nil {
		log.Error(err, "recording that a cluster's Kubernetes version is out of support", "cluster", client.ObjectKeyFromObject(c))
		return
	}
	if u.Events == nil {
		return
	}
	switch {
	case newlyMarked(before, cond):
		u.Events.Eventf(c, nil, corev1.EventTypeWarning, KubernetesOutOfSupportCondition, "CompareVersions", "%s", cond.Message)
	case before != nil && cond == nil:
		u.Events.Eventf(c, nil, corev1.EventTypeNormal, "KubernetesSupported", "CompareVersions", "Kubernetes 1.%d is offered for new clusters again", running.Minor)
	}
}

// vetUpgrade tells whether an automatic upgrade of the cluster to version may start. A removed API
// in use, a fail-closed webhook with a thin backend and a check that could not be made each hold it
// back, with a condition on the cluster and an event that name why.
func (u *Upgrader) vetUpgrade(ctx context.Context, c *clusterv1.Cluster, to string) bool {
	if u.Checker == nil {
		return true
	}
	found := u.Checker.Check(ctx, c, to)
	found.Unacknowledged(c, to)
	if !found.Blocks() {
		// The note goes once the upgrade is patched: clearing it here would change the Cluster under the
		// patch's optimistic lock.
		return true
	}
	reason := AutoUpgradeSkippedUnchecked
	switch {
	case len(found.RemovedAPIs) > 0:
		reason = AutoUpgradeSkippedRemovedAPIs
	case len(found.Webhooks) > 0:
		reason = AutoUpgradeSkippedWebhooks
	}
	u.markSkipped(ctx, c, to, reason,
		fmt.Sprintf("%s%s waits. %s Choose Upgrade anyway to accept these, or upgrade by hand.", skippedPrefix, to, strings.Join(found.Lines(), " ")))
	return false
}

// markSkipped records on the cluster, and in an event, that the automatic upgrade to version does
// not start.
func (u *Upgrader) markSkipped(ctx context.Context, c *clusterv1.Cluster, version, reason, message string) {
	cond := &metav1.Condition{Status: metav1.ConditionTrue, Reason: reason, Message: message}
	log := ctrl.LoggerFrom(ctx).WithName("upgrader")
	before, err := markCluster(ctx, u.Client, nil, client.ObjectKeyFromObject(c), AutoUpgradeSkippedCondition, cond)
	if err != nil {
		log.Error(err, "recording a skipped automatic upgrade", "cluster", client.ObjectKeyFromObject(c))
		return
	}
	if u.Events != nil && (before == nil || before.Reason != reason || before.Message != shorten(cond.Message, watchMessageMax)) {
		u.Events.Eventf(c, nil, corev1.EventTypeWarning, "AutoUpgradeSkipped", "Upgrade", "%s", shorten(cond.Message, eventNoteMax))
	}
	log.Info("automatic upgrade skipped", "cluster", client.ObjectKeyFromObject(c), "to", version, "reason", reason)
}

// heldBy is why an automatic upgrade of the cluster to version waits for something outside the
// cluster's own health, or "" when nothing holds it. It asks the rules BeforeClusterUpgrade
// applies, so the hook would not hold back what the upgrader starts, and adds the Talos release
// that must run the version.
func (u *Upgrader) heldBy(ctx context.Context, c *clusterv1.Cluster, version string) (string, string, error) {
	holds := upgradeHolds{reader: u.Client, classNamespace: u.ClassNamespace, rancher: u.RancherKubernetes}
	cond, held, err := holds.hold(ctx, c, version)
	if err != nil || held {
		return cond.Reason, cond.Message, err
	}
	if isTalos(c) {
		talos, err := TalosVersion(c)
		if err != nil {
			return "", "", err
		}
		if err := templates.TalosRunsKubernetes(talos, version); err != nil {
			return AutoUpgradeSkippedTalosRelease, err.Error(), nil //nolint:nilerr // a release that does not run the version holds the upgrade, it is no failure
		}
	}
	return "", "", nil
}

// clearSkipped takes AutoUpgradeSkippedCondition away once nothing holds the upgrade back.
func (u *Upgrader) clearSkipped(ctx context.Context, c *clusterv1.Cluster) {
	if meta.FindStatusCondition(c.Status.Conditions, AutoUpgradeSkippedCondition) == nil {
		return
	}
	if _, err := markCluster(ctx, u.Client, nil, client.ObjectKeyFromObject(c), AutoUpgradeSkippedCondition, nil); err != nil {
		ctrl.LoggerFrom(ctx).WithName("upgrader").Error(err, "clearing a skipped automatic upgrade", "cluster", client.ObjectKeyFromObject(c))
	}
}
