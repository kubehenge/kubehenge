package appliance

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func v4Nodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("10.0.0.%d/24", 11+i)
	}
	return out
}

func v6Nodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("fd00:1::%d/64", 11+i)
	}
	return out
}

// nodeCountSettings are valid settings of each address family set, with n node addresses in each.
func nodeCountSettings(n int) map[string]*Settings {
	v4 := settings("pve1")
	v4.NodeAddresses = v4Nodes(n)
	v6 := ipv6Settings()
	v6.NodeAddressesIPv6 = v6Nodes(n)
	dual := dualSettings("IPv4", "IPv6")
	dual.NodeAddresses, dual.NodeAddressesIPv6 = v4Nodes(n), v6Nodes(n)
	return map[string]*Settings{"IPv4": v4, "IPv6": v6, "dual-stack": dual}
}

func TestNodeCountsOneThreeAndFiveAreAccepted(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 3, 5} {
		for name, s := range nodeCountSettings(n) {
			if err := s.Validate(); err != nil {
				t.Errorf("%s with %d nodes: %v", name, n, err)
			}
			if got := s.NodeCount(); got != n {
				t.Errorf("%s with %d nodes: NodeCount = %d", name, n, got)
			}
		}
	}
}

func TestNodeCountsOtherThanOneThreeAndFiveAreRefused(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 2, 4, 6} {
		for name, s := range nodeCountSettings(n) {
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), "one, three or five") {
				t.Errorf("%s with %d nodes: %v, want a message naming one, three or five", name, n, err)
			}
		}
	}
}

func TestDualStackFamiliesNeedTheSameCountAtFive(t *testing.T) {
	t.Parallel()
	s := nodeCountSettings(5)["dual-stack"]
	s.NodeAddressesIPv6 = v6Nodes(3)
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "IPv6 node addresses: as many as the IPv4 ones") {
		t.Errorf("five IPv4 and three IPv6: %v", err)
	}
}

func TestAnExistingManagementClusterGrowsAfterItsNodes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ was, now int }{{1, 3}, {1, 5}, {3, 5}, {5, 5}} {
		for name, now := range nodeCountSettings(c.now) {
			if err := now.KeepsFixed(nodeCountSettings(c.was)[name]); err != nil {
				t.Errorf("%s from %d to %d: %v", name, c.was, c.now, err)
			}
		}
	}
}

func TestAnExistingManagementClusterDoesNotShrink(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ was, now int }{{3, 1}, {5, 3}, {5, 1}} {
		for name, now := range nodeCountSettings(c.now) {
			err := now.KeepsFixed(nodeCountSettings(c.was)[name])
			if err == nil || !strings.Contains(err.Error(), "the nodes there are keep theirs") {
				t.Errorf("%s from %d to %d: %v", name, c.was, c.now, err)
			}
		}
	}
}

func TestPreflightAllowsFiveNodesOnFewerProxmoxNodes(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddresses = v4Nodes(5)
	f := s.Preflight(healthy(), false)
	if len(f.Errors) != 0 {
		t.Errorf("errors = %v", f.Errors)
	}
	mentions(t, f.Warnings, "5 management VMs share 1 Proxmox VE node")
	inv := healthy()
	inv.Storages[0].Avail = 100 * gib
	mentions(t, s.Preflight(inv, false).Errors, "for 160.0 GiB of disks")
}

func TestPreflightReservesFiveVMIDs(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.NodeAddresses = v4Nodes(5)
	inv := healthy()
	inv.NextID = pve.NextID{Lower: 1000, Upper: 1004}
	inv.UsedVMIDs = map[int]bool{1000: true}
	mentions(t, s.Preflight(inv, false).Errors, "3 IDs free", "needs 5")
	inv.NextID = pve.NextID{Lower: 1000, Upper: 1010}
	inv.UsedVMIDs = nil
	if f := s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("errors = %v", f.Errors)
	}
}

func TestVMIDRangeSizesFiveNodes(t *testing.T) {
	t.Parallel()
	a := &Appliance{NextID: func(context.Context, *pve.Ticket) (pve.NextID, error) {
		return pve.NextID{Lower: 1000, Upper: 1060}, nil
	}}
	five := settings("pve1")
	five.NodeAddresses = v4Nodes(5)
	if got, err := a.vmIDRange(context.Background(), nil, five); err != nil || got != (VMIDRange{1000, 1060}) {
		t.Errorf("five nodes: %+v, %v, want the whole pool", got, err)
	}
	three := settings("pve1")
	three.NodeAddresses = v4Nodes(3)
	if got, _ := a.vmIDRange(context.Background(), nil, three); got != (VMIDRange{1030, 1060}) {
		t.Errorf("three nodes: %+v, want the upper half", got)
	}
}
