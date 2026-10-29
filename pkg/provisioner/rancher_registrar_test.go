package provisioner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// ranchersProxy is where Rancher's cluster serves Rancher's own API through its API server.
const ranchersProxy = "/api/v1/namespaces/cattle-system/services/https:rancher:443/proxy"

// recordedApply is one server-side apply a fakeKubeAPI received.
type recordedApply struct {
	path, contentType, fieldManager, force string
	object                                 map[string]any
}

// fakeKubeAPI is as much of a cluster's API server as the registrar uses: discovery of the kinds of
// Rancher's agent, server-side apply, which it records, and the proxy to Rancher's service, which
// answers with the agent's manifest.
type fakeKubeAPI struct {
	manifest string
	// proxyStatus and applyStatus, when set, are the answers to every proxy request and apply.
	proxyStatus, applyStatus int

	mu      sync.Mutex
	proxied []string
	applied []recordedApply
}

var fakeKubeResources = map[string]metav1.APIResourceList{
	"/api/v1": {Kind: "APIResourceList", GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "namespaces", Kind: "Namespace", Verbs: []string{"get", "patch"}},
		{Name: "serviceaccounts", Kind: "ServiceAccount", Namespaced: true, Verbs: []string{"get", "patch"}},
		{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: []string{"get", "patch"}},
	}},
	"/apis/apps/v1": {Kind: "APIResourceList", GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
		{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: []string{"get", "patch"}},
	}},
}

// writeJSON answers a request with v.
func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (f *fakeKubeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	answer := func(v any) { writeJSON(w, http.StatusOK, v) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api":
		answer(metav1.APIVersions{Kind: "APIVersions", Versions: []string{"v1"}})
	case r.Method == http.MethodGet && r.URL.Path == "/apis":
		apps := metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"}
		answer(metav1.APIGroupList{Kind: "APIGroupList", APIVersion: "v1", Groups: []metav1.APIGroup{
			{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{apps}, PreferredVersion: apps},
		}})
	case r.Method == http.MethodGet && fakeKubeResources[r.URL.Path].GroupVersion != "":
		answer(fakeKubeResources[r.URL.Path])
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, ranchersProxy+"/"):
		f.mu.Lock()
		f.proxied = append(f.proxied, strings.TrimPrefix(r.URL.Path, ranchersProxy))
		f.mu.Unlock()
		if f.proxyStatus != 0 {
			http.Error(w, "Rancher is restarting", f.proxyStatus)
			return
		}
		_, _ = io.WriteString(w, f.manifest)
	case r.Method == http.MethodPatch:
		body, err := io.ReadAll(r.Body)
		object := map[string]any{}
		if err == nil {
			err = json.Unmarshal(body, &object)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.applied = append(f.applied, recordedApply{
			path: r.URL.Path, contentType: r.Header.Get("Content-Type"),
			fieldManager: r.URL.Query().Get("fieldManager"), force: r.URL.Query().Get("force"), object: object,
		})
		f.mu.Unlock()
		if f.applyStatus != 0 {
			http.Error(w, "admission webhook refused it", f.applyStatus)
			return
		}
		answer(object)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeKubeAPI) proxiedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.proxied)
}

func (f *fakeKubeAPI) applies() []recordedApply {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.applied)
}

// serveKubeAPI serves a fake API server over TLS on loopback, and returns a rest.Config that
// reaches and trusts it.
func serveKubeAPI(t *testing.T, h http.Handler) *rest.Config {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return &rest.Config{Host: srv.URL, CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})}
}

// kubeconfigSecret is the kubeconfig Cluster API keeps for a cluster, for the API server at config.
func kubeconfigSecret(c *clusterv1.Cluster, config *rest.Config) *corev1.Secret {
	kubeconfig := "apiVersion: v1\nkind: Config\ncurrent-context: admin\n" +
		"clusters:\n- name: c\n  cluster:\n    server: " + config.Host + "\n    certificate-authority-data: " +
		base64.StdEncoding.EncodeToString(config.CAData) + "\ncontexts:\n- name: admin\n  context: {cluster: c, user: admin}\n" +
		"users:\n- name: admin\n  user: {token: admin-token}\n"
	return &corev1.Secret{
		Namespace: c.Namespace, Name: c.Name + "-kubeconfig",
		Data: map[string][]byte{"value": []byte(kubeconfig)},
	}
}

