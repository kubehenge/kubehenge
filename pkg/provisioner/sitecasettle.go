package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// SiteRollAnnotation on a Cluster notes the generations of its control plane and
	// MachineDeployments at the moment it was given a site CA. The controller owns it.
	SiteRollAnnotation = "kubehenge.dev/site-ca-roll"
	// SiteRollCondition on a Cluster says why the site CA cannot be rolled out to it.
	SiteRollCondition = "SiteCARoll"
	// UpgradeWindowInvalidReason is the reason of SiteRollCondition for a window that cannot be read.
	UpgradeWindowInvalidReason = "UpgradeWindowInvalid"
	// SiteTimeZoneCondition on a Cluster says the site time zone is not a time zone, so that
	// schedules naming none run in UTC.
	SiteTimeZoneCondition = "SiteTimeZone"
	// SiteTimeZoneInvalidReason is the reason of SiteTimeZoneCondition.
	SiteTimeZoneInvalidReason = "TimeZoneInvalid"

	// siteRollSettle bounds how long a cluster waits for its control plane and MachineDeployments to
	// show a new generation. A change that alters neither would otherwise hold its place for good.
	siteRollSettle = 15 * time.Minute

	controlPlaneKey = "ControlPlane"
)

// siteRollNote is what the annotation holds.
type siteRollNote struct {
	At time.Time `json:"at"`
	// Generations maps the control plane and each MachineDeployment to the generation it had.
	Generations map[string]int64 `json:"generations"`
}

// rolloutObject is the part of a control plane or MachineDeployment that shows it has taken up a
// change and has its machines up to date.
type rolloutObject struct {
	Observed int64
	// UpToDate and Desired are nil where the object does not report them.
	UpToDate, Desired *int32
}

// pending tells whether any object has yet to take up the change the note starts from: it has not
// observed a generation past the recorded one, or its machines are not all up to date.
func (n siteRollNote) pending(now time.Time, current map[string]rolloutObject) bool {
	waiting := now.Sub(n.At) < siteRollSettle
	for name, was := range n.Generations {
		o, ok := current[name]
		if !ok {
			continue
		}
		if waiting && o.Observed <= was {
			return true
		}
		if o.UpToDate != nil && o.Desired != nil && *o.UpToDate < *o.Desired {
			return true
		}
	}
	return false
}

// controlPlaneOf reads the cluster's control plane object, nil when there is none.
func controlPlaneOf(ctx context.Context, rd client.Reader, mapper meta.RESTMapper, c *clusterv1.Cluster) (*unstructured.Unstructured, error) {
	ref := c.Spec.ControlPlaneRef
	if ref.Name == "" {
		return nil, nil //nolint:nilnil // no control plane object to read
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: ref.APIGroup, Kind: ref.Kind})
	if err != nil {
		return nil, fmt.Errorf("finding the control plane's kind: %w", err)
	}
	cp := &unstructured.Unstructured{}
	cp.SetGroupVersionKind(mapping.GroupVersionKind)
	if err = rd.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: ref.Name}, cp); err != nil {
		return nil, client.IgnoreNotFound(err) //nolint:wrapcheck // the caller names the cluster
	}
	return cp, nil
}

// replicasOf reads a control plane's desired and up-to-date machines, nil for the ones it omits.
// Contracts of Cluster API name the second upToDateReplicas or updatedReplicas.
func replicasOf(cp *unstructured.Unstructured) (*int32, *int32) {
	read := func(path ...string) *int32 {
		v, found, err := unstructured.NestedInt64(cp.Object, path...)
		if err != nil || !found {
			return nil
		}
		n := int32(v) //nolint:gosec // a replica count
		return &n
	}
	upToDate := read("status", "upToDateReplicas")
	if upToDate == nil {
		upToDate = read("status", "updatedReplicas")
	}
	return upToDate, read("spec", "replicas")
}

