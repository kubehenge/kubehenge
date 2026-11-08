package provisioner

import (
	"context"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The network variables of a cluster of each family, its nodes on 10.0.0.0/24 and 2001:db8::/64.
const (
	ipv4Network      = `{"bridge":"vmbr0","addresses":["10.0.0.10-10.0.0.20"],"prefix":24,"gateway":"10.0.0.1","dnsServers":["10.0.0.1"]}`
	dualNetwork      = `{"bridge":"vmbr0","ipFamilies":["IPv4","IPv6"],"addresses":["10.0.0.10-10.0.0.20"],"prefix":24,"gateway":"10.0.0.1","ipv6":{"addresses":["2001:db8::10-2001:db8::20"],"prefix":64,"gateway":"2001:db8::1"},"dnsServers":["10.0.0.1","2001:db8::1"]}`
	dualIPv6Network  = `{"bridge":"vmbr0","ipFamilies":["IPv6","IPv4"],"addresses":["10.0.0.10-10.0.0.20"],"prefix":24,"gateway":"10.0.0.1","ipv6":{"addresses":["2001:db8::10-2001:db8::20"],"prefix":64,"gateway":"2001:db8::1"},"dnsServers":["2001:db8::1","10.0.0.1"]}`
	ipv6OnlyNetwork  = `{"bridge":"vmbr0","ipFamilies":["IPv6"],"ipv6":{"addresses":["2001:db8::10-2001:db8::20"],"prefix":64,"gateway":"2001:db8::1"},"dnsServers":["2001:db8::1"]}`
	ipv4Endpoint     = "10.0.0.5"
	ipv6Endpoint     = "2001:db8::5"
	familiesWorkerMD = "md-0"
)

// familyCluster is a cluster with that network variable and control-plane endpoint, raw, and one
// worker.
func familyCluster(network, endpoint string) *clusterv1.Cluster {
	c := cluster(
		clusterv1.ClusterVariable{Name: "network", Value: apiextensionsv1.JSON{Raw: []byte(network)}},
		clusterv1.ClusterVariable{Name: "controlPlaneEndpoint", Value: apiextensionsv1.JSON{Raw: []byte(`"` + endpoint + `"`)}},
	)
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: familiesWorkerMD, Replicas: new(int32(1))}}
	return c
}

func rke2Class(t *testing.T) *clusterv1.ClusterClass {
	t.Helper()
	hooks, _ := hooksFor(t)
	class := &clusterv1.ClusterClass{}
	if err := hooks.Reader.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	return class
}

func TestNetworkVariableHoldsAddressesOfItsFamilies(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ network, want string }{
		"IPv4":                         {network: ipv4Network},
		"dual-stack, IPv4 first":       {network: dualNetwork},
		"dual-stack, IPv6 first":       {network: dualIPv6Network},
		"IPv6 only":                    {network: ipv6OnlyNetwork},
		"IPv6 in the IPv4 addresses":   {network: strings.Replace(ipv4Network, `"10.0.0.10-10.0.0.20"`, `"2001:db8::10"`, 1), want: "network.addresses holds IPv4 addresses, and 2001:db8::10 is IPv6"},
		"IPv4 in the IPv6 addresses":   {network: strings.Replace(ipv6OnlyNetwork, `"2001:db8::10-2001:db8::20"`, `"10.0.0.10"`, 1), want: "network.ipv6.addresses holds IPv6 addresses, and 10.0.0.10 is IPv4"},
		"dual-stack without IPv6 DNS":  {network: strings.Replace(dualNetwork, `,"2001:db8::1"]`, `]`, 1), want: "a dual-stack cluster needs a DNS server of each family, and none is IPv6"},
		"dual-stack without IPv4 DNS":  {network: strings.Replace(dualIPv6Network, `,"10.0.0.1"]`, `]`, 1), want: "a dual-stack cluster needs a DNS server of each family, and none is IPv4"},
		"IPv6 DNS for an IPv4 cluster": {network: strings.Replace(ipv4Network, `["10.0.0.1"]`, `["2001:db8::1"]`, 1), want: "DNS server 2001:db8::1 is IPv6, which the cluster's nodes do not have"},
		"IPv4 DNS for an IPv6 cluster": {network: strings.Replace(ipv6OnlyNetwork, `["2001:db8::1"]`, `["10.0.0.1"]`, 1), want: "DNS server 10.0.0.1 is IPv4, which the cluster's nodes do not have"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := validateFamilies(nil, familyCluster(tc.network, ipv4Endpoint)).ToAggregate()
			text := ""
			if got != nil {
				text = got.Error()
			}
			switch {
			case tc.want == "" && text != "":
				t.Errorf("refused: %s", text)
			case tc.want != "" && !strings.Contains(text, tc.want):
				t.Errorf("got %q, want %q", text, tc.want)
			}
		})
	}
}

