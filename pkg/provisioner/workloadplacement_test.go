package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// memNodes are online nodes whose free memory, in MiB, a test moves between looks.
type memNodes struct {
	mu   sync.Mutex
	free map[string]int64
}

func (m *memNodes) OnlineNodes(context.Context) ([]Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Node, 0, len(m.free))
	for name, free := range m.free {
		out = append(out, Node{Name: name, CPULevel: 3, FreeMiB: free, MemoryKnown: true})
	}
	slices.SortFunc(out, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (*memNodes) TemplateTags(context.Context) ([][]string, error) { return nil, nil }

func (m *memNodes) set(name string, free int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.free[name] = free
}

func (m *memNodes) drop(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.free, name)
}

// bigWorkers is a cluster whose largest machine, its worker, takes 8192 MiB. A node takes it in with
// 8192 MiB free and joins a placement with a tenth more, 9011.
func bigWorkers(t *testing.T, recorded *string) *clusterv1.Cluster {
	t.Helper()
	web := cluster(
		clusterVariable(t, "controlPlaneMachine", map[string]int{"cores": 2, "memoryMiB": 4096, "diskGiB": 30}),
		clusterVariable(t, "workerMachine", map[string]int{"cores": 2, "memoryMiB": 8192, "diskGiB": 30}),
	)
	web.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0"}}
	if recorded != nil {
		web.Annotations = map[string]string{OnlineNodesAnnotation: *recorded}
	}
	return web
}

func placedBy(t *testing.T, web *clusterv1.Cluster, nodes Nodes) (*Placer, client.Client) {
	t.Helper()
	_, c := newReconciler(t, nil, web)
	return &Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: nodes}, c
}

// lookAt is what the Placer recorded on the cluster, and its NodesLackMemory condition.
func lookAt(t *testing.T, c client.Client, web *clusterv1.Cluster) (string, *metav1.Condition) {
	t.Helper()
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	return got.Annotations[OnlineNodesAnnotation], meta.FindStatusCondition(got.Status.Conditions, NodesLackMemoryCondition)
}

func TestPlacerJudgesNodesByTheirRealFreeMemory(t *testing.T) {
	t.Parallel()
	web := bigWorkers(t, nil)
	// Whatever the guests on pve1 are configured with, Proxmox VE reports 20 GiB free there.
	p, c := placedBy(t, web, &memNodes{free: map[string]int64{"pve1": 20_000, "pve2": 2048, "pve3": 8192}})
	p.placeAll(t.Context())
	value, cond := lookAt(t, c, web)
	if value != "pve1,pve3" {
		t.Errorf("web records %q, want the node with room and the one with exactly the machine's size", value)
	}
	if cond != nil {
		t.Errorf("%s = %+v while nodes have room", NodesLackMemoryCondition, cond)
	}
}

func TestPlacerKeepsThePlacementWhenNoNodeHasRoom(t *testing.T) {
	t.Parallel()
	before := "pve1,pve2"
	web := bigWorkers(t, &before)
	nodes := &memNodes{free: map[string]int64{"pve1": 4096, "pve2": 5120 + 1023}}
	p, c := placedBy(t, web, nodes)
	p.placeAll(t.Context())
	value, cond := lookAt(t, c, web)
	if value != before {
		t.Errorf("web records %q, want the placement it had", value)
	}
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "NoRoom" ||
		cond.Message != "No Proxmox VE node this cluster may use has 8 GiB of free memory for its largest machine. The best, pve2, has 5 GiB" {
		t.Errorf("%s = %+v, want the memory needed and the best node's free memory", NodesLackMemoryCondition, cond)
	}

	nodes.set("pve2", 30_000)
	p.placeAll(t.Context())
	value, cond = lookAt(t, c, web)
	if value != "pve2" || cond != nil {
		t.Errorf("web records %q with %+v once pve2 has room, want pve2 and no condition", value, cond)
	}
}

func TestPlacerHoldsOnlyTheNodesThatCanStillRunTheCluster(t *testing.T) {
	t.Parallel()
	before := "pve1,pve2"
	web := bigWorkers(t, &before)
	nodes := &memNodes{free: map[string]int64{"pve1": 1000, "pve2": 1000}}
	p, c := placedBy(t, web, nodes)
	nodes.drop("pve2")
	p.placeAll(t.Context())
	if value, cond := lookAt(t, c, web); value != "pve1" || cond == nil {
		t.Errorf("web records %q with %+v, want the node still up, and the condition", value, cond)
	}
}

