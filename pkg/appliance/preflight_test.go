package appliance

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// healthy is an inventory the settings("pve1") pass against.
func healthy() *Inventory {
	return &Inventory{
		Storages: []pve.Storage{
			{Storage: "local-lvm", Node: "pve1", Type: "lvm", Content: []string{"images", "rootdir"}, Active: true, Avail: 200 * gib},
			{Storage: "local", Node: "pve1", Content: []string{"iso", "import", "vztmpl"}, Active: true, Avail: 50 * gib},
		},
		Nodes:         []pve.Node{{Node: "pve1", Online: true, Memory: 2 * gib, MaxMemory: 64 * gib}, {Node: "pve2", Online: true, Memory: 2 * gib, MaxMemory: 64 * gib}},
		Bridges:       map[string][]pve.Bridge{"pve1": {{Bridge: "vmbr0", Node: "pve1", CIDR: "10.0.0.5/24", Gateway: "10.0.0.1"}}},
		Pools:         map[string]bool{"other": true},
		Roles:         map[string]bool{"PVEAdmin": true},
		Users:         map[string]bool{"root@pam": true},
		Groups:        map[string]bool{},
		NodeAddresses: map[string][]netip.Addr{"pve1": {netip.MustParseAddr("10.0.0.5")}},
		Answering:     map[netip.Addr]bool{},
		ProxmoxURL:    "https://pve.example.org:8006",
	}
}

func mentions(t *testing.T, messages []string, parts ...string) {
	t.Helper()
	for _, m := range messages {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(m, p)
		}
		if ok {
			return
		}
	}
	t.Errorf("no message holds all of %q in %q", parts, messages)
}

func TestPreflightPassesAHealthyCluster(t *testing.T) {
	t.Parallel()
	f := settings("pve1").Preflight(healthy(), false)
	if len(f.Errors) != 0 || len(f.Warnings) != 0 {
		t.Errorf("findings = %+v", f)
	}
	if len(f.PodRanges) != 1 || len(f.ServiceRanges) != 1 {
		t.Errorf("the chosen ranges are missing: %+v", f)
	}
}

func TestPreflightStoragesNeedTheContentActiveAndEnabledOnEveryNode(t *testing.T) {
	t.Parallel()
	two := settings("pve1")
	two.Nodes, two.NodeAddresses = []string{"pve1", "pve2"}, []string{"10.0.0.11/24", "10.0.0.12/24"}
	for _, c := range []struct {
		change func(*Inventory)
		parts  []string
	}{
		{func(i *Inventory) { i.Storages[1].Content = []string{"iso"} }, []string{"Storage local on node pve1", "import content type", "Datacenter, Storage, Edit, Content"}},
		{func(i *Inventory) { i.Storages[1].Content = []string{"import"} }, []string{"Storage local on node pve1", "iso content type"}},
		{func(i *Inventory) { i.Storages[0].Content = []string{"rootdir"} }, []string{"Storage local-lvm on node pve1", "images content type"}},
		{func(i *Inventory) { i.Storages[0].Active = false }, []string{"Storage local-lvm is not active on node pve1"}},
		{func(i *Inventory) { i.Storages = i.Storages[1:] }, []string{"Storage local-lvm is not enabled on node pve1", "Datacenter, Storage, Edit, Nodes"}},
		{func(i *Inventory) { i.Storages[0].Avail = 10 * gib }, []string{"Storage local-lvm has 10.0 GiB free on pve1 for 32.0 GiB of disks."}},
	} {
		inv := healthy()
		c.change(inv)
		mentions(t, settings("pve1").Preflight(inv, false).Errors, c.parts...)
	}
	// pve2 has neither storage.
	f := two.Preflight(healthy(), false)
	mentions(t, f.Errors, "Storage local-lvm is not enabled on node pve2")
	mentions(t, f.Errors, "Storage local is not enabled on node pve2")
}

func TestPreflightSharedStorageHoldsEveryNodesDisks(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.Nodes, s.NodeAddresses = []string{"pve1", "pve2", "pve3"}, []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
	inv := healthy()
	inv.Nodes = append(inv.Nodes, pve.Node{Node: "pve3", Online: true, MaxMemory: 64 * gib})
	inv.Storages = nil
	for _, n := range s.Nodes {
		inv.Storages = append(inv.Storages,
			pve.Storage{Storage: "local-lvm", Node: n, Type: "lvm", Shared: true, Content: []string{"images"}, Active: true, Avail: 80 * gib},
			pve.Storage{Storage: "local", Node: n, Content: []string{"iso", "import"}, Active: true, Avail: 50 * gib})
	}
	inv.Bridges = map[string][]pve.Bridge{}
	f := s.Preflight(inv, false)
	mentions(t, f.Errors, "Storage local-lvm has 80.0 GiB free, shared by every node, for 96.0 GiB of disks.")
	if len(f.Errors) != 1 {
		t.Errorf("one shared storage is one finding: %+v", f.Errors)
	}
	for i := range inv.Storages {
		if inv.Storages[i].Storage == "local-lvm" {
			inv.Storages[i].Avail = 100 * gib
		}
	}
	if f = s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Fatalf("96 GiB of disks on 100 GiB shared: %+v", f)
	}
}

