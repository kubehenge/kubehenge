package provisioner

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// SiteCAsVariable holds the site CA a cluster's nodes trust, which the controller writes into a
	// cluster that has no trustedCAs of its own. Writing it replaces the nodes, one at a time.
	SiteCAsVariable = "siteCAs"
	// SiteRollLimit is how many clusters take a new site CA at once, as many as reconcile at once.
	SiteRollLimit = concurrentReconciles
)

// siteTimeZone is the time zone of a schedule that names none.
var siteTimeZone = func() *atomic.Pointer[string] {
	p := &atomic.Pointer[string]{}
	zone := DefaultTimeZone
	p.Store(&zone)
	return p
}()

// siteZone is the time zone a schedule that names none runs in.
func siteZone() string { return *siteTimeZone.Load() }

// CheckTimeZone refuses a name that is no IANA time zone.
func CheckTimeZone(zone string) error {
	if _, err := time.LoadLocation(zone); err != nil || zone == "Local" || zone == "" {
		return fmt.Errorf("time zone %q is not an IANA time zone", zone)
	}
	return nil
}

// SetDefaultTimeZone sets the time zone of a schedule that names none.
func SetDefaultTimeZone(zone string) error {
	if err := CheckTimeZone(zone); err != nil {
		return err
	}
	siteTimeZone.Store(&zone)
	return nil
}

// CheckCertificateBundle holds a PEM bundle to certificates alone, none of them expired or not yet
// valid. A zero now leaves the validity unchecked.
func CheckCertificateBundle(bundle string, now time.Time) error {
	return checkCertificates(bundle, now)
}

// NormalizeCertificateBundle writes a PEM bundle's certificates as the chart and the module take
// them: LF line ends and nothing between the blocks, so that CRLF files and openssl's text output
// are accepted the same everywhere.
func NormalizeCertificateBundle(bundle string) string {
	var out strings.Builder
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return out.String()
		}
		if block.Type == certificateBlock {
			out.Write(pem.EncodeToMemory(&pem.Block{Type: certificateBlock, Bytes: block.Bytes}))
		}
	}
}

// appliedSiteCA is the site CA the nodes of a cluster that sets no trustedCAs of its own were
// given. Empty when none was.
func appliedSiteCA(c *clusterv1.Cluster) string {
	applied, _, err := optionalVariable[[]string](c, SiteCAsVariable)
	if err != nil {
		return ""
	}
	return strings.Join(applied, "\n")
}

// InheritsSiteCA reports whether a cluster takes the site CA: it sets no trustedCAs of its own.
func InheritsSiteCA(c *clusterv1.Cluster) bool {
	own, _, err := optionalVariable[[]string](c, trustedCAsVariable)
	return err == nil && len(own) == 0
}

// clusterBuilt tells whether the cluster's control plane has been up, so that it has nodes.
func clusterBuilt(c *clusterv1.Cluster) bool {
	return meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition)
}

// SiteRollCluster is a cluster as the site CA's rollout sees it.
type SiteRollCluster struct {
	Key client.ObjectKey
	// Inherits is true for a cluster without trustedCAs of its own.
	Inherits bool
	// Applied is the site CA its nodes were last given.
	Applied string
	// Rolling is true while the cluster replaces nodes.
	Rolling bool
	// Settling is true while the cluster has not yet taken up what its topology says: the topology
	// controller has not seen the latest change, or machines are not up to date. A cluster just
	// given the site CA is settling before Cluster API marks it rolling.
	Settling bool
	// Paused is true for a cluster Cluster API does not act on, which therefore replaces no node.
	Paused bool
	// Built is true once the cluster's control plane is up. Before that it has no node to replace,
	// so the site CA is part of its first nodes and no pacing applies.
	Built bool
	// Policy is the cluster's upgrade policy, nil when it sets none.
	Policy *UpgradePolicy
}

