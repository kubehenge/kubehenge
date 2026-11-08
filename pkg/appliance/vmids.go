package appliance

import (
	"context"
	"errors"
	"fmt"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// The range the management VMs' random IDs come from when the datacenter sets no next-id.
const (
	DefaultVMIDStart = 10000
	DefaultVMIDEnd   = 99999
	// minVMIDsPerNode is the fewest IDs the upper half of a next-id band must hold for each
	// management node, or the whole band is used.
	minVMIDsPerNode = 10
	bandHalves      = 2
)

// VMIDRange is where the module's Proxmox VE provider draws the management VMs' IDs at random.
// Start is inclusive and End exclusive, as the provider draws rand.Intn(End-Start)+Start.
type VMIDRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// ChooseVMIDRange picks the range for nodes management VMs from the datacenter's next-id pool.
// Proxmox VE hands out the lowest free ID of that pool, so the upper half keeps the random IDs away
// from what anything else is offered at the same moment. A pool whose upper half is too small is
// used whole. Where next-id sets neither bound, the default range applies, and a bound left out
// takes Proxmox VE's own (100, and 1000000 exclusive).
func ChooseVMIDRange(next pve.NextID, nodes int) VMIDRange {
	if next.Lower == 0 && next.Upper == 0 {
		return VMIDRange{DefaultVMIDStart, DefaultVMIDEnd}
	}
	lower, upper := next.Lower, next.Upper
	if lower == 0 {
		lower = pve.DefaultNextIDLower
	}
	if upper == 0 {
		upper = pve.DefaultNextIDUpper
	}
	if upper <= lower {
		return VMIDRange{DefaultVMIDStart, DefaultVMIDEnd}
	}
	half := lower + (upper-lower)/bandHalves
	if upper-half < minVMIDsPerNode*nodes {
		return VMIDRange{lower, upper}
	}
	return VMIDRange{half, upper}
}

// Free counts the IDs of the range that no VM in used holds.
func (r VMIDRange) Free(used map[int]bool) int {
	free := r.End - r.Start
	for id := range used {
		if id >= r.Start && id < r.End {
			free--
		}
	}
	return free
}

// vmIDRange reads the datacenter's next-id with the ticket and chooses the range. A user without
// the privilege to read it, or an appliance that cannot ask, gets the default.
func (a *Appliance) vmIDRange(ctx context.Context, t *pve.Ticket, nodes int) (VMIDRange, error) {
	if a.NextID == nil {
		return ChooseVMIDRange(pve.NextID{}, nodes), nil
	}
	next, err := a.NextID(ctx, t)
	if err != nil && !errors.Is(err, pve.ErrForbidden) {
		return VMIDRange{}, fmt.Errorf("reading the VM ID range under Datacenter, Options: %w", err)
	}
	return ChooseVMIDRange(next, nodes), nil
}
