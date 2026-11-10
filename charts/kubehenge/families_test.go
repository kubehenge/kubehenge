package kubehenge_test

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The network variables of a cluster of each family, its nodes on 10.0.0.0/24 and 2001:db8::/64.
const (
	ipv4Net     = `{"bridge":"vmbr0","addresses":["10.0.0.10-10.0.0.20"],"prefix":24,"gateway":"10.0.0.1","dnsServers":["10.0.0.1"],"searchDomains":["example.org"]}`
	dualNet     = `{"bridge":"vmbr0","ipFamilies":["IPv4","IPv6"],"addresses":["10.0.0.10-10.0.0.20"],"prefix":24,"gateway":"10.0.0.1","ipv6":{"addresses":["2001:db8::10-2001:db8::20"],"prefix":64,"gateway":"2001:db8::1"},"dnsServers":["10.0.0.1","2001:db8::1"],"searchDomains":["example.org"]}`
	dualIPv6Net = `{"bridge":"vmbr0","ipFamilies":["IPv6","IPv4"],"addresses":["10.0.0.10-10.0.0.20"],"prefix":24,"gateway":"10.0.0.1","ipv6":{"addresses":["2001:db8::10-2001:db8::20"],"prefix":64,"gateway":"2001:db8::1"},"dnsServers":["2001:db8::1","10.0.0.1"],"searchDomains":["example.org"]}`
	ipv6OnlyNet = `{"bridge":"vmbr0","ipFamilies":["IPv6"],"ipv6":{"addresses":["2001:db8::10-2001:db8::20"],"prefix":64,"gateway":"2001:db8::1"},"dnsServers":["2001:db8::1"],"searchDomains":["example.org"]}`
)

var everyClass = []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml", "talos.yaml"}

func jsonValue(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return v
}

// checkRule validates a value against a variable's schema and wants it accepted, or refused with
// a message that holds want.
func checkRule(t *testing.T, class renderedClass, variable, raw, oldRaw, want string) {
	t.Helper()
	var old any
	if oldRaw != "" {
		old = jsonValue(t, oldRaw)
	}
	errs := validateVariable(t, class.variables[variable], jsonValue(t, raw), old)
	switch {
	case want == "" && len(errs) > 0:
		t.Errorf("%s refused: %v", raw, errs)
	case want != "" && (len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), want)):
		t.Errorf("%s: got %v, want %q", raw, errs, want)
	}
}

func TestEveryClassHoldsTheNetworkToItsFamilies(t *testing.T) {
	t.Parallel()
	for _, file := range everyClass {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			class := renderClass(t, file)
			for name, tc := range map[string]struct{ value, old, want string }{
				"IPv4 by default":          {value: ipv4Net},
				"IPv4 named":               {value: strings.Replace(ipv4Net, `"bridge":"vmbr0",`, `"bridge":"vmbr0","ipFamilies":["IPv4"],`, 1)},
				"dual-stack, IPv4 first":   {value: dualNet},
				"dual-stack, IPv6 first":   {value: dualIPv6Net},
				"IPv6 only":                {value: ipv6OnlyNet},
				"IPv6 without its network": {value: strings.Replace(ipv6OnlyNet, `"ipv6":{"addresses":["2001:db8::10-2001:db8::20"],"prefix":64,"gateway":"2001:db8::1"},`, "", 1), want: "IPv6 needs ipv6 with addresses, prefix and gateway."},
				"IPv4 without a gateway":   {value: strings.Replace(dualNet, `"gateway":"10.0.0.1",`, "", 1), want: "IPv4 needs addresses, prefix and gateway."},
				"IPv4 settings, IPv6 only": {value: strings.Replace(ipv6OnlyNet, `"bridge":"vmbr0",`, `"bridge":"vmbr0","prefix":24,`, 1), want: "addresses, prefix and gateway are IPv4 settings."},
				"IPv6 settings, IPv4 only": {value: strings.Replace(dualNet, `"ipFamilies":["IPv4","IPv6"],`, "", 1), want: "ipv6 holds IPv6 settings."},
				"a family twice":           {value: strings.Replace(ipv6OnlyNet, `["IPv6"]`, `["IPv6","IPv6"]`, 1), want: "Each address family once."},
				"no such family":           {value: strings.Replace(ipv6OnlyNet, `["IPv6"]`, `["IPv5"]`, 1), want: "ipFamilies"},
				"an IPv4 IPv6 gateway":     {value: strings.Replace(ipv6OnlyNet, `"gateway":"2001:db8::1"`, `"gateway":"10.0.0.1"`, 1), want: "gateway"},
				"an IPv6 prefix past 128":  {value: strings.Replace(ipv6OnlyNet, `"prefix":64`, `"prefix":129`, 1), want: "prefix"},
				"an IPv6 DNS server":       {value: strings.Replace(ipv4Net, `["10.0.0.1"]`, `["2001:db8::53"]`, 1)},
				"a DNS server by name":     {value: strings.Replace(ipv4Net, `["10.0.0.1"]`, `["dns.example.org"]`, 1), want: "A DNS server is an IPv4 or IPv6 address."},
				"families kept":            {value: dualNet, old: dualNet},
				"families changed":         {value: dualNet, old: strings.Replace(ipv4Net, `"bridge":"vmbr0",`, `"bridge":"vmbr0","ipFamilies":["IPv4"],`, 1), want: "The address families are chosen when the cluster is created."},
			} {
				t.Run(name, func(t *testing.T) {
					checkRule(t, class, "network", tc.value, tc.old, tc.want)
				})
			}
			for endpoint, want := range map[string]string{
				`"10.0.0.5"`:        "",
				`"2001:db8::5"`:     "",
				`"api.example.org"`: "The control-plane endpoint is an IPv4 or IPv6 address.",
			} {
				checkRule(t, class, "controlPlaneEndpoint", endpoint, "", want)
			}
		})
	}
}

