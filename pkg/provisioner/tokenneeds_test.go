package provisioner

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	roleBlock = regexp.MustCompile(`(?s)resource "proxmox_virtual_environment_role" "([a-z_]+)" \{(.*?)\n\}\n`)
	privName  = regexp.MustCompile(`"([A-Z][A-Za-z]+\.[A-Za-z.]+)"`)
)

// moduleRoles reads the privileges of each role resource in the management cluster's module, by the
// resource's name. A privilege behind a condition counts as listed.
func moduleRoles(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "management-cluster", "proxmox.tf"))
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string][]string{}
	for _, m := range roleBlock.FindAllStringSubmatch(string(raw), -1) {
		body := m[2]
		_, list, found := strings.Cut(body, "privileges")
		if !found {
			t.Fatalf("role %s lists no privileges", m[1])
		}
		for _, p := range privName.FindAllStringSubmatch(list, -1) {
			roles[m[1]] = append(roles[m[1]], p[1])
		}
		slices.Sort(roles[m[1]])
	}
	if len(roles) < 10 {
		t.Fatalf("read only %d roles from the module: the pattern no longer fits it", len(roles))
	}
	return roles
}

// grantedAt lists what the roles hold together on a path.
func grantedAt(roles map[string][]string, names ...string) []string {
	var out []string
	for _, n := range names {
		out = append(out, roles[n]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func privsAt(needs []Need, path string) []string {
	var out []string
	for _, n := range needs {
		if n.Path == path {
			out = append(out, n.Privilege)
		}
	}
	slices.Sort(out)
	return out
}

// What a token is tested for is what the module grants it. A role the module takes from Proxmox VE
// (PVEVMAdmin, PVETemplateUser, PVEDatastoreAdmin) lists no privileges there, so capmox's pool and
// bootstrap storage are the only places not compared.
func TestTheTestedPrivilegesAreTheModulesRoles(t *testing.T) {
	t.Parallel()
	roles := moduleRoles(t)
	places := testPlaces()
	pool, templates, group := pathPool+places.WorkloadPool, pathPool+places.TemplatePool, "/access/groups/"+places.ClusterGroup
	local, node := pathStorage+"local", "/nodes/pve1"
	for name, tc := range map[string]struct {
		needs []Need
		// roles names, for each path, the module's roles the token holds there.
		roles map[string][]string
	}{
		"controller": {ControllerNeeds(&places), map[string][]string{
			pathRoot: {"audit"}, pathNodes: {"audit"}, pool: {"provisioner_pools"}, group: {"provisioner_users"},
			"/access/realm/pve": {"provisioner_realm"}, templates: {"templates_audit"}, pathStorage + "local-lvm": {"cluster_storage"},
		}},
		"Cluster API": {CapmoxNeeds(&places), map[string][]string{
			pathRoot: {"audit"}, pathNodes: {"audit"}, pathStorage + "local-lvm": {"allocate_space"},
		}},
		"template builder": {TemplateBuilderNeeds(&places), map[string][]string{
			pathRoot: {"audit"}, pathNodes: {"audit"}, node: {"template_builder_network"}, templates: {"template_builder_vms"},
			local: {"template_builder_files"}, pathStorage + "local-lvm": {"cluster_storage"},
		}},
		"HA":      {HANeeds(), map[string][]string{pathRoot: {"ha"}}},
		"restore": {RestoreNeeds(&places), map[string][]string{pool: {"etcd_restore"}}},
		"cluster": {ClusterNeeds(places.WorkloadPool+"/team-a.web", &places), map[string][]string{
			pathNodes: {"audit"}, pool + "/team-a.web": {"cluster_vms"}, pathStorage + "local-lvm": {"cluster_storage"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for path, names := range tc.roles {
				if want, got := grantedAt(roles, names...), privsAt(tc.needs, path); !slices.Equal(got, want) {
					t.Errorf("on %s the code tests %v, the module's roles %v grant %v", path, got, names, want)
				}
			}
			for _, n := range tc.needs {
				if _, listed := tc.roles[n.Path]; !listed && !strings.HasPrefix(n.Path, pool) {
					t.Errorf("%v is tested but no role of the module is compared on %s", n, n.Path)
				}
			}
		})
	}
}
