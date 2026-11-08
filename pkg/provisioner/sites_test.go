package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

type fakeSites struct {
	sites *Sites
	err   error
}

func (f fakeSites) Sites(context.Context) (*Sites, error) { return f.sites, f.err }

// templateSiteOf is the newest build of the cluster's RKE2 line, on node and storages.
func templateSiteOf(t *testing.T, c *clusterv1.Cluster, node, cpu string, storages ...string) TemplateSite {
	t.Helper()
	lines, err := ClusterLines(c)
	if err != nil || len(lines) == 0 {
		t.Fatalf("the cluster's template lines: %v, %v", lines, err)
	}
	at := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	tags := templates.NormaliseTags(append(slices.Clone(lines[0]), templates.BuildTag(at)))
	return TemplateSite{Name: "kubehenge-rke2-build", Node: node, Tags: tags, Storages: storages, CPU: cpu, Built: at}
}

func threeNodes() []Node {
	return []Node{{Name: "pve1", CPULevel: 3}, {Name: "pve2", CPULevel: 3}, {Name: "pve3", CPULevel: 3}}
}

func TestSitesKeepAClusterToTheNodesThatCanCloneItsTemplate(t *testing.T) {
	t.Parallel()
	c := cluster(clusterVariable(t, "storage", "ceph"))
	local := StorageSite{Nodes: []string{"pve1", "pve2", "pve3"}, Active: []string{"pve1", "pve2", "pve3"}}
	shared := local
	shared.Shared = true
	for _, tc := range []struct {
		name    string
		tmpl    StorageSite
		allowed []string
		want    []string
		refused string
	}{
		{"node-local storage keeps clones on the template's node", local, nil, []string{"pve1"}, ""},
		{"shared storage reaches every node", shared, nil, []string{"pve1", "pve2", "pve3"}, ""},
		{
			"node-local storage with the node not allowed", local,
			[]string{"pve2", "pve3"},
			[]string{},
			"template kubehenge-rke2-build is on storage templates, which is not shared, so it clones only on node pve1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Sites{
				Storages:  map[string]StorageSite{"ceph": shared, "templates": tc.tmpl},
				Templates: []TemplateSite{templateSiteOf(t, c, "pve1", "", "templates")},
			}
			online := threeNodes()
			if len(tc.allowed) > 0 {
				online = slices.DeleteFunc(online, func(n Node) bool { return !slices.Contains(tc.allowed, n.Name) })
			}
			if got := s.Hosts(c, online, 2); !slices.Equal(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
				t.Errorf("Hosts = %v, want %v", got, tc.want)
			}
			err := s.Refusal(c, online)
			if tc.refused == "" && err != nil || tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused) || !strings.Contains(err.Error(), "shared storage")) {
				t.Errorf("Refusal = %v, want %q", err, tc.refused)
			}
		})
	}
}

func TestSitesIntersectPlacementWithTheClusterStorage(t *testing.T) {
	t.Parallel()
	c := cluster(clusterVariable(t, "storage", "nvme"))
	s := &Sites{Storages: map[string]StorageSite{
		"nvme": {Nodes: []string{"pve1", "pve2"}, Active: []string{"pve2"}},
		"ceph": {Shared: true, Nodes: []string{"pve1", "pve2", "pve3"}, Active: []string{"pve1", "pve2", "pve3"}},
	}}
	if got := s.Hosts(c, threeNodes(), 2); !slices.Equal(got, []string{"pve2"}) {
		t.Errorf("Hosts = %v, want only pve2, where the storage is available", got)
	}
	err := s.Refusal(c, []Node{{Name: "pve1", CPULevel: 3}, {Name: "pve3", CPULevel: 3}})
	if err == nil || !strings.Contains(err.Error(), "storage nvme is available only on pve2, not on the nodes this cluster may use (pve1, pve3)") {
		t.Errorf("Refusal = %v, want the storage named with the nodes", err)
	}
	if err = s.Refusal(c, threeNodes()); err != nil {
		t.Errorf("Refusal = %v for a storage that is available on an allowed node", err)
	}
	missing := cluster(clusterVariable(t, "storage", "gone"))
	if err = s.Refusal(missing, threeNodes()); err == nil || !strings.Contains(err.Error(), "storage gone does not exist") {
		t.Errorf("Refusal = %v for a storage Proxmox VE does not list", err)
	}
}