// clusterSettling tells whether a cluster has yet to take up its latest topology: the topology
// controller has not reconciled the latest generation, or machines of the control plane or the
// workers are not up to date.
func clusterSettling(c *clusterv1.Cluster) bool {
	if cond := meta.FindStatusCondition(c.Status.Conditions, clusterv1.ClusterTopologyReconciledCondition); cond != nil && cond.ObservedGeneration != c.Generation {
		return true
	}
	behind := func(upToDate, desired *int32) bool { return upToDate != nil && desired != nil && *upToDate < *desired }
	if cp := c.Status.ControlPlane; cp != nil && behind(cp.UpToDateReplicas, cp.DesiredReplicas) {
		return true
	}
	w := c.Status.Workers
	return w != nil && behind(w.UpToDateReplicas, w.DesiredReplicas)
}

// SiteRollClusters reads the state of the live clusters of Kubehenge's classes for PlanSiteRoll.
// A cluster of another class or one being deleted is left out: the controller never gives it a
// site CA, so it would hold a place for good.
func SiteRollClusters(clusters []clusterv1.Cluster, classNamespace string) []SiteRollCluster {
	out := make([]SiteRollCluster, 0, len(clusters))
	for i := range clusters {
		c := &clusters[i]
		if !Owns(c, classNamespace) || !c.DeletionTimestamp.IsZero() {
			continue
		}
		policy, set, err := optionalVariable[UpgradePolicy](c, upgradesVariable)
		sc := SiteRollCluster{
			Key:      client.ObjectKeyFromObject(c),
			Inherits: InheritsSiteCA(c),
			Applied:  appliedSiteCA(c),
			Rolling:  meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterRollingOutCondition),
			Built:    clusterBuilt(c),
			Settling: clusterSettling(c),
			Paused:   c.Spec.Paused != nil && *c.Spec.Paused,
		}
		if err == nil && set && (policy.Mode == upgradePatch || policy.Mode == upgradeMinor || policy.RefreshesNodeImages()) {
			sc.Policy = &policy
		}
		out = append(out, sc)
	}
	return out
}

// WindowWait is a cluster that takes the site CA when its upgrade window opens.
type WindowWait struct {
	Key   client.ObjectKey
	Opens time.Time
}

// SiteRollPlan says which inheriting clusters take a site CA now, which wait for their upgrade
// window and which wait for a free place among the clusters rolling.
type SiteRollPlan struct {
	Now      []client.ObjectKey
	Windows  []WindowWait
	Waiting  []client.ObjectKey
	Rolling  int
	Inherits int
}

// windowOpens tells whether the cluster's upgrade window is closed now, and when it opens next.
// A cluster without a window is never closed.
func (c *SiteRollCluster) windowOpens(now time.Time) (time.Time, bool, error) {
	if c.Policy == nil {
		return time.Time{}, false, nil
	}
	open, err := c.Policy.Window.Open(now)
	if err != nil || open {
		return time.Time{}, false, wrapCluster(c.Key, err)
	}
	opens, err := c.Policy.Window.Next(now)
	return opens, err == nil, wrapCluster(c.Key, err)
}

func wrapCluster(key client.ObjectKey, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("cluster %s: %w", key, err)
}

// PlanSiteRoll plans the rollout of the site CA want. A cluster with its own trustedCAs is left
// alone. A cluster with an upgrade window rolls only inside it, and a paused one waits for its
// unpause. At most limit clusters roll at the same time, those already rolling or settling after
// being given a site CA counting.
func PlanSiteRoll(want string, clusters []SiteRollCluster, limit int, now time.Time) (SiteRollPlan, error) {
	var plan SiteRollPlan
	free := limit
	for _, c := range clusters {
		if c.Rolling || (c.Settling && c.Built) {
			free--
			plan.Rolling++
		}
	}
	ordered := slices.Clone(clusters)
	slices.SortFunc(ordered, func(a, b SiteRollCluster) int { return strings.Compare(a.Key.String(), b.Key.String()) })
	for _, c := range ordered {
		if !c.Inherits || !c.Built {
			continue
		}
		plan.Inherits++
		if c.Applied == want {
			continue
		}
		if c.Paused {
			plan.Waiting = append(plan.Waiting, c.Key)
			continue
		}
		opens, closed, err := c.windowOpens(now)
		if err != nil {
			return plan, err
		}
		if closed {
			plan.Windows = append(plan.Windows, WindowWait{Key: c.Key, Opens: opens})
			continue
		}
		if free <= 0 {
			plan.Waiting = append(plan.Waiting, c.Key)
			continue
		}
		free--
		plan.Now = append(plan.Now, c.Key)
	}
	return plan, nil
}

