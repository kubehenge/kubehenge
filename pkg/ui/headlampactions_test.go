package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// headlampBody sends a request with a body to the gate.
func (b *browser) headlampBody(method, path, body string) *httptest.ResponseRecorder {
	b.h.t.Helper()
	req := httptest.NewRequestWithContext(b.h.t.Context(), method, "https://kubehenge.example:8444"+path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:50000"
	req.Header.Set("Content-Type", "application/json")
	headlampPage(req)
	if b.cookie != "" {
		req.Header.Set("Cookie", sessionCookie+"="+b.cookie)
	}
	rec := httptest.NewRecorder()
	b.h.server.HeadlampHandler().ServeHTTP(rec, req)
	return rec
}

func offerHeadlampActions(h *harness) {
	h.server.Headlamp.mu.Lock()
	h.server.Headlamp.actions = true
	h.server.Headlamp.mu.Unlock()
}

type gateCase struct {
	method, path, body string
	want               int
}

func checkGate(t *testing.T, b *browser, seen *[]upstreamRequest, cases []gateCase) {
	t.Helper()
	for _, c := range cases {
		*seen = nil
		rec := b.headlampBody(c.method, c.path, c.body)
		passed := len(*seen) == 1
		if rec.Code != c.want || passed != (c.want == http.StatusOK) {
			t.Errorf("%s %s %s: %d, passed on %v; want %d", c.method, c.path, c.body, rec.Code, passed, c.want)
			continue
		}
		if passed && ((*seen)[0].method != c.method || (*seen)[0].body != c.body) {
			t.Errorf("%s %s: Headlamp got %s with %q", c.method, c.path, (*seen)[0].method, (*seen)[0].body)
		}
	}
}

const helm = "/clusters/default.one/helm/"

// With the actions off, the gate offers none of Headlamp's Helm routes, nor the plugins Headlamp
// ships, and leaves those out of its plugin list. A cluster's node drain is offered either way.
func TestHeadlampActionsOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	seen := withHeadlamp(t, h)
	a := h.browser()
	a.signIn("admin@pve")

	install := `{"name":"web","namespace":"apps","chart":"bitnami/nginx","version":"1.0.0","description":"web"}`
	checkGate(t, a, seen, []gateCase{
		{"GET", helm + "releases/list", "", http.StatusForbidden},
		{"GET", helm + "releases?name=web&namespace=apps", "", http.StatusForbidden},
		{"POST", helm + "release/install?namespace=apps", install, http.StatusForbidden},
		{"GET", helm + "repositories", "", http.StatusForbidden},
		{"POST", helm + "repositories", `{"name":"bitnami","url":"https://charts.bitnami.com/bitnami"}`, http.StatusForbidden},
		{"GET", "/static-plugins/app-catalog/main.js", "", http.StatusForbidden},
		{"POST", "/clusters/default.one/drain-node", `{"cluster":"default.one","nodeName":"w1"}`, http.StatusOK},
		{"GET", "/clusters/default.one/drain-node-status?nodeName=w1", "", http.StatusOK},
		{"GET", "/clusters/default.one/apis/metrics.k8s.io/v1beta1/nodes", "", http.StatusOK},
	})

	rec := a.headlamp("GET", "/plugins")
	var plugins []map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &plugins); err != nil || len(plugins) != 1 || plugins[0]["name"] != "own" {
		t.Errorf("the plugin list with the actions off: %d %s", rec.Code, rec.Body)
	}
}

// With the actions on, a cluster's Helm routes are offered to whoever sees the cluster, exactly
// those the app catalog takes, with each method it takes them with. Headlamp acts there with the
// user's own credentials.
func TestHeadlampActionsHelm(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"))
	seen := withHeadlamp(t, h)
	offerHeadlampActions(h)
	v := h.browser()
	v.signIn("viewer@pve")

	install := `{"name":"web","namespace":"apps","values":"replicas: 2","chart":"bitnami/nginx","version":"1.0.0","description":"web"}`
	checkGate(t, v, seen, []gateCase{
		{"GET", helm + "releases/list", "", http.StatusOK},
		{"GET", helm + "releases?name=web&namespace=apps", "", http.StatusOK},
		{"GET", helm + "release/history?name=web&namespace=apps", "", http.StatusOK},
		{"GET", helm + "action/status?name=web&action=install", "", http.StatusOK},
		{"PUT", helm + "releases/rollback?name=web&namespace=apps", `{"name":"web","namespace":"apps","revision":1}`, http.StatusOK},
		{"DELETE", helm + "releases/uninstall?name=web&namespace=apps", "", http.StatusOK},
		{"POST", helm + "release/install?namespace=apps", install, http.StatusOK},
		{"PUT", helm + "releases/upgrade?name=web&namespace=apps", install, http.StatusOK},
		{"GET", helm + "repositories", "", http.StatusOK},
		{"GET", helm + "charts?filter=nginx", "", http.StatusOK},

		// Another method on a route offered, and routes the catalog does not take.
		{"POST", helm + "releases/list", "", http.StatusForbidden},
		{"DELETE", helm + "releases", "", http.StatusForbidden},
		{"GET", helm + "release/install", "", http.StatusForbidden},
		{"PUT", helm + "releases/uninstall", "", http.StatusForbidden},
		{"GET", helm + "releases/list/more", "", http.StatusForbidden},
		{"GET", helm + "other/releases/list", "", http.StatusForbidden},
		{"GET", helm, "", http.StatusForbidden},
		{"GET", "/clusters/default.one/helm", "", http.StatusForbidden},

		// A cluster the user does not see.
		{"GET", "/clusters/default.two/helm/releases/list", "", http.StatusNotFound},
		{"POST", "/clusters/default.two/helm/release/install?namespace=apps", install, http.StatusNotFound},
	})
}

