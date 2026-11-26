package provisioner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// AddonsWaitingCondition on a Cluster says that add-ons wait, for cert-manager or for a volume,
	// and why.
	AddonsWaitingCondition = "AddonsWaiting"
	// AddonsWaitingForCertManager is its reason while cert-manager installs or waits for the
	// cluster's Gateway API.
	AddonsWaitingForCertManager = "WaitingForCertManager"
	// AddonsWaitingGatewayAPIMissing is its reason once the Gateway API is still unrecorded
	// gatewayAPIOverdue after the control plane is initialised: the distribution's chart has not
	// deployed, or has and the controller did not record it.
	AddonsWaitingGatewayAPIMissing = "GatewayAPIMissing"
	// AddonsWaitingVolumeMissing is its reason while only add-ons whose volume cannot be made wait.
	AddonsWaitingVolumeMissing = "VolumeMissing"

	// gatewayAPIOverdue is how long after the control plane is initialised the Gateway API may go
	// unrecorded before the wait says so.
	gatewayAPIOverdue = 10 * time.Minute
)

// certManagerUsers names the proxies of the charts that make cert-manager objects, which Helm
// cannot install before the cluster serves their kinds.
func certManagerUsers(cluster *clusterv1.Cluster) []string {
	return []string{rancherProxy(cluster), monitoringProxy(cluster), lokiProxy(cluster), alloyProxy(cluster), alloyNodeProxy(cluster)}
}

// certManagerWaits reports whether the proxy named waits for cert-manager's release to be deployed,
// and records it in addons.certManagerWaiting. The Add-on Provider for Helm puts each install after
// a failed one further off. A proxy already installed stays while cert-manager is upgraded.
func (r *Reconciler) certManagerWaits(ctx context.Context, cluster *clusterv1.Cluster, name string, addons *Addons) (bool, error) {
	installed, err := r.anyProxy(ctx, cluster, name)
	if err != nil || installed {
		return false, err
	}
	ready, err := r.proxyReady(ctx, cluster, certManagerProxy(cluster))
	if err != nil || ready {
		return false, err
	}
	addons.certManagerWaiting = append(addons.certManagerWaiting, strings.TrimPrefix(name, cluster.Name+"-"))
	return true, nil
}

// recordAddonsWaiting sets the cluster's AddonsWaiting condition to the add-ons that wait for
// cert-manager and why, then the volume lines, or takes it off. It returns the add-on conflicts to
// record, cert-manager's with the add-ons it holds back, so that a conflict is said once.
func (r *Reconciler) recordAddonsWaiting(ctx context.Context, cluster *clusterv1.Cluster, addons *Addons, volumes []string) ([]addonConflict, error) {
	conflicts, waiting := addons.conflicts, andJoined(addons.certManagerWaiting)
	var cond *metav1.Condition
	switch held := addons.certManagerConflict; {
	case held.message != "" && waiting != "":
		held.message += ". Until then " + waiting + " wait"
		conflicts = append(conflicts, held)
	case held.message != "":
		conflicts = append(conflicts, held)
	case waiting != "":
		reason, why, err := r.certManagerHeldBy(ctx, cluster)
		if err != nil {
			return nil, err
		}
		cond = &metav1.Condition{Type: AddonsWaitingCondition, Status: metav1.ConditionTrue, Reason: reason, Message: waiting + " wait for cert-manager" + why}
	}
	if len(volumes) > 0 {
		if cond == nil {
			cond = &metav1.Condition{Type: AddonsWaitingCondition, Status: metav1.ConditionTrue, Reason: AddonsWaitingVolumeMissing}
		}
		if cond.Message != "" {
			volumes = append([]string{cond.Message}, volumes...)
		}
		cond.Message = strings.Join(volumes, ". ")
	}
	found := meta.FindStatusCondition(cluster.Status.Conditions, AddonsWaitingCondition)
	switch {
	case cond == nil && found == nil:
		return conflicts, nil
	case cond != nil && found != nil && found.Reason == cond.Reason && found.Message == shorten(cond.Message, watchMessageMax):
		return conflicts, nil
	}
	_, err := markCluster(ctx, r.Client, r.Reader, client.ObjectKeyFromObject(cluster), AddonsWaitingCondition, cond)
	return conflicts, err
}

// andJoined lists names as a sentence does: "a, b and c".
func andJoined(names []string) string {
	last := len(names) - 1
	if last <= 0 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:last], ", ") + " and " + names[last]
}

// certManagerHeldBy says what cert-manager itself waits for: its release, or the cluster's Gateway
// API, which it serves. Once nothing has recorded the Gateway API for gatewayAPIOverdue, it names
// the distribution's chart and whether it has deployed.
func (r *Reconciler) certManagerHeldBy(ctx context.Context, cluster *clusterv1.Cluster) (string, string, error) {
	if _, has := cluster.Annotations[gatewayAPIAnnotation]; has {
		return AddonsWaitingForCertManager, " to be deployed", nil
	}
	const gatewayWait = ", which waits for the cluster's Gateway API definitions"
	charts, has := chartsOf(distributionOf(cluster))
	initialised := meta.FindStatusCondition(cluster.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition)
	if !has || initialised == nil || initialised.Status != metav1.ConditionTrue || r.now().Sub(initialised.LastTransitionTime.Time) < gatewayAPIOverdue {
		return AddonsWaitingForCertManager, gatewayWait, nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return AddonsWaitingForCertManager, gatewayWait, err
	}
	deployed, err := helmReleaseDeployed(ctx, workload.client, metav1.NamespaceSystem, charts.traefikCRD)
	switch {
	case unreachable(err):
		r.Remotes.forget(cluster)
		return AddonsWaitingForCertManager, gatewayWait, nil
	case err != nil:
		return "", "", fmt.Errorf("reading the cluster's %s release: %w", charts.traefikCRD, err)
	case !deployed:
		return AddonsWaitingGatewayAPIMissing, fmt.Sprintf("%s: %s has not deployed in %d minutes. Check the HelmChart %s and the Job helm-install-%s in kube-system",
			gatewayWait, charts.traefikCRD, int(gatewayAPIOverdue.Minutes()), charts.traefikCRD, charts.traefikCRD), nil
	}
	return AddonsWaitingGatewayAPIMissing, fmt.Sprintf("%s: %s is deployed, yet none were recorded in %d minutes", gatewayWait, charts.traefikCRD, int(gatewayAPIOverdue.Minutes())), nil
}
