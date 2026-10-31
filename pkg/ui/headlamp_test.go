package ui

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func adminKubeconfig(namespace, name, server string) *corev1.Secret {
	raw := `apiVersion: v1
kind: Config
clusters:
- name: ` + name + `
  cluster: {server: "` + server + `", certificate-authority-data: Q0EtREFUQQ==}
users:
- name: ` + name + `-admin
  user: {token: admin-secret, client-certificate-data: Q0VSVA==, client-key-data: S0VZ}
contexts:
- name: ` + name + `-admin@` + name + `
  context: {cluster: ` + name + `, user: ` + name + `-admin}
current-context: ` + name + `-admin@` + name + `
`
	return &corev1.Secret{
		Namespace: namespace, Name: name + "-kubeconfig",
		Data: map[string][]byte{"value": []byte(raw)},
	}
}

type upstreamRequest struct {
	path    string
	cookies []string
	proto   string
	host    string
	// stateless are the headers with which Headlamp's dynamic clusters pick a kubeconfig and user.
	stateless []string
	// authorization is the credential Headlamp would pass on to the cluster.
	authorization string
	method        string
	body          string
}

func withHeadlamp(t *testing.T, h *harness) *[]upstreamRequest {
	t.Helper()
	var mu sync.Mutex
	seen := &[]upstreamRequest{}
	var own string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Headlamp's own check: a loopback Host with its port or none.
		if r.Host != "127.0.0.1" && r.Host != own {
			http.Error(w, "invalid Host header", http.StatusForbidden)
			return
		}
		var cookies []string
		for _, c := range r.Cookies() {
			cookies = append(cookies, c.Name)
		}
		var stateless []string
		for _, name := range []string{"Kubeconfig", "X-Headlamp-User-Id"} {
			if r.Header.Get(name) != "" {
				stateless = append(stateless, name)
			}
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*seen = append(*seen, upstreamRequest{
			path: r.URL.Path, cookies: cookies, proto: r.Header.Get("X-Forwarded-Proto"), host: r.Host, stateless: stateless,
			authorization: r.Header.Get("Authorization"), method: r.Method, body: string(body),
		})
		mu.Unlock()
		// Headlamp's answers, as Headlamp gives them.
		switch r.URL.Path {
		case "/oidc":
			http.Redirect(w, r, "https://idp.example/auth?client_id=k8s&state=st-"+r.URL.Query().Get("cluster"), http.StatusFound) //nolint:gosec // the fake Headlamp
			return
		case "/oidc-callback":
			http.SetCookie(w, &http.Cookie{
				Name: "headlamp-auth-defaultone.0", Value: "id-token", Path: "/clusters/default.one",
				Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
			// Headlamp's own address, which the browser cannot reach: only its path may be kept.
			http.Redirect(w, r, "https://"+r.Host+"/auth?cluster=default.one", http.StatusSeeOther) //nolint:gosec // the fake Headlamp
			return
		}
		if r.URL.Path == "/plugins" {
			_, _ = w.Write([]byte(`[{"path":"static-plugins/app-catalog","type":"shipped","source":"shipped","name":"app-catalog"},` +
				`{"path":"plugins/own","type":"development","source":"development","name":"own"}]`))
			return
		}
		if r.URL.Path == "/config" {
			_, _ = w.Write([]byte(`{"clusters":[{"name":"default.one","server":"https://10.0.0.20:6443"},{"name":"default.two"},{"name":"main"}],"isDynamicClusterEnabled":false}`))
			return
		}
		_, _ = w.Write([]byte("from headlamp"))
	}))
	t.Cleanup(up.Close)
	own = up.Listener.Addr().String()
	u, _ := url.Parse("http://" + forward(t, own))
	h.server.Headlamp = &Headlamp{Upstream: u, Kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"), Port: 8444, InterfacePort: 443}
	return seen
}

// forward stands for the socket that starts Headlamp: it listens on a port of its own and passes
// each connection on to the address to.
func forward(t *testing.T, to string) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = in.Close() }()
				out, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", to)
				if err != nil {
					return
				}
				defer func() { _ = out.Close() }()
				go func() { _, _ = io.Copy(out, in) }()
				_, _ = io.Copy(in, out)
			}()
		}
	}()
	return l.Addr().String()
}

