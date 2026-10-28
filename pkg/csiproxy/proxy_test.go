package csiproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const token = "PVEAPIToken=kubehenge.team-a.web@pve!cluster=s3cret" //nolint:gosec // test data

// fakePVE is the Proxmox VE API as the cluster's token sees it, recording each call it gets as
// "METHOD path token".
type fakePVE struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	f.mu.Unlock()
	switch r.URL.Path {
	case "/api2/json/storage":
		writeData(w, []map[string]any{{"storage": "local", "type": "dir"}, {"storage": "local-zfs", "type": "zfspool", "sparse": 1}})
	case "/api2/json/nodes/pve1/storage/local-zfs/content":
		writeData(w, []map[string]any{{"volid": "local-zfs:vm-4242-pvc-1"}, {"volid": "local-zfs:vm-4242-snapshot-1"}})
	case "/api2/json/version":
		writeData(w, map[string]any{"version": "9.1"})
	default:
		writeErrors(w, http.StatusForbidden, map[string]string{"permission": "denied"})
	}
}

func (f *fakePVE) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newProxy(t *testing.T, objs ...client.Object) (*httptest.Server, *fakePVE, client.Client) {
	t.Helper()
	pve := &fakePVE{}
	upstream := httptest.NewServer(pve)
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	p := &Proxy{Upstream: u, Transport: http.DefaultTransport, Client: c, Poll: 10 * time.Millisecond, Log: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return srv, pve, c
}

// answer is a Proxmox VE answer as the proxy gives it.
type answer[T any] struct {
	Data   T                 `json:"data"`
	Errors map[string]string `json:"errors"`
}

func call[T any](t *testing.T, srv *httptest.Server, method, path string, form url.Values) (int, answer[T]) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out answer[T]
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s %s answered %s", method, path, body)
	}
	return resp.StatusCode, out
}

