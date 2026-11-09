package cli

import (
	"bytes"
	"encoding/base64"
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

	"filippo.io/age"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
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
			{"namespace":"a","name":"twin","phase":"Provisioned","conditions":[
			 {"type":"UpgradeHeld","status":"True","reason":"RancherKubernetes","message":"for Rancher, choose Kubernetes 1.34 to 1.36, not v1.37.1+k3s1"}]},
			{"namespace":"b","name":"twin","phase":"Provisioned","conditions":[
			 {"type":"NodeImageHeld","status":"True","reason":"TemplateNodeDown","message":"Build build-202610150000 is on node pve2, which is down"}]},
			{"namespace":"c","name":"stuck","phase":"Provisioned","conditions":[
			 {"type":"TemplateNodeDown","status":"True","reason":"NodeDown","message":"Template x is on node pve2, which is down"}]}]`)
	case "GET /api/clusters/clusters/web":
		_, _ = io.WriteString(w, `{"namespace":"clusters","name":"web"}`)
	case "GET /api/clusters/clusters/web/upgrade-checks":
		if r.URL.Query().Get("version") == "v1.35.1" {
			_, _ = io.WriteString(w, `{"removedAPIs":[],"webhooks":[],"unchecked":[],"lines":[
				"Removed APIs still in use: policy/v1beta1 podsecuritypolicies (removed in 1.35, last requested 2026-10-12).",
				"The validating webhook guard fails closed and its Service apps/guard has 1 ready endpoint."]}`)
			return
		}
		_, _ = io.WriteString(w, `{"removedAPIs":[],"webhooks":[],"unchecked":[],"lines":[]}`)
	case "POST /api/clusters/clusters/web/upgrade-checks/accept":
		_, _ = io.WriteString(w, `{"removedAPIs":[],"webhooks":[],"unchecked":[],"lines":["Removed APIs still in use: policy/v1beta1 podsecuritypolicies (removed in 1.35)."]}`)
	case "GET /api/clusters/clusters/web/kubeconfig":
		_, _ = io.WriteString(w, "apiVersion: v1\nkind: Config\n")
	case "PATCH /api/clusters/clusters/web", "DELETE /api/clusters/clusters/web":
		_, _ = io.WriteString(w, `{}`)
	case "POST /api/clusters/clusters/web/etcd-defrag":
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{}`)
	case "POST /api/clusters":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"namespace":"clusters","name":"new"}`)
	case "GET /api/clusters/clusters/web/spec":
		_, _ = io.WriteString(w, "# Secrets: sealed, 2\nkind: Cluster\nname: web\n")
	case "POST /api/cluster-specs":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"namespace":"clusters","name":"web"}`)
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
		!strings.Contains(out, "1/2") || !strings.Contains(out, "Provisioned, upgrade held (RancherKubernetes)") ||
		strings.Count(out, "upgrade held") != 1 || !strings.Contains(out, "Provisioned, node image refresh held (TemplateNodeDown)") ||
		!strings.Contains(out, "Provisioned, template node down") {
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
	_, err = runCluster(t, srv.URL, token, "timeouts", "web", "--control-planes", "drain=30m,detach=10m,deletion=0",
		"--pool", "md-0=drain=1h", "--pool", "gpu=detach=90s")
	if err != nil {
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
		{"forget", "web", "web-md-0-c", "--yes"},
	} {
		if out, err = runCluster(t, srv.URL, token, append([]string{"machine"}, args...)...); err != nil || !strings.HasPrefix(out, "machine "+args[2]+" ") {
			t.Errorf("machine %v = %q, %v", args, out, err)
		}
	}
	file := filepath.Join(t.TempDir(), "new.yaml")
	if err = os.WriteFile(file, []byte("name: new\nclass: talos\ncontrolPlaneReplicas: 1\ncontrolPlaneTimeouts:\n  nodeDrainTimeoutSeconds: 600\n"), 0o600); err != nil {
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
		`PATCH /api/clusters/clusters/web {"controlPlaneTimeouts":{"nodeDeletionTimeoutSeconds":0,"nodeDrainTimeoutSeconds":1800,"nodeVolumeDetachTimeoutSeconds":600},"pools":{"gpu":{"timeouts":{"nodeVolumeDetachTimeoutSeconds":90}},"md-0":{"timeouts":{"nodeDrainTimeoutSeconds":3600}}}}`,
		`DELETE /api/clusters/clusters/web {"confirm":"web"}`,
		"PUT /api/clusters/clusters/web/machines/web-cp-1/replacement",
		"DELETE /api/clusters/clusters/web/machines/web-cp-1/replacement",
		"DELETE /api/clusters/clusters/web/machines/web-md-0-a",
		"PUT /api/clusters/clusters/web/machines/web-md-0-b/cordon",
		"DELETE /api/clusters/clusters/web/machines/web-md-0-b/cordon",
		"POST /api/clusters/clusters/web/machines/web-md-0-b/reboot",
		`PUT /api/clusters/clusters/web/machines/web-md-0-c/forgotten {"confirm":"web-md-0-c"}`,
		"PUT /api/clusters/clusters/web/machines/web-md-0-c/forgotten",
		`POST /api/clusters {"class":"talos","controlPlaneReplicas":1,"controlPlaneTimeouts":{"nodeDrainTimeoutSeconds":600},"name":"new"}`,
	}
	if strings.Join(sent, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent\n%s\nwant\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

func TestClusterEtcdCommands(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

	if out, err := runCluster(t, srv.URL, token, "etcd", "defrag", "web"); err != nil || out != "cluster web: etcd defragmentation asked for\n" {
		t.Errorf("defrag = %q, %v", out, err)
	}
	if out, err := runCluster(t, srv.URL, token, "etcd", "set", "web", "--quota-gib", "4"); err != nil || out != "cluster web changed\n" {
		t.Errorf("set = %q, %v", out, err)
	}
	fake.mu.Lock()
	sent := strings.Join(fake.sent, "\n")
	fake.mu.Unlock()
	if want := `PATCH /api/clusters/clusters/web {"variables":{"etcd":{"quotaGiB":4}}}`; sent != want {
		t.Errorf("sent %q, want %q", sent, want)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"etcd", "set", "web", "--quota-gib", "1"}, "--quota-gib is 2 to 8, not 1"},
		{[]string{"etcd", "set", "web", "--quota-gib", "9"}, "--quota-gib is 2 to 8, not 9"},
		{[]string{"etcd", "set", "web"}, "quota-gib"},
		{[]string{"etcd", "defrag"}, "give the cluster's name"},
	} {
		if _, err := runCluster(t, srv.URL, token, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: error %v, want %q", c.args, err, c.want)
		}
	}
}