func (b *browser) headlamp(method, path string, cookies ...string) *httptest.ResponseRecorder {
	b.h.t.Helper()
	req := httptest.NewRequestWithContext(b.h.t.Context(), method, "https://kubehenge.example:8444"+path, nil)
	req.RemoteAddr = "192.0.2.10:50000"
	headlampPage(req)
	if b.cookie != "" {
		cookies = append(cookies, sessionCookie+"="+b.cookie)
	}
	if len(cookies) > 0 {
		req.Header.Set("Cookie", strings.Join(cookies, "; "))
	}
	rec := httptest.NewRecorder()
	b.h.server.HeadlampHandler().ServeHTTP(rec, req)
	return rec
}

// headlampPage sends a request as Headlamp's own pages do: a browser names the origin of anything
// but a read.
func headlampPage(req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		req.Header.Set("Origin", "https://kubehenge.example:8444")
	}
}

// A page of another origin of the same site gets the session cookie along, but may change nothing
// through the gate, nor open a WebSocket: that would be exec in a pod with the user's token.
func TestHeadlampRefusesOtherOrigins(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	seen := withHeadlamp(t, h)
	b := h.browser()
	b.signIn("dev@pve")
	send := func(method, path string, headers map[string]string) int {
		t.Helper()
		*seen = nil
		req := httptest.NewRequestWithContext(t.Context(), method, "https://kubehenge.example:8444"+path, nil)
		req.Header.Set("Cookie", sessionCookie+"="+b.cookie)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.server.HeadlampHandler().ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden && len(*seen) != 0 {
			t.Errorf("%s %s %v: refused, but Headlamp got it", method, path, headers)
		}
		return rec.Code
	}
	exec := "/clusters/default.one/api/v1/namespaces/apps/pods/web/exec?command=sh"
	websocket := func(origin string) map[string]string {
		headers := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}
		if origin != "" {
			headers["Origin"] = origin
		}
		return headers
	}
	for _, c := range []struct {
		method, path string
		headers      map[string]string
	}{
		{"GET", exec, websocket("https://apps.kubehenge.example")},
		{"GET", exec, websocket("https://kubehenge.example")},
		{"GET", exec, websocket("http://kubehenge.example:8444")},
		{"GET", exec, websocket("null")},
		{"GET", exec, websocket("")},
		{"POST", "/clusters/default.one/api/v1/namespaces/apps/pods", map[string]string{"Origin": "https://apps.kubehenge.example"}},
		{"DELETE", "/clusters/default.one/api/v1/namespaces/apps/pods/web", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"PATCH", "/clusters/default.one/api/v1/namespaces/apps/pods/web", nil},
	} {
		if code := send(c.method, c.path, c.headers); code != http.StatusForbidden {
			t.Errorf("%s %s %v: %d, want refused", c.method, c.path, c.headers, code)
		}
	}
	for _, c := range []struct {
		method, path string
		headers      map[string]string
	}{
		{"GET", exec, websocket("https://kubehenge.example:8444")},
		{"GET", exec, map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-Fetch-Site": "same-origin"}},
		{"POST", "/clusters/default.one/api/v1/namespaces/apps/pods", map[string]string{"Origin": "https://KUBEHENGE.example:8444"}},
		{"DELETE", "/clusters/default.one/api/v1/namespaces/apps/pods/web", map[string]string{"Sec-Fetch-Site": "same-origin"}},
		{"GET", "/clusters/default.one/version", map[string]string{"Sec-Fetch-Site": "cross-site"}},
	} {
		if code := send(c.method, c.path, c.headers); code == http.StatusForbidden || len(*seen) != 1 {
			t.Errorf("%s %s %v: %d, Headlamp got %d requests; want it passed on", c.method, c.path, c.headers, code, len(*seen))
		}
	}
}

// secretReads counts the Secrets read through it.
type secretReads struct {
	client.Client

	n int
}

func (c *secretReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		c.n++
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestHeadlampKubeconfigReadsAClustersSecretOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), adminKubeconfig("default", "one", "https://10.0.0.20:6443"))
	withHeadlamp(t, h)
	kube := &secretReads{Client: h.kube}
	for range 3 {
		if err := h.server.writeHeadlampKubeconfig(t.Context(), kube); err != nil {
			t.Fatal(err)
		}
	}
	if kube.n != 1 {
		t.Fatalf("the kubeconfig Secret was read %d times in three rounds", kube.n)
	}
	if raw, err := os.ReadFile(h.server.Headlamp.Kubeconfig); err != nil || !strings.Contains(string(raw), "https://10.0.0.20:6443") {
		t.Fatalf("kubeconfig: %v\n%s", err, raw)
	}
}

