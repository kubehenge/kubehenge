package ui

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// probePorts are ports hosts on a server network commonly listen on: SSH, HTTP, HTTPS, and the
// Kubernetes, Proxmox VE and Talos APIs. A host that takes or refuses a connection on one, or
// answers ping, is there. One that ignores them all looks free, to the controller too.
var probePorts = []string{"22", "80", "443", "6443", "8006", "50000"}

const (
	probeWait  = time.Second
	probeDials = 256
	// The protocol numbers of ICMP for IPv4 and of ICMPv6.
	icmpProtocol   = 1
	icmp6Protocol  = 58
	echoSize       = 1500
	dialNetworkV4  = "tcp4"
	dialNetworkV6  = "tcp6"
	listenAnyIPv4  = "0.0.0.0"
	listenAnyIPv6  = "::"
	echoSocketIPv4 = "udp4"
	echoSocketIPv6 = "udp6"
)

// dialNetwork is the network to dial an address on: its own family, so a host is never reached
// over the other one.
func dialNetwork(a netip.Addr) string {
	if a.Is4() {
		return dialNetworkV4
	}
	return dialNetworkV6
}

// probeAddresses tells which of the addresses a host answers on within probeWait.
func probeAddresses(ctx context.Context, addrs []netip.Addr, log *slog.Logger) map[netip.Addr]bool {
	ctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		answering = map[netip.Addr]bool{}
	)
	mark := func(a netip.Addr) {
		mu.Lock()
		answering[a] = true
		mu.Unlock()
	}
	wg.Go(func() {
		for _, a := range echoReplies(ctx, addrs, log) {
			mark(a)
		}
	})
	slots := make(chan struct{}, probeDials)
	var d net.Dialer
	for _, a := range addrs {
		for _, port := range probePorts {
			wg.Go(func() {
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-slots }()
				conn, err := d.DialContext(ctx, dialNetwork(a), net.JoinHostPort(a.String(), port))
				if err == nil {
					_ = conn.Close()
				}
				if answered(err) {
					mark(a)
				}
			})
		}
	}
	wg.Wait()
	return answering
}

// answered tells whether a connection attempt reached a host: it connected, or the host refused it.
func answered(err error) bool {
	return err == nil || errors.Is(err, syscall.ECONNREFUSED)
}

// echoReplies sends each address an ICMP echo request and returns those that reply before ctx
// ends. It needs ICMP sockets without privileges (net.ipv4.ping_group_range), which the chart
// gives the interface's pod. Where none opens, it finds no one.
func echoReplies(ctx context.Context, addrs []netip.Addr, log *slog.Logger) []netip.Addr {
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		if a.Is4() {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	return append(echoFamily(ctx, v4, ipv4Echo, log), echoFamily(ctx, v6, ipv6Echo, log)...)
}

// echo is what differs between ping over IPv4 and over IPv6: the socket, the ICMP protocol number
// and the message types.
type echo struct {
	socket, listen string
	protocol       int
	request, reply icmp.Type
}

var (
	ipv4Echo = echo{echoSocketIPv4, listenAnyIPv4, icmpProtocol, ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply}
	ipv6Echo = echo{echoSocketIPv6, listenAnyIPv6, icmp6Protocol, ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply}
)

// echoFamily pings addresses of one family over its own unprivileged socket.
func echoFamily(ctx context.Context, addrs []netip.Addr, e echo, log *slog.Logger) []netip.Addr {
	if len(addrs) == 0 {
		return nil
	}
	conn, err := icmp.ListenPacket(e.socket, e.listen)
	if err != nil {
		log.Debug("probing addresses without ping", "network", e.socket, "error", err)
		return nil
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	wanted := make(map[netip.Addr]bool, len(addrs))
	for i, a := range addrs {
		wanted[a] = true
		msg := icmp.Message{Type: e.request, Body: &icmp.Echo{Seq: i, Data: []byte("kubehenge")}}
		if b, err := msg.Marshal(nil); err == nil {
			_, _ = conn.WriteTo(b, &net.UDPAddr{IP: a.AsSlice()})
		}
	}
	var replied []netip.Addr
	buf := make([]byte, echoSize)
	for len(wanted) > 0 {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		m, err := icmp.ParseMessage(e.protocol, buf[:n])
		udp, ok := from.(*net.UDPAddr)
		if err != nil || !ok || m.Type != e.reply {
			continue
		}
		if a, ok := netip.AddrFromSlice(udp.IP); ok && wanted[a.Unmap()] {
			delete(wanted, a.Unmap())
			replied = append(replied, a.Unmap())
		}
	}
	return replied
}
