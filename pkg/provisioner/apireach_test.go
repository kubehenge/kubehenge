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

func localReach(local string) *pve.APIReach {
	own := netip.MustParsePrefix(local)
	return &pve.APIReach{Nodes: []pve.NodeReach{{Node: "pve1", Address: own.Addr(), Interfaces: []netip.Prefix{own.Masked()}}}}
}

func TestValidatorRefusesSecurityGroupsThatCutTheManagementSideOffTheAPI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		reach      *pve.APIReach
		readable   bool
		management fakeManagement
		refusal    string
	}{
		{"on the node's network", localReach("10.0.0.5/24"), true, fakeManagement{addresses: []string{"10.0.0.20", "10.0.0.21"}}, ""},
		{"outside it", localReach("10.0.0.5/24"), true, fakeManagement{addresses: []string{"10.0.0.20", "10.3.0.4", "2001:db8::4"}},
			"cut off 10.3.0.4, 2001:db8::4. Add 10.3.0.4, 2001:db8::4 to the datacenter firewall's IPSet management."},
		{"in the IPSet management", func() *pve.APIReach {
			r := localReach("10.0.0.5/24")
			r.Management = []pve.IPSetEntry{{CIDR: "10.3.0.0/24"}}
			return r
		}(), true, fakeManagement{addresses: []string{"10.3.0.4"}}, ""},
		{"unreadable firewall", nil, false, fakeManagement{addresses: []string{"10.3.0.4"}}, ""},
		{"unreadable local network", &pve.APIReach{Nodes: []pve.NodeReach{{Node: "pve1"}}}, true, fakeManagement{addresses: []string{"10.3.0.4"}}, ""},
		{"no management addresses", localReach("10.0.0.5/24"), true, fakeManagement{}, ""},
		{"unreadable addresses", localReach("10.0.0.5/24"), true, fakeManagement{addresses: []string{"10.3.0.4"}, err: errors.New("down")}, ""},
	} {
		hooks, _ := hooksFor(t)
		hooks.SecurityGroups = &fakeSecurityGroups{groups: []string{"k8s"}, readable: true}
		hooks.APIReach = &fakeAPIReach{reach: tc.reach, ok: tc.readable}
		hooks.Management = tc.management
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

func TestValidatorReadsTheFirewallOnlyForAClusterWithSecurityGroups(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	reach := &fakeAPIReach{reach: localReach("10.0.0.5/24"), ok: true}
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
	reach := &fakeAPIReach{reach: localReach("10.0.0.5/24"), ok: true}
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
		t.Errorf("read the firewall for an edit that leaves the groups alone")
	}
}
