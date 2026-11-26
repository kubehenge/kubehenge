package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// addonVolumeNamespaces are the namespaces of the add-ons that keep data on persistent volumes,
// by the add-on's name.
var addonVolumeNamespaces = map[string]string{
	monitoringAddon: monitoringAddon, logsNamespace: logsAddon, rancherNamespace: rancherAddon, argoCDNamespace: "argocd",
}

// provisioningFailed is the reason of the event a CSI plugin's provisioner records on a claim it
// cannot make a volume for.
const provisioningFailed = "ProvisioningFailed"

// volumeReasonMax bounds the plugin's reason in a waiting line.
const volumeReasonMax = 200

// addonVolumeWaits are the lines "<add-on> has no volume: <the plugin's reason>" for each add-on
// with a PersistentVolumeClaim that stays Pending after its volume failed to be provisioned. They
// are read from the workload cluster afresh each time, so a line goes once the claim is bound.
func (r *Reconciler) addonVolumeWaits(ctx context.Context, cluster *clusterv1.Cluster) ([]string, error) {
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return nil, err
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err = workload.client.List(ctx, claims); err != nil {
		r.Remotes.forget(cluster)
		return nil, fmt.Errorf("listing the cluster's persistent volume claims: %w", err)
	}
	var lines []string
	seen := map[string]bool{}
	for i := range claims.Items {
		claim := &claims.Items[i]
		addon, ok := addonVolumeNamespaces[claim.Namespace]
		if !ok || seen[addon] || claim.Status.Phase != corev1.ClaimPending {
			continue
		}
		reason, err := provisioningFailure(ctx, workload.client, claim)
		if err != nil {
			r.Remotes.forget(cluster)
			return nil, err
		}
		if reason != "" {
			seen[addon] = true
			lines = append(lines, addon+" has no volume: "+reason)
		}
	}
	slices.Sort(lines)
	return lines, nil
}

// provisioningFailure is the plugin's reason from the latest ProvisioningFailed event of the
// claim, or "" when it has none.
func provisioningFailure(ctx context.Context, workload client.Client, claim *corev1.PersistentVolumeClaim) (string, error) {
	events := &corev1.EventList{}
	if err := workload.List(ctx, events, client.InNamespace(claim.Namespace)); err != nil {
		return "", fmt.Errorf("listing the events of the claims in %s: %w", claim.Namespace, err)
	}
	var latest *corev1.Event
	var at time.Time
	for i := range events.Items {
		e := &events.Items[i]
		if e.Reason != provisioningFailed || e.InvolvedObject.Kind != "PersistentVolumeClaim" || e.InvolvedObject.Name != claim.Name {
			continue
		}
		when := e.LastTimestamp.Time
		if when.IsZero() {
			when = e.EventTime.Time
		}
		if latest == nil || when.After(at) {
			latest, at = e, when
		}
	}
	if latest == nil {
		return "", nil
	}
	// The provisioner wraps the plugin's gRPC error: "...: rpc error: code = Internal desc = <reason>".
	_, reason, ok := strings.Cut(latest.Message, "desc = ")
	if !ok {
		reason = latest.Message
	}
	return clip(strings.TrimSpace(reason), volumeReasonMax), nil
}
