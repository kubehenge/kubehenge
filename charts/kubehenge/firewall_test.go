package kubehenge_test

import (
	"reflect"
	"strings"
	"testing"
)

// The security groups reach every ProxmoxMachineTemplate of every class as the annotation that
// Kubehenge's capmox build turns into the VM's firewall. No address is part of a template, so a new
// one replaces no node. Without groups nothing is touched.
func TestEveryClassAnnotatesTheMachinesWithTheFirewall(t *testing.T) {
	t.Parallel()
	for _, file := range everyClass {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			class := renderClass(t, file)
			variables := func(network, extra string) map[string]any {
				return variablesOf(t, `{"builtin": {"cluster": {"name": "c", "namespace": "n", "network": {"pods": ["10.244.0.0/16", "fd5e:4b1c:9a20::/56"]}}},
					"controlPlaneEndpoint": "10.0.0.5", "network": `+network+extra+`}`)
			}
			for _, network := range []string{dualNet, ipv6OnlyNet} {
				with := variables(network, `, "securityGroups": ["k8s", "corp-base"]`)
				machine := field(t, class.patched(t, "ProxmoxMachineTemplate", with), "spec", "template", "metadata", "annotations")
				if want := map[string]any{"kubehenge.dev/firewall-groups": "k8s,corp-base"}; !reflect.DeepEqual(machine, want) {
					t.Errorf("machine annotations = %v, want %v", machine, want)
				}
				if metadata, ok := asMap(t, field(t, class.patched(t, "ProxmoxClusterTemplate", with), "spec", "template"))["metadata"]; ok {
					t.Errorf("the class writes the ProxmoxCluster's metadata, which the controller owns: %v", metadata)
				}
			}
			without := class.patched(t, "ProxmoxMachineTemplate", variables(ipv4Net, ""))
			if metadata, ok := asMap(t, field(t, without, "spec", "template"))["metadata"]; ok {
				t.Errorf("metadata without security groups: %v", metadata)
			}
			patch := classDocumentOf(t, file).patch(t, "proxmox-firewall")
			if len(patch.Definitions) != 1 {
				t.Fatalf("%d definitions, want one for the control planes and the workers", len(patch.Definitions))
			}
		})
	}
}

// Every class defaults a new cluster's firewall on, and the defaulted value writes the serving
// ports, ICMP echo of both families, Talos's API on Talos, and the cluster's own further ports
// beside any groups. With the serving ports closed, only the groups and the further ports are
// written, and the ports annotation stays to keep the firewall on. Off, no firewall is written.
func TestEveryClassDefaultsTheFirewallOn(t *testing.T) {
	t.Parallel()
	for _, file := range everyClass {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			serving := "tcp/6443,tcp/80,tcp/443,tcp/30000:32767,udp/30000:32767,icmp/echo-request,ipv6-icmp/echo-request"
			release := ""
			if file == "talos.yaml" {
				serving += ",tcp/50000"
				release = `, "talosVersion": "v1.12.0"`
			}
			class := renderClass(t, file)
			schema := class.variables["firewall"]
			if schema == nil || schema.Default == nil || string(schema.Default.Raw) != `{"enabled":true}` {
				t.Fatalf("the firewall variable's default = %v, want on", schema)
			}
			if serving, ok := schema.Properties["servingPorts"]; !ok || serving.Type != "boolean" || serving.Default == nil || string(serving.Default.Raw) != "true" {
				t.Errorf("the firewall's servingPorts = %+v, want a boolean that defaults to true", serving)
			}
			annotations := func(extra string) any {
				vars := variablesOf(t, `{"builtin": {"cluster": {"name": "c", "namespace": "n", "network": {"pods": ["10.244.0.0/16"]}}},
					"controlPlaneEndpoint": "10.0.0.5", "network": `+ipv4Net+release+extra+`}`)
				spec := asMap(t, field(t, class.patched(t, "ProxmoxMachineTemplate", vars), "spec", "template"))
				if metadata, ok := spec["metadata"]; ok {
					return asMap(t, metadata)["annotations"]
				}
				return nil
			}
			if got, want := annotations(`, "firewall": {"enabled": true}`), map[string]any{"kubehenge.dev/firewall-ports": serving}; !reflect.DeepEqual(got, want) {
				t.Errorf("default annotations = %v, want %v", got, want)
			}
			got := annotations(`, "securityGroups": ["k8s"], "firewall": {"enabled": true, "ports": [{"protocol": "udp", "port": 51820}, {"protocol": "tcp", "port": 8000, "endPort": 8100}]}`)
			want := map[string]any{"kubehenge.dev/firewall-groups": "k8s", "kubehenge.dev/firewall-ports": serving + ",udp/51820,tcp/8000:8100"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("annotations with groups and ports = %v, want %v", got, want)
			}
			for extra, want := range map[string]map[string]any{
				`, "securityGroups": ["k8s"], "firewall": {"enabled": true, "servingPorts": false}`: {
					"kubehenge.dev/firewall-groups": "k8s", "kubehenge.dev/firewall-ports": "",
				},
				`, "firewall": {"enabled": true, "servingPorts": false, "ports": [{"protocol": "tcp", "port": 6443}]}`: {
					"kubehenge.dev/firewall-ports": "tcp/6443",
				},
				`, "firewall": {"enabled": true, "servingPorts": true}`: {"kubehenge.dev/firewall-ports": serving},
			} {
				if got := annotations(extra); !reflect.DeepEqual(got, want) {
					t.Errorf("annotations with%s = %v, want %v", extra, got, want)
				}
			}
			off := variablesOf(t, `{"builtin": {"cluster": {"name": "c", "namespace": "n", "network": {"pods": ["10.244.0.0/16"]}}},
				"controlPlaneEndpoint": "10.0.0.5", "network": `+ipv4Net+`, "firewall": {"enabled": false}}`)
			if metadata, ok := asMap(t, field(t, class.patched(t, "ProxmoxMachineTemplate", off), "spec", "template"))["metadata"]; ok {
				t.Errorf("metadata with the firewall off = %v, want none", metadata)
			}
		})
	}
}

