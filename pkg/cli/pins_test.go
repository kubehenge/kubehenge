package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

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
