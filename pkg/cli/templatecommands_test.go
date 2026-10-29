package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// templatePool is a Proxmox VE whose template pool holds templates, on node pve1: what listing
// them and deleting one reads and does.
type templatePool struct {
	t    *testing.T
	mu   sync.Mutex
	vms  map[int]map[string]any
	gone []int
	// refuse names a VM whose deletion Proxmox VE refuses.
	refuse int
}

var poolVMPath = regexp.MustCompile(`^/api2/json/nodes/pve1/qemu/(\d+)(/config|/status/current)?$`)

func (p *templatePool) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var data any
	switch m := poolVMPath.FindStringSubmatch(r.URL.Path); {
	case r.URL.Path == "/api2/json/cluster/status":
		data = []any{}
	case r.URL.Path == "/api2/json/cluster/resources":
		list := make([]map[string]any, 0, len(p.vms))
		for vmid, c := range p.vms {
			list = append(list, map[string]any{"vmid": vmid, "node": "pve1", "type": "qemu", "name": c["name"], "tags": c["tags"], "template": 1, "pool": "kubehenge-templates"})
		}
		data = list
	case r.URL.Path == "/api2/json/nodes/pve1/status":
		data = map[string]any{}
	case strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pve1/tasks/"):
		data = map[string]any{"status": "stopped", "exitstatus": "OK"}
	case m != nil:
		vmid, err := strconv.Atoi(m[1])
		if err != nil {
			p.t.Error(err)
		}
		c, ok := p.vms[vmid]
		switch {
		case !ok || vmid == p.refuse && r.Method == http.MethodDelete:
			http.Error(w, "refused", http.StatusInternalServerError)
			return
		case r.Method == http.MethodDelete:
			delete(p.vms, vmid)
			p.gone = append(p.gone, vmid)
			data = fmt.Sprintf("UPID:pve1:00001234:00005678:6700AB12:qmdestroy:%d:root@pam:", vmid)
		case m[2] == "/config":
			data = map[string]any{"name": c["name"], "tags": c["tags"], "template": 1}
		default:
			data = map[string]any{"vmid": vmid, "name": c["name"], "status": "stopped"}
		}
	default:
		p.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		p.t.Error(err)
	}
}

func (p *templatePool) deleted() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.gone)
}

// start serves the pool over HTTPS and returns the connection to it, its CA trusted.
func (p *templatePool) start(t *testing.T) *pve.Config {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &pve.Config{URL: srv.URL, CAFile: caFile, Token: testToken}
}

// lineBuilds puts builds of a line made at times into the pool, from VM ID first on, and returns them.
func (p *templatePool) lineBuilds(t *testing.T, line *templates.Template, first int, times ...time.Time) []templates.Template {
	t.Helper()
	out := make([]templates.Template, 0, len(times))
	for i, at := range times {
		build, err := line.Build(at)
		if err != nil {
			t.Fatal(err)
		}
		p.vms[first+i] = map[string]any{"name": build.Name, "tags": strings.Join(build.Tags, ";")}
		out = append(out, build)
	}
	return out
}

func connected(t *testing.T, conn *pve.Config) *templates.Builder {
	t.Helper()
	b, err := connect(conn)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func command(ctx context.Context) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	return cmd, &out
}