const testAgentManifest = `---
apiVersion: v1
kind: Namespace
metadata:
  name: cattle-system
---
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cattle
  namespace: cattle-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cattle-cluster-agent
  namespace: cattle-system
spec:
  selector:
    matchLabels: {app: cattle-cluster-agent}
  template:
    metadata:
      labels: {app: cattle-cluster-agent}
    spec:
      serviceAccountName: cattle
      containers:
      - name: cluster-register
        image: rancher/rancher-agent:v2.14.1
`

// rancherFixture is a management cluster with Rancher installed into the cluster "rancher", and
// the cluster "web" to register with it.
type rancherFixture struct {
	reg           *Registrar
	kube          client.Client
	ranchers      client.Client
	ranchersAPI   *fakeKubeAPI
	workloadAPI   *fakeKubeAPI
	rancher, web  *clusterv1.Cluster
	internalName  string
	registeredKey client.ObjectKey
}

func availableNow(c *clusterv1.Cluster) *clusterv1.Cluster {
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: clusterv1.AvailableCondition, Status: metav1.ConditionTrue, Reason: "Available"})
	return c
}

// ranchersScheme holds the kinds Rancher serves that the registrar reads and writes.
func ranchersScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{provisioningClusterGVK, registrationTokenGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		list := gvk
		list.Kind += "List"
		scheme.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
		metav1.AddToGroupVersion(scheme, gvk.GroupVersion())
	}
	return scheme
}

// newRancherFixture builds the fixture around web; funcs, if any, stand between the registrar and
// Rancher's cluster.
func newRancherFixture(t *testing.T, web *clusterv1.Cluster, funcs *interceptor.Funcs, inRancher ...client.Object) *rancherFixture {
	t.Helper()
	if funcs == nil {
		funcs = &interceptor.Funcs{}
	}
	rancher := availableNow(clusterWithRancher("rancher", true))
	rancher.UID = "6c1e1a52-0d0e-4bd5-9a4c-5a3e9e6f0b11"
	f := &rancherFixture{
		ranchersAPI:  &fakeKubeAPI{manifest: testAgentManifest},
		workloadAPI:  &fakeKubeAPI{},
		rancher:      rancher,
		web:          web,
		internalName: "c-m-4x7k2",
	}
	f.registeredKey = client.ObjectKey{Namespace: rancherWorkspace, Name: rancherName(web)}
	workload := serveKubeAPI(t, f.workloadAPI)
	_, f.kube = newReconciler(t, &fakeCloud{}, rancher, web, kubeconfigSecret(web, workload))
	f.ranchers = interceptor.NewClient(fake.NewClientBuilder().WithScheme(ranchersScheme(t)).WithObjects(inRancher...).Build(), *funcs)
	f.reg = &Registrar{Client: f.kube, ClassNamespace: "kubehenge-system", Remotes: &Remotes{
		Reader: f.kube,
		byCluster: map[string]remote{remoteKey(rancher): {
			config: serveKubeAPI(t, f.ranchersAPI), client: f.ranchers, until: time.Now().Add(time.Hour),
		}},
	}}
	return f
}

func (f *rancherFixture) reconcile(t *testing.T, c *clusterv1.Cluster) (ctrl.Result, error) {
	t.Helper()
	return f.reg.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
}

