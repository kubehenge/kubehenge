package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// fakeClusterAPI answers the interface's cluster API for the token dev@pve!ci=secret, and records
// each change it is sent.
type fakeClusterAPI struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeClusterAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "PVEAPIToken=dev@pve!ci=secret" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Proxmox VE refused the API token"}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		f.mu.Lock()
		f.sent = append(f.sent, r.Method+" "+r.URL.Path+" "+string(body))
		f.mu.Unlock()
	}
	if strings.HasPrefix(r.URL.Path, "/api/clusters/clusters/web/machines/") {
		f.mu.Lock()
		f.sent = append(f.sent, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/clusters":
		_, _ = io.WriteString(w, `[
			{"namespace":"clusters","name":"web","distribution":"talos","version":"v1.34.1","phase":"Provisioned",
			 "controlPlane":{"desired":3,"ready":3},"workers":{"desired":2,"ready":1}},
			{"namespace":"a","name":"twin","phase":"Provisioned"},{"namespace":"b","name":"twin","phase":"Provisioned"}]`)
	case "GET /api/clusters/clusters/web":
		_, _ = io.WriteString(w, `{"namespace":"clusters","name":"web"}`)
	case "GET /api/clusters/clusters/web/kubeconfig":
		_, _ = io.WriteString(w, "apiVersion: v1\nkind: Config\n")
	case "PATCH /api/clusters/clusters/web", "DELETE /api/clusters/clusters/web":
		_, _ = io.WriteString(w, `{}`)
	case "POST /api/clusters":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"namespace":"clusters","name":"new"}`)
	case "PATCH /api/clusters/b/twin":
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"you need VM.Allocate on pool b"}`)
	default:
		http.NotFound(w, r)
	}
}

