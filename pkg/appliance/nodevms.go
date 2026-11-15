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
}

func (v clusterVM) path() string {
	return fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(v.Node), v.VMID)
}

// vmsByUUID lists the qemu VMs c may see by their SMBIOS UUID, in lower case. A management node is
// its VM's by that UUID alone, as the CSI plugin finds it in capmox mode: the nodes carry no
// provider ID, and a VM's name may be a leftover's or changed by hand.
func vmsByUUID(ctx context.Context, c *proxmox.Client) (map[string]clusterVM, error) {
	var vms []clusterVM
	if err := c.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return nil, fmt.Errorf("listing the VMs: %w", err)
	}
	byUUID := make(map[string]clusterVM, len(vms))
	for _, v := range vms {
		if v.Type != "qemu" {
			continue
		}
		var config struct {
			SMBIOS1 string `json:"smbios1"`
		}
		if err := c.Get(ctx, v.path()+"/config", &config); err != nil {
			return nil, fmt.Errorf("reading VM %d's configuration: %w", v.VMID, err)
		}
		if uuid := smbiosUUID(config.SMBIOS1); uuid != "" {
			byUUID[strings.ToLower(uuid)] = v
		}
	}
	return byUUID, nil
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