// A chart is installed or upgraded only from a repository, never from a path on the appliance, an
// address or an OCI reference, and without dependencies fetched from where the chart says.
func TestHeadlampActionsChartsComeFromRepositories(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	seen := withHeadlamp(t, h)
	offerHeadlampActions(h)
	v := h.browser()
	v.signIn("viewer@pve")

	body := func(chart, more string) string {
		return `{"name":"web","namespace":"apps","chart":` + chart + `,"version":"1.0.0","description":"web"` + more + `}`
	}
	cases := make([]gateCase, 0, 30)
	for _, route := range []struct{ method, path string }{
		{"POST", helm + "release/install?namespace=apps"},
		{"PUT", helm + "releases/upgrade?name=web&namespace=apps"},
	} {
		for chart, want := range map[string]int{
			`"bitnami/nginx"`:                 http.StatusOK,
			`"prometheus-community/kube-1.2"`: http.StatusOK,
			`"/etc/kubehenge"`:                http.StatusForbidden,
			`"etc/kubehenge/x"`:               http.StatusForbidden,
			`"../state/chart"`:                http.StatusForbidden,
			`".cache/helm"`:                   http.StatusForbidden,
			`"https://example.org/c.tgz"`:     http.StatusForbidden,
			`"oci://registry.example/c"`:      http.StatusForbidden,
			`"file:///etc/c"`:                 http.StatusForbidden,
			`"nginx"`:                         http.StatusForbidden,
			`""`:                              http.StatusForbidden,
			`7`:                               http.StatusForbidden,
		} {
			cases = append(cases, gateCase{route.method, route.path, body(chart, ""), want})
		}
		cases = append(cases,
			gateCase{route.method, route.path, body(`"bitnami/nginx"`, `,"dependencyUpdate":true`), http.StatusForbidden},
			gateCase{route.method, route.path, body(`"bitnami/nginx"`, `,"dependencyUpdate":false,"createNamespace":true`), http.StatusOK},
			gateCase{route.method, route.path, "not json", http.StatusForbidden})
	}
	checkGate(t, v, seen, cases)

	*seen = nil
	big := body(`"bitnami/nginx"`, `,"values":"`+strings.Repeat("a", maxHeadlampHelmBody)+`"`)
	if rec := v.headlampBody("POST", helm+"release/install?namespace=apps", big); rec.Code != http.StatusRequestEntityTooLarge || len(*seen) != 0 {
		t.Errorf("values over Helm's limit: %d, passed %d times", rec.Code, len(*seen))
	}
}