// ensureNodeSite keeps the cluster's node customisation revision and site CA current. It returns
// when to look again, 0 for the usual resync.
func (r *Reconciler) ensureNodeSite(ctx context.Context, cluster *clusterv1.Cluster) (time.Duration, error) {
	if err := r.ensureNodeCustomisation(ctx, cluster); err != nil {
		return 0, err
	}
	return r.rollSiteCA(ctx, cluster)
}

// rollSiteCA gives an inheriting cluster the site CA when the plan lets it. A cluster that sets
// trustedCAs of its own is kept current at once, as its nodes ignore the variable. It returns when to look again, 0 for the usual resync.
func (r *Reconciler) rollSiteCA(ctx context.Context, cluster *clusterv1.Cluster) (time.Duration, error) {
	key := client.ObjectKeyFromObject(cluster)
	if appliedSiteCA(cluster) == r.SiteCA {
		return 0, nil
	}
	if r.SiteCA != "" && r.SiteCAProblem != "" {
		if r.Events != nil {
			r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, "SiteCAInvalid", "RollSiteCA", "the site CA is not given to the nodes: %s", r.SiteCAProblem)
		}
		return 0, nil
	}
	if !InheritsSiteCA(cluster) {
		// The nodes of a cluster with trustedCAs of its own trust those alone, so keeping the variable
		// current replaces nothing and takes nothing from the pacing.
		return 0, r.setSiteCA(ctx, key, r.SiteCA)
	}
	if !clusterBuilt(cluster) {
		return 0, r.setSiteCA(ctx, key, r.SiteCA)
	}
	var all clusterv1.ClusterList
	if err := r.Client.List(ctx, &all); err != nil {
		return 0, fmt.Errorf("listing clusters for the site CA: %w", err)
	}
	now := r.now()
	plan, err := PlanSiteRoll(r.SiteCA, SiteRollClusters(all.Items, r.ClassNamespace), SiteRollLimit, now)
	if err != nil {
		return 0, err
	}
	if slices.Contains(plan.Now, key) {
		return 0, r.setSiteCA(ctx, key, r.SiteCA)
	}
	for _, w := range plan.Windows {
		if w.Key == key {
			return max(w.Opens.Sub(now), 0) + time.Second, nil
		}
	}
	return waitInterval, nil
}

// setSiteCA writes the site CA into the cluster's siteCAs variable, or takes the variable away
// when ca is empty.
func (r *Reconciler) setSiteCA(ctx context.Context, key client.ObjectKey, ca string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var live clusterv1.Cluster
		if err := r.Reader.Get(ctx, key, &live); err != nil {
			return client.IgnoreNotFound(err)
		}
		if appliedSiteCA(&live) == ca {
			return nil
		}
		live.Spec.Topology.Variables = slices.DeleteFunc(live.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool {
			return v.Name == SiteCAsVariable
		})
		if ca != "" {
			raw, err := json.Marshal([]string{ca})
			if err != nil {
				return fmt.Errorf("encoding the site CA: %w", err)
			}
			live.Spec.Topology.Variables = append(live.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: SiteCAsVariable, Value: apiextensionsv1.JSON{Raw: raw}})
		}
		return r.Client.Update(ctx, &live)
	})
	if err != nil {
		return fmt.Errorf("writing the site CA of %s: %w", key, err)
	}
	return nil
}