// Each flag of update sends its own variable and no other, alone or mixed.
func TestClusterCommandsSetTheHostOutageGrace(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own
	file := filepath.Join(t.TempDir(), "new.yaml")
	if err := os.WriteFile(file, []byte("name: new\nclass: talos\nvariables:\n  hostOutageGraceMinutes: 10\n  other: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"create", "-f", file, "--host-outage-grace-minutes", "0"},
		{"update", "web", "--host-outage-grace-minutes", "45"},
		{"update", "web", "--host-outage-grace-minutes", "0"},
		{"update", "web", "--label", "team=a", "--taint", "dedicated=db:NoSchedule"},
		{"update", "web", "--cis", "--host-outage-grace-minutes", "60", "--label", "team=b"},
	} {
		if _, err := runCluster(t, srv.URL, token, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	fake.mu.Lock()
	sent := strings.Join(fake.sent, "\n")
	fake.mu.Unlock()
	want := strings.Join([]string{
		`POST /api/clusters {"class":"talos","name":"new","variables":{"hostOutageGraceMinutes":0,"other":1}}`,
		`PATCH /api/clusters/clusters/web {"variables":{"hostOutageGraceMinutes":45}}`,
		`PATCH /api/clusters/clusters/web {"variables":{"hostOutageGraceMinutes":0}}`,
		`PATCH /api/clusters/clusters/web {"variables":{"workerNodes":{"labels":[{"key":"team","value":"a"}],"taints":[{"effect":"NoSchedule","key":"dedicated","value":"db"}]}}}`,
		`PATCH /api/clusters/clusters/web {"variables":{"cis":true,"hostOutageGraceMinutes":60,"workerNodes":{"labels":[{"key":"team","value":"b"}],"taints":[]}}}`,
	}, "\n")
	if sent != want {
		t.Errorf("sent\n%s\nwant\n%s", sent, want)
	}

	// The node customisation flags mix with the others too, on update and on create.
	for _, args := range [][]string{
		{"update", "web", "--host-outage-grace-minutes", "30", "--kubelet-arg", "event-qps=50"},
		{"create", "-f", file, "--host-outage-grace-minutes", "30", "--kubelet-arg", "event-qps=50"},
	} {
		fake.mu.Lock()
		fake.sent = nil
		fake.mu.Unlock()
		if _, err := runCluster(t, srv.URL, token, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		fake.mu.Lock()
		got := strings.Join(fake.sent, "\n")
		fake.mu.Unlock()
		if !strings.Contains(got, `"hostOutageGraceMinutes":30`) || !strings.Contains(got, `"nodeCustomisation":{`) ||
			!strings.Contains(got, "event-qps") || strings.Contains(got, `"cis"`) || strings.Contains(got, `"workerNodes"`) {
			t.Errorf("%v sent %s, want the wait and the node customisation alone", args, got)
		}
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"update", "web", "--host-outage-grace-minutes", "3"}, "give 0, or 5 to 10080"},
		{[]string{"update", "web", "--host-outage-grace-minutes", "10081"}, "give 0, or 5 to 10080"},
		{[]string{"update", "web"}, "give --cis, --host-outage-grace-minutes, --label, --taint, a node customisation flag"},
		{[]string{"update", "web", "--cis=false"}, "cannot be switched off"},
		{[]string{"update", "web", "--taint", "dedicated=db"}, "--taint"},
		{[]string{"create", "-f", file, "--host-outage-grace-minutes", "4"}, "give 0, or 5 to 10080"},
	} {
		if _, err := runCluster(t, srv.URL, token, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: error %v, want %q", c.args, err, c.want)
		}
	}
	// An exported spec is created as it was saved, and -f's own flags stay with -f.
	for _, args := range [][]string{
		{"create", "--from", file, "--host-outage-grace-minutes", "30"},
		{"create", "--from", file, "--kubelet-arg", "event-qps=50"},
		{"create", "-f", file, "--without-secrets"},
	} {
		if _, err := runCluster(t, srv.URL, token, args...); err == nil || !strings.Contains(err.Error(), "belong") {
			t.Errorf("%v: error %v, want the flag refused", args, err)
		}
	}
}

func TestClusterExportSavesTheSpecAndCreateFromRecreatesIt(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

	out, err := runCluster(t, srv.URL, token, "export", "web", "-n", "clusters")
	if err != nil || out != "# Secrets: sealed, 2\nkind: Cluster\nname: web\n" {
		t.Fatalf("export = %q, %v", out, err)
	}
	file := filepath.Join(t.TempDir(), "web.yaml")
	if out, err = runCluster(t, srv.URL, token, "export", "web", "-n", "clusters", "-o", file); err != nil ||
		!strings.Contains(out, file) || !strings.Contains(out, "2 Secrets are included, sealed") {
		t.Fatalf("export -o = %q, %v", out, err)
	}
	if out, err = runCluster(t, srv.URL, token, "create", "--from", file); err != nil || out != "cluster clusters/web created\n" {
		t.Fatalf("create --from = %q, %v", out, err)
	}
	if _, err = runCluster(t, srv.URL, token, "create", "--from", file, "--without-secrets"); err != nil {
		t.Fatal(err)
	}
	if _, err = runCluster(t, srv.URL, token, "create", "--from", file, "-f", file); err == nil {
		t.Error("create took both -f and --from")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sent) != 2 || !strings.HasPrefix(fake.sent[0], "POST /api/cluster-specs ") || !strings.Contains(fake.sent[0], "kind: Cluster") ||
		!strings.Contains(fake.sent[0], `"withoutSecrets":false`) || !strings.Contains(fake.sent[1], `"withoutSecrets":true`) {
		t.Errorf("sent %v", fake.sent)
	}
}

func TestCreateFromOpensSecretsOfflineWithAnIdentityFile(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own
	offline, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	seal := func(to age.Recipient, plain string) string {
		var out bytes.Buffer
		w, sErr := age.Encrypt(&out, to)
		if sErr != nil {
			t.Fatal(sErr)
		}
		_, _ = w.Write([]byte(plain))
		_ = w.Close()
		return base64.StdEncoding.EncodeToString(out.Bytes())
	}
	secret := func(name, sealed string) string {
		return "---\nkind: Secret\nmetadata:\n  name: " + name + "\n  namespace: clusters\n  annotations:\n    " +
			provisioner.SealedSecretAnnotation + ": " + sealed + "\n"
	}
	spec := "# Secrets: sealed, 2\nkind: Cluster\nname: web\n" +
		secret("web-gitops", seal(offline.Recipient(), `{"key":"dmFsdWU="}`)) +
		secret("web-encryption", seal(stranger.Recipient(), `{"key":"b3RoZXI="}`))
	dir := t.TempDir()
	specFile, identityFile := filepath.Join(dir, "web.yaml"), filepath.Join(dir, "offline.key")
	if err = os.WriteFile(specFile, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(identityFile, []byte("# created by the administrator\n"+offline.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, runErr := runCluster(t, srv.URL, token, "create", "--from", specFile, "--identity", identityFile); runErr != nil || out != "cluster clusters/web created\n" {
		t.Fatalf("create --from --identity = %q, %v", out, runErr)
	}
	if _, err = runCluster(t, srv.URL, token, "create", "--from", specFile, "--identity", filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing identity file was accepted")
	}
	if _, err = runCluster(t, srv.URL, token, "create", "-f", specFile, "--identity", identityFile); err == nil {
		t.Error("--identity was accepted without --from")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sent) != 1 {
		t.Fatalf("sent %v", fake.sent)
	}
	sent := fake.sent[0]
	if !strings.Contains(sent, `"opened":[{"data":{"key":"dmFsdWU="},"name":"web-gitops","namespace":"clusters"}]`) {
		t.Errorf("only the Secret the identity opens is sent opened: %s", sent)
	}
	if strings.Contains(sent, offline.String()) || strings.Contains(sent, "b3RoZXI=") {
		t.Errorf("the private key or a foreign value left the machine: %s", sent)
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
		{"dev@pve!ci=secret", []string{"timeouts", "web"}, "give --control-planes, --pool or both"},
		{"dev@pve!ci=secret", []string{"timeouts", "web", "--pool", "md-0"}, `--pool "md-0": give POOL=drain=30m`},
		{"dev@pve!ci=secret", []string{"timeouts", "web", "--control-planes", "drain=soon"}, `"soon" is not a duration such as 30m`},
		{"dev@pve!ci=secret", []string{"timeouts", "web", "--control-planes", "wait=5m"}, "give drain, detach or deletion"},
		{"dev@pve!ci=secret", []string{"timeouts", "web", "--control-planes", "drain=-5m"}, "is not a duration"},
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
	for _, path := range [][]string{{"get"}, {"kubeconfig"}, {"update"}, {"scale"}, {"delete"}, {"machine", "reboot"}} {
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
