package installer

import (
	"context"
	"errors"
	"net/netip"
	"strings"
)

// AddrState is what the check of an address found.
type AddrState int

// The results of an address check.
const (
	// AddrUnchecked is no answer either way: the check could not ask the network.
	AddrUnchecked AddrState = iota
	AddrFree
	AddrInUse
)

// AddrCheck is the result of asking a network whether an address is taken.
type AddrCheck struct {
	State AddrState
	// Detail says how the state was reached, or why there is none.
	Detail string
}

// Taken reports whether something on the network holds the address.
func (c AddrCheck) Taken() bool { return c.State == AddrInUse }

// Text is the check as one short phrase for the summary.
func (c AddrCheck) Text() string {
	switch c.State {
	case AddrFree:
		return "free, " + c.Detail
	case AddrInUse:
		return "in use, " + c.Detail
	case AddrUnchecked:
	}
	return "not checked, " + c.Detail
}

// errNoProbe is the check on a system that cannot send ARP.
var errNoProbe = errors.New("this system cannot send ARP probes")

// CheckAddress asks the network behind dev whether the address is taken: the node's neighbour table,
// then an ARP probe the installer sends itself. An unanswered probe counts as free only where the
// node is known to hear that network, which is where it holds an address of its own, or where the
// gateway answers a probe the same way. Any other no-answer is "not checked".
func (n *Node) CheckAddress(ctx context.Context, dev, address, witness string, holds bool) AddrCheck {
	if dev == "" {
		return AddrCheck{AddrUnchecked, "this node has no interface on the network to ask"}
	}
	if out, err := n.run(ctx, "ip", "neigh", "show", address, "dev", dev); err == nil {
		for _, state := range []string{"REACHABLE", "STALE", "DELAY", "PERMANENT", "PROBE"} {
			if strings.Contains(out, state) {
				return AddrCheck{AddrInUse, "this node's neighbour table lists it"}
			}
		}
	}
	target, err := netip.ParseAddr(address)
	if err != nil || n.Probe == nil {
		return AddrCheck{AddrUnchecked, errNoProbe.Error()}
	}
	answered, perr := n.Probe(ctx, dev, target)
	if perr != nil {
		return AddrCheck{AddrUnchecked, perr.Error()}
	}
	if answered {
		return AddrCheck{AddrInUse, "it answered an ARP probe on " + dev}
	}
	if holds {
		return AddrCheck{AddrFree, "no answer to an ARP probe on " + dev}
	}
	if witness == "" {
		return AddrCheck{AddrUnchecked, "no gateway known to test that this node hears " + dev}
	}
	w, err := netip.ParseAddr(witness)
	if err != nil {
		return AddrCheck{AddrUnchecked, "no gateway to test the probe against"}
	}
	if heard, werr := n.Probe(ctx, dev, w); werr != nil || !heard {
		return AddrCheck{AddrUnchecked, "the gateway " + witness + " does not answer a probe on " + dev + " either, so this node may not hear that network"}
	}
	return AddrCheck{AddrFree, "no answer to an ARP probe on " + dev + ", the gateway answers one"}
}
