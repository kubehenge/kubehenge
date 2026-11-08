package appliance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func TestChooseVMIDRange(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		next  pve.NextID
		nodes int
		want  VMIDRange
	}{
		"unset keeps the default":                {pve.NextID{}, 3, VMIDRange{10000, 99999}},
		"both bounds take the upper half":        {pve.NextID{Lower: 1000, Upper: 2000}, 3, VMIDRange{1500, 2000}},
		"an odd band rounds the half down":       {pve.NextID{Lower: 1000, Upper: 2001}, 3, VMIDRange{1500, 2001}},
		"lower only runs to Proxmox VE's upper":  {pve.NextID{Lower: 200000}, 3, VMIDRange{600000, 1000000}},
		"upper only starts at Proxmox VE's 100":  {pve.NextID{Upper: 1100}, 3, VMIDRange{600, 1100}},
		"a small band is used whole":             {pve.NextID{Lower: 5000, Upper: 5040}, 3, VMIDRange{5000, 5040}},
		"ten per node keeps the upper half":      {pve.NextID{Lower: 5000, Upper: 5060}, 3, VMIDRange{5030, 5060}},
		"one short of ten per node uses it all":  {pve.NextID{Lower: 5000, Upper: 5058}, 3, VMIDRange{5000, 5058}},
		"more nodes need a bigger half":          {pve.NextID{Lower: 5000, Upper: 5060}, 5, VMIDRange{5000, 5060}},
		"an upside-down band falls back":         {pve.NextID{Lower: 300, Upper: 200}, 3, VMIDRange{10000, 99999}},
		"a lower above Proxmox VE's upper falls": {pve.NextID{Lower: 2000000}, 3, VMIDRange{10000, 99999}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ChooseVMIDRange(c.next, c.nodes); got != c.want {
				t.Errorf("ChooseVMIDRange(%+v, %d) = %+v, want %+v", c.next, c.nodes, got, c.want)
			}
		})
	}
}

func TestVMIDRangeFree(t *testing.T) {
	t.Parallel()
	if got := (VMIDRange{100, 110}).Free(map[int]bool{99: true, 100: true, 105: true, 110: true}); got != 8 {
		t.Errorf("free = %d, want 8: 100 and 105 are in the range, 99 and 110 are not", got)
	}
}

func TestRunReadsTheRangeWithTheTicket(t *testing.T) {
	t.Parallel()
	token := &pve.Ticket{Username: "u@pve!t", Token: "u@pve!t=secret"}
	for name, c := range map[string]struct {
		read    func(context.Context, *pve.Ticket) (pve.NextID, error)
		want    VMIDRange
		wantErr bool
	}{
		"no reader": {nil, VMIDRange{10000, 99999}, false},
		"set": {func(context.Context, *pve.Ticket) (pve.NextID, error) {
			return pve.NextID{Lower: 1000, Upper: 2000}, nil
		}, VMIDRange{1500, 2000}, false},
		"forbidden":     {func(context.Context, *pve.Ticket) (pve.NextID, error) { return pve.NextID{}, pve.ErrForbidden }, VMIDRange{10000, 99999}, false},
		"another error": {func(context.Context, *pve.Ticket) (pve.NextID, error) { return pve.NextID{}, errors.New("down") }, VMIDRange{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &Appliance{NextID: c.read}
			s := settings("pve1")
			s.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
			got, err := a.vmIDRange(context.Background(), token, s)
			if got != c.want || (err != nil) != c.wantErr {
				t.Errorf("vmIDRange = %+v, %v, want %+v (error %v)", got, err, c.want, c.wantErr)
			}
		})
	}
}

// A pool whose upper half has room for one node but not three is used whole, whichever family
// numbers the nodes, so that recreating an IPv6-only management cluster sizes its range as applying
// it does.
func TestVMIDRangeCountsTheNodesOfTheirFamily(t *testing.T) {
	t.Parallel()
	pool := func(context.Context, *pve.Ticket) (pve.NextID, error) {
		return pve.NextID{Lower: 1000, Upper: 1050}, nil
	}
	a := &Appliance{NextID: pool}
	three := []string{"fd00:1::11/64", "fd00:1::12/64", "fd00:1::13/64"}
	ipv6 := ipv6Settings()
	ipv6.NodeAddressesIPv6 = three
	dual := dualSettings("IPv6", "IPv4")
	dual.NodeAddressesIPv6 = three
	dual.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
	for name, s := range map[string]*Settings{"IPv6 only": ipv6, "dual-stack": dual} {
		got, err := a.vmIDRange(context.Background(), nil, s)
		if err != nil || got != (VMIDRange{1000, 1050}) {
			t.Errorf("%s: vmIDRange = %+v, %v, want the whole pool for three nodes", name, got, err)
		}
	}
	one := ipv6Settings()
	if got, _ := a.vmIDRange(context.Background(), nil, one); got != (VMIDRange{1025, 1050}) {
		t.Errorf("one IPv6 node: %+v, want the upper half", got)
	}
}

func TestVMIDRangeReachesTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
		t.Fatal(err)
	}
	if err := a.writeConfig(settings("pve1"), VMIDRange{1500, 2000}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(a.workdir(), "terraform.tfvars.json"))
	if err != nil || !strings.Contains(string(raw), `"vmIdRange": {`) || !strings.Contains(string(raw), `"start": 1500`) || !strings.Contains(string(raw), `"end": 2000`) {
		t.Fatalf("variables: %s, %v", raw, err)
	}
	main, err := os.ReadFile(filepath.Join(a.workdir(), "main.tf"))
	if err != nil || !strings.Contains(string(main), "vm_id_range            = var.settings.vmIdRange") {
		t.Fatalf("root module: %s, %v", main, err)
	}
}

func TestPreflightRefusesAFullVMIDBand(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	inv := healthy()
	inv.NextID = pve.NextID{Lower: 1000, Upper: 1004}
	inv.UsedVMIDs = map[int]bool{1000: true, 1001: true, 1002: true, 1003: true, 5: true}
	f := s.Preflight(inv, false)
	mentions(t, f.Errors, "VM IDs 1000 to 1003", "0 IDs free", "needs 1", "Widen next-id under Datacenter, Options")
	// An existing management cluster keeps its VMs' IDs.
	if f = s.Preflight(inv, true); len(f.Errors) != 0 {
		t.Errorf("an owned cluster was refused: %v", f.Errors)
	}
	// IDs that could not be read leave the check out.
	inv.UsedVMIDs = nil
	if f = s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("unreadable IDs were refused: %v", f.Errors)
	}
}
