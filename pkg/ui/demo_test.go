package ui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

var demoAt = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

// refusingTransport fails the test on any request made with Go's default HTTP client.
type refusingTransport struct{ t *testing.T }

func (r refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Errorf("the demo made a request to %s", req.URL)
	return nil, errors.New("no network in the demo")
}

type demoBrowser struct {
	t       *testing.T
	handler http.Handler
	cookie  *http.Cookie
	csrf    string
}

func newDemo(t *testing.T) (*Server, *demoBrowser) {
	t.Helper()
	s := &Server{Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Kubehenge</title>")}}}
	if err := SetUpDemo(s, func() time.Time { return demoAt }); err != nil {
		t.Fatal(err)
	}
	return s, &demoBrowser{t: t, handler: s.Handler()}
}

func (b *demoBrowser) do(method, path, body string) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.send(b.t.Context(), method, path, body)
}

func (b *demoBrowser) doContext(ctx context.Context, method, path string) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.send(ctx, method, path, "")
}

func (b *demoBrowser) send(ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequestWithContext(ctx, method, "http://localhost"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	if b.csrf != "" {
		req.Header.Set(csrfHeader, b.csrf)
	}
	rec := httptest.NewRecorder()
	b.handler.ServeHTTP(rec, req)
	if rec.Header().Get(demoHeader) != "1" {
		b.t.Errorf("%s %s: the answer does not say it is the demo's", method, path)
	}
	return rec
}

// signIn opens the demo as a browser does, which signs in the demo's administrator.
func (b *demoBrowser) signIn() {
	b.t.Helper()
	rec := b.do("GET", "/api/session", "")
	if rec.Code != http.StatusOK {
		b.t.Fatalf("GET /api/session: %d %s", rec.Code, rec.Body)
	}
	var v sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		b.t.Fatal(err)
	}
	if v.User != demoUser || v.CanCreate == nil || !*v.CanCreate {
		b.t.Fatalf("the demo signs in %+v, want its administrator", v)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			b.cookie = c
		}
	}
	b.csrf = v.CSRF
}

func TestDemoUsesNoRealClient(t *testing.T) {
	t.Parallel()
	s, _ := newDemo(t)
	if _, ok := s.Accounts.(*demoAccounts); !ok {
		t.Errorf("Accounts is %T", s.Accounts)
	}
	if s.ConnectWorkload == nil || s.OpenStore == nil || s.Probe == nil || s.NewKube == nil || s.TalosReleases == nil {
		t.Error("a client of the demo is left to its real default")
	}
	if s.NewClientset != nil || s.MakeTemplate != nil || s.Appliance != nil || s.Upgrades != nil || s.Headlamp != nil || s.ProxmoxURL != "" {
		t.Error("the demo reaches past its own fakes")
	}
	channels, ok := s.Releases.(*provisioner.ChannelServer)
	if !ok {
		t.Fatalf("Releases is %T", s.Releases)
	}
	if _, ok := channels.HTTP.Transport.(demoChannels); !ok {
		t.Errorf("the release channels are read through %T", channels.HTTP.Transport)
	}
	if _, err := s.ConnectWorkload("https://192.0.2.1:6443", nil, "token"); err == nil {
		t.Error("the demo connects to a cluster that is not its own")
	}
}

// demoPaths fills in a route's wildcards with what the demo holds.
var demoPaths = strings.NewReplacer(
	"{namespace}", demoNamespace, "{name}", "web", "{node}", "pve1", "{distribution}", "rke2", "{purpose}", "dns",
	"{machine}", "web-control-plane-"+demoSuffix("web", "control-plane", 0), "{backup}", "before-ingress-change",
	"{restore}", "none", "{snapshot}", "none", "{podNamespace}", "kube-system",
	"{pod}", "metrics-server-"+demoSuffix("web", "web-control-plane-"+demoSuffix("web", "control-plane", 0), 0),
)

// demoAnswers are the reads that answer other than 200 in the demo, by design: the appliance's
// own, what the sample clusters hold none of, and the sign-in redirects.
var demoAnswers = map[string]int{
	"/api/health":                                       http.StatusNoContent,
	"/api/openid/start":                                 http.StatusSeeOther,
	"/api/openid/callback":                              http.StatusSeeOther,
	"/api/templates":                                    http.StatusNotFound,
	"/api/appliance/upgrades":                           http.StatusNotFound,
	"/api/appliance/etcd-backups":                       http.StatusNotFound,
	"/api/appliance/etcd-backups/snapshots":             http.StatusNotFound,
	"/api/credentials/dns":                              http.StatusNotFound,
	"/api/clusters/clusters/web/credentials/dns":        http.StatusNotFound,
	"/api/clusters/clusters/web/kubeconfig":             http.StatusConflict,
	"/api/clusters/clusters/web/talosconfig":            http.StatusNotFound,
	"/api/clusters/clusters/web/certificates/ca":        http.StatusNotFound,
	"/api/clusters/clusters/web/control-plane-recovery": http.StatusUnprocessableEntity,
	// A backup's log is fetched through a Velero download request, which nothing in the demo serves.
	"/api/clusters/clusters/web/backups/before-ingress-change/log": http.StatusGatewayTimeout,
}