// controller answers each copy request the way the controller in the management cluster does,
// with state and message, until the test ends; it returns what reads the requests it saw.
func controller(t *testing.T, c client.Client, state, message string) func() []Copy {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var seen []Copy
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			list := &corev1.ConfigMapList{}
			if err := c.List(ctx, list, client.InNamespace(Namespace), client.HasLabels{RequestLabel}); err == nil {
				for i := range list.Items {
					cm := &list.Items[i]
					if r := ParseRequest(cm); r.State == "" {
						mu.Lock()
						seen = append(seen, r.Copy)
						mu.Unlock()
						Record(cm, state, "UPID:pve1:0001:copy", message)
						_ = c.Update(ctx, cm)
					}
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return func() []Copy {
		mu.Lock()
		defer mu.Unlock()
		return append([]Copy(nil), seen...)
	}
}

func requestsLeft(t *testing.T, c client.Client) int {
	t.Helper()
	list := &corev1.ConfigMapList{}
	if err := c.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

func TestProxyPassesOtherCallsOnWithTheirToken(t *testing.T) {
	t.Parallel()
	srv, pve, _ := newProxy(t)
	if status, body := call[map[string]any](t, srv, http.MethodGet, "/api2/json/version", nil); status != http.StatusOK || body.Data["version"] != "9.1" {
		t.Errorf("version: %d %v", status, body)
	}
	if status, _ := call[any](t, srv, http.MethodDelete, "/api2/json/nodes/pve1/storage/local-zfs/content/vm-4242-pvc-1", nil); status != http.StatusForbidden {
		t.Errorf("a delete the token may not make answered %d", status)
	}
	want := []string{
		"GET /api2/json/version " + token,
		"DELETE /api2/json/nodes/pve1/storage/local-zfs/content/vm-4242-pvc-1 " + token,
	}
	if got := pve.seen(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestProxyReadsAStoragesConfigurationFromTheList(t *testing.T) {
	t.Parallel()
	srv, pve, _ := newProxy(t)
	status, body := call[map[string]any](t, srv, http.MethodGet, "/api2/json/storage/local-zfs", nil)
	if status != http.StatusOK || body.Data["storage"] != "local-zfs" || body.Data["type"] != "zfspool" {
		t.Errorf("storage configuration: %d %v", status, body)
	}
	if status, _ := call[any](t, srv, http.MethodGet, "/api2/json/storage/ceph", nil); status != http.StatusForbidden {
		t.Errorf("a storage the list does not show answered %d, want Proxmox VE's own answer", status)
	}
	if got := pve.seen()[0]; got != "GET /api2/json/storage "+token {
		t.Errorf("the list was read with %q", got)
	}
}

func TestProxyHidesCopiesUnderWay(t *testing.T) {
	t.Parallel()
	under := NewRequest(&Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-1"})
	under.Name = "copy-1"
	elsewhere := NewRequest(&Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-1", TargetNode: "pve2"})
	elsewhere.Name = "copy-2"
	srv, _, c := newProxy(t, under, elsewhere)
	volumes := func() string {
		status, body := call[[]struct {
			VolID string `json:"volid"`
		}](t, srv, http.MethodGet, "/api2/json/nodes/pve1/storage/local-zfs/content", nil)
		if status != http.StatusOK {
			t.Fatalf("content: %d %v", status, body)
		}
		ids := make([]string, 0, len(body.Data))
		for _, e := range body.Data {
			ids = append(ids, e.VolID)
		}
		return strings.Join(ids, ",")
	}
	if got := volumes(); got != "local-zfs:vm-4242-pvc-1" {
		t.Errorf("content with the copy under way: %s", got)
	}
	Record(under, StateDone, "UPID:pve1:0001:copy", "")
	if err := c.Update(t.Context(), under); err != nil {
		t.Fatal(err)
	}
	if got := volumes(); got != "local-zfs:vm-4242-pvc-1,local-zfs:vm-4242-snapshot-1" {
		t.Errorf("content once the copy is done: %s", got)
	}
}

func TestProxyCopiesThroughTheController(t *testing.T) {
	t.Parallel()
	srv, pve, c := newProxy(t)
	seen := controller(t, c, StateDone, "")
	form := url.Values{"target": {"vm-4242-snapshot-1.raw"}, "target_node": {"pve2"}}
	status, body := call[any](t, srv, http.MethodPost, "/api2/json/nodes/pve1/storage/local/content/local:4242/vm-4242-pvc-1.raw", form)
	if status != http.StatusOK || body.Data != nil {
		t.Errorf("copy: %d %v", status, body)
	}
	want := Copy{Node: "pve1", Storage: "local", Volume: "4242/vm-4242-pvc-1.raw", Target: "vm-4242-snapshot-1.raw", TargetNode: "pve2"}
	if got := seen(); len(got) != 1 || got[0] != want {
		t.Errorf("requests %+v, want %+v", got, want)
	}
	if calls := pve.seen(); len(calls) != 0 {
		t.Errorf("the copy reached Proxmox VE with the token: %v", calls)
	}
	if n := requestsLeft(t, c); n != 0 {
		t.Errorf("%d requests left behind", n)
	}
}

func TestProxyReportsARefusedCopy(t *testing.T) {
	t.Parallel()
	srv, _, c := newProxy(t)
	controller(t, c, StateFailed, "volume vm-100-disk-0 is not one of the cluster's own")
	status, body := call[any](t, srv, http.MethodPost, "/api2/json/nodes/pve1/storage/local-zfs/content/vm-100-disk-0", url.Values{"target": {"vm-4242-snapshot-1"}})
	if status != http.StatusBadRequest || !strings.Contains(body.Errors["copy"], "not one of the cluster's own") {
		t.Errorf("copy: %d %v", status, body)
	}
}

func TestProxyWaitsOnACopyAskedForAlready(t *testing.T) {
	t.Parallel()
	asked := NewRequest(&Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-1"})
	asked.Name = "copy-1"
	Record(asked, StateCopying, "UPID:pve1:0001:copy", "")
	srv, _, c := newProxy(t, asked)
	finished := asked.DeepCopy()
	go func() {
		time.Sleep(50 * time.Millisecond)
		Record(finished, StateDone, "UPID:pve1:0001:copy", "")
		_ = c.Update(context.Background(), finished)
	}()
	status, body := call[any](t, srv, http.MethodPost, "/api2/json/nodes/pve1/storage/local-zfs/content/vm-4242-pvc-1", url.Values{"target": {"vm-4242-snapshot-1"}})
	if status != http.StatusOK {
		t.Errorf("copy: %d %v", status, body)
	}
	if n := requestsLeft(t, c); n != 0 {
		t.Errorf("%d requests: a second one, or the first left behind", n)
	}
}

func TestProxyRefusesACopyWithoutATarget(t *testing.T) {
	t.Parallel()
	srv, _, c := newProxy(t)
	if status, _ := call[any](t, srv, http.MethodPost, "/api2/json/nodes/pve1/storage/local-zfs/content/vm-4242-pvc-1", url.Values{}); status != http.StatusBadRequest {
		t.Errorf("copy without a target: %d", status)
	}
	if n := requestsLeft(t, c); n != 0 {
		t.Errorf("%d requests without a target", n)
	}
}
