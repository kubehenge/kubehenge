package provisioner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// fakeBridges answers every bridge with one MTU, or none where unreadable.
type fakeBridges struct {
	mtu      int
	readable bool
	asked    []string
}

func (f *fakeBridges) BridgeMTU(_ context.Context, bridge string, nodes []string) (int, bool) {
	f.asked = append(f.asked, bridge+"@"+strings.Join(nodes, ","))
	return f.mtu, f.readable
}

// withNIC gives a networkCluster's nodes these interface options, as the JSON of the variable.
func withNIC(c *clusterv1.Cluster, options string) *clusterv1.Cluster {
	for i := range c.Spec.Topology.Variables {
		if v := &c.Spec.Topology.Variables[i]; v.Name == "network" {
			v.Value = apiextensionsv1.JSON{Raw: []byte(`{"bridge":"vmbr0","addresses":["192.168.1.0/24"],"prefix":24,"gateway":"192.168.1.1","dnsServers":["192.168.1.1"]` + options + `}`)}
		}
	}
	return c
}

func TestValidatorHoldsTheNodeInterfaceToItsBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		options string
		refusal string
	}{
		{``, ""},
		{`,"mtu":1280`, ""},
		{`,"mtu":1450`, ""},
		{`,"mtu":9000`, ""},
		{`,"mtu":65520`, ""},
		{`,"mtu":1279`, "MTU: 1280 to 65520"},
		{`,"mtu":576`, "MTU: 1280 to 65520"},
		{`,"mtu":1`, "MTU: 1280 to 65520"},
		{`,"mtu":0`, "MTU: 1280 to 65520"},
		{`,"mtu":65521`, "MTU: 1280 to 65520"},
		{`,"queues":1`, ""},
		{`,"queues":64`, ""},
		{`,"queues":0`, "Queues: 1 to 64"},
		{`,"queues":65`, "Queues: 1 to 64"},
	} {
		hooks, _ := hooksFor(t)
		v := &ClusterValidator{Hooks: hooks}
		_, err := v.ValidateCreate(context.Background(), withNIC(networkCluster("192.168.1.1", 24), tc.options))
		switch {
		case tc.refusal == "" && err != nil && (strings.Contains(err.Error(), "MTU") || strings.Contains(err.Error(), "Queues")):
			t.Errorf("%s: refused: %v", tc.options, err)
		case tc.refusal != "" && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("%s: %v, want a refusal with %q", tc.options, err, tc.refusal)
		}
	}
}

func TestValidatorHoldsTheNodeMTUToTheBridges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		bridge   fakeBridges
		options  string
		refusal  string
		wantSeen bool
	}{
		{"jumbo frames on a jumbo bridge", fakeBridges{mtu: 9000, readable: true}, `,"mtu":9000`, "", true},
		{"below the bridge", fakeBridges{mtu: 1500, readable: true}, `,"mtu":1450`, "", true},
		{"equal to the bridge", fakeBridges{mtu: 1450, readable: true}, `,"mtu":1450`, "", true},
		{"above the bridge", fakeBridges{mtu: 1500, readable: true}, `,"mtu":9000`, "The bridge vmbr0 has MTU 1500. Use 1500 or less, or raise the bridge's MTU", true},
		{"a bridge that cannot be read", fakeBridges{mtu: 1500}, `,"mtu":9000`, "", true},
		{"no MTU to compare", fakeBridges{mtu: 1500, readable: true}, ``, "", false},
		{"queues alone", fakeBridges{mtu: 1500, readable: true}, `,"queues":4`, "", false},
	} {
		hooks, _ := hooksFor(t)
		hooks.Bridges = &tc.bridge
		v := &ClusterValidator{Hooks: hooks}
		_, err := v.ValidateCreate(context.Background(), withNIC(networkCluster("192.168.1.1", 24), tc.options))
		if tc.refusal == "" && err != nil && strings.Contains(err.Error(), "bridge") {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
		if tc.refusal != "" && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.refusal)) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.refusal)
		}
		if seen := len(tc.bridge.asked) > 0; seen != tc.wantSeen {
			t.Errorf("%s: asked the bridge %v, want %v", tc.name, tc.bridge.asked, tc.wantSeen)
		}
	}
}

