package provisioner

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	// provisioningFailed is the reason of the event a CSI plugin's provisioner records on a claim
	// it cannot make a volume for.
	provisioningFailed = "ProvisioningFailed"

	// volumeReasonMax bounds the plugin's reason in a waiting line.
	volumeReasonMax = 200

	// prometheusNameLabel names the Prometheus whose StatefulSet's claims the operator labels.
	prometheusNameLabel = "operator.prometheus.io/name"

	// The plugin's held release reads a storage it may not see as one of these. Its client turns
	// every 403 into "not authorized to access endpoint", whatever Proxmox VE or the proxy said.
	storageRefused = "storage config: not authorized to access endpoint"
	storageUnseen  = "storage config: not found"
)

// addonClaims are, by add-on, where the claims Kubehenge's releases of the add-ons switched on make
// are, and how each is known: Prometheus's by the operator's label for the release's Prometheus,
// Loki's by the release that applies it.
func addonClaims(addons *Addons) []addonClaim {
	var claims []addonClaim
	if addons.Monitoring.Enabled {
		claims = append(claims, addonClaim{monitoringAddon, monitoringAddon, func(c *corev1.PersistentVolumeClaim) bool {
			return c.Labels[prometheusNameLabel] == monitoringRelease+"-prometheus"
		}})
	}
	if addons.logsOn() {
		claims = append(claims, addonClaim{logsAddon, logsNamespace, func(c *corev1.PersistentVolumeClaim) bool {
			return c.Name == lokiStorage && c.Annotations[helmReleaseNameAnnotation] == lokiRelease && c.Annotations[helmReleaseNamespaceAnnotation] == logsNamespace
		}})
	}
	return claims
}

type addonClaim struct {
	addon, namespace string
	ours             func(*corev1.PersistentVolumeClaim) bool
}

// addonVolumeWaits are the lines "<add-on> has no volume: <reason>" for each add-on switched on with
// a claim of Kubehenge's release that stays Pending after its volume failed to be provisioned. They
// are read from the workload cluster afresh each time, so a line goes once the claim is bound. The
// read is best effort: a cluster that does not answer gets no line, and the rest of the reconcile
// goes on.
func (r *Reconciler) addonVolumeWaits(ctx context.Context, cluster *clusterv1.Cluster, addons *Addons) []string {
	wanted := addonClaims(addons)
	if len(wanted) == 0 {
		return nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return nil
	}
	var lines []string
	for _, want := range wanted {
		reason, err := pendingReason(ctx, workload.client, want)
		if err != nil {
			if unreachable(err) {
				r.Remotes.forget(cluster)
			}
			ctrl.LoggerFrom(ctx).Info("cannot read an add-on's volume claims", "addon", want.addon, "error", err.Error())
			continue
		}
		if reason != "" {
			lines = append(lines, want.addon+" has no volume: "+namedCause(cluster, reason))
		}
	}
	return lines
}

// pendingReason is the plugin's reason for the first Pending claim of want that has one, or "".
func pendingReason(ctx context.Context, workload client.Client, want addonClaim) (string, error) {
	claims := &corev1.PersistentVolumeClaimList{}
	if err := workload.List(ctx, claims, client.InNamespace(want.namespace)); err != nil {
		return "", fmt.Errorf("listing the claims in %s: %w", want.namespace, err)
	}
	var events *corev1.EventList
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Status.Phase != corev1.ClaimPending || !want.ours(claim) {
			continue
		}
		if events == nil {
			events = &corev1.EventList{}
			if err := workload.List(ctx, events, client.InNamespace(want.namespace)); err != nil {
				return "", fmt.Errorf("listing the events in %s: %w", want.namespace, err)
			}
		}
		if reason := provisioningFailure(events, claim); reason != "" {
			return reason, nil
		}
	}
	return "", nil
}

// provisioningFailure is the plugin's reason from the latest ProvisioningFailed event of the
// claim, or "" when it has none.
func provisioningFailure(events *corev1.EventList, claim *corev1.PersistentVolumeClaim) string {
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
		return ""
	}
	// The provisioner wraps the plugin's gRPC error: "...: rpc error: code = Internal desc = <reason>".
	_, reason, ok := strings.Cut(latest.Message, "desc = ")
	if !ok {
		reason = latest.Message
	}
	return clip(strings.TrimSpace(reason), volumeReasonMax)
}

// namedCause says what the plugin's reason means where Kubehenge knows it: a storage the cluster's
// token may not see, which the held release names only as refused or not found.
func namedCause(cluster *clusterv1.Cluster, reason string) string {
	storage, err := variable[string](cluster, "storage")
	if err != nil || storage == "" {
		return reason
	}
	audit := "needs Datastore.Audit on /storage/" + storage
	switch {
	case strings.Contains(reason, storageRefused):
		return "the cluster's token may not see storage " + storage + " and " + audit
	case strings.HasSuffix(reason, storageUnseen):
		return "storage " + storage + " is not shown to the cluster's token, which " + audit + ", or it does not exist"
	}
	return reason
}
