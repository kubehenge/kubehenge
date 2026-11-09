package provisioner

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

type fakeAPIReach struct {
	reach *pve.APIReach
	ok    bool
	asked int
}

func (f *fakeAPIReach) APIReach(context.Context) (*pve.APIReach, bool) {
	f.asked++
	return f.reach, f.ok
}

type fakeManagement struct {
	addresses []string
	err       error
}

func (f fakeManagement) Addresses(context.Context) ([]string, error) { return f.addresses, f.err }

// localReach is a datacenter whose one node, 10.0.0.5, has 10.0.0.0/24 as its local network.
func localReach() *pve.APIReach {
	own := netip.MustParsePrefix("10.0.0.5/24")
	return &pve.APIReach{Nodes: []pve.NodeReach{{Node: "pve1", Address: own.Addr(), Interfaces: []netip.Prefix{own.Masked()}}}}
}

const cutRefusal = "cut off 10.3.0.4. Add 10.3.0.4 to the datacenter firewall's IPSet management."

// endpointV4 is the Proxmox VE address of a datacenter whose node is localReach's.
const endpointV4 = "https://10.0.0.5:8006"

func TestValidatorRefusesSecurityGroupsThatCutTheManagementSideOffTheAPI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		reach      *pve.APIReach
		readable   bool
		management fakeManagement
		refusal    string
	}{
		{"on the node's network", localReach(), true, fakeManagement{addresses: []string{"10.0.0.20", "10.0.0.21"}}, ""},
		{"outside it", localReach(), true, fakeManagement{addresses: []string{"10.0.0.20", "10.3.0.4", "2001:db8::4"}}, cutRefusal},
		{"an IPv6 address towards an IPv4 endpoint", localReach(), true, fakeManagement{addresses: []string{"10.0.0.20", "2001:db8::4"}}, ""},
		{"in the IPSet management", func() *pve.APIReach {
			r := localReach()
			r.Management = []pve.IPSetEntry{{CIDR: "10.3.0.0/24"}}
			return r
		}(), true, fakeManagement{addresses: []string{"10.3.0.4"}}, ""},
		{"unreadable firewall", nil, false, fakeManagement{addresses: []string{"10.3.0.4"}}, ""},
		{"unreadable local network", &pve.APIReach{Nodes: []pve.NodeReach{{Node: "pve1"}}}, true, fakeManagement{addresses: []string{"10.3.0.4"}}, ""},
		{"no management addresses", localReach(), true, fakeManagement{}, ""},
		{"unreadable addresses", localReach(), true, fakeManagement{addresses: []string{"10.3.0.4"}, err: errors.New("down")}, ""},
	} {
		hooks, _ := hooksFor(t)
		hooks.SecurityGroups = &fakeSecurityGroups{groups: []string{"k8s"}, readable: true}
		hooks.APIReach = &fakeAPIReach{reach: tc.reach, ok: tc.readable}
		hooks.Management = tc.management
		hooks.Endpoint = endpointV4
		v := &ClusterValidator{Hooks: hooks}
		_, err := v.ValidateCreate(context.Background(), withSecurityGroups(networkCluster("192.168.1.1", 24), `["k8s"]`))
		switch {
		case tc.refusal == "" && err != nil && strings.Contains(err.Error(), "IPSet management"):
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.refusal != "" && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.refusal)
		}
	}
}

func TestValidatorChecksTheFamiliesOfTheEndpoint(t *testing.T) {
	t.Parallel()
	v6 := netip.MustParsePrefix("2001:db8:1::5/64")
	reach := &pve.APIReach{Nodes: []pve.NodeReach{{Node: "pve1", Address: v6.Addr(), Interfaces: []netip.Prefix{v6.Masked()}}}}
	for _, tc := range []struct {
		name, endpoint, refusal string
	}{
		{"an IPv6 endpoint", "https://[2001:db8:1::5]:8006", "cut off 2001:db8:2::4. Add 2001:db8:2::4 to"},
		{"an endpoint that gives no address", "not a url", ""},
	} {
		hooks, _ := hooksFor(t)
		hooks.SecurityGroups = &fakeSecurityGroups{groups: []string{"k8s"}, readable: true}
		hooks.APIReach = &fakeAPIReach{reach: reach, ok: true}
		hooks.Management = fakeManagement{addresses: []string{"2001:db8:1::30", "2001:db8:2::4", "10.9.0.4"}}
		hooks.Endpoint = tc.endpoint
		v := &ClusterValidator{Hooks: hooks}
		_, err := v.ValidateCreate(context.Background(), withSecurityGroups(networkCluster("192.168.1.1", 24), `["k8s"]`))
		switch {
		case tc.refusal == "" && err != nil && strings.Contains(err.Error(), "IPSet management"):
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.refusal != "" && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.refusal)
		case tc.refusal != "" && strings.Contains(err.Error(), "10.9.0.4"):
			t.Errorf("%s: named an IPv4 address towards an IPv6 endpoint: %v", tc.name, err)
		}
	}
}

func TestValidatorReadsTheFirewallOnlyForAClusterWithSecurityGroups(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	reach := &fakeAPIReach{reach: localReach(), ok: true}
	hooks.SecurityGroups = &fakeSecurityGroups{groups: []string{"k8s"}, readable: true}
	hooks.APIReach = reach
	hooks.Management = fakeManagement{addresses: []string{"10.3.0.4"}}
	v := &ClusterValidator{Hooks: hooks}
	if _, err := v.ValidateCreate(context.Background(), withSecurityGroups(networkCluster("192.168.1.1", 24), "")); err != nil && strings.Contains(err.Error(), "IPSet management") {
		t.Errorf("refused a cluster without groups: %v", err)
	}
	if reach.asked != 0 {
		t.Errorf("read the firewall %d times for a cluster without groups", reach.asked)
	}
}

func TestValidatorKeepsTheFirewallCheckOnAnUnrelatedEdit(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	hooks.SecurityGroups = &fakeSecurityGroups{groups: []string{"k8s"}, readable: true}
	reach := &fakeAPIReach{reach: localReach(), ok: true}
	hooks.APIReach = reach
	hooks.Management = fakeManagement{addresses: []string{"10.3.0.4"}}
	v := &ClusterValidator{Hooks: hooks}
	running := withSecurityGroups(networkCluster("192.168.1.1", 24), `["k8s"]`)
	edited := running.DeepCopy()
	edited.Labels = map[string]string{"edited": "yes"}
	if _, err := v.ValidateUpdate(context.Background(), running, edited); err != nil && strings.Contains(err.Error(), "IPSet management") {
		t.Errorf("refused an unrelated edit: %v", err)
	}
	if reach.asked != 0 {
		t.Error("read the firewall for an edit that leaves the groups alone")
	}
}