func TestHeadlampKubeconfigHoldsNoCredentials(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"), cluster("elsewhere", "three"),
		adminKubeconfig("default", "one", "https://10.0.0.20:6443"), adminKubeconfig("elsewhere", "three", "https://10.0.0.30:6443"))
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
	// two has no kubeconfig yet, three is outside the interface's namespace.
	if len(cfg.Contexts) != 1 || cfg.Contexts["default.one"] == nil {
		t.Fatalf("contexts = %v", cfg.Contexts)
	}
	c := cfg.Clusters["default.one"]
	if c == nil || c.Server != "https://10.0.0.20:6443" || string(c.CertificateAuthorityData) != "CA-DATA" {
		t.Fatalf("cluster = %+v", c)
	}
	for _, secret := range []string{"admin-secret", "Q0VSVA", "S0VZ", "token", "client-certificate", "client-key", "client-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the kubeconfig holds %q:\n%s", secret, raw)
		}
	}
}

func TestHeadlampGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"))
	seen := withHeadlamp(t, h)
	anonymous := h.browser()
	rec := anonymous.headlamp("GET", "/c/default.one/")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://kubehenge.example/" || len(*seen) != 0 {
		t.Fatalf("without a session: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	for host, want := range map[string]string{
		"evil.example/x?@y:8444": "https://localhost/",
		"[2001:db8::1]:8444":     "https://[2001:db8::1]/",
		"192.0.2.7:8444":         "https://192.0.2.7/",
	} {
		req := httptest.NewRequestWithContext(t.Context(), "GET", "https://kubehenge.example:8444/", nil)
		req.Host = host
		if got := h.server.interfaceURL(req); got != want {
			t.Errorf("Host %q: sign-in at %q, want %q", host, got, want)
		}
	}

	v := h.browser()
	v.signIn("viewer@pve")
	if session := v.do("GET", "/api/session", ""); !strings.Contains(session.Body.String(), `"headlamp":8444`) {
		t.Fatalf("session = %s", session.Body)
	}

	rec = v.headlamp("GET", "/config")
	type entry struct {
		Name string `json:"name"`
	}
	var config struct {
		Clusters []entry `json:"clusters"`
		Dynamic  *bool   `json:"isDynamicClusterEnabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &config); err != nil || len(config.Clusters) != 1 || config.Clusters[0].Name != "default.one" || config.Dynamic == nil {
		t.Fatalf("config = %s", rec.Body)
	}

	*seen = nil
	rec = v.headlamp("GET", "/clusters/default.one/version", "headlamp-auth-default.one.0=user-token")
	if rec.Code != http.StatusOK || rec.Body.String() != "from headlamp" || len(*seen) != 1 {
		t.Fatalf("a visible cluster: %d %s", rec.Code, rec.Body)
	}
	if got := (*seen)[0]; strings.Join(got.cookies, ",") != "headlamp-auth-default.one.0" || got.proto != "https" {
		t.Fatalf("upstream got cookies %v, X-Forwarded-Proto %q", got.cookies, got.proto)
	}

	*seen = nil
	req := httptest.NewRequestWithContext(t.Context(), "GET", "https://kubehenge.example:8444/clusters/default.one/version", nil)
	req.Header.Set("Cookie", sessionCookie+"="+v.cookie)
	req.Header.Set("Kubeconfig", "YXBpVmVyc2lvbjogdjE=")
	req.Header.Set("X-Headlamp-User-Id", "someone-else")
	rec = httptest.NewRecorder()
	h.server.HeadlampHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(*seen) != 1 || len((*seen)[0].stateless) != 0 {
		t.Fatalf("a browser's kubeconfig and user headers: %d, upstream %+v", rec.Code, *seen)
	}

	for path, status := range map[string]int{
		"/clusters/default.two/version":                    http.StatusNotFound,
		"/clusters/nodots/version":                         http.StatusNotFound,
		"/clusters/default.one/portforward":                http.StatusForbidden,
		"/clusters/default.one/helm/releases/list":         http.StatusForbidden,
		"/externalproxy":                                   http.StatusForbidden,
		"/wsMultiplexer":                                   http.StatusForbidden,
		"/drain-node-status":                               http.StatusForbidden,
		"/oidc":                                            http.StatusNotFound,
		"/oidc?cluster=default.two":                        http.StatusNotFound,
		"/oidc-callback?state=abc":                         http.StatusForbidden,
		"/user-plugins/x/main.js":                          http.StatusForbidden,
		"/clusters/default.one/../default.two/version":     http.StatusBadRequest,
		"/clusters/default.one%2F..%2Fdefault.two/version": http.StatusBadRequest,
	} {
		*seen = nil
		if rec := v.headlamp("GET", path); rec.Code != status || len(*seen) != 0 {
			t.Errorf("GET %s: %d, upstream called %d times; want %d", path, rec.Code, len(*seen), status)
		}
	}
	if rec := v.headlamp("DELETE", "/plugins/x"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /plugins/x: %d", rec.Code)
	}
	if rec := v.headlamp("POST", "/clusters/default.one/set-token"); rec.Code != http.StatusOK {
		t.Errorf("POST set-token on a visible cluster: %d", rec.Code)
	}
	if rec := v.headlamp("GET", "/c/default.one/pods"); rec.Code != http.StatusOK {
		t.Errorf("the frontend: %d", rec.Code)
	}
}

func TestHeadlampOIDCSignIn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	seen := withHeadlamp(t, h)
	v := h.browser()
	v.signIn("viewer@pve")

	rec := v.headlamp("GET", "/oidc?cluster=default.one")
	binding := cookie(rec, headlampOIDCCookie)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://idp.example/auth") || binding == nil ||
		binding.Value != stateDigest("st-default.one") || binding.SameSite != http.SameSiteLaxMode || !binding.HttpOnly || !binding.Secure {
		t.Fatalf("start: %d %s, binding %+v", rec.Code, rec.Header().Get("Location"), binding)
	}
	// Headlamp refuses any Host but its loopback address, with its own port or none. The upstream is
	// the socket in front of it, whose port is another.
	if got := (*seen)[len(*seen)-1].host; got != h.server.Headlamp.Upstream.Hostname() {
		t.Fatalf("Headlamp saw Host %q", got)
	}

	// The identity provider sends the browser back cross-site: no session cookie comes along.
	back := h.browser()
	if refused := back.headlamp("GET", "/oidc-callback?code=c&state=st-default.one"); refused.Code != http.StatusForbidden {
		t.Fatalf("a return without the binding cookie: %d", refused.Code)
	}
	if refused := back.headlamp("GET", "/oidc-callback?code=c&state=st-other", headlampOIDCCookie+"="+binding.Value); refused.Code != http.StatusForbidden {
		t.Fatalf("a return with another state: %d", refused.Code)
	}
	rec = back.headlamp("GET", "/oidc-callback?code=c&state=st-default.one", headlampOIDCCookie+"="+binding.Value)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `url=/auth?cluster=default.one"`) || rec.Header().Get("Location") != "" {
		t.Fatalf("return: %d %s", rec.Code, rec.Body)
	}
	if c := cookie(rec, "headlamp-auth-defaultone.0"); c == nil {
		t.Fatal("Headlamp's token cookie was dropped")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == headlampOIDCCookie && c.MaxAge >= 0 {
			t.Fatal("the binding cookie outlived its use")
		}
	}
}