func TestSitesTakeTheCPULevelFromTheTemplate(t *testing.T) {
	t.Parallel()
	c := cluster()
	online := []Node{{Name: "pve1", CPULevel: 2}, {Name: "pve2", CPULevel: 3}, {Name: "pve3", CPULevel: 4}}
	for _, tc := range []struct {
		cpu  string
		want []string
	}{
		{"", []string{"pve1", "pve2", "pve3"}},
		{"host", []string{"pve1", "pve2", "pve3"}},
		{"x86-64-v4", []string{"pve3"}},
		{"cputype=x86-64-v3,flags=+aes", []string{"pve2", "pve3"}},
	} {
		s := &Sites{Templates: []TemplateSite{templateSiteOf(t, c, "pve1", tc.cpu)}}
		if got := s.Hosts(c, online, templates.CPULevel(templates.DefaultNodeOS)); !slices.Equal(got, tc.want) {
			t.Errorf("a template with cpu %q gives hosts %v, want %v", tc.cpu, got, tc.want)
		}
	}
	// The same check refuses a cluster none of whose allowed nodes reaches the template's level.
	h := &Hooks{Nodes: &fakeNodes{online: []string{"pve1"}, levels: map[string]int{"pve1": 3}}, Sites: fakeSites{sites: &Sites{Templates: []TemplateSite{templateSiteOf(t, c, "pve1", "x86-64-v4")}}}}
	if err := h.checkNodeOS(t.Context(), h.reads(), c); err == nil || !strings.Contains(err.Error(), "no allowed node (pve1)") {
		t.Errorf("checkNodeOS = %v, want a refusal for a v4 template on a v3 node", err)
	}
}

func TestHooksRefuseAClusterNoNodeCanCloneForOrStoreOn(t *testing.T) {
	t.Parallel()
	shared := StorageSite{Shared: true, Nodes: []string{"pve1", "pve2"}, Active: []string{"pve1", "pve2"}}
	local := StorageSite{Nodes: []string{"pve1", "pve2"}, Active: []string{"pve1", "pve2"}}
	web := storedCluster("team-a", "web", "ceph", variableOf(allowedNodesVariable, `["pve2"]`))
	sites := &Sites{
		Storages:  map[string]StorageSite{"ceph": shared, "local-lvm": local},
		Templates: []TemplateSite{templateSiteOf(t, web, "pve1", "", "local-lvm")},
	}
	h, _ := hooksFor(t)
	h.Nodes, h.Sites = &fakeNodes{online: []string{"pve1", "pve2"}}, fakeSites{sites: sites}

	resp := callBeforeClusterCreate(t, h, web)
	if resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "template kubehenge-rke2-build is on storage local-lvm") ||
		!strings.Contains(resp.Message, "Put the templates on shared storage") {
		t.Errorf("BeforeClusterCreate = %s %q, want the template, its storage and the fix", resp.Status, resp.Message)
	}
	_, err := (&ClusterValidator{Hooks: h}).ValidateCreate(t.Context(), web)
	if err == nil || !strings.Contains(err.Error(), "clones only on node pve1") {
		t.Errorf("the validator let the cluster in: %v", err)
	}

	// Without a node restriction the template's node is enough.
	open := storedCluster("team-a", "open", "ceph")
	if errs := (&ClusterValidator{Hooks: h}).validatePlacement(t.Context(), h.reads(), nil, open, &clusterv1.ClusterClass{}); len(errs) > 0 {
		t.Errorf("the validator refused a cluster the template's node can host: %v", errs)
	}
}

// lateSites answers after the context is done, as a Proxmox VE that does not answer in time.
type lateSites struct{}

