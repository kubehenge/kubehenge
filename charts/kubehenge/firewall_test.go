package kubehenge_test

import (
	"reflect"
	"strings"
	"testing"
)

// The security groups reach every ProxmoxMachineTemplate of every class as the annotation that
// Kubehenge's capmox build turns into the VM's firewall. The ProxmoxCluster carries the management
// side's addresses, which capmox adds to each VM's IPSet beside the cluster's own. No address is part
// of a machine template, so a new one replaces no node. Without groups nothing is touched.
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
				cluster := field(t, class.patched(t, "ProxmoxClusterTemplate", with), "spec", "template", "metadata", "annotations")
				if want := map[string]any{"kubehenge.dev/firewall-sources": ""}; !reflect.DeepEqual(cluster, want) {
					t.Errorf("cluster annotations = %v, want %v", cluster, want)
				}
			}
			without := variables(ipv4Net, "")
			for _, kind := range []string{"ProxmoxMachineTemplate", "ProxmoxClusterTemplate"} {
				if metadata, ok := asMap(t, field(t, class.patched(t, kind, without), "spec", "template"))["metadata"]; ok {
					t.Errorf("%s metadata without security groups: %v", kind, metadata)
				}
			}
			patch := classDocumentOf(t, file).patch(t, "proxmox-firewall")
			if len(patch.Definitions) != 2 {
				t.Fatalf("%d definitions, want one for the machines and one for the cluster", len(patch.Definitions))
			}
		})
	}
}

// The management cluster's nodes and the appliance are the further sources every cluster's
// ProxmoxCluster names, in the order the module gives them.
func TestTheFirewallAcceptsTheManagementCluster(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/kubeadm.yaml", "managementAddresses={192.0.2.11,2001:db8::11}")
	if !strings.Contains(out, `kubehenge.dev/firewall-sources: "192.0.2.11,2001:db8::11"`) {
		t.Error("the ProxmoxCluster's firewall sources leave out the management cluster's addresses")
	}
	if strings.Count(out, "192.0.2.11") != 1 {
		t.Error("a management address is outside the ProxmoxCluster's annotation, where a change would replace nodes")
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