// Every read the interface makes answers in the demo, as server.go lists them.
func TestDemoAnswersEveryRead(t *testing.T) { //nolint:paralleltest // swaps Go's default HTTP transport, which the whole process shares
	was := http.DefaultTransport
	http.DefaultTransport = refusingTransport{t}
	t.Cleanup(func() { http.DefaultTransport = was })
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`HandleFunc\("GET (/api/[^"]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) < 40 {
		t.Fatalf("found %d GET routes in server.go", len(routes))
	}
	_, b := newDemo(t)
	b.signIn()
	for _, r := range routes {
		path := demoPaths.Replace(r[1])
		if path == "/api/events" {
			// The change stream stays open until the browser leaves.
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			rec := b.doContext(ctx, "GET", path)
			cancel()
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body)
			}
			continue
		}
		want, special := demoAnswers[path]
		switch {
		case strings.HasSuffix(path, "/events"):
			path += "?since=" + demoAt.Add(-time.Hour).Format(time.RFC3339)
		case strings.HasSuffix(path, "/logs"):
			path += "?container=app"
		}
		rec := b.do("GET", path, "")
		switch {
		case special:
			if rec.Code != want {
				t.Errorf("GET %s: %d, want %d: %s", path, rec.Code, want, rec.Body)
			}
		case strings.Contains(path, "/none"):
			if rec.Code >= http.StatusInternalServerError {
				t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body)
			}
		case rec.Code != http.StatusOK:
			t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
	}
}

func TestDemoShowsItsFleet(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	var clusters []clusterView
	decodeDemo(t, b.do("GET", "/api/clusters", ""), &clusters)
	fleet, err := newDemoFleet(demoAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != len(fleet.data.Clusters) {
		t.Fatalf("%d clusters, want %d", len(clusters), len(fleet.data.Clusters))
	}
	var health []fleetHealthView
	decodeDemo(t, b.do("GET", "/api/fleet/health", ""), &health)
	verdicts := map[string]string{}
	for _, h := range health {
		verdicts[h.Name] = h.Verdict
	}
	if verdicts["payments"] != "degraded" || verdicts["web"] != "healthy" || verdicts["edge"] != "changing" {
		t.Errorf("verdicts %v", verdicts)
	}
	var runs []runView
	decodeDemo(t, b.do("GET", "/api/runs", ""), &runs)
	found := map[string]string{}
	for _, r := range runs {
		found[r.ID] = r.State
	}
	if found["change/"+clusterKey(demoNamespace, "edge")] != runRunning {
		t.Errorf("runs %v, want edge's upgrade under way", found)
	}
	var backups struct {
		Backups []json.RawMessage `json:"backups"`
	}
	decodeDemo(t, b.do("GET", "/api/clusters/"+demoNamespace+"/web/backups", ""), &backups)
	if len(backups.Backups) != 8 {
		t.Errorf("%d backups of web, want 8", len(backups.Backups))
	}
}

// lockedBuffer is a log that handlers write from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestDemoReadsEveryClustersHealth(t *testing.T) {
	t.Parallel()
	logged := &lockedBuffer{}
	s := &Server{
		Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Kubehenge</title>")}},
		Log:    slog.New(slog.NewTextHandler(logged, nil)),
	}
	if err := SetUpDemo(s, func() time.Time { return demoAt }); err != nil {
		t.Fatal(err)
	}
	b := &demoBrowser{t: t, handler: s.Handler()}
	b.signIn()
	fleet, err := newDemoFleet(demoAt)
	if err != nil {
		t.Fatal(err)
	}
	for i := range fleet.data.Clusters {
		name := fleet.data.Clusters[i].Name
		var h healthView
		decodeDemo(t, b.do("GET", "/api/clusters/"+demoNamespace+"/"+name+"/health", ""), &h)
		if h.ControlPlane.Error != "" || len(h.ControlPlane.Conditions) == 0 {
			t.Errorf("%s: control plane read %q with %d conditions", name, h.ControlPlane.Error, len(h.ControlPlane.Conditions))
		}
		for what, failed := range map[string]string{
			"nodes": h.Nodes.Error, "pods": h.Pods.Error, "volumes": h.Volumes.Error, "add-ons": h.Addons.Error,
			"pools": h.Pools.Error, "health checks": h.HealthChecks.Error, "events": h.Events.Error,
		} {
			if failed != "" {
				t.Errorf("%s: %s read failed: %s", name, what, failed)
			}
		}
	}
	if got := logged.String(); strings.Contains(got, "level=ERROR") {
		t.Errorf("the demo logged errors reading health:\n%s", got)
	}
}

// A page's first reads go out together, before any has brought a cookie. They share one session.
func TestDemoSignsInTheFirstReads(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	var ids []string
	for _, path := range []string{"/api/clusters", "/api/session", "/api/runs"} {
		rec := b.do("GET", path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookie {
				ids = append(ids, c.Value)
			}
		}
	}
	if len(ids) != 3 || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Errorf("sessions %v, want one", ids)
	}
}

func TestDemoRefusesChanges(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", "/api/clusters/" + demoNamespace + "/web", `{"version":"v1.37.1+rke2r1"}`},
		{"DELETE", "/api/clusters/" + demoNamespace + "/web", ""},
		{"POST", "/api/clusters", `{"name":"new"}`},
		{"POST", "/api/clusters/" + demoNamespace + "/web/backups", "{}"},
		{"POST", "/api/setup", "{}"},
		{"PUT", "/api/credentials/dns", "{}"},
	} {
		rec := b.do(c.method, c.path, c.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "the demo changes nothing") {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if rec := b.do("POST", "/api/clusters/"+demoNamespace+"/web/plan", `{}`); rec.Code == http.StatusForbidden {
		t.Errorf("a change's plan is refused: %s", rec.Body)
	}
	if rec := b.do("DELETE", "/api/session", ""); rec.Code >= http.StatusBadRequest {
		t.Errorf("signing out: %d %s", rec.Code, rec.Body)
	}
}

func decodeDemo(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatal(err)
	}
}