func (lateSites) Sites(ctx context.Context) (*Sites, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// onceBroken fails its first look and then answers with sites.
type onceBroken struct {
	sites *Sites
	asked int
}

func (o *onceBroken) Sites(context.Context) (*Sites, error) {
	o.asked++
	if o.asked == 1 {
		return nil, errors.New("connection refused")
	}
	return o.sites, nil
}

func TestAdmissionAdmitsAClusterWhenProxmoxVECannotBeReadInTime(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "ceph")
	class := &clusterv1.ClusterClass{}
	for name, src := range map[string]SiteSource{
		"late":      lateSites{},
		"erroring":  fakeSites{err: errors.New("connection refused")},
		"unreached": &onceBroken{},
	} {
		h, _ := hooksFor(t)
		h.Nodes, h.Sites, h.AdmissionBudget = &fakeNodes{online: []string{"pve1"}}, src, 20*time.Millisecond
		v := &ClusterValidator{Hooks: h}
		start := time.Now()
		ctx, cancel := h.admissionContext(t.Context())
		reads := h.reads()
		if errs := v.validatePlacement(ctx, reads, nil, web, class); len(errs) > 0 {
			t.Errorf("%s: validatePlacement refused the cluster: %v", name, errs)
		}
		if errs := v.validateNodeOS(ctx, reads, nil, web); len(errs) > 0 {
			t.Errorf("%s: validateNodeOS refused the cluster: %v", name, errs)
		}
		cancel()
		if took := time.Since(start); took > time.Second {
			t.Errorf("%s: admission took %s, past its budget", name, took)
		}
	}
	// Nodes that cannot be read are the same.
	h, _ := hooksFor(t)
	h.Nodes, h.Sites = &fakeNodes{err: errors.New("connection refused")}, fakeSites{sites: &Sites{}}
	if errs := (&ClusterValidator{Hooks: h}).validatePlacement(t.Context(), h.reads(), nil, web, class); len(errs) > 0 {
		t.Errorf("an unreadable node list refused the cluster: %v", errs)
	}
}

// lateNodes answers after the context is done.
type lateNodes struct{ asked int }

func (l *lateNodes) OnlineNodes(ctx context.Context) ([]Node, error) {
	l.asked++
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAdmissionSpendsOneBudgetOnAllItsChecksOfProxmoxVE(t *testing.T) {
	t.Parallel()
	const budget = 400 * time.Millisecond
	web := storedCluster("team-a", "web", "ceph")
	h, _ := hooksFor(t)
	nodes := &lateNodes{}
	h.Nodes, h.Sites, h.AdmissionBudget = nodes, lateSites{}, budget
	start := time.Now()
	// The cluster's size is refused for its own reasons. Neither check of Proxmox VE may refuse it.
	if _, err := (&ClusterValidator{Hooks: h}).ValidateCreate(t.Context(), web); err != nil && strings.Contains(err.Error(), "spec.topology.variables") {
		t.Fatalf("a Proxmox VE that never answers refused the cluster: %v", err)
	}
	// The node OS check and the placement check each wait for the nodes. Separate budgets would
	// take twice the one.
	if took := time.Since(start); took >= budget*3/2 {
		t.Errorf("admission took %s, want one budget of %s", took, budget)
	}
	if nodes.asked != 1 {
		t.Errorf("the node list was read %d times, want once", nodes.asked)
	}
}

func TestAdmissionReadsTheNodesOnceForBothChecks(t *testing.T) {
	t.Parallel()
	shared := StorageSite{Shared: true, Nodes: []string{"pve1"}, Active: []string{"pve1"}}
	web := storedCluster("team-a", "web", "ceph")
	h, _ := hooksFor(t)
	nodes := &fakeNodes{online: []string{"pve1"}}
	sites := &Sites{Storages: map[string]StorageSite{"ceph": shared}, Templates: []TemplateSite{templateSiteOf(t, web, "pve1", "", "ceph")}}
	h.Nodes, h.Sites = nodes, fakeSites{sites: sites}
	if _, err := (&ClusterValidator{Hooks: h}).ValidateCreate(t.Context(), web); err != nil && strings.Contains(err.Error(), "spec.topology.variables") {
		t.Fatalf("ValidateCreate = %v", err)
	}
	if nodes.asked != 1 {
		t.Errorf("a create that needs both checks read the nodes %d times, want once", nodes.asked)
	}
}

func TestCreationHookHoldsAClusterAdmissionLetIn(t *testing.T) {
	t.Parallel()
	local := StorageSite{Nodes: []string{"pve1", "pve2"}, Active: []string{"pve1", "pve2"}}
	web := storedCluster("team-a", "web", "local-lvm")
	web.Spec.Topology.Variables = append(web.Spec.Topology.Variables, variableOf(allowedNodesVariable, `["pve2"]`))
	sites := &Sites{
		Storages:  map[string]StorageSite{"local-lvm": local},
		Templates: []TemplateSite{templateSiteOf(t, web, "pve1", "", "local-lvm")},
	}
	src := &onceBroken{sites: sites}
	h, _ := hooksFor(t)
	h.Nodes, h.Sites = &fakeNodes{online: []string{"pve1", "pve2"}}, src
	if errs := (&ClusterValidator{Hooks: h}).validatePlacement(t.Context(), h.reads(), nil, web, &clusterv1.ClusterClass{}); len(errs) > 0 {
		t.Fatalf("admission refused on a failed read: %v", errs)
	}
	resp := callBeforeClusterCreate(t, h, web)
	if resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "clones only on node pve1") {
		t.Errorf("BeforeClusterCreate = %s %q, want the template that reaches no allowed node", resp.Status, resp.Message)
	}
}

