package provisioner

import (
	"cmp"
	"context"
	"crypto/x509"
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
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
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

// siteCALapse says which certificate of the bundle has expired or is not yet valid at now, empty
// when none is. A certificate that does not parse is left to the checks that refuse it.
func siteCALapse(bundle string, now time.Time) string {
	rest := []byte(bundle)
	for n := 1; ; n++ {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return ""
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		switch {
		case err != nil:
		case now.After(cert.NotAfter):
			return fmt.Sprintf("has certificate %d (%s) expired on %s", n, cert.Subject.CommonName, cert.NotAfter.UTC().Format(time.DateOnly))
		case now.Before(cert.NotBefore):
			return fmt.Sprintf("has certificate %d (%s) valid only from %s", n, cert.Subject.CommonName, cert.NotBefore.UTC().Format(time.DateOnly))
		}
	}
}

// controllerAccount is the name of the controller's service account in the class namespace.
const controllerAccount = "kubehenge-controller"

// validateSiteCAsOwner refuses a change of the siteCAs variable from anyone but the controller. The
// variable is what the controller's pacing and windows decide, and a value written elsewhere would
// replace the nodes outside them with an authority no administrator chose.
func validateSiteCAsOwner(ctx context.Context, old, c *clusterv1.Cluster, classNamespace string) field.ErrorList {
	if old != nil && !variableChanged(old, c, SiteCAsVariable) {
		return nil
	}
	if _, set, err := optionalVariable[json.RawMessage](c, SiteCAsVariable); old == nil && err == nil && !set {
		return nil
	}
	if req, err := admission.RequestFromContext(ctx); err == nil && req.UserInfo.Username == "system:serviceaccount:"+classNamespace+":"+controllerAccount {
		return nil
	}
	return field.ErrorList{field.Forbidden(field.NewPath("spec", "topology", "variables").Key(SiteCAsVariable),
		"the controller sets this from the site CA, set the site CA instead")}
}

// validateUpgradeWindow refuses an upgrade window whose time zone or start the controller cannot
// read, which would otherwise keep the site CA from the cluster.
func validateUpgradeWindow(old, c *clusterv1.Cluster) field.ErrorList {
	if old != nil && !variableChanged(old, c, upgradesVariable) {
		return nil
	}
	policy, set, err := optionalVariable[UpgradePolicy](c, upgradesVariable)
	if err != nil || !set {
		return nil
	}
	path := field.NewPath("spec", "topology", "variables").Key(upgradesVariable).Child("window")
	if CheckTimeZone(cmp.Or(policy.Window.TimeZone, "UTC")) != nil {
		return field.ErrorList{field.Invalid(path.Child("timeZone"), policy.Window.TimeZone, "not an IANA time zone")}
	}
	if _, _, err = policy.Window.zoneAndStart(); err != nil {
		return field.ErrorList{field.Invalid(path.Child("start"), policy.Window.Start, err.Error())}
	}
	return nil
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
	// Settling is true while the control plane or a MachineDeployment has not taken up the site CA
	// the cluster was given, which Cluster API shows as rolling only some time later.
	Settling bool
	// Paused is true for a cluster Cluster API does not act on, which therefore replaces no node
	// and holds no place. It takes one again when it is unpaused.
	Paused bool
	// Built is true once the cluster's control plane is up. Before that it has no node to replace,
	// so the site CA is part of its first nodes and no pacing applies.
	Built bool
	// Policy is the cluster's upgrade policy, nil when it sets none.
	Policy *UpgradePolicy
}

// SiteRollClusters reads the state of the live clusters of Kubehenge's classes for PlanSiteRoll,
// with settling the clusters SiteRollSettling found settling. A cluster of another class or one
// being deleted is left out: the controller never gives it a site CA, so it would hold a place for
// good.
func SiteRollClusters(clusters []clusterv1.Cluster, classNamespace string, settling map[client.ObjectKey]bool) []SiteRollCluster {
	out := make([]SiteRollCluster, 0, len(clusters))
	for i := range clusters {
		c := &clusters[i]
		if !Owns(c, classNamespace) || !c.DeletionTimestamp.IsZero() {
			continue
		}
		policy, set, err := optionalVariable[UpgradePolicy](c, upgradesVariable)
		key := client.ObjectKeyFromObject(c)
		sc := SiteRollCluster{
			Key:      key,
			Inherits: InheritsSiteCA(c),
			Applied:  appliedSiteCA(c),
			Rolling:  meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterRollingOutCondition),
			Built:    clusterBuilt(c),
			Settling: settling[key],
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
	Now     []client.ObjectKey
	Windows []WindowWait
	Waiting []client.ObjectKey
	// Skipped are the clusters whose upgrade window cannot be read, with the reason. They take no
	// part in the rollout and hold no place.
	Skipped  map[client.ObjectKey]error
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
// being given a site CA counting. A cluster whose window cannot be read is skipped and reported,
// so that it stops no other cluster.
func PlanSiteRoll(want string, clusters []SiteRollCluster, limit int, now time.Time) SiteRollPlan {
	plan := SiteRollPlan{Skipped: map[client.ObjectKey]error{}}
	free := limit
	for _, c := range clusters {
		// A paused cluster replaces no node. It takes a place again once it is unpaused.
		if !c.Paused && (c.Rolling || (c.Settling && c.Built)) {
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
			plan.Skipped[c.Key] = err
			continue
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
	return plan
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
	// Read on every resync, so that a site CA that lapses while the controller runs is reported too.
	lapse := siteCALapse(r.SiteCA, r.now())
	if lapse != "" && r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, "SiteCAInvalid", "RollSiteCA", "the site CA %s", lapse)
	}
	if r.SiteTimeZoneProblem != "" {
		if err := r.reportSiteIssue(ctx, cluster, SiteTimeZoneCondition, SiteTimeZoneInvalidReason, r.SiteTimeZoneProblem); err != nil {
			return 0, err
		}
	} else if err := r.clearSiteIssue(ctx, cluster, SiteTimeZoneCondition); err != nil {
		return 0, err
	}
	if appliedSiteCA(cluster) == r.SiteCA || lapse != "" {
		return 0, r.clearSiteRollBlocked(ctx, cluster)
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
	settling := SiteRollSettling(ctx, r.Client, all.Items, now)
	plan := PlanSiteRoll(r.SiteCA, SiteRollClusters(all.Items, r.ClassNamespace, settling), SiteRollLimit, now)
	if why, skipped := plan.Skipped[key]; skipped {
		return 0, r.reportSiteRollBlocked(ctx, cluster, why)
	}
	if err := r.clearSiteRollBlocked(ctx, cluster); err != nil {
		return 0, err
	}
	if slices.Contains(plan.Now, key) {
		return 0, r.giveSiteCA(ctx, key, r.SiteCA)
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
	return r.writeSiteCA(ctx, key, ca, nil)
}

// giveSiteCA is setSiteCA for a cluster whose nodes the change replaces. It notes the generations
// of the control plane and the MachineDeployments the change starts from, for SiteRollSettling.
func (r *Reconciler) giveSiteCA(ctx context.Context, key client.ObjectKey, ca string) error {
	return r.writeSiteCA(ctx, key, ca, r.noteSiteRoll)
}

// noteSiteRoll sets the cluster's SiteRollAnnotation.
func (r *Reconciler) noteSiteRoll(ctx context.Context, c *clusterv1.Cluster) error {
	note, err := r.siteRollNote(ctx, c)
	if err != nil {
		return err
	}
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[SiteRollAnnotation] = note
	return nil
}

func (r *Reconciler) writeSiteCA(ctx context.Context, key client.ObjectKey, ca string, annotate func(context.Context, *clusterv1.Cluster) error) error {
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
		if annotate != nil {
			if err := annotate(ctx, &live); err != nil {
				return err
			}
		}
		return r.Client.Update(ctx, &live)
	})
	if err != nil {
		return fmt.Errorf("writing the site CA of %s: %w", key, err)
	}
	return nil
}