func TestBGPRouterIDIsADottedID(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			class := renderClass(t, file)
			lb := func(id string) string {
				return `{"enabled":true,"mode":"bgp","addresses":["2001:db8:1::/124"],"bgp":{"asn":65001,"routerID":` + id + `,"peers":[{"address":"2001:db8::1","asn":64512}]}}`
			}
			const want = "A router id is a dotted 32-bit id other than 0.0.0.0, such as 10.0.0.1."
			for id, want := range map[string]string{`"10.255.0.1"`: "", `"2001:db8::9"`: want, `"0.0.0.0"`: want, `"router-1"`: want} {
				checkRule(t, class, "loadBalancer", lb(id), "", want)
			}
			// An IPv6 router is taken as it is: kube-vip and MetalLB peer over either family.
			checkRule(t, class, "loadBalancer", strings.Replace(lb(`"10.255.0.1"`), `,"routerID":"10.255.0.1"`, "", 1), "", "")
		})
	}
}

func TestProxmoxClusterTakesTheNetworkOfEachFamily(t *testing.T) {
	t.Parallel()
	for _, file := range everyClass {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			class := renderClass(t, file)
			for name, tc := range map[string]struct {
				network, endpoint string
				ipv4, ipv6        bool
			}{
				"IPv4":                   {network: ipv4Net, endpoint: "10.0.0.5", ipv4: true},
				"dual-stack, IPv4 first": {network: dualNet, endpoint: "10.0.0.5", ipv4: true, ipv6: true},
				"dual-stack, IPv6 first": {network: dualIPv6Net, endpoint: "2001:db8::5", ipv4: true, ipv6: true},
				"IPv6 only":              {network: ipv6OnlyNet, endpoint: "2001:db8::5", ipv6: true},
			} {
				t.Run(name, func(t *testing.T) {
					network, ok := jsonValue(t, tc.network).(map[string]any)
					if !ok {
						t.Fatal("the network is not an object")
					}
					variables := map[string]any{"controlPlaneEndpoint": tc.endpoint, "network": network, "builtin": map[string]any{"cluster": map[string]any{"name": "web"}}}
					spec := asMap(t, field(t, class.patched(t, "ProxmoxClusterTemplate", variables), "spec", "template", "spec"))
					if got := field(t, spec, "controlPlaneEndpoint", "host"); got != tc.endpoint {
						t.Errorf("endpoint %v, want %s", got, tc.endpoint)
					}
					if got := spec["dnsServers"]; !reflect.DeepEqual(got, network["dnsServers"]) {
						t.Errorf("DNS servers %v, want %v", got, network["dnsServers"])
					}
					want := map[string]any{"addresses": network["addresses"], "prefix": network["prefix"], "gateway": network["gateway"]}
					switch got, has := spec["ipv4Config"].(map[string]any); {
					case tc.ipv4 && !maps.EqualFunc(got, want, reflect.DeepEqual):
						t.Errorf("ipv4Config %v, want %v", got, want)
					case !tc.ipv4 && has:
						t.Errorf("an IPv6-only cluster has ipv4Config %v", got)
					}
					switch got, has := spec["ipv6Config"]; {
					case tc.ipv6 && !reflect.DeepEqual(got, network["ipv6"]):
						t.Errorf("ipv6Config %v, want %v", got, network["ipv6"])
					case !tc.ipv6 && has:
						t.Errorf("an IPv4 cluster has ipv6Config %v", got)
					}
				})
			}
		})
	}
}