func TestPlacementPatchFollowsTheSites(t *testing.T) {
	t.Parallel()
	c := cluster()
	shared := StorageSite{Nodes: []string{"pve1", "pve2"}, Active: []string{"pve1", "pve2"}}
	h := &Hooks{
		Nodes: &fakeNodes{online: []string{"pve1", "pve2"}},
		Sites: fakeSites{sites: &Sites{Storages: map[string]StorageSite{"t": shared}, Templates: []TemplateSite{templateSiteOf(t, c, "pve2", "", "t")}}},
	}
	got, _, err := h.onlineNodes(t.Context(), c, nil)
	if err != nil || !slices.Equal(got, []string{"pve2"}) {
		t.Errorf("onlineNodes = %v, %v; want only the template's node", got, err)
	}
	// The template's node in maintenance leaves no node, and the cluster says why.
	h.Nodes = &fakeNodes{online: []string{"pve1", "pve2"}, maintenance: []string{"pve2"}}
	got, blocked, err := h.onlineNodes(t.Context(), c, []string{"pve2"})
	if err != nil || len(got) != 0 || !strings.Contains(blocked, "pve2 is in maintenance") {
		t.Errorf("onlineNodes = %v, %q, %v; want none, blocked by pve2's maintenance", got, blocked, err)
	}
	h.Sites = fakeSites{err: errors.New("down")}
	if _, _, err = h.onlineNodes(t.Context(), c, nil); err == nil {
		t.Error("a failed look at the storages was ignored")
	}
}

func TestPlacerRecordsOnlyTheNodesTheTemplateCanBeClonedOn(t *testing.T) {
	t.Parallel()
	web := cluster()
	_, c := newReconciler(t, nil, web)
	local := StorageSite{Nodes: []string{"pve1", "pve2"}, Active: []string{"pve1", "pve2"}}
	p := &Placer{
		Client: c, ClassNamespace: "kubehenge-system", Nodes: &fakeNodes{online: []string{"pve1", "pve2"}},
		Sites: fakeSites{sites: &Sites{Storages: map[string]StorageSite{"t": local}, Templates: []TemplateSite{templateSiteOf(t, web, "pve1", "", "t")}}},
	}
	p.placeAll(t.Context())
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[OnlineNodesAnnotation]; v != "pve1" {
		t.Errorf("web records %q, want pve1", v)
	}
}

type skippingFake struct {
	nodes   *fakeNodes
	skipped []NodeFault
}

func (s *skippingFake) OnlineNodes(ctx context.Context) ([]Node, error) {
	return s.nodes.OnlineNodes(ctx)
}

func (s *skippingFake) Unreachable() []NodeFault { return s.skipped }

func TestOnlineNodesSkipsANodeWhoseStatusCannotBeRead(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api2/json/nodes":
			data = []map[string]any{{"node": "pve1", "status": "online"}, {"node": "pve2", "status": "online"}}
		case "/api2/json/nodes/pve1/status":
			data = map[string]any{"cpuinfo": map[string]any{"flags": zen3Flags}}
		case "/api2/json/cluster/resources":
			data = []any{}
		default:
			http.Error(w, "500 connection refused", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	p := &Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}
	online, err := p.OnlineNodes(t.Context())
	if err != nil || !reflect.DeepEqual(online, []Node{{Name: "pve1", CPULevel: 3}}) {
		t.Fatalf("OnlineNodes = %v, %v; want pve1 alone and no error", online, err)
	}
	if got := p.Unreachable(); len(got) != 1 || got[0].Node != "pve2" || !strings.Contains(got[0].Reason, "500") {
		t.Errorf("Unreachable = %v, want pve2 with the 500", got)
	}
}

