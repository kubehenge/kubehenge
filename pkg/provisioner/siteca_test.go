package provisioner

import (
	"slices"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const siteCAPEM = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

const siteClassNamespace = "kubehenge-system"

// siteNow is a Wednesday noon.
var siteNow = time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)

func siteCluster(name string, variables ...clusterv1.ClusterVariable) clusterv1.Cluster {
	c := cluster(variables...)
	c.Name = name
	c.Status.Conditions = []metav1.Condition{{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue}}
	return *c
}

// unbuilt is a cluster whose control plane never came up.
func unbuilt(name string, variables ...clusterv1.ClusterVariable) clusterv1.Cluster {
	c := siteCluster(name, variables...)
	c.Status.Conditions = nil
	return c
}

func jsonVar(name, raw string) clusterv1.ClusterVariable {
	return clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(raw)}}
}

// mondayWindow is closed on a Wednesday.
const mondayWindow = `{"mode":"patch","window":{"weekdays":["Mon"],"start":"02:00","timeZone":"UTC"}}`

func keys(names ...string) []client.ObjectKey {
	out := make([]client.ObjectKey, 0, len(names))
	for _, n := range names {
		out = append(out, client.ObjectKey{Namespace: "team-a", Name: n})
	}
	return out
}

func TestSiteCARollsInheritingClustersWithinTheLimitAndTheirWindows(t *testing.T) {
	t.Parallel()
	clusters := []clusterv1.Cluster{
		siteCluster("a"),
		siteCluster("b", jsonVar(upgradesVariable, mondayWindow)),
		siteCluster("c"),
		siteCluster("own", jsonVar(trustedCAsVariable, `["x"]`)),
		siteCluster("done", jsonVar(SiteCAsVariable, `["new"]`)),
	}
	plan, err := PlanSiteRoll("new", SiteRollClusters(clusters, siteClassNamespace), 1, siteNow)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Now, keys("a")) {
		t.Errorf("rolling now = %v, want a alone, since the limit is 1", plan.Now)
	}
	if len(plan.Windows) != 1 || plan.Windows[0].Key.Name != "b" || !plan.Windows[0].Opens.After(siteNow) || plan.Windows[0].Opens.Weekday() != time.Monday {
		t.Errorf("windows = %v, want b on its Monday", plan.Windows)
	}
	if !slices.Equal(plan.Waiting, keys("c")) {
		t.Errorf("waiting for a place = %v, want c", plan.Waiting)
	}
	if plan.Inherits != 4 {
		t.Errorf("inheriting = %d, want 4 (the cluster with trustedCAs of its own is left alone)", plan.Inherits)
	}
}

func TestSiteCARollLeavesUnbuiltClustersOutOfThePacing(t *testing.T) {
	t.Parallel()
	clusters := []clusterv1.Cluster{unbuilt("new-1"), unbuilt("new-2", jsonVar(upgradesVariable, mondayWindow)), siteCluster("a")}
	plan, err := PlanSiteRoll("new", SiteRollClusters(clusters, siteClassNamespace), 1, siteNow)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Now, keys("a")) || len(plan.Waiting) != 0 || len(plan.Windows) != 0 {
		t.Errorf("plan = %+v, want a to roll alone and the unbuilt clusters outside the plan", plan)
	}
}

func TestSiteCARollCountsClustersAlreadyRolling(t *testing.T) {
	t.Parallel()
	busy := siteCluster("busy")
	busy.Status.Conditions = append(busy.Status.Conditions, metav1.Condition{Type: clusterv1.ClusterRollingOutCondition, Status: metav1.ConditionTrue})
	clusters := []clusterv1.Cluster{busy, siteCluster("a"), siteCluster("b")}
	plan, err := PlanSiteRoll("new", SiteRollClusters(clusters, siteClassNamespace), SiteRollLimit, siteNow)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Rolling != 1 || len(plan.Now) != SiteRollLimit-1 {
		t.Errorf("rolling = %d, now = %v, want 1 rolling and %d more", plan.Rolling, plan.Now, SiteRollLimit-1)
	}
}

func TestSiteCAUnsetLeavesClustersAlone(t *testing.T) {
	t.Parallel()
	plan, err := PlanSiteRoll("", SiteRollClusters([]clusterv1.Cluster{siteCluster("a"), siteCluster("b", jsonVar(upgradesVariable, mondayWindow))}, siteClassNamespace), 4, siteNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Now)+len(plan.Windows)+len(plan.Waiting) != 0 {
		t.Errorf("plan = %+v, want nothing to roll", plan)
	}
}