func TestPlacerStartsAFirstPlacementEvenWithoutRoom(t *testing.T) {
	t.Parallel()
	web := bigWorkers(t, nil)
	p, c := placedBy(t, web, &memNodes{free: map[string]int64{"pve1": 100, "pve2": 200}})
	p.placeAll(t.Context())
	value, cond := lookAt(t, c, web)
	if value != "pve1,pve2" || cond == nil {
		t.Errorf("web records %q with %+v, want every capable node and the condition", value, cond)
	}
}

func TestPlacerDoesNotFlapAroundTheThreshold(t *testing.T) {
	t.Parallel()
	before := "pve1,pve2"
	web := bigWorkers(t, &before)
	nodes := &memNodes{free: map[string]int64{"pve1": 8200, "pve2": 30_000, "pve3": 8500}}
	p, c := placedBy(t, web, nodes)
	for _, step := range []struct {
		free int64
		want string
		why  string
	}{
		{8500, "pve1,pve2", "pve3 has room but not the margin to join, and pve1 stays above the size"},
		{9100, "pve1,pve2,pve3", "pve3 clears the margin"},
		{8300, "pve1,pve2,pve3", "pve3 falls back above the size and stays"},
		{8000, "pve1,pve2", "pve3 falls below the size"},
	} {
		nodes.set("pve3", step.free)
		p.placeAll(t.Context())
		if value, _ := lookAt(t, c, web); value != step.want {
			t.Errorf("pve3 at %d: web records %q, want %q (%s)", step.free, value, step.want, step.why)
		}
	}
}

func TestCreationHookLeavesOutNodesWithoutRoom(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	nodes := &memNodes{free: map[string]int64{"pve1": 20_000, "pve2": 2048}}
	h.Nodes = nodes
	web := bigWorkers(t, nil)
	got, _, err := h.onlineNodes(t.Context(), web, nil)
	if err != nil || !slices.Equal(got, []string{"pve1"}) {
		t.Errorf("onlineNodes = %v, %v; want only the node with room", got, err)
	}
	nodes.set("pve1", 1000)
	got, _, err = h.onlineNodes(t.Context(), web, nil)
	if err != nil || !slices.Equal(got, []string{"pve1", "pve2"}) {
		t.Errorf("onlineNodes = %v, %v; want every capable node while none has room", got, err)
	}
}

func TestLargestMachineIsTheBiggestPool(t *testing.T) {
	t.Parallel()
	web := bigWorkers(t, nil)
	pool := clusterVariable(t, "workerMachine", map[string]int{"cores": 2, "memoryMiB": 16_384, "diskGiB": 30})
	big := clusterv1.MachineDeploymentTopology{Class: "worker", Name: "md-1"}
	big.Variables.Overrides = []clusterv1.ClusterVariable{pool}
	web.Spec.Topology.Workers.MachineDeployments = append(web.Spec.Topology.Workers.MachineDeployments, big)
	class := &clusterv1.ClusterClass{Name: "kubehenge-rke2"}
	if got, err := largestMachineMiB(web, class); err != nil || got != 16_384 {
		t.Errorf("largest machine = %d, %v; want the pool's 16384", got, err)
	}
	web.Spec.Topology.Workers.MachineDeployments = nil
	if got, err := largestMachineMiB(web, class); err != nil || got != 4096 {
		t.Errorf("largest machine = %d, %v; want the control plane's 4096", got, err)
	}
}

// networkOn is a cluster on the bridge, with a VLAN tag when vlan is not 0.
func networkOn(t *testing.T, bridge string, vlan int, extra ...clusterv1.ClusterVariable) *clusterv1.Cluster {
	t.Helper()
	network := map[string]any{"bridge": bridge, "gateway": "192.0.2.1", "prefix": 24}
	if vlan != 0 {
		network["vlan"] = vlan
	}
	return storedCluster("team-a", "web", "ceph", append([]clusterv1.ClusterVariable{clusterVariable(t, "network", network)}, extra...)...)
}