func TestPreflightCountsAreSingularForOne(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddresses = []string{"10.0.0.11/24"}
	inv := healthy()
	inv.Nodes[0].Memory = 63 * gib
	f := s.Preflight(inv, false)
	mentions(t, f.Errors, "its 1 management VM need")
}

func TestPreflightDiskSpaceByHowTheStorageAllocates(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	for _, c := range []struct {
		name    string
		storage pve.Storage
		thin    bool
		unknown bool
	}{
		{"thick LVM", pve.Storage{Type: "lvm"}, false, false},
		{"LVM-thin", pve.Storage{Type: "lvmthin"}, true, false},
		{"sparse ZFS", pve.Storage{Type: "zfspool", Sparse: &yes}, true, false},
		{"thick ZFS", pve.Storage{Type: "zfspool", Sparse: &no}, false, false},
		{"ZFS of unknown sparseness", pve.Storage{Type: "zfspool"}, false, true},
		{"Ceph RBD", pve.Storage{Type: "rbd"}, true, false},
		{"raw files on a directory", pve.Storage{Type: "dir"}, true, false},
		{"raw files on NFS", pve.Storage{Type: "nfs"}, true, false},
		{"raw files on CIFS", pve.Storage{Type: "cifs"}, true, false},
		{"files preallocated with falloc", pve.Storage{Type: "dir", Preallocation: "falloc"}, false, false},
		{"unknown type", pve.Storage{}, false, true},
	} {
		inv := healthy()
		inv.Storages[0].Type, inv.Storages[0].Sparse, inv.Storages[0].Preallocation = c.storage.Type, c.storage.Sparse, c.storage.Preallocation
		inv.Storages[0].Avail = 10 * gib
		f := settings("pve1").Preflight(inv, false)
		switch {
		case c.unknown:
			mentions(t, f.Warnings, "Storage local-lvm has 10.0 GiB free on pve1 for 32.0 GiB of disks.", "did not say")
		case c.thin:
			mentions(t, f.Warnings, "Storage local-lvm ("+c.storage.Type+") has 10.0 GiB free on pve1 for 32.0 GiB of disks. Writes fail once it fills.")
		default:
			mentions(t, f.Errors, "Storage local-lvm has 10.0 GiB free on pve1 for 32.0 GiB of disks. Free space there")
		}
		if (c.thin || c.unknown) && len(f.Errors) != 0 {
			t.Errorf("%s: errors %q", c.name, f.Errors)
		}
	}
}

func TestPreflightBridgeMustExistOnEveryNode(t *testing.T) {
	t.Parallel()
	inv := healthy()
	inv.Bridges["pve1"] = []pve.Bridge{{Bridge: "vmbr1", Node: "pve1"}}
	mentions(t, settings("pve1").Preflight(inv, false).Errors, "Node pve1 has no bridge vmbr0", "pve1, System, Network")
	delete(inv.Bridges, "pve1")
	if f := settings("pve1").Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("bridges that could not be read must not fail the run: %+v", f)
	}
}

func TestPreflightVNetMustReachEveryNodeWhereItCanBeRead(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.Bridge = "evpn/tenants"
	inv := healthy()
	inv.Networks = map[string]pve.Network{"tenants": {Zone: "evpn", ZoneType: "evpn"}}
	mentions(t, s.Preflight(inv, false).Errors, "Node pve1 does not carry VNet tenants. Add the node to zone evpn")
	inv.Bridges["pve1"] = append(inv.Bridges["pve1"], pve.Bridge{Bridge: "tenants", Node: "pve1", Zone: "evpn"})
	if f := s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("a VNet the node carries: %+v", f.Errors)
	}
	// A VNet the user may not read is listed on no node, and Proxmox VE judges it at the clone.
	inv = healthy()
	if f := s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("a VNet that could not be read must not fail the run: %+v", f.Errors)
	}
}

func TestPreflightNodeMemory(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.Nodes, s.NodeAddresses = []string{"pve1"}, []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
	inv := healthy()
	inv.Nodes[0].MaxMemory, inv.Nodes[0].Memory = 16*gib, 6*gib
	f := s.Preflight(inv, false)
	mentions(t, f.Errors, "Node pve1 has 10.0 GiB memory free", "its 3 management VMs need 12.0 GiB")
	inv.Nodes[0].Memory = 2 * gib
	for _, e := range s.Preflight(inv, false).Errors {
		if strings.Contains(e, "memory") {
			t.Errorf("14 GiB free for 12 GiB: %s", e)
		}
	}
}

