package appliance

import (
	"fmt"
	"net"
	"slices"
)

// addresses are the appliance's own addresses, which the workload clusters' firewalls accept: its
// interface and its Headlamp reach their API servers directly.
func (a *Appliance) addresses() ([]string, error) {
	if a.Addresses != nil {
		return a.Addresses()
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing the appliance's network interfaces: %w", err)
	}
	var addrs []net.Addr
	for i := range interfaces {
		if interfaces[i].Flags&net.FlagUp == 0 || interfaces[i].Flags&net.FlagLoopback != 0 {
			continue
		}
		own, err := interfaces[i].Addrs()
		if err != nil {
			return nil, fmt.Errorf("reading the addresses of %s: %w", interfaces[i].Name, err)
		}
		addrs = append(addrs, own...)
	}
	return sourceAddresses(addrs), nil
}

// sourceAddresses are the addresses among addrs that traffic to another network leaves from: no
// loopback or link-local ones. IPv4 come first, and each comes once.
func sourceAddresses(addrs []net.Addr) []string {
	// The module takes no null for a list.
	v4, v6 := []string{}, []string{}
	for _, addr := range addrs {
		n, ok := addr.(*net.IPNet)
		if !ok || !n.IP.IsGlobalUnicast() {
			continue
		}
		if ip := n.IP.To4(); ip != nil {
			if !slices.Contains(v4, ip.String()) {
				v4 = append(v4, ip.String())
			}
		} else if !slices.Contains(v6, n.IP.String()) {
			v6 = append(v6, n.IP.String())
		}
	}
	return append(v4, v6...)
}
