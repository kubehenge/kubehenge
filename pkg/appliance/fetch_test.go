package appliance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyServer answers the first fails requests with fail and the rest with body.
func flakyServer(t *testing.T, fails int32, fail func(w http.ResponseWriter), body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= fails {
			fail(w)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func status(code int) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

// cutOff sends half of what it promises and drops the connection, as a proxy that resets it does.
func cutOff(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "1000")
	_, _ = w.Write([]byte(strings.Repeat("x", 500)))
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

// A busy or failing server, and one that drops the connection, are tried again.
func TestDownloadsAreTriedAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fail func(w http.ResponseWriter)
	}{
		{"bad gateway", status(http.StatusBadGateway)},
		{"gateway timeout", status(http.StatusGatewayTimeout)},
		{"too many requests", status(http.StatusTooManyRequests)},
		{"connection dropped", cutOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := []byte("the signed index")
			srv, requests := flakyServer(t, 1, tc.fail, body)
			got, err := get(t.Context(), srv.Client(), srv.URL+"/index.json", 1<<20)
			if err != nil || string(got) != string(body) {
				t.Fatalf("get = %q, %v", got, err)
			}
			if n := requests.Load(); n != 2 {
				t.Errorf("%d requests, want 2", n)
			}

			sum := sha256.Sum256(body)
			srv, requests = flakyServer(t, 1, tc.fail, body)
			file, err := download(t.Context(), srv.Client(), srv.URL+"/release.tar.gz", hex.EncodeToString(sum[:]), t.TempDir())
			if err != nil {
				t.Fatalf("download = %v", err)
			}
			if raw, err := os.ReadFile(file); err != nil || string(raw) != string(body) {
				t.Errorf("downloaded %q, %v", raw, err)
			}
			if n := requests.Load(); n != 2 {
				t.Errorf("%d requests to download, want 2", n)
			}
		})
	}
}

// A refusal, a wrong checksum and a server that keeps failing end the download.
func TestDownloadsFailForGood(t *testing.T) {
	t.Parallel()
	srv, requests := flakyServer(t, 1, status(http.StatusNotFound), nil)
	if _, err := get(t.Context(), srv.Client(), srv.URL+"/index.json", 1<<20); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("get of a missing file = %v", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests for a missing file, want 1", n)
	}

	srv, requests = flakyServer(t, 0, nil, []byte("tampered"))
	dir := t.TempDir()
	if _, err := download(t.Context(), srv.Client(), srv.URL+"/release.tar.gz", strings.Repeat("0", 64), dir); err == nil || !strings.Contains(err.Error(), "does not match the checksum") {
		t.Errorf("download of a tampered file = %v", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests for a tampered file, want 1", n)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left %d files behind", len(left))
	}

	srv, requests = flakyServer(t, downloadAttempts, status(http.StatusServiceUnavailable), nil)
	if _, err := get(t.Context(), srv.Client(), srv.URL+"/index.json", 1<<20); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("get from a server that keeps failing = %v", err)
	}
	if n := requests.Load(); n != downloadAttempts {
		t.Errorf("%d requests to a server that keeps failing, want %d", n, downloadAttempts)
	}
}

// A download that receives nothing for stallTimeout ends and is tried again, however long a
// download that keeps receiving takes.
func TestStalledDownloadsAreTriedAgain(t *testing.T) { //nolint:paralleltest // shortens stallTimeout, which every download reads
	saved := stallTimeout
	stallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { stallTimeout = saved })
	var requests atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte("half"))
			flusher.Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		// Slower in all than stallTimeout, but never silent that long.
		for range 5 {
			_, _ = w.Write([]byte("tick"))
			flusher.Flush()
			time.Sleep(stallTimeout / 2)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	got, err := get(t.Context(), srv.Client(), srv.URL+"/index.json", 1<<20)
	if err != nil || string(got) != strings.Repeat("tick", 5) {
		t.Fatalf("get = %q, %v", got, err)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
	if !errors.Is(transientError{errStalled}, errStalled) {
		t.Error("a stall is not told as one")
	}
}
