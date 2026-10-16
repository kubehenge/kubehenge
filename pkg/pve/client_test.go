package pve

import (
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
)

func TestClientsShareTheirConnections(t *testing.T) {
	t.Parallel()
	var opened atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	// Two clients, as the controller makes one per cluster, each asking four things at once, three
	// times over.
	clients := make([]*proxmox.Client, 0, 2)
	for range cap(clients) {
		c, err := Connect(Config{URL: srv.URL, Token: "kubehenge@pve!controller=s3cret", CAFile: ca}) //nolint:gosec // test data
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	for range 3 {
		var wg sync.WaitGroup
		for _, c := range clients {
			for range 4 {
				wg.Go(func() {
					var v map[string]any
					if err := c.Get(context.Background(), "/version", &v); err != nil {
						t.Error(err)
					}
				})
			}
		}
		wg.Wait()
	}
	if n := opened.Load(); n > 8 {
		t.Fatalf("%d connections for 24 requests, at most eight at a time", n)
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