func TestPreflightOfflineOrUnknownNode(t *testing.T) {
	t.Parallel()
	inv := healthy()
	inv.Nodes[0].Online = false
	mentions(t, settings("pve1").Preflight(inv, false).Errors, "Node pve1 is offline")
	mentions(t, settings("pve9").Preflight(healthy(), false).Errors, "Node pve9 is not in the Proxmox VE cluster")
}

func TestPreflightAddressesInUse(t *testing.T) {
	t.Parallel()
	inv := healthy()
	inv.Answering[netip.MustParseAddr("10.0.0.11")] = true
	inv.Answering[netip.MustParseAddr("10.0.0.10")] = true
	f := settings("pve1").Preflight(inv, false)
	mentions(t, f.Errors, "Address 10.0.0.11 answers")
	mentions(t, f.Errors, "Address 10.0.0.10 answers")
	if f := settings("pve1").Preflight(inv, true); len(f.Errors) != 0 {
		t.Errorf("the module's own VMs answer once its state exists: %+v", f)
	}
}

func TestPreflightNamesTheModuleWouldCreate(t *testing.T) {
	t.Parallel()
	inv := healthy()
	inv.Pools["kubehenge"] = true
	inv.Roles["kubehenge-audit"] = true
	inv.Users["kubehenge-provisioner@pve"] = true
	inv.Groups["kubehenge-clusters"] = true
	f := settings("pve1").Preflight(inv, false)
	mentions(t, f.Errors, "Pool kubehenge exists", "Datacenter, Permissions, Pools")
	mentions(t, f.Errors, "Role kubehenge-audit exists")
	mentions(t, f.Errors, "User kubehenge-provisioner@pve exists")
	mentions(t, f.Errors, "Group kubehenge-clusters exists")
	if f := settings("pve1").Preflight(inv, true); len(f.Errors) != 0 {
		t.Errorf("the state owns them: %+v", f)
	}
	// A role of a feature that is off is no collision.
	inv = healthy()
	inv.Roles["kubehenge-ha"] = true
	if f := settings("pve1").Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("HA is off: %+v", f)
	}
	on := settings("pve1")
	on.ProxmoxHA = true
	mentions(t, on.Preflight(inv, false).Errors, "Role kubehenge-ha exists")
}

func TestPreflightWarnsOfVMsSharingNodes(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
	mentions(t, s.Preflight(healthy(), false).Warnings, "3 management VMs share 1 Proxmox VE node.", "Choose 3 nodes")
	s.Nodes = []string{"pve1", "pve2"}
	mentions(t, s.Preflight(healthy(), false).Warnings, "3 management VMs share 2 Proxmox VE nodes.")
	s.Nodes = []string{"pve1", "pve2", "pve3"}
	for _, w := range s.Preflight(healthy(), false).Warnings {
		if strings.Contains(w, "share") {
			t.Errorf("one VM per node: %s", w)
		}
	}
}

func TestPreflightWarnsWhenTheAPIAddressIsOneNodes(t *testing.T) {
	t.Parallel()
	for _, url := range []string{"https://10.0.0.5:8006", "https://pve1:8006"} {
		inv := healthy()
		inv.ProxmoxURL = url
		mentions(t, settings("pve1").Preflight(inv, false).Warnings, "is node pve1 alone")
	}
}

func TestPreflightWarnsOfRangesOverlappingTheNetworks(t *testing.T) {
	t.Parallel()
	inv := healthy()
	inv.Bridges["pve1"] = append(inv.Bridges["pve1"], pve.Bridge{Bridge: "vmbr1", Node: "pve1", CIDR: "10.244.7.1/24", Gateway: "10.96.0.1"})
	inv.NodeAddresses["pve2"] = []netip.Addr{netip.MustParseAddr("172.20.3.4")}
	s := settings("pve1")
	s.PodSubnets, s.ServiceSubnets = []string{"10.244.0.0/16"}, []string{"10.96.0.0/12"}
	f := s.Preflight(inv, false)
	mentions(t, f.Warnings, "pod range 10.244.0.0/16 overlaps the network 10.244.7.0/24 on bridge vmbr1 of node pve1")
	mentions(t, f.Warnings, "service range 10.96.0.0/12 overlaps the gateway 10.96.0.1 of bridge vmbr1")
	if len(f.PodRanges) != 1 || f.PodRanges[0] != "10.244.0.0/16" {
		t.Errorf("ranges = %+v", f)
	}
	s.ServiceSubnets = []string{"172.20.0.0/16"}
	mentions(t, s.Preflight(inv, false).Warnings, "service range 172.20.0.0/16 overlaps the address 172.20.3.4 of node pve2")
}
