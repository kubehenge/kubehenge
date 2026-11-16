package appliance

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/luthermonson/go-proxmox"
)

// clusterVM is a VM as /cluster/resources lists it.
type clusterVM struct {
	VMID int    `json:"vmid"`
	Name string `json:"name"`
	Node string `json:"node"`
	Type string `json:"type"`
	// Status is "running", "stopped", or "unknown" for a VM on a node that is down.
	Status string `json:"status"`
}

func (v clusterVM) String() string {
	return fmt.Sprintf("VM %d (%s) on %s", v.VMID, v.Name, v.Node)
}

func (v clusterVM) path() string {
	return fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(v.Node), v.VMID)
}

// vmIndex holds the qemu VMs a client may see by their SMBIOS UUID, in lower case. A management
// node is its VM's by that UUID alone, as the CSI plugin finds it in capmox mode: the nodes carry no
// provider ID, and a VM's name may be a leftover's or changed by hand. A live node is no stopped
// VM, so a stopped copy restored with the node's UUID is never taken for it. A VM on a node that is
// down has status unknown and counts as unreadable.
type vmIndex struct {
	byUUID map[string][]clusterVM
	// unreadable are the VMs whose configuration could not be read. Proxmox VE reads it on the VM's
	// own node, so these are mostly VMs on a node that is down.
	unreadable []clusterVM
}

func readVMIndex(ctx context.Context, c *proxmox.Client) (*vmIndex, error) {
	var vms []clusterVM
	if err := c.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return nil, fmt.Errorf("listing the VMs: %w", err)
	}
	index := &vmIndex{byUUID: make(map[string][]clusterVM, len(vms))}
	for _, v := range vms {
		if v.Type != "qemu" || v.Status == "stopped" {
			continue
		}
		var config struct {
			SMBIOS1 string `json:"smbios1"`
		}
		if err := c.Get(ctx, v.path()+"/config", &config); err != nil {
			index.unreadable = append(index.unreadable, v)
			continue
		}
		if uuid := strings.ToLower(smbiosUUID(config.SMBIOS1)); uuid != "" {
			index.byUUID[uuid] = append(index.byUUID[uuid], v)
		}
	}
	return index, nil
}

// find is the VM with SMBIOS UUID uuid. ok is false when no readable VM has it. Two VMs with one
// UUID are an error, since either could be the node's.
func (x *vmIndex) find(uuid string) (clusterVM, bool, error) {
	vms := x.byUUID[strings.ToLower(uuid)]
	switch len(vms) {
	case 0:
		return clusterVM{}, false, nil
	case 1:
		return vms[0], true, nil
	}
	names := make([]string, len(vms))
	for i, v := range vms {
		names[i] = v.String()
	}
	return clusterVM{}, false, fmt.Errorf("%s share SMBIOS UUID %s. Give each copy its own: qm set <id> --smbios1 uuid=<new>",
		strings.Join(names, " and "), uuid)
}

// missing says that no VM has SMBIOS UUID uuid, and names the unreadable VMs, one of which may be
// the node's.
func (x *vmIndex) missing(uuid string) error {
	if len(x.unreadable) == 0 {
		return fmt.Errorf("no VM has SMBIOS UUID %s", uuid)
	}
	names := make([]string, len(x.unreadable))
	for i, v := range x.unreadable {
		names[i] = v.String()
	}
	return fmt.Errorf("no readable VM has SMBIOS UUID %s. The configuration of %s could not be read, so its Proxmox VE node may be down",
		uuid, strings.Join(names, ", "))
}

// smbiosUUID reads the uuid of a VM's smbios1 setting.
func smbiosUUID(smbios1 string) string {
	for option := range strings.SplitSeq(smbios1, ",") {
		if uuid, ok := strings.CutPrefix(option, "uuid="); ok {
			return uuid
		}
	}
	return ""
}