func TestValidatorAsksTheBridgeOnlyOfTheAllowedNodes(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	bridges := &fakeBridges{mtu: 9000, readable: true}
	hooks.Bridges = bridges
	c := withNIC(networkCluster("192.168.1.1", 24), `,"mtu":9000`)
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: allowedNodesVariable, Value: apiextensionsv1.JSON{Raw: []byte(`["pve1","pve2"]`)},
	})
	v := &ClusterValidator{Hooks: hooks}
	if _, err := v.ValidateCreate(context.Background(), c); err != nil && strings.Contains(err.Error(), "bridge") {
		t.Fatal(err)
	}
	if len(bridges.asked) != 1 || bridges.asked[0] != "vmbr0@pve1,pve2" {
		t.Errorf("asked %v", bridges.asked)
	}
}

func TestValidatorHoldsARunningClusterToTheNodeInterfaceOnlyWhenItChanges(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	bridges := &fakeBridges{mtu: 1500, readable: true}
	hooks.Bridges = bridges
	v := &ClusterValidator{Hooks: hooks}
	// A cluster made before the bridge's MTU was lowered is not refused for an unrelated edit.
	running := withNIC(networkCluster("192.168.1.1", 24), `,"mtu":9000`)
	edited := running.DeepCopy()
	edited.Labels = map[string]string{"edited": "yes"}
	if _, err := v.ValidateUpdate(context.Background(), running, edited); err != nil && strings.Contains(err.Error(), "MTU") {
		t.Errorf("an unrelated edit: %v", err)
	}
	if len(bridges.asked) != 0 {
		t.Errorf("asked the bridge for an unrelated edit: %v", bridges.asked)
	}
	changed := withNIC(networkCluster("192.168.1.1", 24), `,"mtu":9000`)
	was := withNIC(networkCluster("192.168.1.1", 24), ``)
	if _, err := v.ValidateUpdate(context.Background(), was, changed); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "The bridge vmbr0 has MTU 1500") {
		t.Errorf("raising the MTU above the bridge: %v", err)
	}
}

func TestBridgeMTUIsTheSmallestOnTheOnlineNodes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch r.URL.Path {
		case "/api2/json/nodes":
			body = `[{"node":"pve1","status":"online"},{"node":"pve2","status":"online"},{"node":"pve3","status":"offline"},{"node":"pve4","status":"online"}]`
		case "/api2/json/nodes/pve1/network":
			body = `[{"iface":"vmbr0","mtu":"9000"},{"iface":"vmbr1"}]`
		case "/api2/json/nodes/pve2/network":
			body = `[{"iface":"vmbr0","mtu":1500}]`
		case "/api2/json/nodes/pve3/network":
			body = `[{"iface":"vmbr0","mtu":"1280"}]`
		default:
			// pve4's network is not the controller's to read.
			http.Error(w, "permission denied", http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":%s}`, body)
	}))
	t.Cleanup(srv.Close)
	p := &Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}
	for _, tc := range []struct {
		bridge string
		nodes  []string
		mtu    int
		ok     bool
	}{
		{"vmbr0", nil, 1500, true},
		{"vmbr0", []string{"pve1"}, 9000, true},
		{"vmbr0", []string{"pve3"}, 0, false},
		{"vmbr0", []string{"pve4"}, 0, false},
		// Proxmox VE lists no MTU for a bridge that has the kernel's 1500.
		{"vmbr1", nil, 1500, true},
		{"vmbr9", nil, 0, false},
	} {
		mtu, ok := p.BridgeMTU(t.Context(), tc.bridge, tc.nodes)
		if mtu != tc.mtu || ok != tc.ok {
			t.Errorf("%s on %v = %d, %v; want %d, %v", tc.bridge, tc.nodes, mtu, ok, tc.mtu, tc.ok)
		}
	}
}
