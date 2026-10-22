package provisioner

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// makefileValue is what the Makefile sets a variable to.
func makefileValue(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^` + name + ` := (\S+)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("the Makefile sets no %s", name)
	}
	return string(m[1])
}

func TestGatewayAPIIsThePinnedRelease(t *testing.T) {
	t.Parallel()
	tag, digest := makefileValue(t, "GATEWAY_API_TAG"), makefileValue(t, "GATEWAY_API_SHA256")
	raw, err := gatewayAPIFiles.ReadFile("gatewayapi/standard-install-" + tag + ".yaml")
	if err != nil {
		t.Fatalf("the controller carries no install file of %s: %v", tag, err)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != digest {
		t.Fatalf("the install file of %s is not the one the Makefile pins", tag)
	}
	g, err := LoadGatewayAPI()
	if err != nil {
		t.Fatal(err)
	}
	if g.Version != tag {
		t.Errorf("release %s, want %s", g.Version, tag)
	}
	kinds := map[string][]string{}
	for _, o := range g.objects {
		kinds[o.gvk.Kind] = append(kinds[o.gvk.Kind], o.name)
	}
	for _, crd := range []string{"gatewayclasses", "gateways", "httproutes", "grpcroutes", "referencegrants", "backendtlspolicies", "listenersets", "tlsroutes", "tcproutes", "udproutes"} {
		if !strings.Contains(strings.Join(kinds["CustomResourceDefinition"], " "), crd+".gateway.networking.k8s.io") {
			t.Errorf("no CRD %s among %v", crd, kinds["CustomResourceDefinition"])
		}
	}
	const policy = "safe-upgrades.gateway.networking.k8s.io"
	if len(kinds["ValidatingAdmissionPolicy"]) != 1 || kinds["ValidatingAdmissionPolicy"][0] != policy ||
		len(kinds["ValidatingAdmissionPolicyBinding"]) != 1 || kinds["ValidatingAdmissionPolicyBinding"][0] != policy {
		t.Errorf("the admission policy and its binding: %v", kinds)
	}
	// The policy comes after the CRDs: it is what refuses them, and needs none of them.
	if last := g.objects[len(g.objects)-1]; last.gvk.Kind != "ValidatingAdmissionPolicyBinding" {
		t.Errorf("the last object applied is a %s", last.gvk.Kind)
	}
}

// gatewayDoc is an object of a Gateway API install file of a release.
func gatewayDoc(apiVersion, kind, name, release string) string {
	return fmt.Sprintf("apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n  annotations:\n    gateway.networking.k8s.io/bundle-version: %s\n",
		apiVersion, kind, name, release)
}

func crdDoc(name, release string) string {
	return gatewayDoc("apiextensions.k8s.io/v1", "CustomResourceDefinition", name, release) +
		"spec:\n  group: gateway.networking.k8s.io\n  scope: Namespaced\n  names: {plural: " + strings.Split(name, ".")[0] + ", kind: Thing}\n  versions: []\n"
}

const (
	testGateways   = "gateways.gateway.networking.k8s.io"
	testHTTPRoutes = "httproutes.gateway.networking.k8s.io"
	testPolicy     = "safe-upgrades.gateway.networking.k8s.io"
)

// testGatewayAPI is a release of two CRDs and the admission policy.
func testGatewayAPI(t *testing.T) *GatewayAPI {
	t.Helper()
	const release = "v1.6.3"
	raw := "# a comment of its own\n---\n" + strings.Join([]string{
		crdDoc(testGateways, release),
		crdDoc(testHTTPRoutes, release),
		gatewayDoc("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy", testPolicy, release),
		gatewayDoc("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBinding", testPolicy, release),
	}, "---\n")
	g, err := parseGatewayAPI(release, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestParseGatewayAPIRefusesWhatItWouldNotApply(t *testing.T) {
	t.Parallel()
	for what, raw := range map[string]string{
		"an object of another release": crdDoc(testGateways, "v1.6.3") + "---\n" + crdDoc(testHTTPRoutes, "v1.6.2"),
		"an object naming no release":  crdDoc(testGateways, "v1.6.3") + "---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: x}\n",
		"another kind":                 crdDoc(testGateways, "v1.6.3") + "---\n" + gatewayDoc("v1", "ConfigMap", "x", "v1.6.3"),
		"no CRD":                       gatewayDoc("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy", testPolicy, "v1.6.3"),
	} {
		if _, err := parseGatewayAPI("v1.6.3", []byte(raw)); err == nil {
			t.Errorf("%s: no error", what)
		}
	}
	if _, err := parseGatewayAPI("latest", []byte(crdDoc(testGateways, "latest"))); err == nil {
		t.Error("a release that is no version: no error")
	}
}

// gatewayScheme is the workload clusters' scheme with the distributions' HelmCharts in it, which a
// fake client would otherwise add to the scheme it shares with every other test.
var gatewayScheme = func() *runtime.Scheme {
	s := newRemoteScheme()
	s.AddKnownTypeWithName(helmChartGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(helmChartGVK.GroupVersion().WithKind("HelmChartList"), &unstructured.UnstructuredList{})
	return s
}()

// gatewayWorkload is a workload cluster holding objs.
func gatewayWorkload(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(gatewayScheme).WithObjects(objs...).Build()
}

// helmChart is a distribution's HelmChart of a chart.
func helmChart(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(helmChartGVK)
	u.SetNamespace("kube-system")
	u.SetName(name)
	return u
}

// clusterPolicy is the Gateway API's admission policy and its binding as a workload cluster has
// them, of a release, with the annotations given.
func clusterPolicy(release string, annotations ...string) []client.Object {
	a := map[string]string{bundleVersionAnnotation: release}
	for i := 0; i+1 < len(annotations); i += 2 {
		a[annotations[i]] = annotations[i+1]
	}
	return []client.Object{
		&admissionregistrationv1.ValidatingAdmissionPolicy{Name: testPolicy, Annotations: a},
		&admissionregistrationv1.ValidatingAdmissionPolicyBinding{Name: testPolicy, Annotations: maps.Clone(a)},
	}
}

// policyRelease is the release the workload cluster's admission policy names.
func policyRelease(t *testing.T, workload client.Client) string {
	t.Helper()
	got := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if err := workload.Get(context.Background(), client.ObjectKey{Name: testPolicy}, got); err != nil {
		t.Fatal(err)
	}
	return got.Annotations[bundleVersionAnnotation]
}

// testManifest is a Gateway API release's objects as a Helm chart ships them.
func testManifest(release string) string {
	return "---\n# Source: chart/templates/crds.yaml\n" + crdDoc(testGateways, release) + "---\n" + crdDoc(testHTTPRoutes, release) +
		"---\n# Source: chart/templates/safe-upgrades.yaml\n" +
		gatewayDoc("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy", `"`+testPolicy+`"`, release) + "---\n" +
		gatewayDoc("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBinding", testPolicy, release)
}

// countingApplies is a workload cluster that counts the objects applied to it.
func countingApplies(workload client.WithWatch, applies *int) client.Client {
	return interceptor.NewClient(workload, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			*applies++
			return c.Apply(ctx, obj, opts...)
		},
	})
}

// clusterCRD is a CRD as a workload cluster has it, of a release, with the annotations given.
func clusterCRD(name, release string, annotations ...string) *apiextensionsv1.CustomResourceDefinition {
	a := map[string]string{bundleVersionAnnotation: release}
	for i := 0; i+1 < len(annotations); i += 2 {
		a[annotations[i]] = annotations[i+1]
	}
	return &apiextensionsv1.CustomResourceDefinition{Name: name, Annotations: a}
}

// releaseOf is the release a workload cluster's CRD names.
func releaseOf(t *testing.T, workload client.Client, name string) string {
	t.Helper()
	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := workload.Get(context.Background(), client.ObjectKey{Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got.Annotations[bundleVersionAnnotation]
}

var testClusterKey = types.NamespacedName{Namespace: "team-a", Name: "web"}

func TestGatewayAPIAppliesWhatIsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &GatewayAPIReconciler{Bundle: testGatewayAPI(t)}
	workload := gatewayWorkload(t)
	if _, applied, err := r.ensure(ctx, testClusterKey, workload, ""); err != nil || !applied {
		t.Fatalf("applied %v, %v", applied, err)
	}
	for _, name := range []string{testGateways, testHTTPRoutes} {
		if got := releaseOf(t, workload, name); got != "v1.6.3" {
			t.Errorf("%s of release %q", name, got)
		}
	}
	for _, gvk := range gatewayAPIKinds[1:] {
		o := &metav1.PartialObjectMetadata{}
		o.SetGroupVersionKind(gvk)
		if err := workload.Get(ctx, client.ObjectKey{Name: testPolicy}, o); err != nil {
			t.Errorf("the admission policy's %s: %v", gvk.Kind, err)
		}
	}
	oldest, applied, err := r.ensure(ctx, testClusterKey, workload, "")
	if err != nil || applied || oldest != "v1.6.3" {
		t.Errorf("once applied: oldest %q, applied %v, %v", oldest, applied, err)
	}
}

func TestGatewayAPINeverDowngrades(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &GatewayAPIReconciler{Bundle: testGatewayAPI(t)}
	workload := gatewayWorkload(t, clusterCRD(testGateways, "v1.7.0"), clusterCRD(testHTTPRoutes, "v1.6.3"))
	if _, _, err := r.ensure(ctx, testClusterKey, workload, ""); err != nil {
		t.Fatal(err)
	}
	if got := releaseOf(t, workload, testGateways); got != "v1.7.0" {
		t.Errorf("a newer CRD is now of release %s", got)
	}
}

// helmRevision is the Secret Helm keeps a release's revision in, holding a manifest.
func helmRevision(t *testing.T, release string, revision int, status, manifest string) *corev1.Secret {
	t.Helper()
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if err := json.NewEncoder(zw).Encode(map[string]any{"name": release, "version": revision, "manifest": manifest}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		Namespace: "kube-system", Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", release, revision),
		UID:    types.UID(fmt.Sprintf("%s-%d", release, revision)),
		Labels: map[string]string{"owner": "helm", "name": release, "status": status, "version": strconv.Itoa(revision)},
		Type:   "helm.sh/release.v1",
		Data:   map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(compressed.Bytes()))},
	}
}

func TestGatewayAPIStaysTheDistributionsWhileItShipsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &GatewayAPIReconciler{Bundle: testGatewayAPI(t)}
	helm := []string{helmReleaseName, "traefik-crd", helmReleaseNamespace, "kube-system"}
	traefikCRD := "---\n# Source: traefik-crd/templates/traefik.io_middlewares.yaml\n" + crdDoc("middlewares.traefik.io", "")
	shipping := helmRevision(t, "traefik-crd", 1, "superseded", crdDoc(testGateways, "v1.6.1")+"---\n"+crdDoc(testHTTPRoutes, "v1.6.1")+traefikCRD)
	workload := gatewayWorkload(t, clusterCRD(testGateways, "v1.6.1", helm...), clusterCRD(testHTTPRoutes, "v1.6.1", helm...), shipping)

	oldest, applied, err := r.ensure(ctx, testClusterKey, workload, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := releaseOf(t, workload, testGateways); got != "v1.6.1" || oldest != "v1.6.1" || !applied {
		t.Errorf("while the distribution ships the CRDs: they are of release %s, oldest %s, applied %v (the policy only)", got, oldest, applied)
	}

	// A distribution upgrade brings a chart without them, which Helm keeps in the cluster.
	if err = workload.Create(ctx, helmRevision(t, "traefik-crd", 2, "deployed", traefikCRD)); err != nil {
		t.Fatal(err)
	}
	if _, applied, err = r.ensure(ctx, testClusterKey, workload, ""); err != nil || !applied {
		t.Fatalf("once the distribution no longer ships them: applied %v, %v", applied, err)
	}
	for _, name := range []string{testGateways, testHTTPRoutes} {
		got := &metav1.PartialObjectMetadata{}
		got.SetGroupVersionKind(crdGVK)
		if err := workload.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatal(err)
		}
		if got.Annotations[bundleVersionAnnotation] != "v1.6.3" {
			t.Errorf("%s left at release %s once nobody ships it", name, got.Annotations[bundleVersionAnnotation])
		}
	}
}

