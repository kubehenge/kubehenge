package provisioner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luthermonson/go-proxmox"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// Settings are the Proxmox VE objects every cluster's share, created with the management cluster.
type Settings struct {
	// Group is the clusters' group: rights on the workload storages and the nodes.
	Group string
	// Role is given to each cluster's user on its own pool: VM.Audit for the cloud controller
	// manager, VM.Config.Disk for the CSI plugin.
	Role string
	// DatastoreRole is given to the user of a cluster that holds Datastore.Allocate on its storage,
	// there; it carries the group's privileges on the storage too, which it replaces.
	DatastoreRole string
	// SysAuditGroup holds Sys.Audit on /, unpropagated; the user of a cluster whose cloud
	// controller manager labels nodes with their HA groups joins it. Proxmox VE lets only
	// Permissions.Modify on / grant anything on /, which would let the controller grant itself every
	// privilege, so a cluster gets it through the group rather than an ACL of its own.
	SysAuditGroup string
	// Node, when set, is where volume owner VMs are created; otherwise the first online node.
	Node string
	// Region names the Proxmox VE cluster to the clusters (topology.kubernetes.io/region);
	// empty means the Proxmox VE cluster's name, or the node's when it is not clustered.
	Region string
}

// Credentials are what a cluster's cloud controller manager and CSI plugin sign in with, and what
// the controller has to keep between reconciles: Proxmox VE shows a token's secret only once, and
// lists a new VM only seconds after creating it, so neither can be looked up again reliably.
type Credentials struct {
	TokenID     string
	TokenSecret string
	// VolumeOwner is the VMID that owns the cluster's volumes, last seen on VolumeOwnerNode; it
	// moves to the node of a volume on node-local storage to delete it.
	VolumeOwner     int
	VolumeOwnerNode string
	Region          string
}

// Proxmox creates and removes a cluster's Proxmox VE objects through the API.
type Proxmox struct {
	Client   *proxmox.Client
	Settings Settings

	// region is the Proxmox VE cluster's name once read; it does not change while the controller
	// runs.
	mu     sync.Mutex
	region string
	// cpuLevels are the online nodes' x86-64 levels as read.
	cpuLevels map[string]int
	// newHolders are the holder VMs created lately, by pool, which Proxmox VE may not list yet.
	newHolders map[string][]holderVM
}

// Access is what a cluster's user holds beyond its own pool and its group's rights.
type Access struct {
	// DatastoreAllocate are the storages the user holds Settings.DatastoreRole on.
	DatastoreAllocate []string
	// SysAudit puts the user in Settings.SysAuditGroup, for Sys.Audit on /.
	SysAudit bool
}

// Ensure creates whatever of the cluster's objects is missing, and gives its user access and no
// more. held is what an earlier call returned, as far as it was kept. record keeps a chosen VMID
// before the VM is created, so that a failure after creating it never leads to a second one.
func (p *Proxmox) Ensure(ctx context.Context, n Names, access Access, held Credentials, record func(Credentials) error) (Credentials, error) {
	members, err := p.ensureAccess(ctx, n, access)
	if err != nil {
		return held, err
	}
	c, err := p.ensureVolumeOwner(ctx, n, held, members, record)
	if err != nil {
		return c, err
	}
	if c, err = p.keepToken(ctx, n, c, record); err != nil {
		return c, err
	}
	if c.Region, err = p.clusterRegion(ctx); err != nil {
		return c, err
	}
	return c, nil
}

