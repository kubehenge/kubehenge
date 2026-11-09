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

// fakeSecurityGroups answers with these groups, or none where unreadable.
type fakeSecurityGroups struct {
	groups   []string
	readable bool
	asked    int
}

func (f *fakeSecurityGroups) SecurityGroups(context.Context) ([]string, bool) {
	f.asked++
	return f.groups, f.readable
}

// withSecurityGroups gives a networkCluster the securityGroups variable, as its JSON. Empty, none.
func withSecurityGroups(c *clusterv1.Cluster, groups string) *clusterv1.Cluster {
	if groups != "" {
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
			Name: SecurityGroupsVariable, Value: apiextensionsv1.JSON{Raw: []byte(groups)},
		})
	}
	return c
}

func TestValidatorHoldsTheFirewallToProxmoxVEsSecurityGroups(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		groups    fakeSecurityGroups
		variable  string
		refusal   string
		wantAsked bool
	}{
		{"every group exists", fakeSecurityGroups{groups: []string{"k8s", "base"}, readable: true}, `["k8s","base"]`, "", true},
		{"a missing group", fakeSecurityGroups{groups: []string{"k8s"}, readable: true}, `["k8s","base"]`, "Proxmox VE has no security group base. Create it under Datacenter, Firewall, Security Group", true},
		{"groups that cannot be read", fakeSecurityGroups{}, `["k8s"]`, "", true},
		{"no firewall", fakeSecurityGroups{readable: true}, ``, "", false},
	} {
		hooks, _ := hooksFor(t)
		hooks.SecurityGroups = &tc.groups
		v := &ClusterValidator{Hooks: hooks}
		_, err := v.ValidateCreate(context.Background(), withSecurityGroups(networkCluster("192.168.1.1", 24), tc.variable))
		if tc.refusal == "" && err != nil && strings.Contains(err.Error(), "security group") {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
		if tc.refusal != "" && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.refusal)) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.refusal)
		}
		if asked := tc.groups.asked > 0; asked != tc.wantAsked {
			t.Errorf("%s: asked for the groups %v, want %v", tc.name, asked, tc.wantAsked)
		}
	}
}

// A cluster whose group was deleted later is not refused for an unrelated edit, but a change to its
// groups is held to those Proxmox VE has.
func TestValidatorHoldsARunningClusterToItsSecurityGroupsOnlyWhenTheyChange(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	groups := &fakeSecurityGroups{groups: []string{"k8s"}, readable: true}
	hooks.SecurityGroups = groups
	v := &ClusterValidator{Hooks: hooks}
	running := withSecurityGroups(networkCluster("192.168.1.1", 24), `["gone"]`)
	edited := running.DeepCopy()
	edited.Labels = map[string]string{"edited": "yes"}
	if _, err := v.ValidateUpdate(context.Background(), running, edited); err != nil && strings.Contains(err.Error(), "security group") {
		t.Errorf("an unrelated edit: %v", err)
	}
	if groups.asked != 0 {
		t.Error("asked for the groups on an unrelated edit")
	}
	changed := withSecurityGroups(networkCluster("192.168.1.1", 24), `["k8s","gone"]`)
	if _, err := v.ValidateUpdate(context.Background(), running, changed); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "no security group gone") {
		t.Errorf("a change to a missing group: %v", err)
	}
}

func TestSecurityGroupsAreReadFromTheDatacenterFirewall(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/firewall/groups" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"group":"k8s","digest":"x"},{"group":"base","digest":"x","comment":"baseline"}]}`)
	}))
	t.Cleanup(srv.Close)
	groups, ok := (&Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}).SecurityGroups(t.Context())
	if !ok || strings.Join(groups, ",") != "k8s,base" {
		t.Errorf("groups = %v, %v", groups, ok)
	}
	srv.Close()
	if _, ok := (&Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}).SecurityGroups(t.Context()); ok {
		t.Error("an unreachable Proxmox VE reads as readable")
	}
}
