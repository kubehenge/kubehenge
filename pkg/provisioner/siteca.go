package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

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
var siteTimeZone = func() *atomic.Value {
	v := &atomic.Value{}
	v.Store(DefaultTimeZone)
	return v
}()

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
	siteTimeZone.Store(zone)
	return nil
}

// CheckCertificateBundle holds a PEM bundle to certificates alone, none of them expired or not yet
// valid.
func CheckCertificateBundle(bundle string, now time.Time) error {
	return checkCertificates(bundle, now)
}

// SiteCA is the site CA the nodes of a cluster that sets no trustedCAs of its own are given.
// Empty when none is.
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

// SiteRollCluster is a cluster as the site CA's rollout sees it.
type SiteRollCluster struct {
	Key client.ObjectKey
	// Inherits is true for a cluster without trustedCAs of its own.
	Inherits bool
	// Applied is the site CA its nodes were last given.
	Applied string
	// Rolling is true while the cluster replaces nodes.
	Rolling bool
	// Policy is the cluster's upgrade policy, nil when it sets none.
	Policy *UpgradePolicy
}

// SiteRollClusters reads the clusters' state for PlanSiteRoll.
func SiteRollClusters(clusters []clusterv1.Cluster) []SiteRollCluster {
	out := make([]SiteRollCluster, 0, len(clusters))
	for i := range clusters {
		c := &clusters[i]
		policy, set, err := optionalVariable[UpgradePolicy](c, upgradesVariable)
		sc := SiteRollCluster{
			Key:      client.ObjectKeyFromObject(c),
			Inherits: InheritsSiteCA(c),
			Applied:  appliedSiteCA(c),
			Rolling:  meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterRollingOutCondition),
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

// PlanSiteRoll plans the rollout of the site CA want. A cluster with its own trustedCAs is left
// alone. A cluster with an upgrade window rolls only inside it. At most limit clusters roll at the
// same time, those already rolling counting.
func PlanSiteRoll(want string, clusters []SiteRollCluster, limit int, now time.Time) (SiteRollPlan, error) {
	var plan SiteRollPlan
	free := limit
	for _, c := range clusters {
		if c.Rolling {
			free--
			plan.Rolling++
		}
	}
	ordered := slices.Clone(clusters)
	slices.SortFunc(ordered, func(a, b SiteRollCluster) int { return strings.Compare(a.Key.String(), b.Key.String()) })
	for _, c := range ordered {
		if !c.Inherits {
			continue
		}
		plan.Inherits++
		if c.Applied == want {
			continue
		}
		if c.Policy != nil {
			open, err := c.Policy.Window.Open(now)
			if err != nil {
				return plan, fmt.Errorf("cluster %s: %w", c.Key, err)
			}
			if !open {
				opens, err := c.Policy.Window.Next(now)
				if err != nil {
					return plan, fmt.Errorf("cluster %s: %w", c.Key, err)
				}
				plan.Windows = append(plan.Windows, WindowWait{Key: c.Key, Opens: opens})
				continue
			}
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

// rollSiteCA gives an inheriting cluster the site CA when the plan lets it, and takes a stale one
// off a cluster that sets trustedCAs of its own. It returns when to look again, 0 for the usual resync.
func (r *Reconciler) rollSiteCA(ctx context.Context, cluster *clusterv1.Cluster) (time.Duration, error) {
	key := client.ObjectKeyFromObject(cluster)
	if !InheritsSiteCA(cluster) {
		if appliedSiteCA(cluster) == "" {
			return 0, nil
		}
		return 0, r.setSiteCA(ctx, key, "")
	}
	if appliedSiteCA(cluster) == r.SiteCA {
		return 0, nil
	}
	var all clusterv1.ClusterList
	if err := r.Client.List(ctx, &all); err != nil {
		return 0, fmt.Errorf("listing clusters for the site CA: %w", err)
	}
	now := r.now()
	plan, err := PlanSiteRoll(r.SiteCA, SiteRollClusters(all.Items), SiteRollLimit, now)
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
