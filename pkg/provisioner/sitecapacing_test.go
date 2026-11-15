package provisioner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
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

// markReconciled marks the cluster's topology as reconciled for its current generation.
func markReconciled(c *clusterv1.Cluster) {
	c.Generation = 1
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
		Type: clusterv1.ClusterTopologyReconciledCondition, Status: metav1.ConditionTrue, ObservedGeneration: 1,
	})
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
	objects := make([]client.Object, 0, 4*SiteRollLimit)
	for i := range 2 * SiteRollLimit {
		c := siteCluster(fmt.Sprintf("web-%d", i))
		markReconciled(&c)
		objects = append(objects, &c, workers(&c, rolloutState{Generation: 1, Observed: 1}))
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
	plan := PlanSiteRoll("new", SiteRollClusters(clusters, siteClassNamespace, nil), 1, siteNow)
	if !slices.Equal(plan.Now, keys("z-kubehenge")) || plan.Inherits != 1 {
		t.Errorf("plan = %+v, want the Kubehenge cluster to roll and no other cluster counted", plan)
	}
}

func TestPausedClusterWaitsForItsUnpauseAndTakesNoPlace(t *testing.T) {
	t.Parallel()
	paused := siteCluster("a-paused")
	paused.Spec.Paused = new(true)
	clusters := []clusterv1.Cluster{paused, siteCluster("b")}
	plan := PlanSiteRoll("new", SiteRollClusters(clusters, siteClassNamespace, nil), 1, siteNow)
	if !slices.Equal(plan.Now, keys("b")) || !slices.Equal(plan.Waiting, keys("a-paused")) {
		t.Errorf("plan = %+v, want b to roll and the paused cluster to wait", plan)
	}
}

func TestOwnTrustedCAsKeepTheSiteCAVariableCurrentOutsidePacingAndWindows(t *testing.T) {
	t.Parallel()
	own := siteCluster("own", jsonVar(trustedCAsVariable, `["x"]`), jsonVar(upgradesVariable, mondayWindow))
	objects := make([]client.Object, 0, 1+SiteRollLimit)
	objects = append(objects, &own)
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

// siteAuthority is a CA certificate valid from an hour before siteNow to an hour after.
func siteAuthority(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "site"},
		NotBefore: siteNow.Add(-time.Hour), NotAfter: siteNow.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
}

func TestSiteCAThatLapsesWhileTheControllerRunsIsReportedAndGivenToNoNode(t *testing.T) {
	t.Parallel()
	given, fresh := siteCluster("given"), siteCluster("fresh")
	authority := siteAuthority(t)
	raw, err := json.Marshal([]string{authority})
	if err != nil {
		t.Fatal(err)
	}
	given.Spec.Topology.Variables = append(given.Spec.Topology.Variables, jsonVar(SiteCAsVariable, string(raw)))
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&given, &fresh).Build()
	recorder := events.NewFakeRecorder(8)
	clock := siteNow
	r := &Reconciler{
		Client: kube, Reader: kube, ClassNamespace: siteClassNamespace, SiteCA: authority, Events: recorder,
		Now: func() time.Time { return clock },
	}
	// While the certificate holds, nothing is reported.
	if _, err = r.rollSiteCA(t.Context(), &given); err != nil || len(recorder.Events) != 0 {
		t.Fatalf("before it lapses: %v, %d events", err, len(recorder.Events))
	}
	// The controller keeps running past the lapse, and the next resync reports it on both.
	clock = siteNow.Add(2 * time.Hour)
	for _, c := range []*clusterv1.Cluster{&given, &fresh} {
		if _, err = r.rollSiteCA(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		select {
		case note := <-recorder.Events:
			if !strings.Contains(note, "SiteCAInvalid") || !strings.Contains(note, "expired") {
				t.Errorf("%s: event %q lacks SiteCAInvalid and the expiry", c.Name, note)
			}
		default:
			t.Errorf("%s: no event after the lapse", c.Name)
		}
	}
	var live clusterv1.Cluster
	if err = kube.Get(t.Context(), client.ObjectKeyFromObject(&fresh), &live); err != nil {
		t.Fatal(err)
	}
	if appliedSiteCA(&live) != "" {
		t.Error("a lapsed site CA was given to a cluster")
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
	if got, err := NormalizeCertificateBundle(crlf); got != want || err != nil {
		t.Errorf("normalised to %q (%v), want %q", got, err, want)
	}
	// Blocks joined on one line both stay, and a damaged block is refused rather than dropped.
	joined := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE----------BEGIN CERTIFICATE-----\nMIIC\n-----END CERTIFICATE-----"
	if got, err := NormalizeCertificateBundle(joined); err != nil || strings.Count(got, "BEGIN CERTIFICATE") != 2 {
		t.Errorf("joined blocks normalised to %q (%v), want both", got, err)
	}
	damaged := "----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n" + want
	if got, err := NormalizeCertificateBundle(damaged); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("a damaged block beside a good one normalised to %q (%v), want it refused on line 1", got, err)
	}
}