// kube-apiserver refuses to start when its advertised address is not of the Services' primary
// family, and kubeadm takes an IPv4 address when none is named. Where IPv6 comes first the class
// names a placeholder for kubeadm init and for a control-plane join, which node-ip.sh replaces.
func TestKubeadmAdvertisesAnAddressOfThePrimaryFamily(t *testing.T) {
	t.Parallel()
	const placeholder = "kubehenge-node-address"
	script, err := os.ReadFile("files/node-ip.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "grep -q "+placeholder+" ") {
		t.Errorf("node-ip.sh does not replace %s", placeholder)
	}
	class := renderClass(t, "kubeadm.yaml")
	advertised := func(doc map[string]any, path ...string) any {
		var v any = doc
		for _, p := range path {
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[p]
		}
		return v
	}
	for name, tc := range map[string]struct {
		network string
		ipv6    bool
	}{
		"IPv4":                   {network: ipv4Net},
		"dual-stack, IPv4 first": {network: dualNet},
		"dual-stack, IPv6 first": {network: dualIPv6Net, ipv6: true},
		"IPv6 only":              {network: ipv6OnlyNet, ipv6: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			variables := map[string]any{"controlPlaneEndpoint": "10.0.0.5", "network": jsonValue(t, tc.network), "cni": "calico"}
			var want any
			if tc.ipv6 {
				want = placeholder
			}
			spec := asMap(t, field(t, class.patched(t, "KubeadmControlPlaneTemplate", variables), "spec", "template", "spec", "kubeadmConfigSpec"))
			if got := advertised(spec, "initConfiguration", "localAPIEndpoint", "advertiseAddress"); got != want {
				t.Errorf("kubeadm init advertises %v, want %v", got, want)
			}
			if got := advertised(spec, "joinConfiguration", "controlPlane", "localAPIEndpoint", "advertiseAddress"); got != want {
				t.Errorf("a control-plane join advertises %v, want %v", got, want)
			}
			worker := asMap(t, field(t, class.patched(t, "KubeadmConfigTemplate", variables), "spec", "template", "spec"))
			if got := advertised(worker, "joinConfiguration", "controlPlane"); got != nil {
				t.Errorf("a worker joins as a control-plane node: %v", got)
			}
		})
	}
}

// Each node gives its kubelet its address of each family, the primary first: kubeadm always, RKE2
// and k3s once the cluster has IPv6, as they take IPv4 alone by themselves. Talos takes them from
// the families of the Service range.
func TestNodesGetTheirAddressOfEachFamily(t *testing.T) {
	t.Parallel()
	kinds := map[string][]string{
		"kubeadm.yaml": {"KubeadmControlPlaneTemplate", "KubeadmConfigTemplate"},
		"rke2.yaml":    {"RKE2ControlPlaneTemplate", "RKE2ConfigTemplate"},
		"k3s.yaml":     {"KThreesControlPlaneTemplate", "KThreesConfigTemplate"},
	}
	for file, kinds := range kinds {
		distribution := strings.TrimSuffix(file, ".yaml")
		t.Run(distribution, func(t *testing.T) {
			t.Parallel()
			class := renderClass(t, file)
			for name, tc := range map[string]struct {
				network, families string
			}{
				"IPv4":                   {network: ipv4Net},
				"dual-stack, IPv4 first": {network: dualNet, families: "IPv4 IPv6"},
				"dual-stack, IPv6 first": {network: dualIPv6Net, families: "IPv6 IPv4"},
				"IPv6 only":              {network: ipv6OnlyNet, families: "IPv6"},
			} {
				t.Run(name, func(t *testing.T) {
					for _, kind := range kinds {
						for _, cni := range []string{"flannel", "calico"} {
							variables := map[string]any{"controlPlaneEndpoint": "10.0.0.5", "network": jsonValue(t, tc.network), "cni": cni}
							raw, err := json.Marshal(class.patched(t, kind, variables))
							if err != nil {
								t.Fatal(err)
							}
							doc := string(raw)
							script := distribution == "kubeadm" || tc.families != ""
							if strings.Contains(doc, "/etc/kubehenge/node-ip.sh "+distribution) != script {
								t.Errorf("%s, %s: node-ip.sh run %v, want %v", kind, cni, !script, script)
							}
							if got := strings.Contains(doc, `"content":"\"`+tc.families+`\""`) || strings.Contains(doc, `"content":"`+tc.families+`"`); tc.families != "" && !got {
								t.Errorf("%s: /etc/kubehenge/ip-families does not hold %q", kind, tc.families)
							}
							if tc.families == "" && strings.Contains(doc, `"path":"/etc/kubehenge/ip-families"`) {
								t.Errorf("%s: an IPv4 cluster names its families", kind)
							}
							ipv6 := strings.Contains(tc.families, "IPv6")
							if strings.Contains(doc, "ipv6.dns-search") != ipv6 {
								t.Errorf("%s: IPv6 search domains %v, want %v", kind, !ipv6, ipv6)
							}
							masq := ipv6 && cni == "flannel" && kind == "KThreesControlPlaneTemplate"
							if strings.Contains(doc, "flannel-ipv6-masq") != masq {
								t.Errorf("%s, %s: flannel-ipv6-masq %v, want %v", kind, cni, !masq, masq)
							}
						}
					}
				})
			}
		})
	}
}
