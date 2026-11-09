package ui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
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
	cleanup, err := SetUpDemo(s, func() time.Time { return demoAt })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
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
	if s.NewClientset != nil || s.Upgrades != nil || s.Headlamp != nil || s.ProxmoxURL != "" {
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
	"/api/appliance/upgrades":                           http.StatusNotFound,
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

// The demo's Talos cluster has an upgrade a disruption budget holds, with the event in its activity,
// and no other cluster has either.
func TestDemoHoldsTheTalosUpgradeOfPlatform(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	var clusters []clusterView
	decodeDemo(t, b.do("GET", "/api/clusters", ""), &clusters)
	for i := range clusters {
		c := &clusters[i]
		held := c.Talos != nil && c.Talos.Held != ""
		if held != (c.Name == "platform") {
			t.Errorf("cluster %s: held %v", c.Name, held)
		}
	}
	var platform clusterView
	decodeDemo(t, b.do("GET", "/api/clusters/"+demoNamespace+"/platform", ""), &platform)
	if platform.Talos == nil || !strings.HasPrefix(platform.Talos.Upgrading, "platform-control-plane-") ||
		!strings.Contains(platform.Talos.Held, "The disruption budget orders-db needs") {
		t.Errorf("platform's Talos view %+v", platform.Talos)
	}
	var activity activityView
	decodeDemo(t, b.do("GET", "/api/clusters/"+demoNamespace+"/platform/activity", ""), &activity)
	if !slices.ContainsFunc(activity.Events.Items, func(e eventView) bool { return e.Reason == "TalosDrainBlocked" }) {
		t.Errorf("platform's events %+v", activity.Events)
	}
}

func TestDemoHasANodeThatIsOnlineAndNotReachable(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	var nodes []pve.Node
	decodeDemo(t, b.do("GET", "/api/proxmox/nodes", ""), &nodes)
	faulty := slices.IndexFunc(nodes, func(n pve.Node) bool { return n.Fault != "" })
	if faulty < 0 || nodes[faulty].Online || nodes[faulty].Fault != "HTTP 596: certificate verify failed" {
		t.Fatalf("nodes %+v, want one that is not online and says why", nodes)
	}
	var clusters []clusterView
	decodeDemo(t, b.do("GET", "/api/clusters", ""), &clusters)
	skipped := false
	for i := range clusters {
		for _, cond := range clusters[i].Conditions {
			skipped = skipped || clusters[i].Name == "staging" && cond.Type == provisioner.NodesSkippedCondition && strings.Contains(cond.Message, nodes[faulty].Node)
		}
	}
	if !skipped {
		t.Error("the demo's staging cluster does not show NodesSkipped for the node")
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
	lacking := false
	for i := range clusters {
		for _, cond := range clusters[i].Conditions {
			lacking = lacking || clusters[i].Name == "ci" && cond.Type == provisioner.NodesLackMemoryCondition && cond.Status == "True"
		}
	}
	if !lacking {
		t.Error("the demo's ci cluster does not show NodesLackMemory")
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
	cleanup, err := SetUpDemo(s, func() time.Time { return demoAt })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
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

// The demo's staging cluster sets labels and a taint on its pool, one node has a taint besides, and the
// restricted level would refuse its pods.
func TestDemoShowsPoolLabelsTaintsAndPodChecks(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	var h healthView
	decodeDemo(t, b.do("GET", "/api/clusters/"+demoNamespace+"/staging/health", ""), &h)
	var notes []string
	for _, a := range h.Attention {
		if strings.Contains(a.What, "taints differ") {
			notes = append(notes, a.What+" "+a.Message)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "pool "+workerPool) || !strings.Contains(notes[0], "maintenance") {
		t.Errorf("taint notes %q, want one naming the pool and the taint", notes)
	}
	var check struct {
		Checked int               `json:"checked"`
		Refused []json.RawMessage `json:"refused"`
	}
	decodeDemo(t, b.do("GET", "/api/clusters/"+demoNamespace+"/staging/pod-security-check", ""), &check)
	if check.Checked == 0 || len(check.Refused) == 0 {
		t.Errorf("pod check %+v, want pods checked and some refused", check)
	}
}

func TestDemoShowsTemplatesAndPins(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()

	var setup setupView
	decodeDemo(t, b.do("GET", "/api/setup", ""), &setup)
	if !setup.Appliance || !setup.Ready || !setup.Allowed || setup.Settings == nil ||
		setup.Settings.TemplateRefresh == nil || setup.Settings.TemplateRefresh.KubeadmPatches == nil || !*setup.Settings.TemplateRefresh.KubeadmPatches {
		t.Errorf("the demo's setup is %+v, want an appliance with the kubeadm patch builds on", setup)
	}

	var list templatesView
	decodeDemo(t, b.do("GET", "/api/templates", ""), &list)
	var older []string
	for _, tpl := range list.Templates {
		if tpl.Older {
			older = append(older, tpl.Name)
		}
		if tpl.Older && !tpl.Newest {
			t.Errorf("%s is older but not the line's newest build", tpl.Name)
		}
	}
	if len(older) != 1 || !strings.HasPrefix(older[0], "kubehenge-rke2-") {
		t.Errorf("builds marked older: %v, want the newest RKE2 build alone", older)
	}
	if list.Refresh == nil || list.Refresh.Last == nil || list.Refresh.Last.State != "succeeded" {
		t.Errorf("the refresh is %+v, want one that ran", list.Refresh)
	}

	var pins releasePinsView
	decodeDemo(t, b.do("GET", "/api/release-pins", ""), &pins)
	inUse := map[string]string{}
	for _, p := range pins.Pins {
		inUse[p.Version] = p.InUse
		if p.By == "" || p.At == nil || p.File == "" || p.Script == "" {
			t.Errorf("pin %+v lacks who added it, when or a digest", p)
		}
	}
	if len(pins.Pins) != 2 || !pins.Checks || inUse["v1.36.5+rke2r1"] == "" || inUse["v1.37.2+rke2r1"] != "" {
		t.Errorf("pins %+v, want two with one in use by a cluster", pins)
	}

	var checked releasePinView
	decodeDemo(t, b.do("POST", "/api/release-pins/check", `{"version":"v1.37.3+rke2r1"}`), &checked)
	if len(checked.File) != sha256.Size*2 || len(checked.Script) != sha256.Size*2 {
		t.Errorf("the check of a release shows %+v, want two digests", checked)
	}
	if rec := b.do("POST", "/api/release-pins", `{"version":"v1.37.3+rke2r1"}`); rec.Code != http.StatusForbidden {
		t.Errorf("pinning in the demo: %d %s", rec.Code, rec.Body)
	}
}

// A Talos cluster's machines report the Talos release their nodes run, so that column is never empty.
func TestDemoShowsTheTalosReleaseOfTalosMachines(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	var detail struct {
		Machines []machineView `json:"machines"`
	}
	decodeDemo(t, b.do("GET", "/api/clusters/"+demoNamespace+"/platform", ""), &detail)
	if len(detail.Machines) == 0 {
		t.Fatal("platform has no machines")
	}
	for _, m := range detail.Machines {
		if m.Talos != templates.DefaultTalosVersion {
			t.Errorf("machine %s: Talos %q, want %q", m.Name, m.Talos, templates.DefaultTalosVersion)
		}
	}
}

func TestDemoHoldsOneMachinesReplacement(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	var detail struct {
		Machines []machineView `json:"machines"`
	}
	decodeDemo(t, b.do("GET", "/api/clusters/"+demoNamespace+"/platform", ""), &detail)
	var held []machineView
	for _, m := range detail.Machines {
		if m.Held != "" {
			held = append(held, m)
		}
	}
	if len(held) != 1 || held[0].Held != "Proxmox VE node pve3 is in maintenance" || held[0].HeldUntil.IsZero() {
		t.Fatalf("held machines %+v", held)
	}
	var health struct {
		Attention []attentionView `json:"attention"`
	}
	decodeDemo(t, b.do("GET", "/api/clusters/"+demoNamespace+"/platform/health", ""), &health)
	if !slices.ContainsFunc(health.Attention, func(a attentionView) bool { return a.What == "Replacement waits" }) {
		t.Errorf("platform's health: %+v", health.Attention)
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