func TestGatewayAPITakesOverWhatAnUninstalledReleaseLeft(t *testing.T) {
	t.Parallel()
	r := &GatewayAPIReconciler{Bundle: testGatewayAPI(t)}
	helm := []string{helmReleaseName, "gateway-crds", helmReleaseNamespace, "kube-system"}
	workload := gatewayWorkload(t,
		clusterCRD(testGateways, "v1.6.1", helm...), clusterCRD(testHTTPRoutes, "v1.6.1", helm...),
		helmRevision(t, "gateway-crds", 1, "uninstalled", crdDoc(testGateways, "v1.6.1")+"---\n"+crdDoc(testHTTPRoutes, "v1.6.1")),
	)
	if _, _, err := r.ensure(context.Background(), testClusterKey, workload, ""); err != nil {
		t.Fatal(err)
	}
	if got := releaseOf(t, workload, testHTTPRoutes); got != "v1.6.3" {
		t.Errorf("a CRD of an uninstalled release left at %s", got)
	}
}

// A Helm release holds whatever kinds its chart ships, the admission policy as well as the CRDs, and
// each stays the release's while it ships it.
func TestGatewayAPILeavesEveryKindToTheReleaseShippingIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &GatewayAPIReconciler{Bundle: testGatewayAPI(t)}
	helm := []string{helmReleaseName, "gateway-crds", helmReleaseNamespace, "kube-system"}
	workload := gatewayWorkload(t, append(clusterPolicy("v1.6.1", helm...), helmRevision(t, "gateway-crds", 1, "deployed", testManifest("v1.6.1")))...)
	if _, applied, err := r.ensure(ctx, testClusterKey, workload, ""); err != nil || !applied {
		t.Fatalf("applied %v, %v", applied, err)
	}
	if got := policyRelease(t, workload); got != "v1.6.1" {
		t.Errorf("the policy a Helm release ships is now of release %s", got)
	}
	if got := releaseOf(t, workload, testGateways); got != "v1.6.3" {
		t.Errorf("a CRD nobody had is of release %q", got)
	}
}