func TestFirewallPortsFollowTheirRules(t *testing.T) {
	t.Parallel()
	class := renderClass(t, "kubeadm.yaml")
	for name, tc := range map[string]struct{ value, want string }{
		"on":                {value: `{"enabled": true}`},
		"serving ports off": {value: `{"enabled": true, "servingPorts": false}`},
		"a port":            {value: `{"enabled": true, "ports": [{"protocol": "tcp", "port": 8443}]}`},
		"a range":           {value: `{"enabled": true, "ports": [{"protocol": "udp", "port": 4000, "endPort": 4010}]}`},
		"a range backwards": {value: `{"enabled": true, "ports": [{"protocol": "tcp", "port": 4010, "endPort": 4000}]}`, want: "A range ends above its first port."},
		"sctp":              {value: `{"enabled": true, "ports": [{"protocol": "sctp", "port": 9}]}`, want: "Unsupported value"},
		"port 0":            {value: `{"enabled": true, "ports": [{"protocol": "tcp", "port": 0}]}`, want: "should be greater than or equal to 1"},
		"ports while off":   {value: `{"enabled": false, "ports": [{"protocol": "tcp", "port": 8443}]}`, want: "Ports need the firewall on."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkRule(t, class, "firewall", tc.value, "", tc.want)
		})
	}
}

// The management cluster's nodes reach the controller, which adds them to the firewall sources of
// every firewalled cluster. They are in no class, where a change would replace nodes. The
// appliance's ConfigMap comes without data, so an upgrade keeps what the appliance wrote.
func TestTheFirewallAcceptsTheManagementCluster(t *testing.T) {
	t.Parallel()
	controller := helmTemplate(t, "templates/controller.yaml", "managementAddresses={192.0.2.11,2001:db8::11}")
	if !strings.Contains(controller, "--management-addresses=192.0.2.11,2001:db8::11") {
		t.Error("the controller is not given the management cluster's addresses")
	}
	if class := helmTemplate(t, "templates/kubeadm.yaml", "managementAddresses={192.0.2.11,2001:db8::11}"); strings.Contains(class, "192.0.2.11") {
		t.Error("a management address is in the class, where a change would replace nodes")
	}
	if controller := helmTemplate(t, "templates/controller.yaml"); strings.Contains(controller, "--management-addresses") {
		t.Error("an empty --management-addresses without addresses")
	}
	iface := helmTemplate(t, "templates/interface.yaml")
	start := strings.Index(iface, "name: kubehenge-appliance-addresses\n")
	if start < 0 {
		t.Fatal("no ConfigMap for the appliance's addresses")
	}
	if next := strings.Index(iface[start:], "---"); next < 0 || strings.Contains(iface[start:start+next], "data:") {
		t.Error("the appliance's ConfigMap holds data, which an upgrade would reset")
	}
	if !strings.Contains(iface, "resourceNames: [kubehenge-appliance-addresses]\n    verbs: [get, patch]") {
		t.Error("the interface may not keep the appliance's addresses")
	}
}

func TestSecurityGroupsFollowProxmoxVEsNames(t *testing.T) {
	t.Parallel()
	class := renderClass(t, "kubeadm.yaml")
	for name, tc := range map[string]struct{ groups, want string }{
		"one":               {groups: `["k8s"]`},
		"several, in order": {groups: `["corp_base","k8s-nodes"]`},
		"eighteen long":     {groups: `["abcdefghijklmnopqr"]`},
		"none":              {groups: `[]`, want: "should have at least 1 items"},
		"twice":             {groups: `["k8s","k8s"]`, want: "shouldn't contain duplicates"},
		"starts with digit": {groups: `["1k8s"]`, want: "should match"},
		"a space":           {groups: `["k8s nodes"]`, want: "should match"},
		"one letter":        {groups: `["k"]`, want: "should be at least 2 chars long"},
		"nineteen long":     {groups: `["abcdefghijklmnopqrs"]`, want: "may not be more than 18"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkRule(t, class, "securityGroups", tc.groups, "", tc.want)
		})
	}
}
