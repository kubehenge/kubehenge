package provisioner

import (
	"slices"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNodesToPlaceLeavesOutMaintenanceAndInactiveStorage(t *testing.T) {
	t.Parallel()
	pve := func(maintenance bool, inactive ...string) Node {
		return Node{CPULevel: 3, Maintenance: maintenance, InactiveStorages: inactive}
	}
	named := func(name string, n Node) Node {
		n.Name = name
		return n
	}
	online := []Node{named("pve1", pve(true)), named("pve2", pve(false, "nfs")), named("pve3", pve(false))}
	for _, tc := range []struct {
		name    string
		online  []Node
		chosen  []string
		storage string
		want    []string
		blocked []string
	}{
		{"a node in maintenance", online, nil, "", []string{"pve2", "pve3"}, nil},
		{"a node in maintenance and one without the storage", online, nil, "nfs", []string{"pve3"}, nil},
		{"another storage is not the one inactive", online, nil, "local-lvm", []string{"pve2", "pve3"}, nil},
		{"the chosen node that is eligible", online, []string{"pve1", "pve3"}, "nfs", []string{"pve3"}, nil},
		{
			"every node in maintenance",
			[]Node{named("pve1", pve(true)), named("pve2", pve(true))},
			nil, "",
			nil,
			[]string{"pve1 is in maintenance", "pve2 is in maintenance"},
		},
		{
			"the storage inactive on every node",
			[]Node{named("pve1", pve(false, "nfs")), named("pve2", pve(false, "nfs"))},
			nil, "nfs",
			nil,
			[]string{"storage nfs is not active on pve1", "storage nfs is not active on pve2"},
		},
		{
			"only chosen nodes, all unfit, while another is fine", online,
			[]string{"pve1", "pve2"},
			"nfs",
			nil,
			[]string{"pve1 is in maintenance", "storage nfs is not active on pve2"},
		},
		{"chosen nodes that are all offline stay chosen", online, []string{"pve9"}, "nfs", []string{"pve9"}, nil},
		{"nothing online", nil, nil, "nfs", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, blocked := nodesToPlace(tc.online, 1, tc.chosen, tc.storage)
			if !slices.Equal(got, tc.want) {
				t.Errorf("nodes = %v, want %v", got, tc.want)
			}
			if (len(tc.blocked) == 0) != (blocked == "") {
				t.Fatalf("blocked = %q, want it to name %v", blocked, tc.blocked)
			}
			for _, why := range tc.blocked {
				if !strings.Contains(blocked, why) {
					t.Errorf("blocked = %q, missing %q", blocked, why)
				}
			}
		})
	}
}

func TestPlacerRecordsWhyNoNodeCanTakeAVM(t *testing.T) {
	t.Parallel()
	web := cluster(variableOf("storage", `"nfs"`))
	_, c := newReconciler(t, nil, web)
	nodes := &fakeNodes{online: []string{"pve1", "pve2"}, maintenance: []string{"pve1"}, inactive: map[string][]string{"pve2": {"nfs"}}}
	p := &Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: nodes}
	read := func() *clusterv1.Cluster {
		t.Helper()
		got := &clusterv1.Cluster{}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	p.placeAll(t.Context())
	got := read()
	if v := got.Annotations[OnlineNodesAnnotation]; v != "" {
		t.Errorf("records the nodes %q", v)
	}
	why := got.Annotations[PlacementBlockedAnnotation]
	if !strings.Contains(why, "pve1 is in maintenance") || !strings.Contains(why, "storage nfs is not active on pve2") {
		t.Errorf("records %q", why)
	}
	h, _ := hooksFor(t, got)
	if resp := generate(t, h); resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "pve1 is in maintenance") {
		t.Errorf("the hook answers %s: %q", resp.Status, resp.Message)
	}
	nodes.maintenance, nodes.inactive = nil, nil
	p.placeAll(t.Context())
	got = read()
	if v, ok := got.Annotations[PlacementBlockedAnnotation]; ok || got.Annotations[OnlineNodesAnnotation] != "pve1,pve2" {
		t.Errorf("after the nodes are fit: blocked %q, nodes %q", v, got.Annotations[OnlineNodesAnnotation])
	}
}

func TestGeneratePatchesAvoidsNodesInMaintenanceForANewCluster(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t, cluster(variableOf("storage", `"nfs"`)))
	h.Nodes = &fakeNodes{online: []string{"pve1", "pve2", "pve3"}, maintenance: []string{"pve1"}, inactive: map[string][]string{"pve2": {"nfs"}}}
	if got := patchedNodes(t, generate(t, h)); !slices.Equal(got, []string{"pve3"}) {
		t.Errorf("nodes = %v", got)
	}
	h.Nodes = &fakeNodes{online: []string{"pve1"}, maintenance: []string{"pve1"}}
	if resp := generate(t, h); resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "pve1 is in maintenance") {
		t.Errorf("with every node in maintenance: %s: %q", resp.Status, resp.Message)
	}
}
