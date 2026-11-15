//go:build integration

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
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// integrationAuthority is a CA certificate valid for the day around now.
func integrationAuthority(t *testing.T, name string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
}

// asController is a client acting as the chart's controller service account, with the chart's RBAC.
func asController(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme) client.Client {
	t.Helper()
	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + classNamespace + ":" + controllerAccount,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + classNamespace, "system:authenticated"},
	}
	c, err := client.New(as, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestIntegrationSiteCARollRunsAsTheControllersAccount runs the site CA's pacing reads and the
// write that gives a cluster the site CA as the chart's controller service account, so that the
// chart grants what they need on every distribution's control plane.
func TestIntegrationSiteCARollRunsAsTheControllersAccount(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	api, _ := startClusterAPI(t, "testdata/clusterapi-values.yaml")
	ctx := context.Background()
	controller := asController(t, api.Config, api.Client.Scheme())
	authority := integrationAuthority(t, "site")
	for kind, class := range map[string]string{
		"KubeadmControlPlane": "kubehenge-kubeadm", "KThreesControlPlane": "kubehenge-k3s",
		"RKE2ControlPlane": "kubehenge-rke2", "TalosControlPlane": "kubehenge-talos",
	} {
		c := clusterOf(class, "paced", 3, nil)
		c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: "controlplane.cluster.x-k8s.io", Kind: kind, Name: "paced-cp"}
		if _, err := controlPlaneOf(ctx, controller, controller.RESTMapper(), c); err != nil {
			t.Errorf("%s: the controller cannot read the control plane: %v", kind, err)
		}
		// Given a minute ago, a cluster whose control plane is gone holds no place. One the controller
		// cannot read would.
		c.Annotations = map[string]string{SiteRollAnnotation: `{"at":"` + time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) + `","generations":{"ControlPlane":1}}`}
		if got := SiteRollSettling(ctx, controller, []clusterv1.Cluster{*c}, time.Now()); got[client.ObjectKeyFromObject(c)] {
			t.Errorf("%s: a cluster without a control plane is settling as the controller sees it", kind)
		}
	}
	stored := clusterOf("kubehenge-kubeadm", "given", 4, nil)
	stored.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: "controlplane.cluster.x-k8s.io", Kind: "KubeadmControlPlane", Name: "given-cp"}
	if err := api.Client.Create(ctx, stored); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: controller, Reader: controller, ClassNamespace: classNamespace, SiteCA: authority, Now: time.Now}
	if err := r.giveSiteCA(ctx, client.ObjectKeyFromObject(stored), authority); err != nil {
		t.Fatalf("giving the site CA as the controller: %v", err)
	}
	var live clusterv1.Cluster
	if err := api.Client.Get(ctx, client.ObjectKeyFromObject(stored), &live); err != nil {
		t.Fatal(err)
	}
	if appliedSiteCA(&live) != authority || live.Annotations[SiteRollAnnotation] == "" {
		t.Errorf("site CA given %v with note %q, want the authority and a rollout note", appliedSiteCA(&live) == authority, live.Annotations[SiteRollAnnotation])
	}
}

