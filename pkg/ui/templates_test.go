package ui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// withTemplates makes the harness an appliance with a management cluster and three templates: two
// builds of RKE2's on the default node OS, and one of kubeadm at v1.36.5.
func withTemplates(t *testing.T, h *harness) {
	t.Helper()
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	h.accounts.members = []pve.Member{
		{VMID: 9001, Node: "pve1", Type: "qemu", Name: "kubehenge-rke2-almalinux-10-build-202610010000", Template: 1, Tags: "build-202610010000;kubehenge;os-almalinux-10;rke2"},
		{VMID: 9004, Node: "pve1", Type: "qemu", Name: "kubehenge-rke2-almalinux-10-build-202610150000", Template: 1, Tags: "build-202610150000;kubehenge;os-almalinux-10;rke2"},
		{VMID: 9002, Node: "pve1", Type: "qemu", Name: "kubehenge-kubeadm-v1.36.5-almalinux-10-build-202610010000", Template: 1, Tags: "build-202610010000;kubeadm;kubehenge;kubernetes-v1.36.5;os-almalinux-10"},
		{VMID: 9003, Node: "pve1", Type: "qemu", Name: "being-built", Template: 0},
	}
}

func TestTemplatesListedWithTheClustersThatNeedThem(t *testing.T) {
	t.Parallel()
	// Its nodes went to the build of 1 October. The newer one is not offered by its node image time.
	one := cluster("default", "one")
	one.Annotations = map[string]string{"kubehenge.dev/node-image": "2026-10-10T00:00:00Z"}
	h := newHarness(t, one)
	withTemplates(t, h)
	h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) { return 0, nil }
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("GET", "/api/templates", "")
	expect(t, rec, http.StatusOK)
	var got templatesView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	ids := make([]int, 0, len(got.Templates))
	for _, v := range got.Templates {
		ids = append(ids, v.VMID)
	}
	if !slices.Equal(ids, []int{9002, 9004, 9001}) || got.Templates[0].InUse != "" || got.Templates[0].Line != "kubehenge-kubeadm-v1.36.5-almalinux-10" ||
		got.Templates[0].NodeOS != "AlmaLinux 10" || len(got.NodeOSes) != len(templates.NodeOSes()) ||
		got.Templates[1].InUse != "" || !got.Templates[1].Newest || got.Templates[1].Build != "build-202610150000" ||
		got.Templates[2].InUse != "default/one" || got.Templates[2].Newest || got.Refresh != nil {
		t.Fatalf("templates = %s", rec.Body)
	}

	d := h.browser()
	d.signIn("dev@pve")
	expect(t, d.do("GET", "/api/templates", ""), http.StatusForbidden)
}

func TestTemplatesOnlyInAnApplianceWithItsManagementCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := h.browser()
	a.signIn("admin@pve")
	expect(t, a.do("GET", "/api/templates", ""), http.StatusNotFound)

	withTemplates(t, h)
	h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) { return 0, nil }
	h.server.activeKube.Store(nil)
	expect(t, a.do("GET", "/api/templates", ""), http.StatusConflict)
}

func TestTemplateBuiltOnTheAdministratorsSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withTemplates(t, h)
	release := make(chan struct{})
	var gotBuilder templates.Builder
	var gotTemplate templates.Template
	var gotTicket *pve.Ticket
	h.server.MakeTemplate = func(_ context.Context, ticket *pve.Ticket, b templates.Builder, tpl *templates.Template) (int, error) {
		gotBuilder, gotTemplate, gotTicket = b, *tpl, ticket
		b.Log("Downloading %s", "the image")
		<-release
		return 9010, nil
	}
	a := h.browser()
	a.signIn("admin@pve")
	body := `{"distribution":"kubeadm","kubernetesVersion":"v1.37.1","nodeOS":"rocky-10","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0","vlan":30,` +
		`"buildAddress":"192.0.2.50/24","buildGateway":"192.0.2.1","buildDNS":["192.0.2.1"]}`
	expect(t, a.do("POST", "/api/templates", body), http.StatusAccepted)
	// One job at a time, of any kind.
	expect(t, a.do("POST", "/api/templates", body), http.StatusConflict)
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for h.server.Appliance.TemplateJob().State == "running" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	job := h.server.Appliance.TemplateJob()
	if job.State != "succeeded" || !slices.Contains(job.Log, "Downloading the image") || !strings.Contains(strings.Join(job.Log, "\n"), "is VM 9010") {
		t.Fatalf("job = %+v", job)
	}
	if _, built := templates.BuildOf(gotTemplate.Tags); !built || gotTemplate.LineName != "kubehenge-kubeadm-v1.37.1-rocky-10" ||
		!strings.HasPrefix(gotTemplate.Name, gotTemplate.LineName+"-build-") || gotBuilder.Pool != templatePool || gotBuilder.Node != "pve1" ||
		gotBuilder.Storage != "ceph" || gotBuilder.VLAN != 30 || gotBuilder.Network == nil || gotBuilder.Network.Gateway.String() != "192.0.2.1" {
		t.Fatalf("built %+v with %+v", gotTemplate, gotBuilder)
	}
	if gotTicket == nil || gotTicket.Username != "admin@pve" || h.accounts.renewals == 0 {
		t.Fatalf("ticket %+v after %d renewals", gotTicket, h.accounts.renewals)
	}
	// The setup's own record stays about the management cluster.
	if h.server.Appliance.Current() != nil {
		t.Fatal("the template build shows as a run of the management cluster")
	}
}

