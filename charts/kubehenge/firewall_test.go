package kubehenge_test

import (
	"reflect"
	"strings"
	"testing"
)

// The firewall is not part of a machine: Kubehenge's controller writes it to each VM from the
// variables, so no variable reaches the machine template and a change replaces no node. The only
// annotation on a machine is the prefix its VM is named with.
func TestNoClassWritesTheFirewallIntoTheMachines(t *testing.T) {
	t.Parallel()
	for _, file := range everyClass {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			class := renderClass(t, file)
			machine := func(extra string) any {
				vars := variablesOf(t, `{"builtin": {"cluster": {"name": "c", "namespace": "n", "network": {"pods": ["10.244.0.0/16"]}}},
					"controlPlaneEndpoint": "10.0.0.5", "network": `+ipv4Net+extra+`}`)
				return field(t, class.patched(t, "ProxmoxMachineTemplate", vars), "spec", "template")
			}
			base := machine("")
			if got := firewallAnnotations(t, field(t, asMap(t, base), "metadata", "annotations")); len(got) != 0 {
				t.Errorf("annotations beside the name prefix: %v", got)
			}
			for _, extra := range []string{
				`, "securityGroups": ["k8s", "corp-base"]`,
				`, "firewall": {"enabled": false}`,
				`, "firewall": {"enabled": true, "servingPorts": false}`,
				`, "securityGroups": ["k8s"], "firewall": {"enabled": true, "ports": [{"protocol": "udp", "port": 51820}]}`,
			} {
				if got := machine(extra); !reflect.DeepEqual(got, base) {
					t.Errorf("the machine template with%s = %v, want it unchanged: %v", extra, got, base)
				}
			}
			if len(classDocumentOf(t, file).patch(t, "proxmox-vm-name").Definitions) != 1 {
				t.Error("the name patch needs one definition for the control planes and the workers")
			}
		})
	}
}

// Every class defaults a new cluster's firewall on, with the serving ports open.
func TestEveryClassDefaultsTheFirewallOn(t *testing.T) {
	t.Parallel()
	for _, file := range everyClass {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			schema := renderClass(t, file).variables["firewall"]
			if schema == nil || schema.Default == nil || string(schema.Default.Raw) != `{"enabled":true}` {
				t.Fatalf("the firewall variable's default = %v, want on", schema)
			}
			if serving, ok := schema.Properties["servingPorts"]; !ok || serving.Type != "boolean" || serving.Default == nil || string(serving.Default.Raw) != "true" {
				t.Errorf("the firewall's servingPorts = %+v, want a boolean that defaults to true", serving)
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

// The controller holds clusters off the management side's addresses on the management cluster's
// bridge and VLAN, so it is told them, and with no bridge it is told nothing.
func TestTheControllerKnowsTheManagementNetwork(t *testing.T) {
	t.Parallel()
	controller := helmTemplate(t, "templates/controller.yaml", "managementNetwork.bridge=vmbr1", "managementNetwork.vlan=30", "managementNetwork.nodes.192\\.0\\.2\\.11=kubehenge-cp-0", "managementNetwork.nodes.2001:db8::11=kubehenge-cp-0")
	for _, want := range []string{"--management-bridge=vmbr1", "--management-vlan=30", "--management-nodes=192.0.2.11=kubehenge-cp-0,2001:db8::11=kubehenge-cp-0"} {
		if !strings.Contains(controller, want) {
			t.Errorf("the controller is not given %s", want)
		}
	}
	if controller := helmTemplate(t, "templates/controller.yaml"); strings.Contains(controller, "--management-bridge") || strings.Contains(controller, "--management-vlan") || strings.Contains(controller, "--management-nodes") {
		t.Error("the controller is given a management network without one set")
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
