package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// etcdSnapshotsAPI answers the interface's snapshot routes for cluster web, its snapshots on the
// backup volume, and records the changes it is sent.
type etcdSnapshotsAPI struct {
	mu   sync.Mutex
	sent []string
}

func (f *etcdSnapshotsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.sent = append(f.sent, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
	f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /api/clusters/clusters/web":
		_, _ = io.WriteString(w, `{"namespace":"clusters","name":"web"}`)
	case "GET /api/clusters/clusters/web/etcd-snapshots":
		_, _ = io.WriteString(w, `{"enabled":true,"last":{"name":"b.db.age","time":"2026-11-02T06:00:00Z"},
			"requested":false,"snapshots":[{"name":"b.db.age","time":"2026-11-02T06:00:00Z","size":4096},
			{"name":"a.db.age","time":"2026-11-01T06:00:00Z","size":2048}]}`)
	case "POST /api/clusters/clusters/web/etcd-snapshots", "POST /api/clusters/clusters/web/etcd-restores":
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{}`)
	case "GET /api/clusters/clusters/web/etcd-snapshots/b.db.age":
		_, _ = io.WriteString(w, "etcd database")
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"no such snapshot"}`)
	}
}

// The CLI lists where a cluster's snapshots are kept and when the last was taken, takes one,
// downloads one and restores from one, as the interface does.
func TestClusterEtcdSnapshots(t *testing.T) {
	t.Parallel()
	api := &etcdSnapshotsAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

	out, err := runCluster(t, srv.URL, token, "etcd-snapshots", "list", "web", "-n", "clusters")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Kept on:     the backup volume", "Last taken:  2026-11-02T06:00:00Z", "b.db.age", "a.db.age"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if _, err := runCluster(t, srv.URL, token, "etcd-snapshots", "take", "web", "-n", "clusters"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "web.db")
	if _, err := runCluster(t, srv.URL, token, "etcd-snapshots", "download", "web", "b.db.age", "--out", file, "-n", "clusters"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "etcd database" { //nolint:gosec // the test's own temporary file
		t.Errorf("downloaded %q: %v", got, err)
	}
	if _, err := runCluster(t, srv.URL, token, "etcd-snapshots", "download", "web", "b.db.age", "--out", file, "-n", "clusters"); err == nil {
		t.Error("overwrote a file")
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := runCluster(t, srv.URL, token, "etcd-snapshots", "download", "web", "gone.db.age", "--out", missing, "-n", "clusters"); err == nil || !strings.Contains(err.Error(), "no such snapshot") {
		t.Errorf("a missing snapshot: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("left a file for a missing snapshot: %v", err)
	}
	if _, err := runCluster(t, srv.URL, token, "etcd-snapshots", "restore", "web", "a.db.age", "-n", "clusters"); err == nil {
		t.Error("restored without --yes")
	}
	if _, err := runCluster(t, srv.URL, token, "etcd-snapshots", "restore", "web", "a.db.age", "--yes", "-n", "clusters"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, want := range []string{
		"POST /api/clusters/clusters/web/etcd-snapshots",
		`POST /api/clusters/clusters/web/etcd-restores {"confirm":"web","snapshot":"a.db.age"}`,
	} {
		if !strings.Contains(strings.Join(api.sent, "\n"), want) {
			t.Errorf("sent no %q: %q", want, api.sent)
		}
	}
}