func TestSitesKeepAClusterToTheNodesThatHaveItsBridge(t *testing.T) {
	t.Parallel()
	bridges := map[string][]string{"pve1": {"vmbr0"}, "pve2": {"vmbr0"}, "pve3": {"vmbr0", "vmbr1"}}
	for _, tc := range []struct {
		name   string
		sites  Sites
		bridge string
		vlan   int
		want   []string
	}{
		{"a bridge on every node", Sites{Bridges: bridges}, "vmbr0", 0, []string{"pve1", "pve2", "pve3"}},
		{"a bridge on one node", Sites{Bridges: bridges}, "vmbr1", 0, []string{"pve3"}},
		{"a VLAN-aware bridge on one node, tagged", Sites{Bridges: bridges}, "vmbr1", 20, []string{"pve3"}},
		{"a node whose network could not be read stays", Sites{Bridges: map[string][]string{"pve1": {"vmbr0"}, "pve3": {"vmbr0", "vmbr1"}}}, "vmbr1", 0, []string{"pve2", "pve3"}},
		{"a name no node lists may be a VNet the token cannot see", Sites{Bridges: bridges}, "zone1v1", 0, []string{"pve1", "pve2", "pve3"}},
		{"a VNet applied on two nodes", Sites{Bridges: bridges, VNets: map[string]string{"v1": "z1"}, ZoneNodes: map[string][]string{"z1": {"pve1", "pve2"}}}, "v1", 0, []string{"pve1", "pve2"}},
		{"a VNet whose zone is not applied anywhere", Sites{Bridges: bridges, VNets: map[string]string{"v1": "z1"}, ZoneNodes: map[string][]string{"z1": nil}}, "v1", 0, nil},
		{"a VNet in a zone the token cannot see", Sites{Bridges: bridges, VNets: map[string]string{"v1": "z1"}}, "v1", 0, []string{"pve1", "pve2", "pve3"}},
		{"bridges nobody could read", Sites{}, "vmbr1", 0, []string{"pve1", "pve2", "pve3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.sites.Hosts(networkOn(t, tc.bridge, tc.vlan), threeNodes(), 2); !slices.Equal(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
				t.Errorf("Hosts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHooksRefuseABridgeNoAllowedNodeHas(t *testing.T) {
	t.Parallel()
	bridges := map[string][]string{"pve1": {"vmbr0"}, "pve2": {"vmbr0"}, "pve3": {"vmbr0", "vmbr1"}}
	for _, tc := range []struct {
		name    string
		sites   *Sites
		bridge  string
		allowed string
		refused string
	}{
		{"a bridge on another node", &Sites{Bridges: bridges}, "vmbr1", `["pve1","pve2"]`, "bridge vmbr1 is on pve3, not on the nodes this cluster may use (pve1, pve2)"},
		{"a VNet applied on another node", &Sites{Bridges: bridges, VNets: map[string]string{"v1": "z1"}, ZoneNodes: map[string][]string{"z1": {"pve3"}}}, "v1", `["pve1","pve2"]`, "VNet v1 is on pve3, not on the nodes this cluster may use (pve1, pve2)"},
		{"a VNet applied nowhere", &Sites{VNets: map[string]string{"v1": "z1"}, ZoneNodes: map[string][]string{"z1": nil}}, "v1", `["pve1"]`, "VNet v1 is on no node"},
		{"a bridge on one allowed node", &Sites{Bridges: bridges}, "vmbr1", `["pve1","pve3"]`, ""},
		{"a bridge on every node", &Sites{Bridges: bridges}, "vmbr0", `["pve1","pve2"]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _ := hooksFor(t)
			h.Nodes, h.Sites = &fakeNodes{online: []string{"pve1", "pve2", "pve3"}}, fakeSites{sites: tc.sites}
			web := networkOn(t, tc.bridge, 0, variableOf(allowedNodesVariable, tc.allowed))

			resp := callBeforeClusterCreate(t, h, web)
			_, createErr := (&ClusterValidator{Hooks: h}).ValidateCreate(t.Context(), web)
			if tc.refused == "" {
				if resp.Status == runtimehooksv1.ResponseStatusFailure && strings.Contains(resp.Message, "bridge") || createErr != nil && strings.Contains(createErr.Error(), "bridge") {
					t.Errorf("refused a cluster whose bridge a node has: %q, %v", resp.Message, createErr)
				}
				return
			}
			if resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, tc.refused) {
				t.Errorf("BeforeClusterCreate = %s %q, want %q", resp.Status, resp.Message, tc.refused)
			}
			if createErr == nil || !strings.Contains(createErr.Error(), tc.refused) {
				t.Errorf("the validator let the cluster in or refused otherwise: %v", createErr)
			}
		})
	}
}

func TestAnUpdateOfTheNodesOrTheNetworkIsHeldToTheBridge(t *testing.T) {
	t.Parallel()
	bridges := map[string][]string{"pve1": {"vmbr0"}, "pve2": {"vmbr0"}, "pve3": {"vmbr0", "vmbr1"}}
	h, _ := hooksFor(t)
	h.Nodes, h.Sites = &fakeNodes{online: []string{"pve1", "pve2", "pve3"}}, fakeSites{sites: &Sites{Bridges: bridges}}
	class := &clusterv1.ClusterClass{}
	v := &ClusterValidator{Hooks: h}
	on3 := networkOn(t, "vmbr1", 0, variableOf(allowedNodesVariable, `["pve3"]`))
	on12 := networkOn(t, "vmbr1", 0, variableOf(allowedNodesVariable, `["pve1","pve2"]`))
	if errs := v.validatePlacement(t.Context(), h.reads(), on3, on12, class); len(errs) == 0 || !strings.Contains(errs[0].Detail, "bridge vmbr1 is on pve3") {
		t.Errorf("moving the cluster off the only node with its bridge: %v", errs)
	}
	moved := networkOn(t, "vmbr1", 0, variableOf(allowedNodesVariable, `["pve1","pve2"]`))
	was := networkOn(t, "vmbr0", 0, variableOf(allowedNodesVariable, `["pve1","pve2"]`))
	if errs := v.validatePlacement(t.Context(), h.reads(), was, moved, class); len(errs) == 0 {
		t.Error("changing the network to a bridge the allowed nodes lack was admitted")
	}
	if errs := v.validatePlacement(t.Context(), h.reads(), on12, on12.DeepCopy(), class); len(errs) != 0 {
		t.Errorf("an update of something else was held to the bridge: %v", errs)
	}
}

func TestOnlineNodesReportTheMemoryProxmoxVEHasFree(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = []any{}
		switch r.URL.Path {
		case "/api2/json/nodes":
			data = []map[string]any{
				{"node": "pve1", "status": "online", "mem": 100 * gib, "maxmem": 128 * gib},
				{"node": "pve2", "status": "online"},
			}
		case "/api2/json/nodes/pve1/status", "/api2/json/nodes/pve2/status":
			data = map[string]any{"cpuinfo": map[string]any{"flags": zen3Flags}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	online, err := (&Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}).OnlineNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Node{{Name: "pve1", CPULevel: 3, FreeMiB: 28 * 1024, MemoryKnown: true}, {Name: "pve2", CPULevel: 3}}
	if !reflect.DeepEqual(online, want) {
		t.Errorf("online = %+v, want %+v", online, want)
	}
}

func TestSitesReadTheBridgesAndTheZonesTheTokenSees(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = []any{}
		switch r.URL.Path {
		case "/api2/json/nodes":
			data = []map[string]any{{"node": "pve1", "status": "online"}, {"node": "pve2", "status": "online"}, {"node": "pve3", "status": "offline"}}
		case "/api2/json/nodes/pve1/network":
			if r.URL.Query().Get("type") != "any_bridge" {
				t.Errorf("listed %q, want the bridges only", r.URL.RawQuery)
			}
			data = []map[string]any{{"iface": "vmbr0"}, {"iface": "vmbr1", "bridge_vlan_aware": 1}}
		case "/api2/json/nodes/pve2/network":
			http.Error(w, "permission check failed", http.StatusForbidden)
			return
		case "/api2/json/cluster/sdn/vnets":
			data = []map[string]any{{"vnet": "v1", "zone": "z1"}}
		case "/api2/json/cluster/resources":
			data = []map[string]any{
				{"type": "sdn", "sdn": "z1", "node": "pve1", "status": "ok"},
				{"type": "sdn", "sdn": "z1", "node": "pve2", "status": "error"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	p := &Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}
	s := &Sites{}
	p.readNetworks(t.Context(), s)
	if !slices.Equal(s.Bridges["pve1"], []string{"vmbr0", "vmbr1"}) || len(s.Bridges) != 1 {
		t.Errorf("bridges = %v, want pve1's, with the node that refused and the one that is down left unknown", s.Bridges)
	}
	if s.VNets["v1"] != "z1" || !slices.Equal(s.ZoneNodes["z1"], []string{"pve1"}) {
		t.Errorf("VNets %v and zones %v, want v1 in z1 applied on pve1 only", s.VNets, s.ZoneNodes)
	}
}
