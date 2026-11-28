package provisioner

import "slices"

// UpgraderPlaces are where an automatic upgrade of the management cluster works in Proxmox VE, as
// the appliance's saved settings name them.
type UpgraderPlaces struct {
	// ManagementPool holds the node VMs.
	ManagementPool string
	// VMDatastore holds their disks and cloud-init drives, ImageDatastore the Talos images.
	VMDatastore, ImageDatastore string
	// Network is the ACL path of the nodes' bridge or VNet.
	Network string
	// TokenGroup is the group the module's token users and the upgrader are in.
	TokenGroup string
	// Nodes are the Proxmox VE nodes the Talos image is downloaded to.
	Nodes []string
}

// The roles the upgrader holds. None holds Permissions.Modify, User.Allocate, Sys.Modify,
// Sys.Console or a Realm privilege, so the token cannot grant itself more: users, roles, ACLs and
// tokens are an administrator's, in a first apply or an Upgrade… with a session.
const (
	UpgraderAuditRole  = "KubehengeUpgraderAudit"
	UpgraderRole       = "KubehengeUpgrader"
	UpgraderImagesRole = "KubehengeUpgraderImages"
	UpgraderTokensRole = "KubehengeUpgraderTokens"
)

// UpgraderRoles are the privileges of each role. A privilege of UpgraderRole is held on every path
// that role is granted on, and acts only where Proxmox VE checks it: VM privileges on the pool, the
// space on the storage, SDN.Use on the bridge, Sys.AccessNetwork on the nodes.
var UpgraderRoles = map[string][]string{
	// The plan's refresh reads the module's users, groups and ACLs (Sys.Audit on /access), pools,
	// VMs, storages and bridges. Reading changes nothing.
	UpgraderAuditRole: {"Datastore.Audit", "Pool.Audit", "SDN.Audit", "Sys.Audit", "VM.Audit"},
	// The node VMs' configuration a release changes (proxmox.tf, node), the protection the appliance
	// sets around a plan (pkg/appliance/protection.go, setProtection), the space of their disks and
	// cloud-init drives, their bridge, and the download of a new Talos image.
	UpgraderRole: {
		"Datastore.AllocateSpace", "SDN.Use", "Sys.AccessNetwork", "VM.Allocate", "VM.Audit", "VM.Config.CPU",
		"VM.Config.Cloudinit", "VM.Config.Disk", "VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network",
		"VM.Config.Options", "VM.PowerMgmt",
	},
	// A new Talos release's image (proxmox_download_file.talos, named by version): storing it, and
	// deleting the one it replaces, which Proxmox VE allows only with Datastore.Allocate.
	UpgraderImagesRole: {"Datastore.Allocate", "Datastore.AllocateSpace", "Datastore.AllocateTemplate"},
	// Reading the module's API tokens, which Proxmox VE allows another user only with User.Modify
	// on a group of their users, and revoking the upgrader's own token.
	UpgraderTokensRole: {"User.Modify"},
}

// Grant is a role on a path.
type Grant struct {
	Path      string
	Role      string
	Propagate bool
}

// UpgraderGrants are where the upgrader holds each role. Proxmox VE takes a user's grants on the
// deepest path that has any in place of those above it, so each path with a role also holds the
// audit role, or the reads granted on / would stop there (Pool.Audit on the pool, for one).
func UpgraderGrants(p *UpgraderPlaces) []Grant {
	grants := []Grant{{pathRoot, UpgraderAuditRole, true}}
	for _, g := range []Grant{
		{pathPool + p.ManagementPool, UpgraderRole, true},
		{pathStorage + p.VMDatastore, UpgraderRole, false},
		{p.Network, UpgraderRole, true},
		{pathNodes, UpgraderRole, true},
		{pathStorage + p.ImageDatastore, UpgraderImagesRole, false},
		{"/access/groups/" + p.TokenGroup, UpgraderTokensRole, false},
	} {
		if audit := (Grant{g.Path, UpgraderAuditRole, g.Propagate}); !slices.Contains(grants, audit) {
			grants = append(grants, audit)
		}
		grants = append(grants, g)
	}
	return grants
}

// UpgraderNeeds are what an automatic upgrade of a management cluster that exists asks of Proxmox
// VE: the setup privileges (pkg/ui/setup.go) without those only a first apply needs to create
// users, groups, roles, ACLs, tokens and pools, and with the reads that take their place.
func UpgraderNeeds(p *UpgraderPlaces) []Need {
	pool := pathPool + p.ManagementPool
	needs := slices.Concat(
		needsOn("/access", privSysAudit),
		needsOn("/access/groups/"+p.TokenGroup, "User.Modify"),
		needsOn(pool, "Pool.Audit", "VM.Allocate", privVMAudit, "VM.PowerMgmt", "VM.Config.CPU", "VM.Config.Cloudinit",
			"VM.Config.Disk", "VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options"),
		needsOn(pathStorage+p.VMDatastore, privAllocateSpace),
		needsOn(pathStorage+p.ImageDatastore, "Datastore.Allocate", privAllocateSpace, "Datastore.AllocateTemplate"),
		needsOn(p.Network, "SDN.Audit", "SDN.Use"),
	)
	for _, n := range p.Nodes {
		needs = append(needs, needsOn(pathNodes+"/"+n, "Sys.AccessNetwork")...)
	}
	return needs
}