// TestIntegrationManifestsWithoutTheSiteCAsVariableApplyAndKeepIt holds the flow the docs show,
// a cluster applied from a manifest again and again, to keep working once the controller has given
// it the site CA, and a cluster created from an exported spec to get the current site CA.
func TestIntegrationManifestsWithoutTheSiteCAsVariableApplyAndKeepIt(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	api, hooks := startClusterAPI(t, "testdata/clusterapi-values.yaml")
	ctx := context.Background()
	site, next := integrationAuthority(t, "site"), integrationAuthority(t, "next")
	hooks.SiteCA = func() string { return site }
	admin := api.Client
	controller := asController(t, api.Config, admin.Scheme())
	window := func(start string) map[string]string {
		return map[string]string{"upgrades": `{"mode":"patch","window":{"weekdays":["Sat"],"start":"` + start + `","durationMinutes":240,"timeZone":"Europe/Berlin"}}`}
	}
	key := client.ObjectKey{Namespace: "team-a", Name: "prod"}
	stored := func() *clusterv1.Cluster {
		t.Helper()
		var c clusterv1.Cluster
		if err := admin.Get(ctx, key, &c); err != nil {
			t.Fatal(err)
		}
		return &c
	}
	manifest := func(start string) []byte {
		t.Helper()
		c := clusterOf("kubehenge-k3s", "prod", 5, window(start))
		c.APIVersion, c.Kind = clusterv1.GroupVersion.String(), "Cluster"
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	// kubectl apply's first run creates the cluster, and the defaulter gives it the site CA.
	created := clusterOf("kubehenge-k3s", "prod", 5, window("02:00"))
	if err := admin.Create(ctx, created); err != nil {
		t.Fatal(err)
	}
	if appliedSiteCA(stored()) != site {
		t.Error("a new cluster did not get the site CA")
	}
	// The controller's own write passes as it is.
	r := &Reconciler{Client: controller, Reader: controller, ClassNamespace: classNamespace, SiteCA: next, Now: time.Now}
	if err := r.setSiteCA(ctx, key, next); err != nil {
		t.Fatalf("the controller's write: %v", err)
	}
	if appliedSiteCA(stored()) != next {
		t.Fatal("the controller's write did not land")
	}
	// Client-side apply sends the whole variables list as a JSON merge patch, without siteCAs.
	if err := admin.Patch(ctx, stored(), client.RawPatch(types.MergePatchType, manifest("03:00"))); err != nil {
		t.Fatalf("applying the manifest again after moving the window: %v", err)
	}
	if got := stored(); appliedSiteCA(got) != next || !strings.Contains(storedVariable(got, "upgrades"), `"03:00"`) {
		t.Errorf("after apply: site CA kept %v, upgrades %s", appliedSiteCA(got) == next, storedVariable(got, "upgrades"))
	}
	// kubectl replace sends the whole object, here with a stale value.
	stale := stored()
	stale.Spec.Topology.Variables = slices.DeleteFunc(stale.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool {
		return v.Name == SiteCAsVariable || v.Name == "upgrades"
	})
	withAddons(stale, map[string]string{SiteCAsVariable: jsonOf(t, []string{site}), "upgrades": window("04:00")["upgrades"]})
	if err := admin.Update(ctx, stale); err != nil {
		t.Fatalf("replacing with a stale site CA: %v", err)
	}
	if got := stored(); appliedSiteCA(got) != next || !strings.Contains(storedVariable(got, "upgrades"), `"04:00"`) {
		t.Errorf("after replace: site CA kept %v, upgrades %s", appliedSiteCA(got) == next, storedVariable(got, "upgrades"))
	}
	// A cluster created again from an exported spec gets the site CA a new cluster takes now.
	restored := clusterOf("kubehenge-k3s", "restored", 6, map[string]string{SiteCAsVariable: jsonOf(t, []string{next})})
	if err := admin.Create(ctx, restored); err != nil {
		t.Fatalf("creating a cluster from a spec that holds siteCAs: %v", err)
	}
	var got clusterv1.Cluster
	if err := admin.Get(ctx, client.ObjectKeyFromObject(restored), &got); err != nil {
		t.Fatal(err)
	}
	if appliedSiteCA(&got) != site {
		t.Error("a cluster created from an exported spec kept the spec's site CA")
	}
}

// TestIntegrationTheRolloutNoteIsTheControllers holds the note that paces the site CA to the
// controller through the chart's webhook, also for updates that change no more than the note.
func TestIntegrationTheRolloutNoteIsTheControllers(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	api, hooks := startClusterAPI(t, "testdata/clusterapi-values.yaml")
	ctx := context.Background()
	site := integrationAuthority(t, "site")
	hooks.SiteCA = func() string { return site }
	admin, controller := api.Client, asController(t, api.Config, api.Client.Scheme())
	const planted = `{"at":"2099-01-01T00:00:00Z","generations":{"ControlPlane":999999}}`
	key := client.ObjectKey{Namespace: "team-a", Name: "prod"}
	note := func(c client.Client) string {
		t.Helper()
		var live clusterv1.Cluster
		if err := c.Get(ctx, key, &live); err != nil {
			t.Fatal(err)
		}
		return live.Annotations[SiteRollAnnotation]
	}
	patchNote := func(c client.Client, value string) error {
		var live clusterv1.Cluster
		if err := c.Get(ctx, key, &live); err != nil {
			t.Fatal(err)
		}
		return c.Patch(ctx, &live, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+SiteRollAnnotation+`":`+value+`}}}`)))
	}

	created := clusterOf("kubehenge-k3s", "prod", 5, nil)
	created.Annotations = map[string]string{SiteRollAnnotation: planted}
	if err := admin.Create(ctx, created); err != nil {
		t.Fatal(err)
	}
	if got := note(admin); got != "" {
		t.Errorf("a create kept the note %q it carried", got)
	}
	if err := patchNote(admin, jsonOf(t, planted)); err != nil {
		t.Fatalf("a user adding a note: %v", err)
	}
	if got := note(admin); got != "" {
		t.Errorf("a user added the note %q", got)
	}
	written := `{"at":"2026-11-18T12:00:00Z","generations":{"ControlPlane":1}}`
	if err := patchNote(controller, jsonOf(t, written)); err != nil {
		t.Fatalf("the controller writing its note: %v", err)
	}
	if got := note(admin); got != written {
		t.Fatalf("the controller's note = %q, want %q", got, written)
	}
	for name, value := range map[string]string{"dropping": "null", "changing": jsonOf(t, planted)} {
		if err := patchNote(admin, value); err != nil {
			t.Fatalf("a user %s the note: %v", name, err)
		}
		if got := note(admin); got != written {
			t.Errorf("after a user %s the note: %q, want the controller's kept", name, got)
		}
	}
	if err := patchNote(controller, "null"); err != nil {
		t.Fatalf("the controller dropping its note: %v", err)
	}
	if got := note(admin); got != "" {
		t.Errorf("the controller's removal left %q", got)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
