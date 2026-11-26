package provisioner

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// managementSide is the management cluster's node, its API and the appliance on vmbr0 without a VLAN.
func managementSide(t *testing.T) *FirewallSources {
	t.Helper()
	appliance := &corev1.ConfigMap{
		Name: ApplianceAddressesName, Namespace: "kubehenge-system",
		Data: map[string]string{ApplianceAddressesKey: "10.0.0.31, 2001:db8::31"},
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(appliance).Build()
	return &FirewallSources{
		Reader: kube, Namespace: "kubehenge-system", Management: []string{"10.0.0.11", "2001:db8::11"},
		API: "https://10.0.0.10:6443", Bridge: "vmbr0",
		Nodes: map[string]string{"10.0.0.11": "kubehenge-cp-0"},
	}
}

func TestManagementHeldAddressesNameTheirHolders(t *testing.T) {
	t.Parallel()
	held, err := managementSide(t).Held(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, h := range held {
		got[h.Address.String()] = h.Holder
		if h.Segment != "vmbr0/0" {
			t.Errorf("%s is held on %q, want vmbr0/0", h.Address, h.Segment)
		}
	}
	want := map[string]string{
		"10.0.0.11": "management node kubehenge-cp-0", "2001:db8::11": holderManagementNode,
		"10.0.0.10": holderManagementAPI, "10.0.0.31": holderAppliance, "2001:db8::31": holderAppliance,
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for a, holder := range want {
		if got[a] != holder {
			t.Errorf("%s is held by %q, want %q", a, got[a], holder)
		}
	}
	unset := managementSide(t)
	unset.Bridge = ""
	if held, err := unset.Held(t.Context()); err != nil || len(held) != 0 {
		t.Errorf("without a bridge: %v, %v", held, err)
	}
}

func TestManagementAddressesAreRefused(t *testing.T) {
	t.Parallel()
	web := func(vlan int, machines, endpoint, lb string) *clusterv1.Cluster {
		return lbCluster("web", vlan, machines, endpoint, lb)
	}
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		want    string
	}{
		"beside everything": {cluster: web(0, "10.0.0.100-10.0.0.110", "10.0.0.99", onAt(`"10.0.0.120-10.0.0.130"`))},
		"a node range with a management node": {
			cluster: web(0, "10.0.0.9-10.0.0.19", "10.0.0.99", ""),
			want:    "node addresses 10.0.0.9-10.0.0.19 include 10.0.0.11, which is management node kubehenge-cp-0",
		},
		"the management API as the endpoint": {
			cluster: web(0, "10.0.0.100-10.0.0.110", "10.0.0.10", ""),
			want:    "control-plane endpoint 10.0.0.10 is the management cluster's API address",
		},
		"the appliance in the load balancer": {
			cluster: web(0, "10.0.0.100-10.0.0.110", "10.0.0.99", onAt(`"10.0.0.30-10.0.0.40"`)),
			want:    "load-balancer addresses 10.0.0.30-10.0.0.40 include 10.0.0.31, which is the appliance's address",
		},
		"a management node as a single node address": {
			cluster: web(0, "10.0.0.11", "10.0.0.99", ""),
			want:    "10.0.0.11, which is management node kubehenge-cp-0",
		},
		"another VLAN": {cluster: web(7, "10.0.0.9-10.0.0.19", "10.0.0.10", onAt(`"10.0.0.30-10.0.0.40"`))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			held, err := managementSide(t).Held(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = checkManagementAddresses(tc.cluster, held)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// An appliance with two cards holds each address on the network of its own card, and on no other,
// also where the management cluster's bridge is not known. An address it recorded no network for
// is held on the management cluster's.
func TestApplianceAddressesAreHeldOnItsOwnNetwork(t *testing.T) {
	t.Parallel()
	for name, bridge := range map[string]string{"management on vmbr0": "vmbr0", "management bridge not known": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			appliance := &corev1.ConfigMap{
				Name: ApplianceAddressesName, Namespace: "kubehenge-system",
				Data: map[string]string{
					ApplianceAddressesKey: "10.0.0.31,10.0.0.35,10.0.0.36", ApplianceNetworksKey: "10.0.0.31=vmbr0/7, 10.0.0.35=vmbr1/0",
				},
			}
			f := managementSide(t)
			f.Reader = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(appliance).Build()
			f.Bridge = bridge
			h, _ := hooksFor(t)
			h.Management = f
			v := &ClusterValidator{Hooks: h}
			lb := onAt(`"10.0.0.30-10.0.0.40"`)
			for _, tc := range []struct {
				network string
				cluster *clusterv1.Cluster
				held    []string
			}{
				{"vmbr0/7", lbCluster("web", 7, "10.0.0.100-10.0.0.110", "10.0.0.99", lb), []string{"10.0.0.31"}},
				{"vmbr1/0", onBridge(lbCluster("web", 0, "10.0.0.100-10.0.0.110", "10.0.0.99", lb), "vmbr1"), []string{"10.0.0.35"}},
				{"vmbr0/0", lbCluster("web", 0, "10.0.0.100-10.0.0.110", "10.0.0.99", lb), map[bool][]string{true: {"10.0.0.36"}}[bridge != ""]},
				{"vmbr1/7", onBridge(lbCluster("web", 7, "10.0.0.100-10.0.0.110", "10.0.0.99", lb), "vmbr1"), nil},
			} {
				_, err := v.ValidateCreate(t.Context(), tc.cluster)
				beside := changedVariable(tc.cluster, loadBalancerVariable, onAt(`"10.0.0.120-10.0.0.130"`))
				_, updateErr := v.ValidateUpdate(t.Context(), beside, tc.cluster)
				for _, address := range []string{"10.0.0.31", "10.0.0.35", "10.0.0.36"} {
					want := slices.Contains(tc.held, address)
					for what, err := range map[string]error{"create": err, "update": updateErr} {
						if got := err != nil && strings.Contains(err.Error(), "include "+address+", which is the appliance's address"); got != want {
							t.Errorf("%s on %s: %s refused %v, want %v: %v", what, tc.network, address, got, want, err)
						}
					}
				}
			}
		})
	}
}

// onBridge is the cluster with its network on another bridge.
func onBridge(c *clusterv1.Cluster, bridge string) *clusterv1.Cluster {
	for i := range c.Spec.Topology.Variables {
		if c.Spec.Topology.Variables[i].Name == "network" {
			raw := string(c.Spec.Topology.Variables[i].Value.Raw)
			c.Spec.Topology.Variables[i].Value.Raw = []byte(strings.Replace(raw, `"vmbr0"`, strconv.Quote(bridge), 1))
		}
	}
	return c
}

// The hooks that admission and the BeforeClusterCreate hook share refuse the management side's
// addresses next to another cluster's, in one answer, and let a cluster through where the hooks
// know no management network.
func TestHooksRefuseManagementAddresses(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(lbCluster("db", 0, "10.0.0.50-10.0.0.60", "10.0.0.49", "")).Build()
	web := lbCluster("web", 0, "10.0.0.11-10.0.0.19", "10.0.0.10", "")
	h := &Hooks{Reader: kube, Management: managementSide(t)}
	err := h.checkAddresses(t.Context(), web)
	if err == nil || !strings.Contains(err.Error(), "10.0.0.11, which is management node kubehenge-cp-0") || !strings.Contains(err.Error(), "the management cluster's API address") {
		t.Errorf("got %v", err)
	}
	clash := lbCluster("web", 0, "10.0.0.50-10.0.0.60", "10.0.0.10", "")
	err = h.checkAddresses(t.Context(), clash)
	if err == nil || !strings.Contains(err.Error(), "of cluster team-a/db") || !strings.Contains(err.Error(), "the management cluster's API address") {
		t.Errorf("another cluster's addresses and the management side's in one answer: %v", err)
	}
	for name, hooks := range map[string]*Hooks{
		"no management":        {Reader: kube},
		"firewall addresses":   {Reader: kube, Management: fakeManagement{addresses: []string{"10.0.0.11"}}},
		"no management bridge": {Reader: kube, Management: &FirewallSources{Reader: kube, Namespace: "kubehenge-system", Management: []string{"10.0.0.11"}}},
	} {
		if err := hooks.checkAddresses(t.Context(), lbCluster("web", 0, "10.0.0.11-10.0.0.19", "10.0.0.99", "")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
