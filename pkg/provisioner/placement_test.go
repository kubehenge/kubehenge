package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/luthermonson/go-proxmox"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type fakeNodes struct {
	online []string
	err    error
	asked  int
}

func (f *fakeNodes) OnlineNodes(context.Context) ([]string, error) {
	f.asked++
	return f.online, f.err
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

func TestOnlineNodesLeavesOutNodesThatAreDown(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = []any{}
		if r.URL.Path == "/api2/json/nodes" {
			data = []map[string]any{{"node": "pve3", "status": "online"}, {"node": "pve2", "status": "offline"}, {"node": "pve1", "status": "online"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	p := &Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}
	online, err := p.OnlineNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(online, []string{"pve1", "pve3"}) {
		t.Errorf("online = %v", online)
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