// Remove deletes the cluster's objects. The cluster's own VMs must be gone first: the pool is
// removed only when empty, and the volume owner and the holder VMs take every volume they own with
// them. held is what was recorded for the cluster, as far as it still exists.
func (p *Proxmox) Remove(ctx context.Context, n Names, held Credentials) error {
	if err := p.removeVolumeHolders(ctx, n); err != nil {
		return err
	}
	members, found, err := p.poolMembers(ctx, n.Pool)
	if err != nil {
		return err
	}
	owners, err := p.volumeOwners(ctx, n, held, members)
	if err != nil {
		return err
	}
	for vmid, created := range owners {
		if err := p.removeVolumeOwner(ctx, vmid, created); err != nil {
			return err
		}
	}
	// Deleting the user takes its token and ACLs with it.
	if err := p.Client.Delete(ctx, "/access/users/"+url.PathEscape(n.User), nil); err != nil && !isMissing(err) {
		return fmt.Errorf("deleting user %s: %w", n.User, err)
	}
	if !found {
		return nil
	}
	if err := p.Client.Delete(ctx, "/pools?"+url.Values{"poolid": {n.Pool}}.Encode(), nil); err != nil && !isMissing(err) {
		return fmt.Errorf("deleting pool %s: %w", n.Pool, err)
	}
	return nil
}

// MoveVolumeOwner migrates a volume owner VM to another node. It is never started and holds no
// disks, so only its configuration moves.
func (p *Proxmox) MoveVolumeOwner(ctx context.Context, vmid int, from, to string) error {
	var upid string
	if err := p.Client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/migrate", from, vmid), map[string]any{"target": to}, &upid); err != nil {
		return fmt.Errorf("starting the migration: %w", err)
	}
	if err := p.waitTask(ctx, from, upid, moveTaskTimeout); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	return nil
}

// ensureAccess creates the cluster's pool, with the members it has, and its user with rights on it
// and access.
func (p *Proxmox) ensureAccess(ctx context.Context, n Names, access Access) ([]poolMember, error) {
	members, err := p.ensurePool(ctx, n.Pool)
	if err != nil {
		return nil, err
	}
	if err := p.ensureUser(ctx, n, access); err != nil {
		return nil, err
	}
	if err := p.ensureGrant(ctx, n, access); err != nil {
		return nil, err
	}
	return members, nil
}

// volumeOwners finds the cluster's volume owner VMs, by VMID with the node each was created on.
// Proxmox VE lists a new VM in its pool only seconds after creating it, so a cluster removed
// straight after creation is found by its recorded VMID.
func (p *Proxmox) volumeOwners(ctx context.Context, n Names, held Credentials, members []poolMember) (map[int]string, error) {
	owners := map[int]string{}
	for _, m := range members {
		if m.Type == qemuType && m.Name == n.VolumeOwner {
			owners[m.VMID] = m.Node
		}
	}
	if _, listed := owners[held.VolumeOwner]; held.VolumeOwner == 0 || listed {
		return owners, nil
	}
	found, err := p.isVolumeOwner(ctx, n, held.VolumeOwner, held.VolumeOwnerNode, nil)
	if err != nil {
		return nil, err
	}
	if found.ours {
		owners[held.VolumeOwner] = found.node
	}
	return owners, nil
}

// removeVolumeOwner destroys a volume owner VM with every volume it owns.
func (p *Proxmox) removeVolumeOwner(ctx context.Context, vmid int, created string) error {
	if err := p.removeLocalVolumes(ctx, vmid, created); err != nil {
		return err
	}
	node, err := locate(ctx, p.Client, vmid, created)
	if err != nil {
		return err
	}
	var upid string
	params := url.Values{"destroy-unreferenced-disks": {"1"}, "purge": {"1"}}
	if err := p.Client.Delete(ctx, fmt.Sprintf("/nodes/%s/qemu/%d?%s", node, vmid, params.Encode()), &upid); err != nil {
		return fmt.Errorf("deleting volume owner VM %d: %w", vmid, err)
	}
	if err := p.waitTask(ctx, node, upid, deleteTaskTimeout); err != nil {
		return fmt.Errorf("deleting volume owner VM %d: %w", vmid, err)
	}
	return nil
}

// nodeOnline is the status /nodes gives a node that is up.
const nodeOnline = "online"

const (
	// qemuType is the type Proxmox VE lists a VM by, in a pool or the cluster's resources.
	qemuType = "qemu"
	// vmidParam names a VM's VMID in Proxmox VE's API.
	vmidParam = "vmid"
)

// pveNode is a node as /nodes lists it.
type pveNode struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

