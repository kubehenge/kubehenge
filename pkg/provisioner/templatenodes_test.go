package provisioner

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

var (
	buildOld = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	buildNew = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	nodeTime = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
)

// rke2Build is the tags of a build of the default cluster's template line.
func rke2Build(t *testing.T, at time.Time) []string {
	t.Helper()
	line, err := MachineLine("rke2", cluster().Spec.Topology.Version, templates.DefaultNodeOS)
	if err != nil {
		t.Fatal(err)
	}
	return templates.NormaliseTags(append(line, templates.BuildTag(at)))
}

// classWithDown is the RKE2 class with the builds recorded and the templates down.
func classWithDown(t *testing.T, builds []time.Time, down ...DownTemplate) *clusterv1.ClusterClass {
	t.Helper()
	line, err := MachineLine("rke2", cluster().Spec.Topology.Version, templates.DefaultNodeOS)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := NodeImageBuilds{templates.LineKey(line): builds}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeTemplatesDown(down)
	if err != nil {
		t.Fatal(err)
	}
	return &clusterv1.ClusterClass{
		Namespace: "kubehenge-system", Name: "kubehenge-rke2",
		Annotations: map[string]string{NodeImageBuildsAnnotation: recorded, TemplatesDownAnnotation: encoded},
	}
}

func atNodeTime() *clusterv1.Cluster {
	c := cluster()
	c.Annotations = map[string]string{NodeImageAnnotation: nodeTime.Format(time.RFC3339)}
	return c
}

func TestCheckTemplateNodeJudgesTheTemplateTheClusterClonesFrom(t *testing.T) {
	t.Parallel()
	newer := DownTemplate{Name: "rke2-new", VMID: 9002, Node: "pve2", Tags: rke2Build(t, buildNew)}
	older := DownTemplate{Name: "rke2-old", VMID: 9001, Node: "pve1", Tags: rke2Build(t, buildOld)}
	h := &Hooks{}
	for _, tc := range []struct {
		name    string
		builds  []time.Time
		down    []DownTemplate
		refused string
	}{
		{"nothing down", []time.Time{buildOld, buildNew}, nil, ""},
		// The newest build is picked, and the older one on a down node is not cloned from.
		{"an older build on a down node, another up", []time.Time{buildOld, buildNew}, []DownTemplate{older}, ""},
		{"the picked build on a down node", []time.Time{buildOld, buildNew}, []DownTemplate{newer}, "Template rke2-new is on node pve2, which is down."},
		{"every build on a down node", []time.Time{buildOld, buildNew}, []DownTemplate{older, newer}, "Template rke2-new is on node pve2, which is down."},
		{"a template of another line", []time.Time{buildNew}, []DownTemplate{{VMID: 9003, Node: "pve3", Tags: []string{"kubehenge", "talos", "talos-v1.14.4"}}}, ""},
	} {
		class := classWithDown(t, tc.builds, tc.down...)
		err := h.checkTemplateNode(atNodeTime(), class)
		switch {
		case tc.refused == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused) || !strings.Contains(err.Error(), "Start the node")):
			t.Errorf("%s: gave %v, want it to say %q and what to do", tc.name, err, tc.refused)
		}
	}
}

func TestCheckTemplateNameFallsBackToTheVMID(t *testing.T) {
	t.Parallel()
	down := DownTemplate{VMID: 9002, Node: "pve2", Tags: rke2Build(t, buildNew)}
	err := (&Hooks{}).checkTemplateNode(atNodeTime(), classWithDown(t, []time.Time{buildNew}, down))
	if err == nil || !strings.HasPrefix(err.Error(), "Template VM 9002 is on node pve2, which is down.") {
		t.Errorf("gave %v, want the template named by its VM ID", err)
	}
}

func TestBeforeClusterCreateRefusesATemplateOnADownNode(t *testing.T) {
	t.Parallel()
	down := DownTemplate{Name: "rke2-new", VMID: 9002, Node: "pve2", Tags: rke2Build(t, buildNew)}
	hooks, kube := hooksFor(t)
	class := classWithDown(t, []time.Time{buildNew}, down)
	existing := &clusterv1.ClusterClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(class), existing); err != nil {
		t.Fatal(err)
	}
	existing.Annotations[NodeImageBuildsAnnotation] = class.Annotations[NodeImageBuildsAnnotation]
	existing.Annotations[TemplatesDownAnnotation] = class.Annotations[TemplatesDownAnnotation]
	if err := kube.Update(context.Background(), existing); err != nil {
		t.Fatal(err)
	}

	resp := callBeforeClusterCreate(t, hooks, atNodeTime())
	if resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "Template rke2-new is on node pve2, which is down.") {
		t.Errorf("response %+v; want the cluster refused for the template on a down node", resp)
	}
}