func plan(t *testing.T, d templates.Distribution) templates.Template {
	t.Helper()
	tmpl, err := templates.Plan(&templates.Request{Distribution: d})
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func TestDeleteTemplateDeletesOnlyTheBuildAskedFor(t *testing.T) {
	t.Parallel()
	p := &templatePool{t: t, vms: map[int]map[string]any{}}
	conn := p.start(t)
	line := plan(t, templates.RKE2)
	builds := p.lineBuilds(t, &line, 101, time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC))
	older, newer := builds[0], builds[1]
	used := strings.Join(templates.NormaliseTags(older.Tags), ";")
	nobody := deleteChecks{users: func([]*proxmox.ClusterResource) (map[string][]string, error) {
		return map[string][]string{used: {"clusters/web"}}, nil
	}}
	b := connected(t, conn)
	b.Pool = "kubehenge-templates"
	ctx := t.Context()

	cmd, out := command(ctx)
	if n, err := deleteTemplate(cmd, b, &line, "build-209912310000", nobody); err != nil || n != 0 {
		t.Errorf("a build that is none: deleted %d, %v", n, err)
	}
	if n, err := deleteTemplate(cmd, b, &line, "", nobody); err == nil || !strings.Contains(err.Error(), "no build of the template would be left") || n != 0 {
		t.Errorf("every build, one of them in use: deleted %d, %v", n, err)
	}
	if n, err := deleteTemplate(cmd, b, &line, templates.BuildTag(time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)), nobody); err == nil || !strings.Contains(err.Error(), "refused, nothing deleted") || n != 0 {
		t.Errorf("the build in use: deleted %d, %v", n, err)
	}
	failing := deleteChecks{users: func([]*proxmox.ClusterResource) (map[string][]string, error) {
		return nil, errors.New("no management cluster")
	}}
	if _, err := deleteTemplate(cmd, b, &line, "", failing); err == nil || !strings.Contains(err.Error(), "no management cluster") {
		t.Errorf("without knowing who runs the builds: %v", err)
	}
	if gone := p.deleted(); len(gone) != 0 {
		t.Fatalf("deleted %v while refusing", gone)
	}

	n, err := deleteTemplate(cmd, b, &line, templates.BuildTag(time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)), nobody)
	if err != nil || n != 1 || !slices.Equal(p.deleted(), []int{102}) {
		t.Errorf("the build nothing runs: deleted %d %v, %v", n, p.deleted(), err)
	}
	if want := "deleted 102 " + newer.Name + "\n"; out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
}

func TestDeleteTemplateStopsAtTheFirstBuildProxmoxVERefuses(t *testing.T) {
	t.Parallel()
	p := &templatePool{t: t, vms: map[int]map[string]any{}, refuse: 101}
	conn := p.start(t)
	line := plan(t, templates.K3s)
	p.lineBuilds(t, &line, 101, time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC))
	b := connected(t, conn)
	cmd, _ := command(t.Context())
	nobody := deleteChecks{users: func([]*proxmox.ClusterResource) (map[string][]string, error) { return map[string][]string{}, nil }}
	// Builds go newest first.
	n, err := deleteTemplate(cmd, b, &line, "", nobody)
	if err == nil || !strings.Contains(err.Error(), "deleting "+line.LineName) || n != 1 || !slices.Equal(p.deleted(), []int{102}) {
		t.Errorf("deleted %d %v, %v; want 102 deleted and 101's refusal", n, p.deleted(), err)
	}
}