func runCluster(t *testing.T, url, token string, args ...string) (string, error) {
	t.Helper()
	cmd := newClusterCommand(&pve.Config{Token: token})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"--url", url}, args...))
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestClusterCommandsTalkToTheInterfaceAsTheToken(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

	out, err := runCluster(t, srv.URL, token, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "CONTROL PLANES") || !strings.Contains(out, "web") || !strings.Contains(out, "3/3") ||
		!strings.Contains(out, "1/2") {
		t.Errorf("list =\n%s", out)
	}
	if out, err = runCluster(t, srv.URL, token, "get", "web"); err != nil || !strings.Contains(out, `"name": "web"`) {
		t.Errorf("get = %q, %v", out, err)
	}
	if out, err = runCluster(t, srv.URL, token, "kubeconfig", "web"); err != nil || out != "apiVersion: v1\nkind: Config\n" {
		t.Errorf("kubeconfig = %q, %v", out, err)
	}
	if _, err = runCluster(t, srv.URL, token, "scale", "web", "--control-planes", "5", "--workers", "md-0=4"); err != nil {
		t.Fatal(err)
	}
	if _, err = runCluster(t, srv.URL, token, "delete", "web"); err == nil {
		t.Error("delete went ahead without --yes")
	}
	if _, err = runCluster(t, srv.URL, token, "delete", "web", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err = runCluster(t, srv.URL, token, "machine", "delete", "web", "web-md-0-a"); err == nil {
		t.Error("a machine was deleted without --yes")
	}
	for _, args := range [][]string{
		{"replace", "web", "web-cp-1", "--yes"},
		{"keep", "web", "web-cp-1"},
		{"delete", "web", "web-md-0-a", "--yes"},
		{"cordon", "web", "web-md-0-b"},
		{"uncordon", "web", "web-md-0-b"},
		{"reboot", "web", "web-md-0-b"},
	} {
		if out, err = runCluster(t, srv.URL, token, append([]string{"machine"}, args...)...); err != nil || !strings.HasPrefix(out, "machine "+args[2]+" ") {
			t.Errorf("machine %v = %q, %v", args, out, err)
		}
	}
	file := filepath.Join(t.TempDir(), "new.yaml")
	if err = os.WriteFile(file, []byte("name: new\nclass: talos\ncontrolPlaneReplicas: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err = runCluster(t, srv.URL, token, "create", "-f", file); err != nil || out != "cluster clusters/new created\n" {
		t.Errorf("create = %q, %v", out, err)
	}

	fake.mu.Lock()
	sent := fake.sent
	fake.mu.Unlock()
	want := []string{
		`PATCH /api/clusters/clusters/web {"controlPlaneReplicas":5,"workerPools":{"md-0":4}}`,
		`DELETE /api/clusters/clusters/web {"confirm":"web"}`,
		"PUT /api/clusters/clusters/web/machines/web-cp-1/replacement",
		"DELETE /api/clusters/clusters/web/machines/web-cp-1/replacement",
		"DELETE /api/clusters/clusters/web/machines/web-md-0-a",
		"PUT /api/clusters/clusters/web/machines/web-md-0-b/cordon",
		"DELETE /api/clusters/clusters/web/machines/web-md-0-b/cordon",
		"POST /api/clusters/clusters/web/machines/web-md-0-b/reboot",
		`POST /api/clusters {"class":"talos","controlPlaneReplicas":1,"name":"new"}`,
	}
	if strings.Join(sent, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent\n%s\nwant\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

func TestClusterCommandsSayWhatWentWrong(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(&fakeClusterAPI{})
	t.Cleanup(srv.Close)

	for _, c := range []struct {
		token string
		args  []string
		want  string
	}{
		{"dev@pve!ci=guess", []string{"list"}, "Proxmox VE refused the API token (401)"},
		{"", []string{"list"}, "set KUBEHENGE_PROXMOX_TOKEN"},
		{"dev@pve!ci=secret", []string{"get", "gone"}, "no cluster gone"},
		{"dev@pve!ci=secret", []string{"get", "twin"}, "clusters named twin are in a, b: give --namespace"},
		{"dev@pve!ci=secret", []string{"scale", "twin", "-n", "b", "--workers", "md-0=1"}, "you need VM.Allocate on pool b (403)"},
		{"dev@pve!ci=secret", []string{"scale", "web", "--workers", "md-0"}, `--workers "md-0": give POOL=N`},
		{"dev@pve!ci=secret", []string{"scale", "web"}, "give --control-planes, --workers or both"},
		{"dev@pve!ci=secret", []string{"list", "-o", "yaml"}, `--output is json or nothing, not "yaml"`},
	} {
		_, err := runCluster(t, srv.URL, c.token, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: error %v, want %q", c.args, err, c.want)
		}
	}
	if out, err := runCluster(t, srv.URL, "dev@pve!ci=secret", "list", "-o", "json"); err != nil || !json.Valid([]byte(out)) {
		t.Errorf("list -o json = %q, %v", out, err)
	}
}

func TestClusterListChecksItsFormatBeforeAsking(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	for _, token := range []string{"dev@pve!ci=secret", ""} {
		_, err := runCluster(t, srv.URL, token, "list", "-o", "yaml")
		if err == nil || err.Error() != `--output is json or nothing, not "yaml"` {
			t.Errorf("token %q: err = %v", token, err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests went out before --output was checked", n)
	}
}

func TestOnlyTheCommandsFindingAClusterTakeANamespace(t *testing.T) {
	t.Parallel()
	cmd := newClusterCommand(&pve.Config{})
	for _, path := range [][]string{{"get"}, {"kubeconfig"}, {"scale"}, {"delete"}, {"machine", "reboot"}} {
		if sub, _, err := cmd.Find(path); err != nil || sub.Flag("namespace") == nil {
			t.Errorf("%q has no --namespace: %v", path, err)
		}
	}
	for _, path := range [][]string{{"list"}, {"create"}} {
		if sub, _, err := cmd.Find(path); err != nil || sub.Flag("namespace") != nil {
			t.Errorf("%q takes --namespace: %v", path, err)
		}
	}
}