func TestBeforeClusterCreateRefusesATalosTemplateOnADownNode(t *testing.T) {
	t.Parallel()
	line, err := TalosLine("v1.14.4")
	if err != nil {
		t.Fatal(err)
	}
	class := talosClass("v1.14.4")
	encoded, err := encodeTemplatesDown([]DownTemplate{{Name: "kubehenge-talos-v1.14.4", VMID: 9005, Node: "pve3", Tags: line}})
	if err != nil {
		t.Fatal(err)
	}
	class.Annotations[TemplatesDownAnnotation] = encoded
	hooks, _ := talosHooks(t, class)

	resp := callBeforeClusterCreate(t, hooks, talosCluster("web", "v1.14.4"))
	if resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "Template kubehenge-talos-v1.14.4 is on node pve3, which is down.") {
		t.Errorf("response %+v; want the Talos cluster refused for its template on a down node", resp)
	}
	// Another release's template being down is none of this cluster's business.
	other, err := TalosLine("v1.13.12")
	if err != nil {
		t.Fatal(err)
	}
	if encoded, err = encodeTemplatesDown([]DownTemplate{{VMID: 9006, Node: "pve3", Tags: other}}); err != nil {
		t.Fatal(err)
	}
	class.Annotations[TemplatesDownAnnotation] = encoded
	if err = hooks.checkTemplateNode(talosCluster("web", "v1.14.4"), class); err != nil {
		t.Errorf("refused over another release's template: %v", err)
	}
}

// resourceView serves the cluster's resources and configurations from templates, each on a node
// that is up or down, and lets a test change which.
type resourceView struct {
	mu   sync.Mutex
	vms  []map[string]any
	down map[int]bool
}

func (v *resourceView) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var data any = []any{}
	switch {
	case r.URL.Path == "/api2/json/cluster/resources":
		vms := make([]map[string]any, 0, len(v.vms))
		for _, vm := range v.vms {
			shown := maps.Clone(vm)
			if id, ok := vm["vmid"].(int); ok && v.down[id] {
				shown["status"] = "unknown"
				delete(shown, "name")
			}
			vms = append(vms, shown)
		}
		data = vms
	case strings.HasSuffix(r.URL.Path, "/config"):
		data = map[string]any{"template": 1, "scsi0": "ceph:base-9000-disk-0,size=30G"}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (v *resourceView) setDown(vmid int, down bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.down[vmid] = down
}

func TestTemplateNodeDownIsAConditionOnTheClusterAndClearsWhenTheNodeIsBack(t *testing.T) {
	t.Parallel()
	view := &resourceView{down: map[int]bool{}, vms: []map[string]any{
		{"vmid": 9001, "type": "qemu", "node": "pve1", "name": "rke2-old", "template": 1, "tags": strings.Join(rke2Build(t, buildOld), ";")},
		{"vmid": 9002, "type": "qemu", "node": "pve2", "name": "rke2-new", "template": 1, "tags": strings.Join(rke2Build(t, buildNew), ";")},
	}}
	srv := httptest.NewServer(view)
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	class := &clusterv1.ClusterClass{}
	class.Namespace, class.Name = "kubehenge-system", ClassPrefix+"rke2"
	web := atNodeTime()
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, web).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	p := &TemplateVersions{Client: kube, ClassNamespace: "kubehenge-system", Proxmox: proxmox.NewClient(srv.URL + "/api2/json")}
	ctx := context.Background()
	condition := func() *metav1.Condition {
		t.Helper()
		got := &clusterv1.Cluster{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(web), got); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(got.Status.Conditions, TemplateNodeDownCondition)
	}

	// The template an older build is on is down: the cluster is cloned from the newer one.
	view.setDown(9001, true)
	if err := p.publish(ctx); err != nil {
		t.Fatalf("publishing failed over a template on a down node: %v", err)
	}
	if got := condition(); got != nil {
		t.Errorf("condition %+v for a template the cluster does not clone from", got)
	}

	// The template it clones from is on a down node as well.
	view.setDown(9002, true)
	if err := p.publish(ctx); err != nil {
		t.Fatalf("publishing failed over templates on down nodes: %v", err)
	}
	got := condition()
	if got == nil || got.Reason != TemplateNodeDownReason || !strings.HasPrefix(got.Message, "Template VM 9002 is on node pve2, which is down.") {
		t.Fatalf("condition %+v; want TemplateNodeDown naming VM 9002 and pve2", got)
	}
	recorded := &clusterv1.ClusterClass{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(class), recorded); err != nil {
		t.Fatal(err)
	}
	if list, err := ClassTemplatesDown(recorded); err != nil || len(list) != 2 {
		t.Errorf("the class records %v, %v as down; want both templates", list, err)
	}
	// The builds stay recorded, so that no cluster's pick moves to another build.
	if builds, err := recordedBuilds(recorded); err != nil || len(builds[templates.LineKey(rke2Build(t, buildNew))]) != 2 {
		t.Errorf("the class records builds %v, %v; want both kept", builds, err)
	}

	// The node is back.
	view.setDown(9001, false)
	view.setDown(9002, false)
	if err := p.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if got = condition(); got != nil {
		t.Errorf("condition %+v once the node is back; want none", got)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(class), recorded); err != nil {
		t.Fatal(err)
	}
	if list, err := ClassTemplatesDown(recorded); err != nil || len(list) != 0 {
		t.Errorf("the class records %v, %v as down once the node is back", list, err)
	}
}