func TestPlacerCountsTheNodesItLeftOut(t *testing.T) {
	t.Parallel()
	web := cluster()
	_, c := newReconciler(t, nil, web)
	nodes := &skippingFake{nodes: &fakeNodes{online: []string{"pve1"}}, skipped: []NodeFault{{Node: "pve2", Reason: "HTTP 596: certificate verify failed"}}}
	p := &Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: nodes}
	p.placeAll(t.Context())
	read := func() *metav1.Condition {
		got := &clusterv1.Cluster{}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(got.Status.Conditions, NodesSkippedCondition)
	}
	if cond := read(); cond == nil || cond.Status != metav1.ConditionTrue || cond.Message != "Not reachable through the cluster: pve2 (HTTP 596: certificate verify failed)" {
		t.Errorf("%s = %+v, want it to name pve2 with the 596 and its reason", NodesSkippedCondition, cond)
	}
	nodes.skipped = nil
	p.placeAll(t.Context())
	if cond := read(); cond != nil {
		t.Errorf("%s = %+v once every node answers, want it gone", NodesSkippedCondition, cond)
	}
}

func TestStorageOutsideTheModulesListIsRefused(t *testing.T) {
	t.Parallel()
	class := &clusterv1.ClusterClass{Spec: clusterv1.ClusterClassSpec{Variables: []clusterv1.ClusterClassVariable{{
		Name: "storage",
		Schema: clusterv1.VariableSchema{OpenAPIV3Schema: clusterv1.JSONSchemaProps{
			Type: "string",
			Enum: []apiextensionsv1.JSON{{Raw: []byte(`"ceph"`)}, {Raw: []byte(`"nvme"`)}},
		}},
	}}}}
	if err := checkStorageListed(storedCluster("team-a", "web", "nvme"), class); err != nil {
		t.Errorf("a listed storage was refused: %v", err)
	}
	err := checkStorageListed(storedCluster("team-a", "web", "scratch"), class)
	if err == nil || !strings.Contains(err.Error(), "storage scratch is not in workload_datastores (ceph, nvme)") || !strings.Contains(err.Error(), "Add it to workload_datastores") {
		t.Errorf("an unlisted storage: %v, want the storage and the setting to add it to", err)
	}
	h, _ := hooksFor(t)
	if err = h.refusal(t.Context(), storedCluster("team-a", "web", "scratch"), class); err == nil || !strings.Contains(err.Error(), "workload_datastores") {
		t.Errorf("BeforeClusterCreate's checks let an unlisted storage through: %v", err)
	}
	if errs := (&ClusterValidator{Hooks: h}).validatePlacement(t.Context(), h.reads(), nil, storedCluster("team-a", "web", "scratch"), class); len(errs) == 0 {
		t.Error("the validator let an unlisted storage through")
	}
	if err := checkStorageListed(storedCluster("team-a", "web", "scratch"), &clusterv1.ClusterClass{}); err != nil {
		t.Errorf("a class that offers any storage refused one: %v", err)
	}
}

func TestAFailedProxmoxObjectShowsInTheHooksWaitingMessage(t *testing.T) {
	t.Parallel()
	if got := failureReason(&cloudError{err: errors.New("x")}); got != CloudFailedReason {
		t.Errorf("failureReason = %q", got)
	}
	if got := failureReason(errors.New("x")); got != ReconcileFailedReason {
		t.Errorf("failureReason = %q for another error", got)
	}
	web := networkCluster("192.168.1.1", 24)
	web.Spec.Topology.Variables = append(web.Spec.Topology.Variables, variableOf("storage", `"local-lvm"`))
	web.Status.Conditions = []metav1.Condition{{
		Type: KubehengeReadyCondition, Status: metav1.ConditionFalse, Reason: CloudFailedReason,
		Message: cloudErrorPrefix + "creating pool kubehenge/team-a.web: permission check failed (Pool.Allocate)",
	}}
	h, _ := hooksFor(t)
	resp := callBeforeClusterCreate(t, h, web)
	want := "Proxmox VE objects: creating pool kubehenge/team-a.web: permission check failed (Pool.Allocate)"
	if resp.Message != want || resp.RetryAfterSeconds == 0 {
		t.Errorf("BeforeClusterCreate = %q retry %d, want %q", resp.Message, resp.RetryAfterSeconds, want)
	}
}