func TestDeleteTemplateOfALineWithoutBuilds(t *testing.T) {
	t.Parallel()
	talos := plan(t, templates.Talos)
	p := &templatePool{t: t, vms: map[int]map[string]any{301: {"name": talos.Name, "tags": strings.Join(talos.Tags, ";")}}}
	b := connected(t, p.start(t))
	cmd, out := command(t.Context())
	never := deleteChecks{users: func([]*proxmox.ClusterResource) (map[string][]string, error) {
		t.Error("asked who runs the builds of a line that has none")
		return map[string][]string{}, nil
	}}
	if _, err := deleteTemplate(cmd, b, &talos, "build-202610180100", never); err == nil || !strings.Contains(err.Error(), "has no builds") {
		t.Errorf("a build of Talos's template: %v", err)
	}
	if n, err := deleteTemplate(cmd, b, &talos, "", never); err != nil || n != 1 || out.String() != "deleted 301 "+talos.Name+"\n" {
		t.Errorf("deleted %d, %v, printing %q", n, err, out.String())
	}
	if n, err := deleteTemplate(cmd, b, &talos, "", never); err != nil || n != 0 {
		t.Errorf("deleting it again: deleted %d, %v", n, err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestReportDeletedSaysWhenItCannotWrite(t *testing.T) {
	t.Parallel()
	cmd, out := command(t.Context())
	if err := reportDeleted(cmd, 301, "kubehenge-talos"); err != nil || out.String() != "deleted 301 kubehenge-talos\n" {
		t.Errorf("printed %q, %v", out.String(), err)
	}
	cmd.SetOut(failingWriter{})
	if err := reportDeleted(cmd, 301, "kubehenge-talos"); err == nil || !strings.Contains(err.Error(), "writing the result") {
		t.Errorf("reportDeleted gave %v", err)
	}
}

func TestTheTemplateCommandsListAndDeleteInThePool(t *testing.T) {
	t.Parallel()
	talos := plan(t, templates.Talos)
	p := &templatePool{t: t, vms: map[int]map[string]any{301: {"name": talos.Name, "tags": strings.Join(talos.Tags, ";")}}}
	conn := p.start(t)
	exec := func(cmd *cobra.Command, args ...string) (string, error) {
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}
	if out, err := exec(newTemplateListCommand(conn)); err != nil || out != fmt.Sprintf("301 pve1 %s %s\n", talos.Name, strings.Join(talos.Tags, ";")) {
		t.Errorf("list printed %q, %v", out, err)
	}
	if out, err := exec(newTemplateDeleteCommand(conn), "talos"); err != nil || out != "deleted 301 "+talos.Name+"\n" {
		t.Errorf("delete printed %q, %v", out, err)
	}
	if out, err := exec(newTemplateDeleteCommand(conn), "talos"); err != nil || !strings.Contains(out, "no template carries the tags "+strings.Join(talos.Tags, ";")) {
		t.Errorf("deleting it again printed %q, %v", out, err)
	}
	if _, err := exec(newTemplateDeleteCommand(conn), "talos", "--build", "build-202610180100"); err == nil || !strings.Contains(err.Error(), "has no builds") {
		t.Errorf("a build of Talos's template: %v", err)
	}
	if _, err := exec(newTemplateDeleteCommand(conn), "kubeadm"); err == nil || !strings.Contains(err.Error(), "the template:") {
		t.Errorf("kubeadm without its Kubernetes version: %v", err)
	}
	noToken := *conn
	noToken.Token = ""
	for _, cmd := range []*cobra.Command{newTemplateListCommand(&noToken), newTemplateDeleteCommand(&noToken)} {
		args := []string{}
		if cmd.Name() == "delete" {
			args = append(args, "talos")
		}
		if _, err := exec(cmd, args...); err == nil || !strings.Contains(err.Error(), "set KUBEHENGE_PROXMOX_TOKEN") {
			t.Errorf("%s without a token: %v", cmd.Name(), err)
		}
	}
}

func TestConnectNeedsATokenAndAnAPI(t *testing.T) {
	t.Parallel()
	if _, err := connect(&pve.Config{URL: "https://pve.example.test:8006"}); err == nil || !strings.Contains(err.Error(), "set KUBEHENGE_PROXMOX_TOKEN") {
		t.Errorf("connect without a token: %v", err)
	}
	if _, err := connect(&pve.Config{URL: "http://pve.example.test:8006", Token: testToken}); err == nil || !strings.Contains(err.Error(), "connecting to Proxmox VE") {
		t.Errorf("connect over HTTP: %v", err)
	}
	b, err := connect(&pve.Config{URL: "https://pve.example.test:8006", Token: testToken})
	if err != nil || b.Client == nil || b.Sources == nil || b.Sources.TalosFactoryURL != templates.DefaultSources().TalosFactoryURL {
		t.Errorf("connect gave %+v, %v", b, err)
	}
}

func TestBuilderFlagsConnect(t *testing.T) {
	t.Parallel()
	conn := &pve.Config{URL: "https://pve.example.test:8006", Token: testToken}
	for _, c := range []struct {
		name             string
		address, gateway string
		dns              []string
		conn             *pve.Config
		want             string
	}{
		{name: "a gateway without an address", gateway: "192.0.2.1", conn: conn, want: "--build-gateway and --build-dns need --build-address"},
		{name: "DNS without an address", dns: []string{"192.0.2.53"}, conn: conn, want: "need --build-address"},
		{name: "an address without its prefix", address: "192.0.2.50", gateway: "192.0.2.1", conn: conn, want: "--build-address"},
		{name: "no token", conn: &pve.Config{URL: conn.URL}, want: "set KUBEHENGE_PROXMOX_TOKEN"},
	} {
		f := &builderFlags{address: c.address, gateway: c.gateway, dns: c.dns}
		f.builder.Node = "pve1"
		cmd, _ := command(t.Context())
		if _, err := f.connect(cmd, c.conn); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: connect gave %v; want %q", c.name, err, c.want)
		}
	}

	f := &builderFlags{address: "192.0.2.50/24", gateway: "192.0.2.1", dns: []string{"192.0.2.53"}}
	f.builder.Node = "pve1"
	cmd, out := command(t.Context())
	b, err := f.connect(cmd, conn)
	if err != nil {
		t.Fatal(err)
	}
	if b.Client == nil || b.Sources == nil || b.Network == nil || b.Network.Address.String() != "192.0.2.50/24" || len(b.Network.DNS) != 1 {
		t.Errorf("the builder is %+v", b)
	}
	b.Log("building %s", "rke2")
	if out.String() != "building rke2\n" {
		t.Errorf("the builder logs %q to the command's standard error", out.String())
	}
}

func TestNodeOSFlag(t *testing.T) {
	t.Parallel()
	var o templates.NodeOS
	cmd := &cobra.Command{}
	nodeOSFlag(cmd.Flags(), &o)
	if o != templates.DefaultNodeOS {
		t.Errorf("--node-os starts at %s, not the default %s", o, templates.DefaultNodeOS)
	}
	f := cmd.Flags().Lookup("node-os")
	if f.Value.Type() != "node-os" {
		t.Errorf("--node-os is of type %q", f.Value.Type())
	}
	if err := f.Value.Set("rocky-10"); err != nil || o.String() != "rocky-10" || f.Value.String() != "rocky-10" {
		t.Errorf("--node-os rocky-10 gave %s, %v", o, err)
	}
	if err := f.Value.Set("windows"); err == nil || o.String() != "rocky-10" {
		t.Errorf("--node-os windows gave %s, %v; want it refused and rocky-10 kept", o, err)
	}
}

func TestBuildUsersNeedsTheManagementCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	if _, err := buildUsers(t.Context(), refreshNamespace, nil); err == nil || !strings.Contains(err.Error(), "needs the management cluster's API (KUBECONFIG)") {
		t.Errorf("buildUsers gave %v", err)
	}
}

func TestInClusterBoundsTheHooksWork(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	called := false
	work := func(context.Context, client.Client) error {
		called = true
		return nil
	}
	if err := inCluster(t.Context(), time.Minute, work); err == nil || !strings.Contains(err.Error(), "the cluster to run in") || called {
		t.Errorf("inCluster without a cluster gave %v, ran the work %v", err, called)
	}

	writeKubeconfig(t, "https://127.0.0.1:1")
	waited := errors.New("waited")
	err := inCluster(t.Context(), time.Minute, func(ctx context.Context, c client.Client) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Minute || c == nil {
			t.Errorf("the work runs with deadline %v (%v) and client %v", deadline, ok, c)
		}
		return waited
	})
	if !errors.Is(err, waited) {
		t.Errorf("inCluster gave %v; want the work's own error", err)
	}
}