func TestFinishedTemplateBuildIsListed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withTemplates(t, h)
	release := make(chan struct{})
	h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) {
		<-release
		h.accounts.mu.Lock()
		defer h.accounts.mu.Unlock()
		h.accounts.members = append(h.accounts.members, pve.Member{
			VMID: 9010, Node: "pve1", Type: "qemu", Name: "kubehenge-kubeadm-v1.37.1-almalinux-10",
			Template: 1, Tags: "kubeadm;kubehenge;kubernetes-v1.37.1;os-almalinux-10",
		})
		return 9010, nil
	}
	a := h.browser()
	a.signIn("admin@pve")
	expect(t, a.do("POST", "/api/templates", `{"distribution":"kubeadm","kubernetesVersion":"v1.37.1","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`),
		http.StatusAccepted)
	// The build ends while the templates are being listed.
	h.accounts.mu.Lock()
	h.accounts.listed = func() {
		h.accounts.mu.Lock()
		h.accounts.listed = nil
		h.accounts.mu.Unlock()
		close(release)
		deadline := time.Now().Add(5 * time.Second)
		for h.server.Appliance.TemplateJob().State == "running" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	h.accounts.mu.Unlock()
	listed := func() templatesView {
		t.Helper()
		rec := a.do("GET", "/api/templates", "")
		expect(t, rec, http.StatusOK)
		var got templatesView
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	for range 2 {
		got := listed()
		built := slices.ContainsFunc(got.Templates, func(v templateView) bool { return v.VMID == 9010 })
		if got.Job.State == "succeeded" && !built {
			t.Fatalf("the build shows as finished, but its template is not listed: %+v", got)
		}
		if built && got.Job.State != "succeeded" {
			t.Fatalf("template 9010 listed beside job %+v", got.Job)
		}
	}
	if got := listed(); got.Job.State != "succeeded" || len(got.Templates) != 4 {
		t.Fatalf("after the build: %+v", got)
	}
}

func TestTemplateRequestsAreChecked(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"distribution": `{"distribution":"flatcar","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`,
		"talos":        `{"distribution":"talos","talosVersion":"1.14","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`,
		"version":      `{"distribution":"kubeadm","kubernetesVersion":"1.37","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`,
		"node":         `{"distribution":"rke2","node":"","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`,
		"node OS":      `{"distribution":"rke2","nodeOS":"leap-16.1","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`,
		"node OS name": `{"distribution":"rke2","nodeOS":"almalinux","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`,
		"gateway":      `{"distribution":"rke2","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0","buildAddress":"192.0.2.50/24","buildGateway":"198.51.100.1","buildDNS":["192.0.2.1"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			withTemplates(t, h)
			h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) {
				t.Error("a refused request was built")
				return 0, nil
			}
			a := h.browser()
			a.signIn("admin@pve")
			expect(t, a.do("POST", "/api/templates", body), http.StatusUnprocessableEntity)
		})
	}
}

func TestFailedTemplateBuildReported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	withTemplates(t, h)
	h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) {
		return 0, errors.New("no storage local on node pve1")
	}
	a := h.browser()
	a.signIn("admin@pve")
	expect(t, a.do("POST", "/api/templates", `{"distribution":"rke2","node":"pve1","storage":"ceph","importStorage":"local","bridge":"vmbr0"}`), http.StatusAccepted)
	deadline := time.Now().Add(5 * time.Second)
	for h.server.Appliance.TemplateJob().State == "running" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if job := h.server.Appliance.TemplateJob(); job.State != "failed" || !strings.HasSuffix(job.Error, ": no storage local on node pve1") {
		t.Fatalf("job = %+v", job)
	}
}

func TestTemplateInUseIsKept(t *testing.T) {
	t.Parallel()
	kubeadm := cluster("default", "two")
	kubeadm.Spec.Topology.ClassRef.Name = "kubehenge-kubeadm"
	kubeadm.Spec.Topology.Version = "v1.37.1"
	// Mid-upgrade: a machine still runs the version before.
	old := &clusterv1.Machine{}
	old.Namespace, old.Name = "default", "two-old"
	old.Labels = map[string]string{clusterv1.ClusterNameLabel: "two"}
	old.Spec.ClusterName, old.Spec.Version = "two", "v1.36.5"
	h := newHarness(t, cluster("default", "one"), kubeadm, old)
	withTemplates(t, h)
	h.server.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) { return 0, nil }
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("DELETE", "/api/templates/9001", "")
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "default/one") {
		t.Fatalf("refusal = %s", rec.Body)
	}
	// A build no cluster's node image time picks can go.
	expect(t, a.do("DELETE", "/api/templates/9004", ""), http.StatusNoContent)
	expect(t, a.do("DELETE", "/api/templates/9002", ""), http.StatusConflict)
	expect(t, a.do("DELETE", "/api/templates/9003", ""), http.StatusNotFound)
	expect(t, a.do("DELETE", "/api/templates/42", ""), http.StatusNotFound)

	if err := h.kube.Delete(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	expect(t, a.do("DELETE", "/api/templates/9002", ""), http.StatusNoContent)
	if !slices.Equal(h.accounts.deleted, []int{9004, 9002}) {
		t.Fatalf("deleted %v", h.accounts.deleted)
	}
}
