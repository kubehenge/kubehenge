package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// --check only reads: it names the first Talos pin its registry no longer serves, prints no
// success and leaves the file as it was.
func TestPinsCheckWritesNothing(t *testing.T) { //nolint:paralleltest // points the package's registry address at its server
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	saved := pinsSources
	pinsSources.registry = srv.URL
	t.Cleanup(func() { pinsSources = saved })

	raw, err := os.ReadFile(filepath.Join("..", "provisioner", "releasepins.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var oldest string
	for line := range strings.Lines(string(raw)) {
		if fields := strings.Fields(line); len(fields) == 5 && !strings.HasPrefix(line, "#") {
			oldest = fields[0]
			break
		}
	}
	file := filepath.Join(t.TempDir(), "releasepins.txt")
	if err = os.WriteFile(file, raw, 0o600); err != nil { //nolint:gosec // the test's own temporary file
		t.Fatal(err)
	}
	out, err := run(t, "release", "pins", "--check", file)
	if oldest == "" || err == nil || !strings.Contains(err.Error(), oldest) || !strings.Contains(err.Error(), "imager") {
		t.Errorf("--check gave %v, want an error naming the oldest Talos release %q and imager", err, oldest)
	}
	if strings.Contains(out, "checked") {
		t.Errorf("--check printed %q on a failure", out)
	}
	after, err := os.ReadFile(file) //nolint:gosec // the test's own temporary file
	if err != nil || !bytes.Equal(after, raw) {
		t.Errorf("--check changed the file: %v", err)
	}
}

// Pinning reads Sidero's images from the registry address the command holds.
func TestPinsReadsTheRegistryForANewTalosRelease(t *testing.T) { //nolint:paralleltest // points the package's addresses at its server
	var registryReads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/"):
			registryReads.Add(1)
			http.NotFound(w, r)
		case r.URL.Path == "/repos/siderolabs/talos/releases" && r.URL.Query().Get("page") == "1":
			_, _ = w.Write([]byte(`[{"tag_name":"v1.14.6"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	saved := pinsSources
	pinsSources.api, pinsSources.github, pinsSources.raw, pinsSources.registry = srv.URL, srv.URL, srv.URL, srv.URL
	t.Cleanup(func() { pinsSources = saved })
	file := filepath.Join(t.TempDir(), "releasepins.txt")
	if err := os.WriteFile(file, []byte("# header\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, "release", "pins", file)
	if err == nil || !strings.Contains(err.Error(), "v1.14.6") || registryReads.Load() == 0 {
		t.Errorf("pins gave %v after %d registry reads, want an error from the registry for v1.14.6", err, registryReads.Load())
	}
}

// A pin never changes, so a run that fails part way keeps the pins file whole for the next one.
func TestPinsKeepsThePinsWhenGitHubFails(t *testing.T) { //nolint:paralleltest // points the package's GitHub addresses at its server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer srv.Close()
	saved := pinsSources
	pinsSources.api, pinsSources.github, pinsSources.raw = srv.URL, srv.URL, srv.URL
	t.Cleanup(func() { pinsSources = saved })

	raw, err := os.ReadFile(filepath.Join("..", "provisioner", "releasepins.txt"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := provisioner.ParseReleasePins(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	// The pins in reverse: only a file written again comes out sorted.
	header := provisioner.PinsHeader(string(raw))
	lines := strings.Split(strings.TrimSpace(strings.TrimPrefix(string(raw), header)), "\n")
	slices.Reverse(lines)
	file := filepath.Join(t.TempDir(), "releasepins.txt")
	if err = os.WriteFile(file, []byte(header+strings.Join(lines, "\n")+"\n"), 0o600); err != nil { //nolint:gosec // the test's own temporary file
		t.Fatal(err)
	}

	out, err := run(t, "release", "pins", file)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("pins gave %v, want GitHub's 500", err)
	}
	if strings.Contains(out, "pinned") {
		t.Errorf("pins printed %q with nothing pinned", out)
	}
	written, err := os.ReadFile(file) //nolint:gosec // the test's own temporary file
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != before.Format(header) {
		t.Errorf("the pins file was not written again before the error:\n%s", written)
	}
}
