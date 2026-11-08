package appliance

import (
	"context"
	"net"
	"net/netip"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// APIRoute is how traffic reaches the Proxmox VE API from here.
type APIRoute struct {
	// Endpoints are the addresses the endpoint names, or its host name resolves to. Either family may
	// be used for a name that resolves to both.
	Endpoints []netip.Addr
	// Sources are the addresses the appliance sends from towards each endpoint.
	Sources []netip.Addr
}

// RouteToAPI finds the endpoint's addresses and the source address the kernel picks towards each.
// A UDP dial sends nothing. It only has the kernel choose the route. What cannot be found is left
// out.
func RouteToAPI(ctx context.Context, endpoint string) APIRoute {
	endpoints, port := pve.EndpointAddresses(ctx, endpoint)
	var route APIRoute
	var dialer net.Dialer
	for _, ip := range endpoints {
		conn, derr := dialer.DialContext(ctx, "udp", net.JoinHostPort(ip.String(), port))
		if derr != nil {
			continue
		}
		local, ok := conn.LocalAddr().(*net.UDPAddr)
		_ = conn.Close()
		if !ok {
			continue
		}
		if src, ok := netip.AddrFromSlice(local.IP); ok {
			route.Endpoints = append(route.Endpoints, ip)
			route.Sources = append(route.Sources, src.Unmap())
		}
	}
	return route
}