// rolloutObjects reads the generation each of the cluster's control plane and MachineDeployments
// has now, and what each has observed and has up to date.
func rolloutObjects(ctx context.Context, rd client.Reader, mapper meta.RESTMapper, c *clusterv1.Cluster) (map[string]int64, map[string]rolloutObject, error) {
	generations, current := map[string]int64{}, map[string]rolloutObject{}
	cp, err := controlPlaneOf(ctx, rd, mapper, c)
	if err != nil {
		return nil, nil, err
	}
	if cp != nil {
		observed, _, _ := unstructured.NestedInt64(cp.Object, "status", "observedGeneration")
		o := rolloutObject{Observed: observed}
		o.UpToDate, o.Desired = replicasOf(cp)
		generations[controlPlaneKey], current[controlPlaneKey] = cp.GetGeneration(), o
	}
	var deployments clusterv1.MachineDeploymentList
	if err = rd.List(ctx, &deployments, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return nil, nil, fmt.Errorf("listing the MachineDeployments: %w", err)
	}
	for i := range deployments.Items {
		md := &deployments.Items[i]
		name := "MachineDeployment/" + md.Name
		generations[name] = md.Generation
		current[name] = rolloutObject{Observed: md.Status.ObservedGeneration, UpToDate: md.Status.UpToDateReplicas, Desired: md.Spec.Replicas}
	}
	return generations, current, nil
}

// siteRollNote is the annotation a cluster is given with a site CA: the generations to be passed.
func (r *Reconciler) siteRollNote(ctx context.Context, c *clusterv1.Cluster) (string, error) {
	generations, _, err := rolloutObjects(ctx, r.Reader, r.Client.RESTMapper(), c)
	if err != nil {
		return "", fmt.Errorf("reading the rollout of %s/%s: %w", c.Namespace, c.Name, err)
	}
	raw, err := json.Marshal(siteRollNote{At: r.now().UTC(), Generations: generations})
	if err != nil {
		return "", fmt.Errorf("encoding the rollout note: %w", err)
	}
	return string(raw), nil
}

// SiteRollSettling finds the clusters that were given a site CA and whose control plane or
// MachineDeployments have not yet taken it up and brought their machines up to date. Cluster API
// marks such a cluster rolling only after those objects have changed, so without this a cluster
// would free its place before its nodes are replaced. A cluster whose objects cannot be read counts
// as settling.
func SiteRollSettling(ctx context.Context, kube client.Client, clusters []clusterv1.Cluster, now time.Time) map[client.ObjectKey]bool {
	settling := map[client.ObjectKey]bool{}
	for i := range clusters {
		c := &clusters[i]
		raw, ok := c.Annotations[SiteRollAnnotation]
		if !ok {
			continue
		}
		var note siteRollNote
		if json.Unmarshal([]byte(raw), &note) != nil {
			continue
		}
		_, current, err := rolloutObjects(ctx, kube, kube.RESTMapper(), c)
		settling[client.ObjectKeyFromObject(c)] = err != nil || note.pending(now, current)
	}
	return settling
}

// reportSiteRollBlocked says on the cluster, as an event and a condition, that its upgrade window
// cannot be read, so that the site CA is not rolled out to it.
func (r *Reconciler) reportSiteRollBlocked(ctx context.Context, c *clusterv1.Cluster, why error) error {
	return r.reportSiteIssue(ctx, c, SiteRollCondition, UpgradeWindowInvalidReason, why.Error())
}

// clearSiteRollBlocked takes SiteRollCondition off a cluster that has it.
func (r *Reconciler) clearSiteRollBlocked(ctx context.Context, c *clusterv1.Cluster) error {
	return r.clearSiteIssue(ctx, c, SiteRollCondition)
}

// reportSiteIssue says on the cluster, as a Warning event and a false condition of condType, what
// is wrong with a site default or the cluster's own setting of it.
func (r *Reconciler) reportSiteIssue(ctx context.Context, c *clusterv1.Cluster, condType, reason, message string) error {
	message = clip(message, conditionMessageMax)
	if r.Events != nil {
		r.Events.Eventf(c, nil, corev1.EventTypeWarning, reason, "RollSiteCA", "%s", clip(message, eventNoteMax))
	}
	cond := metav1.Condition{Type: condType, Status: metav1.ConditionFalse, Reason: reason, Message: message}
	if _, err := setClusterCondition(ctx, r.Client, r.Reader, client.ObjectKeyFromObject(c), condType, &cond); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// clearSiteIssue takes the condition of condType off a cluster that has it.
func (r *Reconciler) clearSiteIssue(ctx context.Context, c *clusterv1.Cluster, condType string) error {
	if meta.FindStatusCondition(c.Status.Conditions, condType) == nil {
		return nil
	}
	if _, err := setClusterCondition(ctx, r.Client, r.Reader, client.ObjectKeyFromObject(c), condType, nil); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
