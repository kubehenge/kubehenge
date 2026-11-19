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
func (c AddrCheck) Text() string { return c.Outcome() + ", " + c.Detail }

// Outcome is the check's state in words alone: free, in use or not checked.
func (c AddrCheck) Outcome() string {
	switch c.State {
	case AddrFree:
		return "free"
	case AddrInUse:
		return "in use"
	case AddrUnchecked:
	}
	return "not checked"
}

// errNoProbe is the check on a system that cannot send ARP.
var errNoProbe = errors.New("this system cannot send ARP probes")

// Link is the network an address is checked on.
type Link struct {
	// Dev is the node's interface on the network.
	Dev string
	// Witness is the gateway, probed to learn whether the node hears the network.
	Witness string
	// NodeHolds is whether the node has an address of its own there, so it hears the network.
	NodeHolds bool
}

// CheckAddress asks the network behind l.Dev whether the address is taken: the node's neighbour
// table, then an ARP probe the installer sends itself. An unanswered probe counts as free only where
// the node is known to hear that network, which is where it holds an address of its own, or where the
// gateway answers a probe the same way. Any other no-answer is "not checked".
func (n *Node) CheckAddress(ctx context.Context, address string, l Link) AddrCheck {
	dev, witness := l.Dev, l.Witness
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
	if l.NodeHolds {
		return AddrCheck{AddrFree, "no answer to an ARP probe on " + dev}
	}
	if witness == "" {
		return AddrCheck{AddrUnchecked, "no gateway to test the probe against"}
	}
	w, err := netip.ParseAddr(witness)
	if err != nil {
		return AddrCheck{AddrUnchecked, "no gateway to test the probe against"}
	}
	if heard, werr := n.Probe(ctx, dev, w); werr != nil || !heard {
		return AddrCheck{AddrUnchecked, "the gateway " + witness + " does not answer a probe either"}
	}
	return AddrCheck{AddrFree, "no answer to an ARP probe on " + dev + ", while the gateway answers one"}
}
