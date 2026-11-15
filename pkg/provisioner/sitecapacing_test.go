package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// reconciledCluster marks the cluster's topology as reconciled for its current generation.
func reconciledCluster(c clusterv1.Cluster) clusterv1.Cluster {
	c.Generation = 1
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
		Type: clusterv1.ClusterTopologyReconciledCondition, Status: metav1.ConditionTrue, ObservedGeneration: 1,
	})
	return c
}

// apiServer makes a fake client count a generation for each update, as the API server does.
func apiServer(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			obj.SetGeneration(obj.GetGeneration() + 1)
			return c.Update(ctx, obj, opts...)
		},
	}).Build()
}

func TestSiteCAPlaceStaysTakenUntilTheRolloutIsDone(t *testing.T) {
	t.Parallel()
	objects := make([]client.Object, 0, 2*SiteRollLimit)
	for i := range 2 * SiteRollLimit {
		c := reconciledCluster(siteCluster(fmt.Sprintf("web-%d", i)))
		objects = append(objects, &c)
	}
	kube := apiServer(t, objects...)
	r := &Reconciler{Client: kube, Reader: kube, ClassNamespace: siteClassNamespace, SiteCA: siteCAPEM, Now: func() time.Time { return siteNow }}
	ctx := t.Context()
	given := 0
	// Each reconcile reads the clusters again, and Cluster API has not yet marked one rolling.
	for i := range 2 * SiteRollLimit {
		var live clusterv1.Cluster
		if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: fmt.Sprintf("web-%d", i)}, &live); err != nil {
			t.Fatal(err)
		}
		if _, err := r.rollSiteCA(ctx, &live); err != nil {
			t.Fatal(err)
		}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(&live), &live); err != nil {
			t.Fatal(err)
		}
		if appliedSiteCA(&live) != "" {
			given++
		}
	}
	if given != SiteRollLimit {
		t.Errorf("%d clusters were given the site CA before any was marked rolling, want the limit %d", given, SiteRollLimit)
	}
}

func TestSiteCARollCountsOnlyLiveClustersOfKubehengesClasses(t *testing.T) {
	t.Parallel()
	foreign := siteCluster("a-foreign")
	foreign.Spec.Topology.ClassRef.Name = "other-class"
	deleting := siteCluster("b-deleting")
	deleting.DeletionTimestamp = &metav1.Time{Time: siteNow}
	clusters := []clusterv1.Cluster{foreign, deleting, siteCluster("z-kubehenge")}
	plan, err := PlanSiteRoll("new", SiteRollClusters(clusters, siteClassNamespace), 1, siteNow)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Now, keys("z-kubehenge")) || plan.Inherits != 1 {
		t.Errorf("plan = %+v, want the Kubehenge cluster to roll and no other cluster counted", plan)
	}
}

func TestPausedClusterWaitsForItsUnpauseAndTakesNoPlace(t *testing.T) {
	t.Parallel()
	paused := siteCluster("a-paused")
	paused.Spec.Paused = new(true)
	clusters := []clusterv1.Cluster{paused, siteCluster("b")}
	plan, err := PlanSiteRoll("new", SiteRollClusters(clusters, siteClassNamespace), 1, siteNow)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Now, keys("b")) || !slices.Equal(plan.Waiting, keys("a-paused")) {
		t.Errorf("plan = %+v, want b to roll and the paused cluster to wait", plan)
	}
}

func TestOwnTrustedCAsKeepTheSiteCAVariableCurrentOutsidePacingAndWindows(t *testing.T) {
	t.Parallel()
	own := siteCluster("own", jsonVar(trustedCAsVariable, `["x"]`), jsonVar(upgradesVariable, mondayWindow))
	objects := []client.Object{&own}
	for i := range SiteRollLimit {
		busy := siteCluster(fmt.Sprintf("busy-%d", i))
		busy.Status.Conditions = append(busy.Status.Conditions, metav1.Condition{Type: clusterv1.ClusterRollingOutCondition, Status: metav1.ConditionTrue})
		objects = append(objects, &busy)
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	r := &Reconciler{Client: kube, Reader: kube, ClassNamespace: siteClassNamespace, SiteCA: siteCAPEM, Now: func() time.Time { return siteNow }}
	var live clusterv1.Cluster
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(&own), &live); err != nil {
		t.Fatal(err)
	}
	after, err := r.rollSiteCA(t.Context(), &live)
	if err != nil {
		t.Fatal(err)
	}
	if err = kube.Get(t.Context(), client.ObjectKeyFromObject(&own), &live); err != nil {
		t.Fatal(err)
	}
	if appliedSiteCA(&live) == "" || after != 0 {
		t.Errorf("site CA %q, look again after %v, want the variable written at once", appliedSiteCA(&live), after)
	}
}

func TestExpiredSiteCAIsReportedOnTheClusterAndGivenToNoNode(t *testing.T) {
	t.Parallel()
	a := siteCluster("a")
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&a).Build()
	recorder := events.NewFakeRecorder(4)
	r := &Reconciler{
		Client: kube, Reader: kube, ClassNamespace: siteClassNamespace, SiteCA: siteCAPEM, Events: recorder,
		SiteCAProblem: "the authority certificate 1 (site) expired on 2026-11-15", Now: func() time.Time { return siteNow },
	}
	if _, err := r.rollSiteCA(t.Context(), &a); err != nil {
		t.Fatal(err)
	}
	var live clusterv1.Cluster
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(&a), &live); err != nil {
		t.Fatal(err)
	}
	if appliedSiteCA(&live) != "" {
		t.Error("an expired site CA was given to a cluster")
	}
	select {
	case note := <-recorder.Events:
		if !strings.Contains(note, "SiteCAInvalid") {
			t.Errorf("event %q lacks SiteCAInvalid", note)
		}
	default:
		t.Error("the cluster got no event")
	}
}

func TestReleaseMirrorAuthorityIsTheSiteCATheClusterWasGiven(t *testing.T) {
	t.Parallel()
	site := "NEW"
	r := &ReleaseChecksums{
		Mirror: func() ReleaseMirror { return ReleaseMirror{URL: "https://mirror.example.org/github"} },
		SiteCA: func() string { return site },
	}
	given := cluster(jsonVar(SiteCAsVariable, `["OLD"]`))
	m, err := r.mirrorFor(given)
	if err != nil || m.CABundle != "OLD" {
		t.Errorf("nodes verify the mirror with %q (%v), want the site CA the cluster was given", m.CABundle, err)
	}
	if m, err = r.mirrorFor(cluster(jsonVar(trustedCAsVariable, `["x"]`))); err != nil || m.CABundle != "" {
		t.Errorf("a cluster with its own trustedCAs got the mirror authority %q (%v), want none", m.CABundle, err)
	}
	if got := r.mirror().CABundle; got != "NEW" {
		t.Errorf("the controller verifies the mirror with %q, want the live site CA", got)
	}
}

func TestSiteCABundleIsWrittenAsTheChartTakesIt(t *testing.T) {
	t.Parallel()
	crlf := "Bag Attributes\r\nsubject=CN=site\r\n-----BEGIN CERTIFICATE-----\r\nMIIB\r\n-----END CERTIFICATE-----\r\n"
	want := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	if got := NormalizeCertificateBundle(crlf); got != want {
		t.Errorf("normalised to %q, want %q", got, want)
	}
}