func (f *rancherFixture) mustReconcile(t *testing.T, c *clusterv1.Cluster) ctrl.Result {
	t.Helper()
	res, err := f.reconcile(t, c)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// holdsRanchersClient tells whether the registrar still keeps its client to Rancher's cluster.
func (f *rancherFixture) holdsRanchersClient() bool {
	f.reg.Remotes.mu.Lock()
	defer f.reg.Remotes.mu.Unlock()
	_, ok := f.reg.Remotes.byCluster[remoteKey(f.rancher)]
	return ok
}

func (f *rancherFixture) registered(t *testing.T) []unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(provisioningClusterGVK.GroupVersion().WithKind("ClusterList"))
	if err := f.ranchers.List(context.Background(), list, client.InNamespace(rancherWorkspace)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// rancherSets sets a field of the status Rancher keeps on its object for web.
func (f *rancherFixture) rancherSets(t *testing.T, value any, fields ...string) {
	t.Helper()
	rc := &unstructured.Unstructured{}
	rc.SetGroupVersionKind(provisioningClusterGVK)
	if err := f.ranchers.Get(context.Background(), f.registeredKey, rc); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(rc.Object, value, append([]string{"status"}, fields...)...); err != nil {
		t.Fatal(err)
	}
	if err := f.ranchers.Update(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
}

// rancherMakesToken has Rancher make the registration token of web, with the manifest URL Rancher
// gives once its server-url is set.
func (f *rancherFixture) rancherMakesToken(t *testing.T, manifestURL string) {
	t.Helper()
	token := &unstructured.Unstructured{}
	token.SetGroupVersionKind(registrationTokenGVK)
	token.SetNamespace(f.internalName)
	token.SetName("default-token")
	token.Object["status"] = map[string]any{"manifestUrl": manifestURL, "tokenSecretName": "crt-x9q2m"} //nolint:gosec // the name of the token's Secret, not a credential
	if err := f.ranchers.Create(context.Background(), token); err != nil {
		t.Fatal(err)
	}
}

// rancherHandsOutTheManifest has Rancher name its cluster for web, make its token and store it.
func (f *rancherFixture) rancherHandsOutTheManifest(t *testing.T) {
	t.Helper()
	f.rancherSets(t, f.internalName, "clusterName")
	f.rancherMakesToken(t, "https://rancher.example.org/v3/import/{token}_"+f.internalName+".yaml")
	f.rancherStoresToken(t)
}

// rancherStoresToken has Rancher store the registration token's Secret.
func (f *rancherFixture) rancherStoresToken(t *testing.T) {
	t.Helper()
	secret := &corev1.Secret{Namespace: f.internalName, Name: "crt-x9q2m", Data: map[string][]byte{"token": []byte("w8k2tq5zx")}}
	if err := f.ranchers.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrarRegistersOnceAndInstallsRanchersAgent(t *testing.T) {
	t.Parallel()
	f := newRancherFixture(t, availableNow(clusterWithRancher("web", false)), nil)

	if res := f.mustReconcile(t, f.web); res.RequeueAfter != registerPoll {
		t.Errorf("after registering: %+v", res)
	}
	got := f.registered(t)
	if len(got) != 1 || got[0].GetName() != f.registeredKey.Name || got[0].GetAnnotations()[rancherClusterAnnotation] != "team-a/web" {
		t.Fatalf("in Rancher: %v", got)
	}
	if spec, _, _ := unstructured.NestedMap(got[0].Object, "spec"); len(spec) != 0 {
		t.Errorf("Rancher would provision rather than import a cluster of spec %v", spec)
	}

	// Until Rancher has filled in its cluster, its token and the token's Secret, there is nothing
	// to install.
	if res := f.mustReconcile(t, f.web); res.RequeueAfter != registerPoll {
		t.Errorf("before Rancher names its cluster: %+v", res)
	}
	if got := f.registered(t); len(got) != 1 {
		t.Errorf("registered again: %d clusters in Rancher", len(got))
	}
	f.rancherSets(t, f.internalName, "clusterName")
	if res := f.mustReconcile(t, f.web); res.RequeueAfter != waitInterval {
		t.Errorf("before Rancher makes its token: %+v", res)
	}
	f.rancherMakesToken(t, "https://rancher.example.org/v3/import/{token}_"+f.internalName+".yaml")
	if res := f.mustReconcile(t, f.web); res.RequeueAfter != waitInterval {
		t.Errorf("before Rancher stores its token: %+v", res)
	}
	if paths := f.ranchersAPI.proxiedPaths(); len(paths) != 0 {
		t.Errorf("asked Rancher for the manifest before it could answer: %v", paths)
	}
	f.rancherStoresToken(t)

	if res := f.mustReconcile(t, f.web); res.RequeueAfter != agentStart {
		t.Errorf("after installing the agent: %+v", res)
	}
	if paths := f.ranchersAPI.proxiedPaths(); !slices.Equal(paths, []string{"/v3/import/w8k2tq5zx_" + f.internalName + ".yaml"}) {
		t.Errorf("the manifest was asked for at %v", paths)
	}
	applied := f.workloadAPI.applies()
	paths := make([]string, 0, len(applied))
	for _, a := range applied {
		paths = append(paths, a.path)
		if a.contentType != "application/apply-patch+yaml" || a.fieldManager != fieldOwner || a.force != "true" {
			t.Errorf("%s was applied as %q by %q, force %q", a.path, a.contentType, a.fieldManager, a.force)
		}
	}
	want := []string{
		"/api/v1/namespaces/cattle-system",
		"/api/v1/namespaces/cattle-system/serviceaccounts/cattle",
		"/apis/apps/v1/namespaces/cattle-system/deployments/cattle-cluster-agent",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("applied %v, want %v", paths, want)
	}
	if len(applied) == 3 {
		if image := lookup[string](t, applied[2].object, "spec", "template", "spec", "containers", 0, "image"); image != "rancher/rancher-agent:v2.14.1" {
			t.Errorf("the agent's Deployment runs %s", image)
		}
	}

	// Once Rancher's agent has connected, the registration is only looked at now and then.
	f.rancherSets(t, true, "ready")
	if res := f.mustReconcile(t, f.web); res.RequeueAfter != resync {
		t.Errorf("once ready: %+v", res)
	}
	if n := len(f.workloadAPI.applies()); n != 3 {
		t.Errorf("the agent was applied again once ready: %d applies", n)
	}
	if got := f.registered(t); len(got) != 1 {
		t.Errorf("%d clusters in Rancher", len(got))
	}
}

// Rancher makes no manifest URL while its server-url setting is empty; the registrar waits rather
// than failing.
func TestRegistrarWaitsForRanchersManifestURL(t *testing.T) {
	t.Parallel()
	f := newRancherFixture(t, availableNow(clusterWithRancher("web", false)), nil)
	f.mustReconcile(t, f.web)
	f.rancherSets(t, f.internalName, "clusterName")
	f.rancherMakesToken(t, "")
	if res := f.mustReconcile(t, f.web); res.RequeueAfter != waitInterval {
		t.Errorf("without a manifest URL: %+v", res)
	}
	if paths := f.ranchersAPI.proxiedPaths(); len(paths) != 0 {
		t.Errorf("asked Rancher at %v", paths)
	}
}

// A cluster whose kubeconfig Cluster API has not written yet gets the agent once it has.
func TestRegistrarWaitsForTheClustersKubeconfig(t *testing.T) {
	t.Parallel()
	f := newRancherFixture(t, availableNow(clusterWithRancher("web", false)), nil)
	if err := f.kube.Delete(context.Background(), &corev1.Secret{Namespace: "team-a", Name: "web-kubeconfig"}); err != nil {
		t.Fatal(err)
	}
	f.mustReconcile(t, f.web)
	f.rancherHandsOutTheManifest(t)
	if res := f.mustReconcile(t, f.web); res.RequeueAfter != waitInterval {
		t.Errorf("without a kubeconfig: %+v", res)
	}
	if applied := f.workloadAPI.applies(); len(applied) != 0 {
		t.Errorf("applied %v", applied)
	}
}

func optingOut(name string) *clusterv1.Cluster {
	c := clusterWithRancher(name, false)
	for i := range c.Spec.Topology.Variables {
		if c.Spec.Topology.Variables[i].Name == "rancher" {
			c.Spec.Topology.Variables[i].Value = apiextensionsv1.JSON{Raw: []byte(`{"install":false,"register":false}`)}
		}
	}
	return c
}

// A cluster that opts out leaves Rancher, available or not, and the registrar looks again until
// Rancher has let go of it.
func TestRegistrarRemovesAClusterThatOptsOut(t *testing.T) {
	t.Parallel()
	web := optingOut("web")
	f := newRancherFixture(t, web, nil, registered(rancherName(web), "team-a/web"), registered("by-hand", ""))
	res := f.mustReconcile(t, f.web)
	if res.RequeueAfter != waitInterval {
		t.Errorf("while Rancher lets go: %+v", res)
	}
	got := f.registered(t)
	if len(got) != 1 || got[0].GetName() != "by-hand" {
		t.Errorf("left in Rancher: %v", got)
	}
	if res := f.mustReconcile(t, f.web); !res.IsZero() {
		t.Errorf("once Rancher let go: %+v", res)
	}
	if applied := f.workloadAPI.applies(); len(applied) != 0 {
		t.Errorf("an opted-out cluster got %v", applied)
	}
}

// A failure in Rancher's cluster comes back as the reconcile's error, which controller-runtime
// retries, and the registrar reads the kubeconfig of Rancher's cluster afresh on that retry.
func TestRegistrarFailsOnRanchersErrorsAndReconnects(t *testing.T) {
	t.Parallel()
	refuse := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if key.Namespace == rancherWorkspace {
			return apierrors.NewServiceUnavailable("Rancher is restarting")
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	f := newRancherFixture(t, availableNow(clusterWithRancher("web", false)), refuse)
	_, err := f.reconcile(t, f.web)
	if !apierrors.IsServiceUnavailable(err) || !strings.Contains(err.Error(), "reading cluster "+f.registeredKey.String()+" in Rancher") {
		t.Errorf("Rancher refusing: %v", err)
	}
	if f.holdsRanchersClient() {
		t.Error("kept the client to Rancher's cluster after it failed")
	}

	f = newRancherFixture(t, optingOut("web"), refuse)
	res, err := f.reconcile(t, f.web)
	if !apierrors.IsServiceUnavailable(err) || res.RequeueAfter != waitInterval {
		t.Errorf("Rancher refusing an opt-out: %+v, %v", res, err)
	}
	if f.holdsRanchersClient() {
		t.Error("kept the client to Rancher's cluster after an opt-out failed")
	}
}

func TestRegistrarFailsWhenRancherCannotHandOutTheManifest(t *testing.T) {
	t.Parallel()
	f := newRancherFixture(t, availableNow(clusterWithRancher("web", false)), nil)
	f.ranchersAPI.proxyStatus = http.StatusServiceUnavailable
	f.mustReconcile(t, f.web)
	f.rancherHandsOutTheManifest(t)
	if _, err := f.reconcile(t, f.web); err == nil || !strings.Contains(err.Error(), "fetching Rancher's agent manifest") {
		t.Errorf("Rancher failing to answer: %v", err)
	}
	if applied := f.workloadAPI.applies(); len(applied) != 0 {
		t.Errorf("applied %v without a manifest", applied)
	}
	if f.holdsRanchersClient() {
		t.Error("kept the client to Rancher's cluster after it failed")
	}
}

func TestRegistrarFailsWhenTheClusterRefusesTheAgent(t *testing.T) {
	t.Parallel()
	f := newRancherFixture(t, availableNow(clusterWithRancher("web", false)), nil)
	f.workloadAPI.applyStatus = http.StatusForbidden
	f.mustReconcile(t, f.web)
	f.rancherHandsOutTheManifest(t)
	_, err := f.reconcile(t, f.web)
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "installing Rancher's agent: Namespace cattle-system") {
		t.Errorf("the cluster refusing the agent: %v", err)
	}
	if n := len(f.workloadAPI.applies()); n != 1 {
		t.Errorf("went on applying after a refusal: %d applies", n)
	}
}

// A cluster in Rancher under the name of this one that stands for another is never taken over.
func TestRegistrarRefusesRanchersClusterOfAnother(t *testing.T) {
	t.Parallel()
	web := availableNow(clusterWithRancher("web", false))
	f := newRancherFixture(t, web, nil, registered(rancherName(web), "team-b/web"))
	if _, err := f.reconcile(t, f.web); err == nil || !strings.Contains(err.Error(), `stands for "team-b/web"`) {
		t.Errorf("another's cluster in Rancher: %v", err)
	}
	if applied := f.workloadAPI.applies(); len(applied) != 0 {
		t.Errorf("applied %v", applied)
	}
}

// Before Rancher serves its own API, its kinds are unknown to its cluster; the registrar waits.
func TestRegistrarWaitsForRanchersAPI(t *testing.T) {
	t.Parallel()
	f := newRancherFixture(t, availableNow(clusterWithRancher("web", false)), &interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return &meta.NoKindMatchError{GroupKind: provisioningClusterGVK.GroupKind(), SearchedVersions: []string{"v1"}}
		},
	})
	res, err := f.reconcile(t, f.web)
	if err != nil || res.RequeueAfter != time.Minute {
		t.Errorf("before Rancher's API: %+v, %v", res, err)
	}
	if !f.holdsRanchersClient() {
		t.Error("dropped the client to Rancher's cluster while it only waited")
	}
}

// The registrar does nothing for Rancher's own cluster, for clusters not yet available, while
// Rancher's cluster is unavailable or going, and with no Rancher at all.
func TestRegistrarLeavesClustersAloneUntilBothSidesAreReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	check := func(t *testing.T, f *rancherFixture, c *clusterv1.Cluster) {
		t.Helper()
		if res := f.mustReconcile(t, c); !res.IsZero() {
			t.Errorf("%s: %+v", c.Name, res)
		}
		if got := f.registered(t); len(got) != 0 {
			t.Errorf("%s: registered %v", c.Name, got)
		}
	}

	f := newRancherFixture(t, clusterWithRancher("web", false), nil)
	check(t, f, f.web)
	check(t, f, f.rancher)
	if res := f.mustReconcile(t, &clusterv1.Cluster{Namespace: "team-a", Name: "gone"}); !res.IsZero() {
		t.Errorf("a cluster that is gone: %+v", res)
	}

	f = newRancherFixture(t, availableNow(clusterWithRancher("web", false)), nil)
	rancher := &clusterv1.Cluster{}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(f.rancher), rancher); err != nil {
		t.Fatal(err)
	}
	rancher.Status.Conditions = nil
	if err := f.kube.Update(ctx, rancher); err != nil {
		t.Fatal(err)
	}
	check(t, f, f.web)

	f = newRancherFixture(t, availableNow(clusterWithRancher("web", false)), nil)
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(f.rancher), rancher); err != nil {
		t.Fatal(err)
	}
	rancher.Finalizers = append(rancher.Finalizers, Finalizer)
	if err := f.kube.Update(ctx, rancher); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, rancher); err != nil {
		t.Fatal(err)
	}
	check(t, f, f.web)

	f = newRancherFixture(t, availableNow(clusterWithRancher("web", false)), nil)
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(f.rancher), rancher); err != nil {
		t.Fatal(err)
	}
	rancher.Finalizers = nil
	if err := f.kube.Delete(ctx, rancher); err != nil {
		t.Fatal(err)
	}
	check(t, f, f.web)
}

func TestRegistrarSetsUpWithAManager(t *testing.T) {
	t.Parallel()
	config := &rest.Config{Host: "https://127.0.0.1:1"}
	skip := true
	_, kube := newReconciler(t, &fakeCloud{})
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: kube.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: &skip},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = (&Registrar{Client: kube, ClassNamespace: "kubehenge-system"}).SetupWithManager(mgr); err != nil {
		t.Errorf("setting up: %v", err)
	}

	bare, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: runtime.NewScheme(), Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: &skip},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = (&Registrar{Client: kube, ClassNamespace: "kubehenge-system"}).SetupWithManager(bare)
	if err == nil || !strings.Contains(err.Error(), "setting up the Rancher registrar") {
		t.Errorf("a manager that knows no clusters: %v", err)
	}
}