func TestNodeSiteSettingsAreCheckedWhenTheyChangeAfterCreation(t *testing.T) {
	t.Parallel()
	old := cluster()
	mirrors := func(raw string) *clusterv1.Cluster {
		return cluster(jsonVar(registryMirrorsVariable, raw))
	}
	now := time.Now()
	for name, tc := range map[string]struct {
		changed *clusterv1.Cluster
		valid   bool
	}{
		"a registry mirror":     {mirrors(`[{"registry":"docker.io","endpoints":["https://m.example.org"]}]`), true},
		"a bad registry mirror": {mirrors(`[{"registry":"docker.io","endpoints":["ftp://m.example.org"]}]`), false},
		"a release mirror":      {cluster(jsonVar(releaseMirrorVariable, `{"url":"https://r.example.org"}`)), true},
		"a bad release mirror":  {cluster(jsonVar(releaseMirrorVariable, `{"url":"r.example.org"}`)), false},
		"no certificate":        {cluster(jsonVar(trustedCAsVariable, `["junk"]`)), false},
	} {
		errs := validateNodeSite(old, tc.changed, now)
		if (len(errs) == 0) != tc.valid {
			t.Errorf("%s: %v, want valid = %t", name, errs, tc.valid)
		}
	}
	// A setting that stays as it was is not checked again, since a certificate expires on its own.
	stale := cluster(jsonVar(trustedCAsVariable, `["junk"]`))
	if errs := validateNodeSite(stale, stale.DeepCopy(), now); len(errs) != 0 {
		t.Errorf("an unchanged setting was checked again: %v", errs)
	}
}

// Not parallel: it sets the process's default time zone.
func TestSiteTimeZoneIsTheDefaultOfSchedules(t *testing.T) { //nolint:paralleltest // sets the process's default time zone
	t.Cleanup(func() { _ = SetDefaultTimeZone(DefaultTimeZone) })
	zoneOf := func(own string) (string, string) {
		b, s := Backups{TimeZone: own}, EtcdSnapshots{TimeZone: own}
		b.withDefaults()
		s.withDefaults()
		return b.TimeZone, s.TimeZone
	}
	if b, s := zoneOf(""); b != "UTC" || s != "UTC" {
		t.Errorf("unset: backups %s, snapshots %s, want UTC", b, s)
	}
	if err := SetDefaultTimeZone("Europe/Zurich"); err != nil {
		t.Fatal(err)
	}
	if b, s := zoneOf(""); b != "Europe/Zurich" || s != "Europe/Zurich" {
		t.Errorf("set: backups %s, snapshots %s, want Europe/Zurich", b, s)
	}
	if b, s := zoneOf("Asia/Tokyo"); b != "Asia/Tokyo" || s != "Asia/Tokyo" {
		t.Errorf("a cluster's own: backups %s, snapshots %s, want Asia/Tokyo", b, s)
	}
	for _, bad := range []string{"", "Local", "Mars/Olympus"} {
		if err := SetDefaultTimeZone(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestSiteCAIsWrittenOnlyWhereThePlanLetsIt(t *testing.T) {
	t.Parallel()
	a, own, windowed := siteCluster("a"), siteCluster("own", jsonVar(trustedCAsVariable, `["x"]`), jsonVar(SiteCAsVariable, `["old"]`)), siteCluster("windowed", jsonVar(upgradesVariable, mondayWindow))
	// A cluster not built yet has no node to replace, so neither its closed window nor the limit holds it.
	fresh := unbuilt("fresh", jsonVar(upgradesVariable, mondayWindow))
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&a, &own, &windowed, &fresh).Build()
	r := &Reconciler{Client: kube, Reader: kube, ClassNamespace: siteClassNamespace, SiteCA: siteCAPEM, Now: func() time.Time { return siteNow }}
	ctx := t.Context()
	for _, tc := range []struct {
		name      string
		wantCA    bool
		wantAfter bool
	}{{"a", true, false}, {"own", true, false}, {"windowed", false, true}, {"fresh", true, false}} {
		var live clusterv1.Cluster
		if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: tc.name}, &live); err != nil {
			t.Fatal(err)
		}
		after, err := r.rollSiteCA(ctx, &live)
		if err != nil {
			t.Fatal(err)
		}
		if err = kube.Get(ctx, client.ObjectKeyFromObject(&live), &live); err != nil {
			t.Fatal(err)
		}
		if got := appliedSiteCA(&live) != ""; got != tc.wantCA {
			t.Errorf("%s: has the site CA = %t, want %t", tc.name, got, tc.wantCA)
		}
		if (after > 0) != tc.wantAfter {
			t.Errorf("%s: looks again after %v, want a wait = %t", tc.name, after, tc.wantAfter)
		}
	}
}
