package pve

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

// The pool is checked rather than the connections a burst of requests opens, which is Go's to
// decide: a request that finds no connection idle dials one at once, and under load the dial can
// beat a connection being handed back.
func TestClientsShareTheirConnections(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := func(caFile string) *http.Transport {
		t.Helper()
		_, c, err := Config{URL: srv.URL, CAFile: caFile}.transport()
		if err != nil {
			t.Fatal(err)
		}
		p, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport %T", c.Transport)
		}
		return p
	}
	// One client per cluster, as the controller makes them.
	first, second, system := pool(ca), pool(ca), pool("")
	if first != second {
		t.Error("two clients of one Proxmox VE have a connection pool each")
	}
	if first == system {
		t.Error("clients trusting different CAs share a connection pool")
	}
	// The interface's eight clusters at once and the controller's four reconciles.
	if first.MaxIdleConnsPerHost < 12 {
		t.Errorf("%d idle connections kept, fewer than are in use at once", first.MaxIdleConnsPerHost)
	}
	c, err := Connect(Config{URL: srv.URL, Token: "kubehenge@pve!controller=s3cret", CAFile: ca}) //nolint:gosec // test data
	if err != nil {
		t.Fatal(err)
	}
	var reused atomic.Int32
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		if i.Reused {
			reused.Add(1)
		}
	}})
	for range 10 {
		var v map[string]any
		if err := c.Get(ctx, "/version", &v); err != nil {
			t.Fatal(err)
		}
	}
	if reused.Load() == 0 {
		t.Error("ten requests one after another reused no connection")
	}
}

func TestConnectedClientFailsOnEveryErrorStatus(t *testing.T) {
	t.Parallel()
	statuses := map[string]int{
		"/api2/json/version":   http.StatusOK,
		"/api2/json/proxy":     http.StatusBadGateway,
		"/api2/json/busy":      http.StatusServiceUnavailable,
		"/api2/json/node-gone": 595,
		"/api2/json/denied":    http.StatusForbidden,
		"/api2/json/missing":   http.StatusInternalServerError,
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statuses[r.URL.Path])
		_, _ = w.Write([]byte(`{"data":null}`))
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Connect(Config{URL: srv.URL, Token: "kubehenge@pve!controller=s3cret", CAFile: ca}) //nolint:gosec // test data
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := c.Put(ctx, "/version", map[string]any{}, nil); err != nil {
		t.Errorf("a 200 failed: %v", err)
	}
	for _, path := range []string{"/proxy", "/busy", "/node-gone"} {
		err := c.Delete(ctx, path, nil)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !strings.HasPrefix(apiErr.Status, strconv.Itoa(statuses["/api2/json"+path])) {
			t.Errorf("%s: got %v, want the status as an error", path, err)
		}
	}
	if err := c.Delete(ctx, "/denied", nil); !proxmox.IsNotAuthorized(err) {
		t.Errorf("a 403 gave %v, want go-proxmox's own not-authorised error", err)
	}
	if err := c.Delete(ctx, "/missing", nil); err == nil || !strings.HasPrefix(err.Error(), "500") {
		t.Errorf("a 500 gave %v, want go-proxmox's status line", err)
	}
}