func TestAddressFamiliesStayAsCreated(t *testing.T) {
	t.Parallel()
	old := familyCluster(ipv4Network, ipv4Endpoint)
	c := familyCluster(dualNetwork, ipv4Endpoint)
	errs := validateFamilies(old, c).ToAggregate()
	if errs == nil || !strings.Contains(errs.Error(), "the address families are chosen when the cluster is created") {
		t.Errorf("IPv4 to dual-stack: %v", errs)
	}
	if errs := validateFamilies(c, c.DeepCopy()); len(errs) > 0 {
		t.Errorf("an unchanged dual-stack cluster: %v", errs)
	}
}

func TestCheckAddressesOfEachFamily(t *testing.T) {
	t.Parallel()
	withLB := func(c *clusterv1.Cluster, lb string) *clusterv1.Cluster {
		return withAddons(c, map[string]string{loadBalancerVariable: lb})
	}
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		want    string
	}{
		"dual-stack":                         {cluster: familyCluster(dualNetwork, ipv4Endpoint)},
		"dual-stack, IPv6 first":             {cluster: familyCluster(dualIPv6Network, ipv6Endpoint)},
		"IPv6 only":                          {cluster: familyCluster(ipv6OnlyNetwork, ipv6Endpoint)},
		"IPv6 only, load balancer":           {cluster: withLB(familyCluster(ipv6OnlyNetwork, ipv6Endpoint), onAt(`"2001:db8::100-2001:db8::1ff"`))},
		"dual-stack, both families":          {cluster: withLB(familyCluster(dualNetwork, ipv4Endpoint), onAt(`"10.0.0.100-10.0.0.120","2001:db8::100/120"`))},
		"endpoint of the second family":      {cluster: familyCluster(dualNetwork, ipv6Endpoint), want: "control-plane endpoint 2001:db8::5 is IPv6, and the cluster's first family is IPv4"},
		"IPv4 endpoint of an IPv6 cluster":   {cluster: familyCluster(ipv6OnlyNetwork, ipv4Endpoint), want: "control-plane endpoint 10.0.0.5 is IPv4, and the cluster's first family is IPv6"},
		"IPv6 endpoint off the network":      {cluster: familyCluster(ipv6OnlyNetwork, "2001:db8:1::5"), want: "control-plane endpoint 2001:db8:1::5 is outside the network 2001:db8::/64"},
		"IPv6 endpoint among the nodes":      {cluster: familyCluster(ipv6OnlyNetwork, "2001:db8::15"), want: "control-plane endpoint 2001:db8::15 is inside node addresses 2001:db8::10-2001:db8::20"},
		"IPv6 endpoint on the gateway":       {cluster: familyCluster(ipv6OnlyNetwork, "2001:db8::1"), want: "control-plane endpoint 2001:db8::1 is the gateway"},
		"IPv6 nodes off the network":         {cluster: familyCluster(strings.Replace(ipv6OnlyNetwork, "2001:db8::10-2001:db8::20", "2001:db8:1::10-2001:db8:1::20", 1), ipv6Endpoint), want: "node addresses 2001:db8:1::10-2001:db8:1::20 are outside the network 2001:db8::/64"},
		"IPv6 load balancer, IPv4 cluster":   {cluster: withLB(familyCluster(ipv4Network, ipv4Endpoint), onAt(`"2001:db8::100"`)), want: "load-balancer addresses 2001:db8::100 are IPv6, which the cluster's nodes do not have"},
		"IPv4 load balancer, IPv6 cluster":   {cluster: withLB(familyCluster(ipv6OnlyNetwork, ipv6Endpoint), onAt(`"10.0.0.100"`)), want: "load-balancer addresses 10.0.0.100 are IPv4, which the cluster's nodes do not have"},
		"IPv6 load balancer off the network": {cluster: withLB(familyCluster(dualNetwork, ipv4Endpoint), onAt(`"2001:db8:5::/120"`)), want: "load-balancer addresses 2001:db8:5::-2001:db8:5::ff are outside the network 2001:db8::/64"},
		"subnet-router anycast":              {cluster: withLB(familyCluster(ipv6OnlyNetwork, ipv6Endpoint), onAt(`"2001:db8::/120"`)), want: "include the network's subnet-router anycast address 2001:db8::"},
		"IPv6 load balancer on the gateway":  {cluster: withLB(familyCluster(dualNetwork, ipv4Endpoint), onAt(`"2001:db8::1"`)), want: "load-balancer addresses 2001:db8::1 include the gateway 2001:db8::1"},
		"IPv6 load balancer on the nodes":    {cluster: withLB(familyCluster(dualNetwork, ipv4Endpoint), onAt(`"2001:db8::18"`)), want: "load-balancer addresses 2001:db8::18 overlap node addresses 2001:db8::10-2001:db8::20"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := checkAddresses(tc.cluster, nil)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
	// Clusters on one bridge take each other's IPv6 addresses too.
	one := familyCluster(ipv6OnlyNetwork, ipv6Endpoint)
	two := familyCluster(strings.Replace(ipv6OnlyNetwork, "2001:db8::10-2001:db8::20", "2001:db8::18-2001:db8::30", 1), "2001:db8::6")
	two.Name = "other"
	if err := checkAddresses(one, []clusterv1.Cluster{*two}); err == nil || !strings.Contains(err.Error(), "overlap 2001:db8::18-2001:db8::30 of cluster team-a/other") {
		t.Errorf("IPv6 node addresses another cluster holds: %v", err)
	}
}

func TestCheckRangesOfEachFamily(t *testing.T) {
	t.Parallel()
	class := rke2Class(t)
	for name, tc := range map[string]struct {
		network        string
		pods, services []string
		want           string
	}{
		"dual-stack":                    {dualNetwork, []string{"10.42.0.0/16", DefaultPodsIPv6}, []string{"10.43.0.0/16", DefaultServicesIPv6}, ""},
		"dual-stack, IPv6 first":        {dualIPv6Network, []string{DefaultPodsIPv6, "10.42.0.0/16"}, []string{DefaultServicesIPv6, "10.43.0.0/16"}, ""},
		"IPv6 only":                     {ipv6OnlyNetwork, []string{DefaultPodsIPv6}, []string{DefaultServicesIPv6}, ""},
		"IPv6 only, no ranges":          {ipv6OnlyNetwork, nil, nil, ""},
		"dual-stack missing IPv6":       {dualNetwork, []string{"10.42.0.0/16"}, []string{"10.43.0.0/16", DefaultServicesIPv6}, "the pod ranges are IPv4, and the cluster's families IPv4 and IPv6"},
		"dual-stack in the wrong order": {dualNetwork, []string{DefaultPodsIPv6, "10.42.0.0/16"}, []string{"10.43.0.0/16", DefaultServicesIPv6}, "the pod ranges are IPv6 and IPv4, and the cluster's families IPv4 and IPv6"},
		"IPv4 range of an IPv6 cluster": {ipv6OnlyNetwork, []string{"10.42.0.0/16"}, []string{DefaultServicesIPv6}, "the pod ranges are IPv4, and the cluster's families IPv6"},
		"IPv6 over the nodes":           {ipv6OnlyNetwork, []string{"2001:db8::/56"}, []string{DefaultServicesIPv6}, "pod range 2001:db8::/56 overlaps the nodes' network 2001:db8::/64"},
		"IPv6 service over the nodes":   {dualNetwork, []string{"10.42.0.0/16", DefaultPodsIPv6}, []string{"10.43.0.0/16", "2001:db8::/112"}, "service range 2001:db8::/112 overlaps the nodes' network 2001:db8::/64"},
		"IPv6 over each other":          {ipv6OnlyNetwork, []string{"fd00::/56"}, []string{"fd00::/112"}, "the service range fd00::/112 overlaps the pod range fd00::/56"},
		"IPv6 pods too small":           {ipv6OnlyNetwork, []string{"fd00::/64"}, []string{DefaultServicesIPv6}, "the IPv6 pod range fd00::/64 has a prefix length outside /48 to /63"},
		"IPv6 pods too large":           {ipv6OnlyNetwork, []string{"fd00::/40"}, []string{DefaultServicesIPv6}, "the IPv6 pod range fd00::/40 has a prefix length outside /48 to /63"},
		"IPv6 services too large":       {ipv6OnlyNetwork, []string{DefaultPodsIPv6}, []string{"fd01::/96"}, "the IPv6 service range fd01::/96 has a prefix length outside /112 to /120"},
		"IPv6 services past k3s":        {ipv6OnlyNetwork, []string{DefaultPodsIPv6}, []string{"fd01::/108"}, "the IPv6 service range fd01::/108 has a prefix length outside /112 to /120"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := familyCluster(tc.network, ipv4Endpoint)
			c.Spec.ClusterNetwork.Pods.CIDRBlocks, c.Spec.ClusterNetwork.Services.CIDRBlocks = tc.pods, tc.services
			err := checkRanges(c, class)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDefaulterGivesEachFamilyRanges(t *testing.T) {
	t.Parallel()
	hooks, kube := hooksFor(t)
	d := &ClusterDefaulter{Hooks: hooks}
	for network, want := range map[string]string{
		dualNetwork:     "10.42.0.0/16,fd5e:4b1c:9a20::/56 10.43.0.0/16,fd5e:4b1c:9a21::/112",
		dualIPv6Network: "fd5e:4b1c:9a20::/56,10.42.0.0/16 fd5e:4b1c:9a21::/112,10.43.0.0/16",
		ipv6OnlyNetwork: "fd5e:4b1c:9a20::/56 fd5e:4b1c:9a21::/112",
		// Nodes in Kubehenge's own unique local range take the next /56 and /112.
		strings.ReplaceAll(ipv6OnlyNetwork, "2001:db8::", "fd5e:4b1c:9a20::"): "fd5e:4b1c:9a20:100::/56 fd5e:4b1c:9a21::/112",
	} {
		c := familyCluster(network, ipv4Endpoint)
		if err := d.Default(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if got := rangesString(c); got != want {
			t.Errorf("%s: got %s, want %s", network, got, want)
		}
	}
	// Cilium clusters keep their IPv6 pods apart from each other's pods and services.
	create := func(name string) *clusterv1.Cluster {
		t.Helper()
		c := withCNI(familyCluster(ipv6OnlyNetwork, ipv6Endpoint), cniCilium)
		c.Name = name
		if err := d.Default(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if err := kube.Create(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	first, second := create("first"), create("second")
	if a, b := first.Spec.ClusterNetwork.Pods.CIDRBlocks[0], second.Spec.ClusterNetwork.Pods.CIDRBlocks[0]; a == b {
		t.Errorf("two Cilium clusters share the IPv6 pod range %s", a)
	}
	if got := checkRanges(second, rke2Class(t)); got != nil {
		t.Errorf("the second Cilium cluster's ranges: %v", got)
	}
}

func TestNodeAddressesCountTheFamilyWithFewest(t *testing.T) {
	t.Parallel()
	c := familyCluster(strings.Replace(dualNetwork, "2001:db8::10-2001:db8::20", "2001:db8::10-2001:db8::11", 1), ipv4Endpoint)
	if err := checkNodeAddresses(nil, c); err == nil || !strings.Contains(err.Error(), "has 2 node addresses") {
		t.Errorf("two IPv6 addresses for three machines: %v", err)
	}
	// A whole /64 counts as more than any cluster takes.
	big := familyCluster(strings.Replace(ipv6OnlyNetwork, `"2001:db8::10-2001:db8::20"`, `"2001:db8::/64"`, 1), ipv6Endpoint)
	if err := checkNodeAddresses(nil, big); err != nil {
		t.Errorf("a /64 of node addresses: %v", err)
	}
	if n, err := addressCount([]string{"2001:db8::/126", "2001:db8:1::/127"}); err != nil || n != 5 {
		t.Errorf("a /126 without its anycast address and a /127: %d, %v", n, err)
	}
}

func TestCNIRunsTheClusterFamilies(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pods       []string
		ipv4, ipv6 any
		v4List     []string
		v6List     []string
		underlay   any
	}{
		"IPv4":                   {pods: []string{"10.42.0.0/16"}, v4List: []string{"10.42.0.0/16"}},
		"dual-stack":             {pods: []string{"10.42.0.0/16", DefaultPodsIPv6}, ipv6: true, v4List: []string{"10.42.0.0/16"}, v6List: []string{DefaultPodsIPv6}},
		"dual-stack, IPv6 first": {pods: []string{DefaultPodsIPv6, "10.42.0.0/16"}, ipv6: true, v4List: []string{"10.42.0.0/16"}, v6List: []string{DefaultPodsIPv6}},
		"IPv6 only":              {pods: []string{DefaultPodsIPv6}, ipv4: false, ipv6: true, v6List: []string{DefaultPodsIPv6}, underlay: "ipv6"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := cluster()
			c.Spec.ClusterNetwork.Pods.CIDRBlocks = tc.pods
			proxies, err := cniChart(c, &testCharts, CNI{Name: cniCilium, Pods: "10.244.0.0/16", ClusterID: 1}, &Addons{})
			if err != nil {
				t.Fatal(err)
			}
			values := proxies[0].values
			if got := enabledOf(values, "ipv4"); got != tc.ipv4 {
				t.Errorf("ipv4.enabled %v, want %v", got, tc.ipv4)
			}
			if got := enabledOf(values, "ipv6"); got != tc.ipv6 {
				t.Errorf("ipv6.enabled %v, want %v", got, tc.ipv6)
			}
			if values["underlayProtocol"] != tc.underlay {
				t.Errorf("underlayProtocol %v, want %v", values["underlayProtocol"], tc.underlay)
			}
			operator := lookup[map[string]any](t, values, "ipam", "operator")
			if !sameList(operator["clusterPoolIPv4PodCIDRList"], tc.v4List) || !sameList(operator["clusterPoolIPv6PodCIDRList"], tc.v6List) {
				t.Errorf("Cilium's pools %v", operator)
			}

			s, err := calicoResources(c, CNI{Name: cniCalico, Pods: "10.244.0.0/16"}, testCharts.CalicoImages)
			if err != nil {
				t.Fatal(err)
			}
			manifest := string(s.Data["calico.yaml"])
			for _, p := range tc.pods {
				if !strings.Contains(manifest, "cidr: "+p) {
					t.Errorf("Calico has no pool of %s: %s", p, manifest)
				}
			}
			if v6 := strings.Contains(manifest, "nodeAddressAutodetectionV6"); v6 != (tc.ipv6 != nil) {
				t.Errorf("Calico finds IPv6 node addresses: %v", v6)
			}
		})
	}
}

// enabledOf is a chart value's enabled, nil where the values leave it to the chart.
func enabledOf(values map[string]any, key string) any {
	m, ok := values[key].(map[string]any)
	if !ok {
		return nil
	}
	return m["enabled"]
}

func sameList(v any, want []string) bool {
	got, ok := v.([]any)
	if !ok {
		return v == nil && len(want) == 0
	}
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestBGPRouterIDOfEachFamily(t *testing.T) {
	t.Parallel()
	routed := func(network, endpoint, pool, routerID, peers string) *clusterv1.Cluster {
		id := ""
		if routerID != "" {
			id = `,"routerID":"` + routerID + `"`
		}
		return withAddons(familyCluster(network, endpoint), map[string]string{loadBalancerVariable: `{"enabled":true,"mode":"bgp","addresses":["` + pool + `"],"bgp":{"asn":65001` + id + `,"peers":[` + peers + `]}}`})
	}
	const v4Pool, v6Pool = "198.51.100.0/28", "2001:db8:1::/124"
	ipv4Peer, ipv6Peer := peerOf("10.0.0.1", "64512"), peerOf("2001:db8::1", "64512")
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		want    string
	}{
		"IPv4, each node's own":             {cluster: routed(ipv4Network, ipv4Endpoint, v4Pool, "", ipv4Peer)},
		"dual-stack, IPv4 and IPv6 routers": {cluster: routed(dualNetwork, ipv4Endpoint, v4Pool, "", ipv4Peer+","+ipv6Peer)},
		"dual-stack, IPv6 first":            {cluster: routed(dualIPv6Network, ipv6Endpoint, v6Pool, "", ipv6Peer)},
		"dual-stack, IPv6 first, set id":    {cluster: routed(dualIPv6Network, ipv6Endpoint, v6Pool, "10.255.0.1", ipv4Peer)},
		"IPv6 only, set id":                 {cluster: routed(ipv6OnlyNetwork, ipv6Endpoint, v6Pool, "10.255.0.1", ipv6Peer)},
		"IPv6 only, no id":                  {cluster: routed(ipv6OnlyNetwork, ipv6Endpoint, v6Pool, "", ipv6Peer), want: "BGP on an IPv6-only cluster needs bgp.routerID"},
		"IPv6 only, IPv4 router":            {cluster: routed(ipv6OnlyNetwork, ipv6Endpoint, v6Pool, "10.255.0.1", ipv4Peer), want: "router 10.0.0.1 is IPv4, which the cluster's nodes do not have"},
		"id is an IPv6 address":             {cluster: routed(ipv6OnlyNetwork, ipv6Endpoint, v6Pool, "2001:db8::9", ipv6Peer), want: `router id "2001:db8::9" is not a dotted 32-bit id`},
		"id is zero":                        {cluster: routed(ipv4Network, ipv4Endpoint, v4Pool, "0.0.0.0", ipv4Peer), want: `router id "0.0.0.0" is not a dotted 32-bit id`},
		"id is no address":                  {cluster: routed(ipv4Network, ipv4Endpoint, v4Pool, "router-1", ipv4Peer), want: `router id "router-1" is not a dotted 32-bit id`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := checkBGP(tc.cluster)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestMetalLBPeersGiveTheRouterID(t *testing.T) {
	t.Parallel()
	spans, err := parseAddresses([]string{"2001:db8:1::/124"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "10.255.0.1"} {
		lb := LoadBalancer{Enabled: true, Mode: modeBGP, BGP: &BGP{ASN: 65001, RouterID: id, Peers: []BGPPeer{{Address: "2001:db8::1", ASN: 64512}}}}
		for _, o := range metalLBPool(spans, lb) {
			if o.GetKind() != bgpPeerKind {
				continue
			}
			got, has := lookup[map[string]any](t, o.Object, "spec")["routerID"]
			if (id == "") == has || (has && got != id) {
				t.Errorf("router id %q: peer %s has %v", id, o.GetName(), got)
			}
			if o.GetName() != metalLBPoolName+"-peer-2001-db8--1" {
				t.Errorf("peer name %s", o.GetName())
			}
		}
	}
}
