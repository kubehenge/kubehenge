//go:build !linux

package installer

import (
	"context"
	"net/netip"
)

// ProbeARP is for systems that cannot send ARP: the address is then not checked.
func ProbeARP(context.Context, string, netip.Addr) (bool, error) { return false, errNoProbe }