// While the distribution's own chart holds the Gateway API, an object of Kubehenge's release that
// the chart does not ship stays out, as it would mix a second owner's release into the chart's.
func TestGatewayAPIMixesNothingIntoTheDistributionsChart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &GatewayAPIReconciler{Bundle: testGatewayAPI(t)}
	helm := []string{helmReleaseName, "gateway-api-crd", helmReleaseNamespace, "kube-system"}
	workload := gatewayWorkload(t, clusterCRD(testGateways, "v1.6.1", helm...),
		helmRevision(t, "gateway-api-crd", 1, "deployed", crdDoc(testGateways, "v1.6.1")))
	oldest, applied, err := r.ensure(ctx, testClusterKey, workload, "kube-system/gateway-api-crd")
	if err != nil || applied || oldest != "v1.6.1" {
		t.Errorf("oldest %q, applied %v, %v", oldest, applied, err)
	}
	got := &metav1.PartialObjectMetadata{}
	got.SetGroupVersionKind(crdGVK)
	if err := workload.Get(ctx, client.ObjectKey{Name: testHTTPRoutes}, got); err == nil {
		t.Error("a CRD the distribution's chart does not ship was applied beside it")
	}
}

func TestHelmReleaseObjectsReadsAnUncompressedRelease(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(map[string]any{"manifest": crdDoc(testGateways, "v1.6.1") + "---\n" + gatewayDoc("v1", "ConfigMap", "x", "")})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := helmReleaseObjects([]byte(base64.StdEncoding.EncodeToString(raw)))
	want := []string{"CustomResourceDefinition.apiextensions.k8s.io/" + testGateways, "ConfigMap/x"}
	if err != nil || !slices.Equal(objects, want) {
		t.Errorf("objects %v, %v", objects, err)
	}
	if _, err := helmReleaseObjects([]byte("not base64!")); err == nil {
		t.Error("no error for a release that is not base64")
	}
}

