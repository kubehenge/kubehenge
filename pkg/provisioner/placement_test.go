package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/go-proxmox"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// fakeNodes are online nodes whose CPUs reach x86-64-v3 unless levels says otherwise.
type fakeNodes struct {
	online []string
	levels map[string]int
	err    error
	asked  int
}

func (f *fakeNodes) OnlineNodes(context.Context) ([]Node, error) {
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Node, len(f.online))
	for i, n := range f.online {
		level, ok := f.levels[n]
		if !ok {
			level = 3
		}
		out[i] = Node{Name: n, CPULevel: level}
	}
	return out, nil
}

func TestPlacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		chosen, online []string
		want           []string
	}{
		{"every online node when none is chosen", nil, []string{"pve1", "pve3"}, []string{"pve1", "pve3"}},
		{"the chosen ones that are online", []string{"pve2", "pve1", "pve3"}, []string{"pve1", "pve2"}, []string{"pve2", "pve1"}},
		{"the chosen ones while every one of them is down", []string{"pve4"}, []string{"pve1"}, []string{"pve4"}},
		{"none while nothing is online and nothing chosen", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chosen := slices.Clone(tc.chosen)
			if got := placement(chosen, tc.online); !slices.Equal(got, tc.want) {
				t.Errorf("placement(%v, %v) = %v, want %v", tc.chosen, tc.online, got, tc.want)
			}
			if !slices.Equal(chosen, tc.chosen) {
				t.Errorf("placement changed the chosen nodes to %v", chosen)
			}
		})
	}
}

// generate asks the hook for the patches of web's templates, with the variables given.
func generate(t *testing.T, h *Hooks, variables ...runtimehooksv1.Variable) *runtimehooksv1.GeneratePatchesResponse {
	t.Helper()
	holder := func(path string) runtimehooksv1.HolderReference {
		return runtimehooksv1.HolderReference{Kind: "Cluster", Namespace: "team-a", Name: "web", FieldPath: path}
	}
	req := &runtimehooksv1.GeneratePatchesRequest{Variables: variables, Items: []runtimehooksv1.GeneratePatchesRequestItem{
		{UID: "control-plane", HolderReference: holder("spec.controlPlaneRef")},
		{UID: "infrastructure", HolderReference: holder("spec.infrastructureRef")},
	}}
	resp := &runtimehooksv1.GeneratePatchesResponse{}
	h.GeneratePatches(t.Context(), req, resp)
	return resp
}

// patchedNodes is the list of nodes the response gives capmox, or nil when it gives none.
func patchedNodes(t *testing.T, resp *runtimehooksv1.GeneratePatchesResponse) []string {
	t.Helper()
	if resp.Status != runtimehooksv1.ResponseStatusSuccess {
		t.Fatalf("status %s: %s", resp.Status, resp.Message)
	}
	if len(resp.Items) == 0 {
		return nil
	}
	if len(resp.Items) != 1 || resp.Items[0].UID != "infrastructure" || resp.Items[0].PatchType != runtimehooksv1.JSONPatchType {
		t.Fatalf("items = %+v", resp.Items)
	}
	var ops []struct {
		Op    string   `json:"op"`
		Path  string   `json:"path"`
		Value []string `json:"value"`
	}
	if err := json.Unmarshal(resp.Items[0].Patch, &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Op != "add" || ops[0].Path != "/spec/template/spec/allowedNodes" {
		t.Fatalf("patch = %s", resp.Items[0].Patch)
	}
	return ops[0].Value
}

func allowed(nodes string) runtimehooksv1.Variable {
	return runtimehooksv1.Variable{Name: "allowedNodes", Value: apiextensionsv1.JSON{Raw: []byte(nodes)}}
}

func TestGeneratePatchesNamesTheOnlineNodes(t *testing.T) {
	t.Parallel()
	c := cluster()
	c.Annotations = map[string]string{OnlineNodesAnnotation: "pve1,pve2"}
	h, _ := hooksFor(t, c)
	nodes := &fakeNodes{online: []string{"pve9"}}
	h.Nodes = nodes
	if got := patchedNodes(t, generate(t, h)); !slices.Equal(got, []string{"pve1", "pve2"}) {
		t.Errorf("unchosen: %v", got)
	}
	if got := patchedNodes(t, generate(t, h, allowed(`["pve2","pve3"]`))); !slices.Equal(got, []string{"pve2"}) {
		t.Errorf("chosen: %v", got)
	}
	if nodes.asked != 0 {
		t.Errorf("asked Proxmox VE %d times although the cluster records its nodes", nodes.asked)
	}
	resp := generate(t, h, allowed(`"pve1"`))
	if resp.Status != runtimehooksv1.ResponseStatusFailure {
		t.Errorf("a malformed variable answers %s", resp.Status)
	}
}

func TestGeneratePatchesAsksForANewCluster(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t, cluster())
	nodes := &fakeNodes{online: []string{"pve1", "pve2"}}
	h.Nodes = nodes
	if got := patchedNodes(t, generate(t, h)); !slices.Equal(got, []string{"pve1", "pve2"}) {
		t.Errorf("nodes = %v", got)
	}
	nodes.err = errors.New("no route to host")
	if resp := generate(t, h); resp.Status != runtimehooksv1.ResponseStatusFailure {
		t.Errorf("without the nodes the hook answers %s", resp.Status)
	}
	// Nothing online and nothing chosen leaves capmox to its own choice.
	c := cluster()
	c.Annotations = map[string]string{OnlineNodesAnnotation: ""}
	h, _ = hooksFor(t, c)
	if resp := generate(t, h); resp.Status != runtimehooksv1.ResponseStatusSuccess || len(resp.Items) != 0 {
		t.Errorf("with no node to name: %s, %+v", resp.Status, resp.Items)
	}
}