func TestHeadlampKubeconfigOffersTheFirstProvider(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "oidc", Value: apiextensionsv1.JSON{
		Raw: []byte(`[{"issuerURL":"https://idp.example","clientID":"k8s","scopes":["email","groups"],"certificateAuthority":"PEM"},{"issuerURL":"https://other.example","clientID":"x"}]`),
	}})
	h := newHarness(t, c, adminKubeconfig("default", "one", "https://10.0.0.20:6443"))
	withHeadlamp(t, h)
	if err := h.server.writeHeadlampKubeconfig(t.Context(), h.kube); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(h.server.Headlamp.Kubeconfig)
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	auth := cfg.AuthInfos[cfg.Contexts["default.one"].AuthInfo]
	want := map[string]string{"idp-issuer-url": "https://idp.example", "client-id": "k8s", "scope": "email,groups", "idp-certificate-authority-data": "UEVN"}
	if auth == nil || auth.AuthProvider == nil || auth.AuthProvider.Name != "oidc" || !reflect.DeepEqual(auth.AuthProvider.Config, want) || auth.Token != "" {
		t.Fatalf("auth = %+v", auth)
	}
}

// A cluster made with kubectl that leaves oidc unset trusts its class's default providers.
func TestHeadlampKubeconfigOffersTheClassDefaultWhenUnset(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), adminKubeconfig("default", "one", "https://10.0.0.20:6443"))
	withHeadlamp(t, h)
	if err := h.server.writeHeadlampKubeconfig(t.Context(), h.kube); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(h.server.Headlamp.Kubeconfig)
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	auth := cfg.AuthInfos[cfg.Contexts["default.one"].AuthInfo]
	if auth == nil || auth.AuthProvider == nil || auth.AuthProvider.Config["idp-issuer-url"] != "https://sso.example" || auth.AuthProvider.Config["client-id"] != "kubernetes" {
		t.Fatalf("auth = %+v", auth)
	}
}