func initialisedCluster(initialised bool) *clusterv1.Cluster {
	c := storageCluster()
	c.Status.Initialization.ControlPlaneInitialized = &initialised
	return c
}

func TestGatewayAPIReconcileRecordsTheRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := initialisedCluster(false)
	_, kube := newReconciler(t, &fakeCloud{}, c)
	workload := gatewayWorkload(t, helmRevision(t, "rke2-traefik-crd", 1, "deployed", ""))
	r := &GatewayAPIReconciler{
		Client: kube, ClassNamespace: "kubehenge-system", Bundle: testGatewayAPI(t),
		Remotes: &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}},
	}
	run := func() ctrl.Result {
		t.Helper()
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	recorded := func() string {
		t.Helper()
		got := &clusterv1.Cluster{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(c), got); err != nil {
			t.Fatal(err)
		}
		return got.Annotations[gatewayAPIAnnotation]
	}

	if result := run(); result.RequeueAfter != waitInterval {
		t.Errorf("before the control plane is up: requeued after %v", result.RequeueAfter)
	}
	got := &metav1.PartialObjectMetadata{}
	got.SetGroupVersionKind(crdGVK)
	if err := workload.Get(ctx, client.ObjectKey{Name: testGateways}, got); err == nil {
		t.Error("the CRDs applied before the control plane is up")
	}

	patch := client.MergeFrom(c.DeepCopy())
	c.Status.Initialization.ControlPlaneInitialized = new(true)
	if err := kube.Patch(ctx, c, patch); err != nil {
		t.Fatal(err)
	}
	if result := run(); result.RequeueAfter != waitInterval || recorded() != "" {
		t.Errorf("applying: requeued after %v, recorded %q", result.RequeueAfter, recorded())
	}
	if result := run(); result.RequeueAfter != gatewayAPIInterval || recorded() != "v1.6.3" {
		t.Errorf("applied: requeued after %v, recorded %q", result.RequeueAfter, recorded())
	}
}