// The CPU flags of a Westmere, which reaches x86-64-v2, and of a Zen 3, which reaches v3.
const (
	westmereFlags = "fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36 clflush mmx fxsr sse sse2 " +
		"syscall nx lm constant_tsc rep_good nopl pni pclmulqdq ssse3 cx16 sse4_1 sse4_2 x2apic popcnt aes hypervisor lahf_lm"
	zen3Flags = westmereFlags + " avx f16c rdrand abm sse4a misalignsse 3dnowprefetch fma bmi1 avx2 smep bmi2 erms movbe xsave xsaveopt"
)

func TestOnlineNodesLeavesOutNodesThatAreDown(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	statuses := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = []any{}
		switch r.URL.Path {
		case "/api2/json/nodes":
			data = []map[string]any{{"node": "pve3", "status": "online"}, {"node": "pve2", "status": "offline"}, {"node": "pve1", "status": "online"}}
		case "/api2/json/nodes/pve1/status", "/api2/json/nodes/pve3/status":
			mu.Lock()
			statuses[r.URL.Path]++
			mu.Unlock()
			flags := zen3Flags
			if strings.Contains(r.URL.Path, "pve1") {
				flags = westmereFlags
			}
			data = map[string]any{"cpuinfo": map[string]any{"flags": flags, "model": "a CPU"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	p := &Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}
	for range 2 {
		online, err := p.OnlineNodes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(online, []Node{{"pve1", 2}, {"pve3", 3}}) {
			t.Errorf("online = %v", online)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if statuses["/api2/json/nodes/pve1/status"] != 1 || statuses["/api2/json/nodes/pve3/status"] != 1 {
		t.Errorf("read the nodes' CPUs %v times; once each is enough", statuses)
	}
}

func TestNodesForTheNodeOSesCPU(t *testing.T) {
	t.Parallel()
	online := []Node{{"pve1", 2}, {"pve2", 3}, {"pve3", 4}}
	if got := nodesFor(online, templates.DefaultNodeOS); !slices.Equal(got, []string{"pve1", "pve2", "pve3"}) {
		t.Errorf("AlmaLinux runs on %v", got)
	}
	if got := nodesFor(online, templates.NodeOS{Family: "rocky", Release: "10"}); !slices.Equal(got, []string{"pve2", "pve3"}) {
		t.Errorf("Rocky Linux runs on %v", got)
	}
}

func TestCheckNodeOSRefusesWhatNoAllowedNodeRuns(t *testing.T) {
	t.Parallel()
	rocky := nodeOSVariable(t, "rocky", "10")
	for _, tc := range []struct {
		name    string
		vars    []clusterv1.ClusterVariable
		online  []string
		levels  map[string]int
		refused string
	}{
		{"the default on any node", nil, []string{"pve1"}, map[string]int{"pve1": 2}, ""},
		{"Rocky Linux on a node of its level", []clusterv1.ClusterVariable{rocky}, []string{"pve1", "pve2"}, map[string]int{"pve1": 2}, ""},
		{"Rocky Linux where no node reaches its level", []clusterv1.ClusterVariable{rocky}, []string{"pve1"}, map[string]int{"pve1": 2}, "x86-64-v3"},
		{
			"Rocky Linux where only a node the cluster may not use reaches it",
			[]clusterv1.ClusterVariable{rocky, clusterVariable(t, allowedNodesVariable, []string{"pve1"})},
			[]string{"pve1", "pve2"},
			map[string]int{"pve1": 2},
			"(pve1)",
		},
		{"nothing online to judge by", []clusterv1.ClusterVariable{rocky}, nil, nil, ""},
		{"a node OS not released", []clusterv1.ClusterVariable{nodeOSVariable(t, "leap", "16.1")}, []string{"pve1"}, nil, "not released"},
		{"an unknown one", []clusterv1.ClusterVariable{nodeOSVariable(t, "plan9", "4")}, []string{"pve1"}, nil, "unknown node OS family"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := cluster(tc.vars...)
			h := &Hooks{Nodes: &fakeNodes{online: tc.online, levels: tc.levels}}
			err := h.checkNodeOS(t.Context(), c)
			if tc.refused == "" && err != nil || tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)) {
				t.Errorf("checkNodeOS = %v, want a refusal naming %q", err, tc.refused)
			}
		})
	}
}

func clusterVariable(t *testing.T, name string, value any) clusterv1.ClusterVariable {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: raw}}
}

