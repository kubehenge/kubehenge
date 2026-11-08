package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingAPI answers the one cluster "web" and records each request that changes it.
type recordingAPI struct {
	mu   sync.Mutex
	sent []string
}

func (f *recordingAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, `[{"namespace":"clusters","name":"web"}]`)
		return
	}
	f.mu.Lock()
	f.sent = append(f.sent, r.Method+" "+r.URL.Path+" "+string(body))
	f.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{}`)
}

func TestClusterAddonCommandsAskTheInterface(t *testing.T) {
	t.Parallel()
	api := &recordingAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

	if out, err := runCluster(t, srv.URL, token, "apply-addon-changes", "web"); err != nil || !strings.Contains(out, "web applies its add-on changes") {
		t.Errorf("apply-addon-changes = %q, %v", out, err)
	}
	if _, err := runCluster(t, srv.URL, token, "namespace-labels", "web", "--label", "team=platform", "--label", "tier=infra"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCluster(t, srv.URL, token, "namespace-labels", "web", "--clear"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /api/clusters/clusters/web/addon-changes ",
		`PATCH /api/clusters/clusters/web {"variables":{"namespaceLabels":{"team":"platform","tier":"infra"}}}`,
		`PATCH /api/clusters/clusters/web {"variables":{"namespaceLabels":{}}}`,
	}
	api.mu.Lock()
	sent := strings.Join(api.sent, "\n")
	api.mu.Unlock()
	if sent != strings.Join(want, "\n") {
		t.Errorf("sent\n%s\nwant\n%s", sent, strings.Join(want, "\n"))
	}
}

func TestNamespaceLabelsCommandChecksItsFlagsBeforeAsking(t *testing.T) {
	t.Parallel()
	api := &recordingAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

	for args, want := range map[string]string{
		"--label no-equals":             "KEY=VALUE",
		"--label =value":                "KEY=VALUE",
		"":                              "give --label",
		"--clear --label team=platform": "not both",
	} {
		_, err := runCluster(t, srv.URL, token, append([]string{"namespace-labels", "web"}, strings.Fields(args)...)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want one saying %q", args, err, want)
		}
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sent) != 0 {
		t.Errorf("a refused flag reached the interface: %v", api.sent)
	}
}