// On RKE2 and k3s nothing is applied before the distribution's Traefik CRD chart is installed,
// which would refuse the CRDs Kubehenge applied first as not its own.
func TestGatewayAPIWaitsForTheDistributionsCRDChart(t *testing.T) {
	t.Parallel()
	for distribution, chart := range map[string]string{"k3s": "traefik-crd", "rke2": "rke2-traefik-crd"} {
		t.Run(distribution, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			c := initialisedCluster(true)
			c.Spec.Topology.ClassRef.Name = ClassPrefix + distribution
			_, kube := newReconciler(t, &fakeCloud{}, c)
			workload := gatewayWorkload(t)
			r := &GatewayAPIReconciler{
				Client: kube, ClassNamespace: "kubehenge-system", Bundle: testGatewayAPI(t),
				Remotes: &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}},
			}
			applied := func() bool {
				t.Helper()
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
					t.Fatal(err)
				}
				got := &metav1.PartialObjectMetadata{}
				got.SetGroupVersionKind(crdGVK)
				return workload.Get(ctx, client.ObjectKey{Name: testGateways}, got) == nil
			}

			if applied() {
				t.Error("applied before the distribution's CRD chart is installed")
			}
			if err := workload.Create(ctx, helmRevision(t, chart, 1, "pending-install", "")); err != nil {
				t.Fatal(err)
			}
			if applied() {
				t.Error("applied while the distribution's CRD chart installs")
			}
			deployed := helmRevision(t, chart, 1, "deployed", "")
			if err := workload.Update(ctx, deployed); err != nil {
				t.Fatal(err)
			}
			if !applied() {
				t.Error("nothing applied once the distribution's CRD chart is installed")
			}
		})
	}
}