func nodeOSVariable(t *testing.T, family, release string) clusterv1.ClusterVariable {
	t.Helper()
	return clusterVariable(t, NodeOSVariable, templates.NodeOS{Family: family, Release: release})
}

func TestPlacerRecordsOnlyTheNodesThatRunTheNodeOS(t *testing.T) {
	t.Parallel()
	web := cluster(nodeOSVariable(t, "rocky", "10"))
	_, c := newReconciler(t, nil, web)
	p := &Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: &fakeNodes{online: []string{"pve1", "pve2"}, levels: map[string]int{"pve1": 2}}}
	p.placeAll(t.Context())
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[OnlineNodesAnnotation]; v != "pve2" {
		t.Errorf("web records %q", v)
	}
}

func TestPlacerRecordsTheOnlineNodes(t *testing.T) {
	t.Parallel()
	other := cluster()
	other.Name = "other"
	other.Spec.Topology.ClassRef.Name = "someone-elses"
	current := cluster()
	current.Name = "current"
	current.Annotations = map[string]string{OnlineNodesAnnotation: "pve1,pve2"}
	r, c := newReconciler(t, nil, cluster(), other, current)
	nodes := &fakeNodes{online: []string{"pve1", "pve2"}}
	p := &Placer{Client: c, ClassNamespace: r.ClassNamespace, Nodes: nodes}
	p.placeAll(t.Context())
	annotation := func(name string) (string, bool) {
		got := &clusterv1.Cluster{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: name}, got); err != nil {
			t.Fatal(err)
		}
		v, ok := got.Annotations[OnlineNodesAnnotation]
		return v, ok
	}
	if v, _ := annotation("web"); v != "pve1,pve2" {
		t.Errorf("web records %q", v)
	}
	if _, ok := annotation("other"); ok {
		t.Error("marked a cluster of another class")
	}
	// Every write has Cluster API compute the topology again, so an unchanged list writes nothing.
	writes := 0
	watched, ok := c.(client.WithWatch)
	if !ok {
		t.Fatalf("the reconciler's client is a %T", c)
	}
	p.Client = interceptor.NewClient(watched, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes++
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	p.placeAll(t.Context())
	if writes != 0 {
		t.Errorf("%d writes with nothing changed", writes)
	}
	nodes.online = []string{"pve1"}
	p.placeAll(t.Context())
	if v, _ := annotation("current"); v != "pve1" {
		t.Errorf("after pve2 went down, current records %q", v)
	}
	nodes.err = errors.New("no route to host")
	p.placeAll(t.Context())
	if v, _ := annotation("web"); v != "pve1" {
		t.Errorf("an unreadable node list changed web to %q", v)
	}
}
