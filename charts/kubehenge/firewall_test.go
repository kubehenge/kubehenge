package kubehenge_test

import (
	"reflect"
	"strings"
	"testing"
)

// The security groups reach every ProxmoxMachineTemplate of every class as the annotations that
// Kubehenge's capmox build turns into the VM's firewall: the groups, and ahead of them every source
// of the cluster's own traffic. Without groups the template is untouched.
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
			with := class.patched(t, "ProxmoxMachineTemplate", variables(dualNet, `, "securityGroups": ["k8s", "corp-base"]`))
			got := field(t, with, "spec", "template", "metadata", "annotations")
			want := map[string]any{
				"kubehenge.dev/firewall-groups":  "k8s,corp-base",
				"kubehenge.dev/firewall-sources": "10.0.0.5,10.0.0.10-10.0.0.20,2001:db8::10-2001:db8::20,10.244.0.0/16,fd5e:4b1c:9a20::/56",
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("annotations = %v, want %v", got, want)
			}
			ipv6Only := class.patched(t, "ProxmoxMachineTemplate", variables(ipv6OnlyNet, `, "securityGroups": ["k8s"]`))
			sources := asMap(t, field(t, ipv6Only, "spec", "template", "metadata", "annotations"))["kubehenge.dev/firewall-sources"]
			if sources != "10.0.0.5,2001:db8::10-2001:db8::20,10.244.0.0/16,fd5e:4b1c:9a20::/56" {
				t.Errorf("IPv6-only sources = %v", sources)
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

// The management cluster's nodes are among the sources every cluster's firewall accepts.
func TestTheFirewallAcceptsTheManagementCluster(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/kubeadm.yaml", "managementAddresses={192.0.2.11,2001:db8::11}")
	if !strings.Contains(out, `list .controlPlaneEndpoint \"192.0.2.11\" \"2001:db8::11\"`) &&
		!strings.Contains(out, `list .controlPlaneEndpoint "192.0.2.11" "2001:db8::11"`) {
		t.Error("the firewall's sources leave out the management cluster's addresses")
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