// A distribution release with a Gateway API chart of its own holds all of the Gateway API in it,
// adopting the CRDs the Traefik CRD chart of the release before left behind and Kubehenge's policy,
// and keeps its own release of them, older than Kubehenge's or not. Kubehenge applies nothing while
// the chart installs or after, and records the release the chart holds.
func TestGatewayAPILeavesItToTheDistributionsOwnChart(t *testing.T) {
	t.Parallel()
	for distribution, charts := range map[string]distributionCharts{
		"k3s":  {traefikCRD: "traefik-crd", gatewayAPI: "gateway-api-crd"},
		"rke2": {traefikCRD: "rke2-traefik-crd", gatewayAPI: "rke2-gateway-api-crd"},
	} {
		t.Run(distribution, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			c := initialisedCluster(true)
			c.Spec.Topology.ClassRef.Name = ClassPrefix + distribution
			_, kube := newReconciler(t, &fakeCloud{}, c)
			left := []string{helmReleaseName, charts.traefikCRD, helmReleaseNamespace, "kube-system"}
			traefikCRD := "---\n# Source: traefik-crd/templates/traefik.io_middlewares.yaml\n" + crdDoc("middlewares.traefik.io", "")
			cluster := gatewayWorkload(t, append(clusterPolicy("v1.6.3"),
				clusterCRD(testGateways, "v1.6.1", left...), clusterCRD(testHTTPRoutes, "v1.6.1", left...),
				helmRevision(t, charts.traefikCRD, 1, "superseded", crdDoc(testGateways, "v1.6.1")+"---\n"+crdDoc(testHTTPRoutes, "v1.6.1")+traefikCRD),
				helmRevision(t, charts.traefikCRD, 2, "deployed", traefikCRD),
				helmChart(charts.gatewayAPI),
				helmRevision(t, charts.gatewayAPI, 1, "pending-install", ""),
			)...)
			applies := 0
			r := &GatewayAPIReconciler{
				Client: kube, ClassNamespace: "kubehenge-system", Bundle: testGatewayAPI(t),
				Remotes: &Remotes{byCluster: map[string]remote{remoteKey(c): {client: countingApplies(cluster, &applies), until: time.Now().Add(time.Hour)}}},
			}
			run := func() ctrl.Result {
				t.Helper()
				result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			recorded := func() string {
				t.Helper()
				got := &clusterv1.Cluster{}
				if err := kube.Get(ctx, client.ObjectKeyFromObject(c), got); err != nil {
					t.Fatal(err)
				}
				return got.Annotations[gatewayAPIAnnotation]
			}

			if result := run(); applies != 0 || result.RequeueAfter != waitInterval || recorded() != "" {
				t.Errorf("while the distribution's Gateway API chart installs: %d applied, requeued after %v, recorded %q", applies, result.RequeueAfter, recorded())
			}

			adopted := map[string]string{bundleVersionAnnotation: "v1.6.1", helmReleaseName: charts.gatewayAPI, helmReleaseNamespace: "kube-system"}
			for _, o := range []client.Object{
				&apiextensionsv1.CustomResourceDefinition{Name: testGateways}, &apiextensionsv1.CustomResourceDefinition{Name: testHTTPRoutes},
				&admissionregistrationv1.ValidatingAdmissionPolicy{Name: testPolicy}, &admissionregistrationv1.ValidatingAdmissionPolicyBinding{Name: testPolicy},
			} {
				if err := cluster.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
					t.Fatal(err)
				}
				o.SetAnnotations(maps.Clone(adopted))
				if err := cluster.Update(ctx, o); err != nil {
					t.Fatal(err)
				}
			}
			if err := cluster.Update(ctx, helmRevision(t, charts.gatewayAPI, 1, "deployed", testManifest("v1.6.1"))); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if result := run(); applies != 0 || result.RequeueAfter != gatewayAPIInterval || recorded() != "v1.6.1" {
					t.Errorf("once the distribution's Gateway API chart holds it: %d applied, requeued after %v, recorded %q", applies, result.RequeueAfter, recorded())
				}
			}
			if got, policy := releaseOf(t, cluster, testGateways), policyRelease(t, cluster); got != "v1.6.1" || policy != "v1.6.1" {
				t.Errorf("the chart's CRDs are of release %s, its policy of %s", got, policy)
			}
		})
	}
}

func TestTraefikWaitsForTheGatewayAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := cluster(
		clusterv1.ClusterVariable{Name: "storage", Value: apiextensionsv1.JSON{Raw: []byte(`"local-lvm"`)}},
		clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"cilium"`)}},
	)
	c.Spec.Topology.ClassRef.Name = "kubehenge-kubeadm"
	r, kube := newReconciler(t, &fakeCloud{}, c)
	traefik := func() bool {
		t.Helper()
		hcp := &unstructured.Unstructured{}
		hcp.SetGroupVersionKind(helmChartProxyGVK)
		return kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: traefikProxy(c)}, hcp) == nil
	}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
	if err != nil {
		t.Fatal(err)
	}
	if traefik() || result.RequeueAfter != waitInterval {
		t.Errorf("before the Gateway API: Traefik's proxy %v, requeued after %v", traefik(), result.RequeueAfter)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(c), c); err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(c.DeepCopy())
	c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	if err := kube.Patch(ctx, c, patch); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if !traefik() {
		t.Error("no Traefik once the cluster has the Gateway API")
	}
}