// removeLocalVolumes deletes the owner VM's volumes on node-local storages. Destroying the VM
// deletes those on shared storage and on its own node, but not those on other nodes.
func (p *Proxmox) removeLocalVolumes(ctx context.Context, vmid int, created string) error {
	all, err := storages(ctx, p.Client)
	if err != nil {
		return err
	}
	var local []storageConfig
	for _, s := range all {
		if s.Shared == 0 && slices.Contains(strings.Split(s.Content, ","), "images") {
			local = append(local, s)
		}
	}
	if len(local) == 0 {
		return nil
	}
	var nodes []pveNode
	if err := p.Client.Get(ctx, "/nodes", &nodes); err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	owner := &ownerVM{pve: p.Client, mover: p, vmid: vmid, created: created, discardUnused: true}
	for _, node := range nodes {
		for _, s := range local {
			if s.Nodes != "" && !slices.Contains(strings.Split(s.Nodes, ","), node.Node) {
				continue
			}
			if err := p.removeVolumesOn(ctx, owner, node, s.Storage); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeVolumesOn deletes the owner VM's volumes on one node's storage.
func (p *Proxmox) removeVolumesOn(ctx context.Context, owner *ownerVM, node pveNode, storage string) error {
	// A node that has left the Proxmox VE cluster is no longer listed; one that is listed may come
	// back with the volumes.
	if node.Status != nodeOnline {
		return fmt.Errorf("node %s is %s; the volumes of owner VM %d on its storage %s are deleted once it is online", node.Node, node.Status, owner.vmid, storage)
	}
	var content []struct {
		VolID string `json:"volid"`
	}
	query := url.Values{"content": {"images"}, vmidParam: {strconv.Itoa(owner.vmid)}}
	path := fmt.Sprintf("/nodes/%s/storage/%s/content?%s", node.Node, storage, query.Encode())
	if err := p.Client.Get(ctx, path, &content); err != nil {
		return fmt.Errorf("listing volumes of owner VM %d on node %s storage %s: %w", owner.vmid, node.Node, storage, err)
	}
	for _, c := range content {
		if err := owner.deleteVolume(ctx, volume{node: node.Node, storage: storage, volid: c.VolID, owner: owner.vmid}); err != nil {
			return err
		}
	}
	return nil
}

type poolMember struct {
	Type string `json:"type"`
	VMID int    `json:"vmid"`
	Name string `json:"name"`
	Node string `json:"node"`
}

// poolMembers lists a pool's VMs, and whether the pool exists.
func (p *Proxmox) poolMembers(ctx context.Context, pool string) ([]poolMember, bool, error) {
	var pools []struct {
		Members []poolMember `json:"members"`
	}
	err := p.Client.Get(ctx, "/pools?"+url.Values{"poolid": {pool}}.Encode(), &pools)
	if isMissing(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading pool %s: %w", pool, err)
	}
	if len(pools) == 0 {
		return nil, false, nil
	}
	return pools[0].Members, true, nil
}

// ensurePool creates the cluster's pool if it is missing, and returns its members.
func (p *Proxmox) ensurePool(ctx context.Context, pool string) ([]poolMember, error) {
	members, found, err := p.poolMembers(ctx, pool)
	if err != nil || found {
		return members, err
	}
	err = p.Client.Post(ctx, "/pools", map[string]any{"poolid": pool, "comment": "Kubernetes cluster created by Kubehenge"}, nil)
	if err != nil && !isExisting(err) {
		return nil, fmt.Errorf("creating pool %s: %w", pool, err)
	}
	return nil, nil
}

// ensureUser creates the cluster's user in the clusters' group, and keeps it in the group holding
// Sys.Audit on / exactly while access asks for that. Proxmox VE shows the token a user of the
// clusters' group; of one that does not exist it says "no such user", and one of the same name
// outside the group it refuses to show, whose creation then fails rather than take it over.
func (p *Proxmox) ensureUser(ctx context.Context, n Names, access Access) error {
	if access.SysAudit && p.Settings.SysAuditGroup == "" {
		return fmt.Errorf("giving %s Sys.Audit on /: the controller has no group for it", n.User)
	}
	var user struct {
		Groups []string `json:"groups"`
	}
	err := p.Client.Get(ctx, "/access/users/"+url.PathEscape(n.User), &user)
	switch {
	case err == nil:
		return p.ensureSysAudit(ctx, n, access, user.Groups)
	case !isMissing(err) && !proxmox.IsNotAuthorized(err):
		return fmt.Errorf("reading user %s: %w", n.User, err)
	}
	groups := p.Settings.Group
	if access.SysAudit {
		groups += "," + p.Settings.SysAuditGroup
	}
	err = p.Client.Post(ctx, "/access/users", map[string]any{
		"userid":    n.User,
		groupsParam: groups,
		"comment":   "Kubernetes cluster " + n.Pool + "; signs in with its API token only",
	}, nil)
	if err != nil {
		return fmt.Errorf("creating user %s: %w", n.User, err)
	}
	return nil
}

// groupsParam is Proxmox VE's parameter for a user's groups, a comma-separated list.
const groupsParam = "groups"

// ensureSysAudit adds the user, a member of groups, to the group holding Sys.Audit on / or takes
// it out, as access asks. Proxmox VE replaces a user's groups with those it is given unless told
// to append, so taking one away gives it the others back.
func (p *Proxmox) ensureSysAudit(ctx context.Context, n Names, access Access, groups []string) error {
	group := p.Settings.SysAuditGroup
	if group == "" || slices.Contains(groups, group) == access.SysAudit {
		return nil
	}
	path := "/access/users/" + url.PathEscape(n.User)
	if access.SysAudit {
		if err := p.Client.Put(ctx, path, map[string]any{groupsParam: group, "append": 1}, nil); err != nil {
			return fmt.Errorf("adding %s to group %s: %w", n.User, group, err)
		}
		return nil
	}
	others := slices.DeleteFunc(slices.Clone(groups), func(g string) bool { return g == group })
	if err := p.Client.Put(ctx, path, map[string]any{groupsParam: strings.Join(others, ",")}, nil); err != nil {
		return fmt.Errorf("taking %s out of group %s: %w", n.User, group, err)
	}
	return nil
}

// storageACLPath is where Proxmox VE grants privileges on a storage.
const storageACLPath = "/storage/"

// grant is one role on one path, propagated to the paths below it or not.
type grant struct {
	role      string
	propagate int
}

// wantedGrants are the roles the cluster's user is to hold, by path.
func (p *Proxmox) wantedGrants(n Names, access Access) (map[string]grant, error) {
	wanted := map[string]grant{"/pool/" + n.Pool: {p.Settings.Role, 1}}
	for _, s := range access.DatastoreAllocate {
		if p.Settings.DatastoreRole == "" {
			return nil, fmt.Errorf("granting %s Datastore.Allocate on storage %s: the controller has no role for it", n.User, s)
		}
		// Not propagated: a storage has nothing below it, and Proxmox VE lets a token without
		// Permissions.Modify grant propagated privileges only where it holds them propagated.
		wanted[storageACLPath+s] = grant{p.Settings.DatastoreRole, 0}
	}
	return wanted, nil
}

// ensureGrant gives the cluster's user the clusters' role on its pool, the datastore role on each
// storage of access, and no other role on its pool or any storage. Every reconcile comes through
// here, and each ACL write rewrites the cluster-wide user.cfg, so it writes only what differs.
// Proxmox VE lists the entries on paths the token may change permissions on: the pool, and the
// storages it holds Datastore.Allocate on, the only ones it may grant the datastore role on.
func (p *Proxmox) ensureGrant(ctx context.Context, n Names, access Access) error {
	var entries []struct {
		Path      string `json:"path"`
		Type      string `json:"type"`
		UGID      string `json:"ugid"`
		RoleID    string `json:"roleid"`
		Propagate int    `json:"propagate"`
	}
	wanted, err := p.wantedGrants(n, access)
	if err != nil {
		return err
	}
	if err = p.Client.Get(ctx, "/access/acl", &entries); err != nil {
		return fmt.Errorf("reading the permissions of %s: %w", n.User, err)
	}
	granted := map[string]bool{}
	for _, e := range entries {
		if e.Type != "user" || e.UGID != n.User {
			continue
		}
		want, ours := wanted[e.Path]
		if !ours && !strings.HasPrefix(e.Path, storageACLPath) {
			continue
		}
		if ours && e.RoleID == want.role && e.Propagate == want.propagate {
			granted[e.Path] = true
			continue
		}
		revoke := map[string]any{"path": e.Path, "users": n.User, "roles": e.RoleID, "delete": 1}
		if err := p.Client.Put(ctx, "/access/acl", revoke, nil); err != nil {
			return fmt.Errorf("revoking %s from %s on %s: %w", e.RoleID, n.User, e.Path, err)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(wanted)) {
		if granted[path] {
			continue
		}
		want := wanted[path]
		grant := map[string]any{"path": path, "users": n.User, "roles": want.role, "propagate": want.propagate}
		if err := p.Client.Put(ctx, "/access/acl", grant, nil); err != nil {
			return fmt.Errorf("granting %s %s on %s: %w", n.User, want.role, path, err)
		}
	}
	return nil
}

func (p *Proxmox) ensureToken(ctx context.Context, n Names, secret string) (string, error) {
	path := fmt.Sprintf("/access/users/%s/token/%s", url.PathEscape(n.User), n.Token)
	err := p.Client.Get(ctx, path, &map[string]any{})
	switch {
	case err == nil && secret != "":
		return secret, nil
	case err == nil:
		if err = p.Client.Delete(ctx, path, nil); err != nil && !isMissing(err) {
			return "", fmt.Errorf("replacing token %s: %w", n.TokenID(), err)
		}
	case !isMissing(err):
		return "", fmt.Errorf("reading token %s: %w", n.TokenID(), err)
	}
	var created struct {
		Value string `json:"value"`
	}
	// Not privilege-separated: the user exists only for this token, and a separated token would
	// need every ACL twice.
	if err := p.Client.Post(ctx, path, map[string]any{"privsep": 0, "comment": "cloud controller manager and CSI plugin"}, &created); err != nil {
		return "", fmt.Errorf("creating token %s: %w", n.TokenID(), err)
	}
	if created.Value == "" {
		return "", fmt.Errorf("creating token %s: no secret in the response", n.TokenID())
	}
	return created.Value, nil
}

// keepToken ensures the cluster's token. Proxmox VE shows a token's secret only when creating it,
// and a token without its secret is replaced; kept at once, a failure later in the reconcile does
// not cost a new token each retry.
func (p *Proxmox) keepToken(ctx context.Context, n Names, c Credentials, record func(Credentials) error) (Credentials, error) {
	secret, err := p.ensureToken(ctx, n, c.TokenSecret)
	if err != nil {
		return c, err
	}
	c.TokenID = n.TokenID()
	if secret == c.TokenSecret {
		return c, nil
	}
	c.TokenSecret = secret
	return c, record(c)
}

func (p *Proxmox) ensureVolumeOwner(ctx context.Context, n Names, c Credentials, members []poolMember, record func(Credentials) error) (Credentials, error) {
	if c.VolumeOwner != 0 {
		found, err := p.isVolumeOwner(ctx, n, c.VolumeOwner, c.VolumeOwnerNode, members)
		if err != nil {
			return c, err
		}
		if found.ours {
			c.VolumeOwnerNode = found.node
			return c, p.describeVolumeOwner(ctx, found.node, c.VolumeOwner, n, found.config)
		}
	}
	node, err := p.ownerNode(ctx)
	if err != nil {
		return c, err
	}
	// A VMID Proxmox VE offers is not reserved: another cluster's owner, a machine or a template
	// build created at the same moment can take it first, and the create is then refused.
	for attempt := 1; ; attempt++ {
		var upid string
		c, upid, err = p.createVolumeOwner(ctx, n, c, node, record)
		switch {
		case isExisting(err) && attempt < volumeOwnerAttempts:
			continue
		case err != nil:
			return c, err
		}
		if err := p.waitTask(ctx, node, upid, createTaskTimeout); err != nil {
			return c, fmt.Errorf("creating volume owner VM %d: %w", c.VolumeOwner, err)
		}
		return c, p.describeVolumeOwner(ctx, node, c.VolumeOwner, n, ownerConfig{})
	}
}

const volumeOwnerAttempts = 5

// createVolumeOwner records the next free VMID as the cluster's volume owner and starts creating
// it on node.
func (p *Proxmox) createVolumeOwner(ctx context.Context, n Names, c Credentials, node string, record func(Credentials) error) (Credentials, string, error) {
	var next string
	if err := p.Client.Get(ctx, "/cluster/nextid", &next); err != nil {
		return c, "", fmt.Errorf("choosing a VMID: %w", err)
	}
	var vmid int
	if _, err := fmt.Sscan(next, &vmid); err != nil {
		return c, "", fmt.Errorf("choosing a VMID: %q: %w", next, err)
	}
	c.VolumeOwner, c.VolumeOwnerNode = vmid, node
	if err := record(c); err != nil {
		return c, "", err
	}
	// Never started and without disks; it exists so that the cluster's volumes have an owner
	// only this cluster's token may configure. Proxmox VE checks every other setting against the
	// VM's own path while creating it, before its pool's rights reach it, so the rest comes after.
	vm := map[string]any{vmidParam: vmid, "name": n.VolumeOwner, "pool": n.Pool}
	var upid string
	if err := p.Client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu", node), vm, &upid); err != nil {
		return c, "", fmt.Errorf("creating volume owner VM %d: %w", vmid, err)
	}
	return c, upid, nil
}

type ownerConfig struct {
	Name        string `json:"name"`
	Tags        string `json:"tags"`
	Description string `json:"description"`
}

// isVolumeOwner reports whether VMID vmid is this cluster's volume owner, with the node it is on
// (created while it is not listed yet) and its configuration. The VMID may have been recorded for
// a VM whose creation never happened and since gone to another VM, which the token then cannot
// read or which carries another name. The name alone is not proof: removing the owner deletes
// every volume it owns, so it must also be in the cluster's pool, which only the controller sets.
// members are the pool's members where already listed: a VM among them is in the pool, on the node
// given, which spares listing every VM of the Proxmox VE cluster.
func (p *Proxmox) isVolumeOwner(ctx context.Context, n Names, vmid int, created string, members []poolMember) (ownerFound, error) {
	for _, m := range members {
		if m.Type != qemuType || m.VMID != vmid {
			continue
		}
		found := ownerFound{node: m.Node}
		err := p.Client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", m.Node, vmid), &found.config)
		switch {
		case isMissing(err) || proxmox.IsNotAuthorized(err):
			return found, nil
		case err != nil:
			return found, fmt.Errorf("reading volume owner VM %d: %w", vmid, err)
		}
		found.ours = found.config.Name == n.VolumeOwner
		return found, nil
	}
	vm, listed, err := listedVM(ctx, p.Client, vmid)
	if err != nil {
		return ownerFound{node: created}, err
	}
	found := ownerFound{node: created}
	if listed {
		found.node = vm.Node
	}
	err = p.Client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", found.node, vmid), &found.config)
	switch {
	case isMissing(err) || proxmox.IsNotAuthorized(err):
		return found, nil
	case err != nil:
		return found, fmt.Errorf("reading volume owner VM %d: %w", vmid, err)
	case found.config.Name != n.VolumeOwner:
		return found, nil
	case !listed:
		return found, fmt.Errorf("volume owner VM %d is not listed yet, so its pool cannot be confirmed", vmid)
	}
	found.ours = vm.Pool == n.Pool
	return found, nil
}

// ownerFound is what isVolumeOwner found of a VMID: the node the VM is on, its configuration, and
// whether it is the cluster's volume owner.
type ownerFound struct {
	node   string
	config ownerConfig
	ours   bool
}

// describeVolumeOwner sets the owner VM's tags and description where they differ from current.
// Every reconcile comes through here, and each write rewrites the VM's configuration file and
// lands in Proxmox VE's task log.
func (p *Proxmox) describeVolumeOwner(ctx context.Context, node string, vmid int, n Names, current ownerConfig) error {
	config := map[string]any{}
	if tags := "kubehenge;volumes"; current.Tags != tags {
		config["tags"] = tags
	}
	if description := "Owns the persistent volumes of Kubernetes cluster " + n.Pool + ". Never started; deleting it deletes them."; current.Description != description {
		config["description"] = description
	}
	if len(config) == 0 {
		return nil
	}
	if err := p.Client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), config, nil); err != nil {
		return fmt.Errorf("describing volume owner VM %d: %w", vmid, err)
	}
	return nil
}

func (p *Proxmox) ownerNode(ctx context.Context) (string, error) {
	if p.Settings.Node != "" {
		return p.Settings.Node, nil
	}
	var nodes []pveNode
	if err := p.Client.Get(ctx, "/nodes", &nodes); err != nil {
		return "", fmt.Errorf("listing nodes: %w", err)
	}
	slices.SortFunc(nodes, func(a, b pveNode) int { return strings.Compare(a.Node, b.Node) })
	for _, node := range nodes {
		if node.Status == nodeOnline {
			return node.Node, nil
		}
	}
	return "", errors.New("no Proxmox VE node is online")
}

func (p *Proxmox) clusterRegion(ctx context.Context) (string, error) {
	if p.Settings.Region != "" {
		return p.Settings.Region, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.region != "" {
		return p.region, nil
	}
	region, err := p.readRegion(ctx)
	p.region = region
	return region, err
}

func (p *Proxmox) readRegion(ctx context.Context) (string, error) {
	var status []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Local int    `json:"local"`
	}
	if err := p.Client.Get(ctx, "/cluster/status", &status); err != nil {
		return "", fmt.Errorf("reading the cluster status: %w", err)
	}
	for _, s := range status {
		if s.Type == "cluster" {
			return s.Name, nil
		}
	}
	for _, s := range status {
		if s.Type == "node" && s.Local == 1 {
			return s.Name, nil
		}
	}
	return "", errors.New("the Proxmox VE cluster status names neither a cluster nor a node")
}

// waitTask waits for a task to end. A task can hang, as one stuck on storage that stopped
// answering does, and the wait holds one of the controller's four reconcile workers, so it gives
// up after timeout; the next reconcile finds whatever the task did by then. Most tasks end within
// a fraction of a second, so it asks again soon at first and less often the longer one runs.
func (p *Proxmox) waitTask(ctx context.Context, node, upid string, timeout time.Duration) error {
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", node, url.PathEscape(upid))
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for interval := taskPollFirst; ; interval = min(taskPollGrowth*interval, taskPollMax) {
		var status struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := p.Client.Get(ctx, path, &status); err != nil {
			return fmt.Errorf("reading task %s: %w", upid, err)
		}
		if status.Status == "stopped" {
			if status.ExitStatus != "OK" {
				return fmt.Errorf("on node %s: %w", node, pve.TaskFailed(ctx, p.Client.Get, node, upid, status.ExitStatus))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for task %s: %w", upid, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("task %s still running after %s", upid, timeout)
		case <-time.After(interval):
		}
	}
}

const (
	taskPollFirst = 100 * time.Millisecond
	// taskPollGrowth multiplies the wait between looks at a task, up to taskPollMax.
	taskPollGrowth = 2
	taskPollMax    = 2 * time.Second
)

// How long the tasks on a volume owner VM may take. It has no disks of its own, so creating and
// moving it are quick; deleting it deletes every volume it owns.
const (
	createTaskTimeout = 2 * time.Minute
	moveTaskTimeout   = 5 * time.Minute
	deleteTaskTimeout = 15 * time.Minute
)

// Proxmox VE reports a missing or existing object as a 500 with the reason in the status line,
// which go-proxmox returns as the error text.
func isMissing(err error) bool {
	return err != nil && (proxmox.IsNotFound(err) || strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "no such"))
}

func isExisting(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}
