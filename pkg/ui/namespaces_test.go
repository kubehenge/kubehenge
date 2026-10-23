package ui

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// managedConfigMap is the chart's list of the namespaces the interface manages.
func managedConfigMap(namespaces string) *corev1.ConfigMap {
	return &corev1.ConfigMap{Namespace: "kubehenge-system", Name: interfaceConfigMap, Data: map[string]string{namespacesKey: namespaces}}
}

func sessionOf(t *testing.T, b *browser) sessionView {
	t.Helper()
	rec := b.do("GET", "/api/session", "")
	expect(t, rec, http.StatusOK)
	var v sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMoreNamespaces(t *testing.T) {
	t.Parallel()
	h := newHarness(t, managedConfigMap(`["default","team-a"]`), cluster("team-a", "two"), cluster("team-b", "three"),
		adminKubeconfig("team-a", "two", "https://10.0.0.30:6443"), adminKubeconfig("team-b", "three", "https://10.0.0.40:6443"))
	b := h.browser()
	b.signIn("admin@pve")

	if s := sessionOf(t, b); s.Namespace != "default" || !slices.Equal(s.Namespaces, []string{"default", "team-a"}) {
		t.Fatalf("session = %+v", s)
	}

	rec := b.do("GET", "/api/clusters", "")
	expect(t, rec, http.StatusOK)
	var views []clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]allowedView{}
	for _, v := range views {
		allowed[v.Namespace+"/"+v.Name] = v.Allowed
	}
	if allowed["team-a/two"] != (allowedView{Change: true, Kubeconfig: true}) || allowed["team-b/three"] != (allowedView{}) {
		t.Fatalf("allowed = %+v", allowed)
	}

	rec = b.do("POST", "/api/clusters", strings.Replace(createBody, `"name"`, `"namespace":"team-a","name"`, 1))
	expect(t, rec, http.StatusCreated)
	var created clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Namespace != "team-a" || created.Pool != "/pool/kubehenge/team-a.web" || !created.Allowed.Change {
		t.Fatalf("created = %+v", created)
	}
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil || c.Annotations[createdBy] != "admin@pve" {
		t.Fatalf("team-a/web: %v %v", err, c.Annotations)
	}
	rec = b.do("POST", "/api/clusters", strings.Replace(createBody, `"name"`, `"namespace":"team-b","name"`, 1))
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "the interface creates clusters in namespaces default, team-a only") {
		t.Fatalf("refusal = %s", rec.Body)
	}
	// Unnamed, a cluster goes to the first.
	expect(t, b.do("POST", "/api/clusters", createBody), http.StatusCreated)
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}

	expect(t, b.do("PATCH", "/api/clusters/team-a/two", `{"workerPools":{"md-0":3}}`), http.StatusOK)
	expect(t, b.do("GET", "/api/clusters/team-a/two/kubeconfig", ""), http.StatusOK)
	for _, req := range [][2]string{
		{"PATCH", `{"workerPools":{"md-0":3}}`}, {"DELETE", ""},
	} {
		rec = b.do(req[0], "/api/clusters/team-b/three", req[1])
		expect(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), "manages the clusters in namespaces default, team-a; this one is managed from elsewhere") {
			t.Fatalf("%s: %s", req[0], rec.Body)
		}
	}
	expect(t, b.do("GET", "/api/clusters/team-b/three/kubeconfig", ""), http.StatusForbidden)
}

func TestNamespacesWithoutTheChartsList(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("team-a", "two"))
	b := h.browser()
	b.signIn("admin@pve")
	if s := sessionOf(t, b); s.Namespace != "default" || !slices.Equal(s.Namespaces, []string{"default"}) {
		t.Fatalf("session = %+v", s)
	}
	expect(t, b.do("PATCH", "/api/clusters/team-a/two", `{"workerPools":{"md-0":3}}`), http.StatusForbidden)
	rec := b.do("POST", "/api/clusters", strings.Replace(createBody, `"name"`, `"namespace":"team-a","name"`, 1))
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "the interface creates clusters in namespace default only") {
		t.Fatalf("refusal = %s", rec.Body)
	}
}

func TestNamespacesUnreadable(t *testing.T) {
	t.Parallel()
	for _, listed := range []string{"", "[]", `["default",""]`, "default"} {
		h := newHarness(t, managedConfigMap(listed))
		b := h.browser()
		b.signIn("admin@pve")
		// A list that cannot be read is no reason to manage fewer namespaces, or more.
		expect(t, b.do("GET", "/api/clusters", ""), http.StatusBadGateway)
		if s := sessionOf(t, b); s.Namespaces != nil {
			t.Errorf("%q: session = %+v", listed, s)
		}
	}
}

func TestHeadlampKubeconfigListsEveryManagedNamespace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, managedConfigMap(`["default","team-a"]`), cluster("default", "one"), cluster("team-a", "two"), cluster("team-b", "three"),
		adminKubeconfig("default", "one", "https://10.0.0.20:6443"), adminKubeconfig("team-a", "two", "https://10.0.0.30:6443"),
		adminKubeconfig("team-b", "three", "https://10.0.0.40:6443"))
	withHeadlamp(t, h)
	if err := h.server.writeHeadlampKubeconfig(t.Context(), h.kube); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(h.server.Headlamp.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Contexts) != 2 || cfg.Contexts["default.one"] == nil || cfg.Contexts["team-a.two"] == nil {
		t.Fatalf("contexts = %v", cfg.Contexts)
	}
}
