package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func testUpgraderPlaces() *UpgraderPlaces {
	return &UpgraderPlaces{ //nolint:gosec // names of Proxmox VE objects, no credential
		ManagementPool: "kubehenge-management", VMDatastore: "local-lvm", ImageDatastore: "local",
		Network: "/sdn/zones/localnetwork/vmbr0", TokenGroup: "kubehenge-tokens", Nodes: []string{"pve1", "pve2"},
	}
}

// grantedOn is what Proxmox VE answers for the effective permissions of a user holding grants: a
// grant reaches its own path, and the paths below it where it propagates.
func grantedOn(grants []Grant, paths []string) Permissions {
	out := Permissions{}
	for _, path := range paths {
		privs := map[string]int{}
		for _, g := range grants {
			below := g.Propagate && (g.Path == pathRoot || strings.HasPrefix(path, g.Path+"/"))
			if g.Path != path && !below {
				continue
			}
			for _, p := range UpgraderRoles[g.Role] {
				privs[p] = 1
			}
		}
		out[path] = privs
	}
	return out
}

// The upgrader's roles, where it holds them, pass the privilege check of what an automatic upgrade
// of an existing management cluster asks of Proxmox VE.
func TestTheUpgraderRolesSufficeForAnUpgrade(t *testing.T) {
	t.Parallel()
	p := testUpgraderPlaces()
	grants := UpgraderGrants(p)
	probe := TokenProbe{Name: "Upgrader", Needs: UpgraderNeeds(p), Ask: func(_ context.Context, paths []string) (Permissions, error) {
		return grantedOn(grants, paths), nil
	}}
	if got := probe.Check(t.Context()); !got.OK {
		t.Fatalf("the upgrader's roles fail the check: %+v", got)
	}
	// The check fails without a grant, so it reads them.
	probe.Ask = func(_ context.Context, paths []string) (Permissions, error) {
		return grantedOn(slices.DeleteFunc(slices.Clone(grants), func(g Grant) bool { return g.Role == UpgraderImagesRole }), paths), nil
	}
	if got := probe.Check(t.Context()); got.OK || got.Problem != TokenPrivileges {
		t.Fatalf("the check passes without the images role: %+v", got)
	}
}

// No role of the upgrader lets its token grant itself more, or reach the nodes' shells.
func TestTheUpgraderCannotGrantItselfMore(t *testing.T) {
	t.Parallel()
	for _, g := range UpgraderGrants(testUpgraderPlaces()) {
		for _, p := range UpgraderRoles[g.Role] {
			switch {
			case p == "Permissions.Modify", p == "User.Allocate", p == "Sys.Modify", p == "Sys.Console", p == "Group.Allocate",
				strings.HasPrefix(p, "Realm."):
				t.Errorf("%s holds %s on %s", g.Role, p, g.Path)
			case p == "User.Modify" && (g.Propagate || g.Path != "/access/groups/kubehenge-tokens"):
				t.Errorf("%s holds User.Modify beyond the token users' group, on %s", g.Role, g.Path)
			case g.Path == pathRoot && !strings.HasSuffix(p, ".Audit"):
				t.Errorf("%s holds %s on /, which is more than reading", g.Role, p)
			}
		}
	}
}