// Headlamp's Helm repositories are one list for every user and cluster: those who may set the
// appliance up change it, over HTTPS with the certificate checked, and others only read it.
func TestHeadlampActionsRepositories(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	seen := withHeadlamp(t, h)
	offerHeadlampActions(h)
	v := h.browser()
	v.signIn("viewer@pve")
	a := h.browser()
	a.signIn("admin@pve")

	repo := `{"name":"bitnami","url":"https://charts.bitnami.com/bitnami"}`
	checkGate(t, v, seen, []gateCase{
		{"GET", helm + "repositories", "", http.StatusOK},
		{"POST", helm + "repositories", repo, http.StatusForbidden},
		{"PUT", helm + "repositories/update", repo, http.StatusForbidden},
		{"DELETE", helm + "repositories/remove?name=bitnami", "", http.StatusForbidden},
	})
	checkGate(t, a, seen, []gateCase{
		{"POST", helm + "repositories", repo, http.StatusOK},
		{"PUT", helm + "repositories/update", repo, http.StatusOK},
		{"DELETE", helm + "repositories/remove?name=bitnami", "", http.StatusOK},
		{"POST", helm + "repositories", `{"name":"own","url":"https://charts.example","username":"u","password":"p"}`, http.StatusOK},
		{"POST", helm + "repositories", `{"name":"own","url":"https://charts.example","insecureSkipTLSverify":false,"passCredentialsAll":false}`, http.StatusOK},

		{"POST", helm + "repositories", `{"name":"own","url":"http://charts.example"}`, http.StatusForbidden},
		{"POST", helm + "repositories", `{"name":"own","url":"oci://registry.example/charts"}`, http.StatusForbidden},
		{"POST", helm + "repositories", `{"name":"own","url":"file:///etc"}`, http.StatusForbidden},
		{"POST", helm + "repositories", `{"name":"own","url":"https://u:p@charts.example"}`, http.StatusForbidden},
		{"POST", helm + "repositories", `{"name":"own","url":"https:///path"}`, http.StatusForbidden},
		{"POST", helm + "repositories", `{"name":"own","url":"https://charts.example","insecureSkipTLSverify":true}`, http.StatusForbidden},
		{"PUT", helm + "repositories/update", `{"name":"own","url":"https://charts.example","passCredentialsAll":true}`, http.StatusForbidden},
		{"POST", helm + "repositories", `{"name":"own"}`, http.StatusForbidden},
		{"DELETE", helm + "repositories", "", http.StatusForbidden},
		{"GET", helm + "repositories/remove", "", http.StatusForbidden},
	})
}

// The actions bring the plugins Headlamp ships, to be read alone. Port forwards, which Headlamp
// offers in its desktop app alone, the routes that name a cluster themselves and the server's own
// metrics stay refused.
func TestHeadlampActionsOn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	seen := withHeadlamp(t, h)
	offerHeadlampActions(h)
	a := h.browser()
	a.signIn("admin@pve")

	forward := `{"namespace":"apps","pod":"web-0","targetPort":"80"}`
	checkGate(t, a, seen, []gateCase{
		{"GET", "/static-plugins/app-catalog/main.js", "", http.StatusOK},
		{"GET", "/static-plugins/prometheus/locales/en/translation.json", "", http.StatusOK},
		{"POST", "/static-plugins/app-catalog/main.js", "", http.StatusMethodNotAllowed},
		{"POST", "/clusters/default.one/portforward", forward, http.StatusForbidden},
		{"GET", "/clusters/default.one/portforward?id=1", "", http.StatusForbidden},
		{"GET", "/clusters/default.one/portforward/list", "", http.StatusForbidden},
		{"DELETE", "/clusters/default.one/portforward", `{"id":"1"}`, http.StatusForbidden},
		{"POST", "/drain-node", `{"cluster":"default.one","nodeName":"w1"}`, http.StatusForbidden},
		{"GET", "/drain-node-status?cluster=default.one&nodeName=w1", "", http.StatusForbidden},
		{"GET", "/metrics", "", http.StatusForbidden},
		{"GET", "/externalproxy", "", http.StatusForbidden},
		{"GET", "/wsMultiplexer", "", http.StatusForbidden},
	})

	rec := a.headlamp("GET", "/plugins")
	var plugins []map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &plugins); err != nil || len(plugins) != 2 {
		t.Errorf("the plugin list with the actions on: %d %s", rec.Code, rec.Body)
	}
}

// The interface learns from the chart's ConfigMap whether the actions are on, and turns them off
// when it says nothing of them or cannot be read.
func TestHeadlampActionsFollowTheChart(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{Data: map[string]string{namespacesKey: `["default"]`, headlampActionsKey: "true"}}
	cm.Namespace, cm.Name = "kubehenge-system", interfaceConfigMap
	h := newHarness(t, cm)
	withHeadlamp(t, h)
	h.server.ClassNamespace = cm.Namespace

	if err := h.server.readHeadlampActions(t.Context(), h.kube); err != nil || !h.server.Headlamp.actionsOffered() {
		t.Fatalf("with the chart's word: %v, offered %v", err, h.server.Headlamp.actionsOffered())
	}
	cm.Data[headlampActionsKey] = "false"
	if err := h.kube.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	if err := h.server.readHeadlampActions(t.Context(), h.kube); err != nil || h.server.Headlamp.actionsOffered() {
		t.Fatalf("switched off: %v, offered %v", err, h.server.Headlamp.actionsOffered())
	}
	offerHeadlampActions(h)
	if err := h.kube.Delete(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	if err := h.server.readHeadlampActions(t.Context(), h.kube); err != nil || h.server.Headlamp.actionsOffered() {
		t.Fatalf("without the ConfigMap: %v, offered %v", err, h.server.Headlamp.actionsOffered())
	}
	offerHeadlampActions(h)
	if err := h.server.readHeadlampActions(t.Context(), nil); err != nil || h.server.Headlamp.actionsOffered() {
		t.Fatalf("without a management cluster: %v, offered %v", err, h.server.Headlamp.actionsOffered())
	}
}
