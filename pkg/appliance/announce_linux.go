//go:build linux

package appliance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// systemAnnouncer sends the frames over raw sockets, which need CAP_NET_RAW, and learns of added
// addresses from the kernel's address notifications. The arp_notify and ndisc_notify settings cannot
// replace them. Both act when a device comes up or changes its hardware address, ndisc_notify also
// when duplicate address detection ends, and a restart of the service alone does none of these. The
// service cannot set them either, with its kernel tunables read-only.
func systemAnnouncer(ctx context.Context, log *slog.Logger) *Announcer {
	n := &Announcer{Log: log, List: announceable, Send: sendAnnouncement}
	changes, err := watchAddresses(ctx)
	if err != nil {
		n.warnOnce("watching for added addresses", err)
	} else {
		n.Changes = changes
	}
	return n
}

// watchAddresses signals on each address the kernel adds or finishes duplicate address detection
// for, until ctx ends.
func watchAddresses(ctx context.Context) (<-chan struct{}, error) {
	conn, err := netlink.Dial(unix.NETLINK_ROUTE, &netlink.Config{Groups: unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR})
	if err != nil {
		return nil, fmt.Errorf("subscribe to address changes: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	changes := make(chan struct{}, 1)
	go func() {
		defer close(changes)
		for {
			msgs, err := conn.Receive()
			if err != nil {
				return
			}
			for _, m := range msgs {
				if m.Header.Type == unix.RTM_NEWADDR {
					select {
					case changes <- struct{}{}:
					default:
					}
					break
				}
			}
		}
	}()
	return changes, nil
}

// announceable lists the addresses of the machine's interfaces to announce.
func announceable() ([]Announcement, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list the network interfaces: %w", err)
	}
	var out []Announcement
	for i := range interfaces {
		ifi := &interfaces[i]
		own, err := ifi.Addrs()
		if err != nil {
			return nil, fmt.Errorf("list the addresses of %s: %w", ifi.Name, err)
		}
		out = append(out, announcementsOf(ifi, own)...)
	}
	return out, nil
}

// sendAnnouncement sends one frame: the ARP announcement on a packet socket for an IPv4 address, the
// advertisement to all nodes (ff02::1) from the address itself for an IPv6 one.
func sendAnnouncement(a Announcement) error {
	if a.IP.Is4() {
		return sendARP(a)
	}
	return sendNA(a)
}

func sendARP(a Announcement) error {
	proto := htons(etherTypeARP)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return fmt.Errorf("open a packet socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	to := &unix.SockaddrLinklayer{Protocol: proto, Ifindex: a.Index, Halen: ethernetLen, Addr: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	if err = unix.Sendto(fd, gratuitousARP(a.MAC, a.IP), 0, to); err != nil {
		return fmt.Errorf("send on %s: %w", a.Interface, err)
	}
	return nil
}

func sendNA(a Announcement) error {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		return fmt.Errorf("open an ICMPv6 socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, ndHopLimit); err != nil {
		return fmt.Errorf("set the hop limit: %w", err)
	}
	if err = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, a.Index); err != nil {
		return fmt.Errorf("choose %s: %w", a.Interface, err)
	}
	if err = unix.Bind(fd, &unix.SockaddrInet6{Addr: a.IP.As16()}); err != nil {
		return senderError(a.IP, err)
	}
	all := netip.MustParseAddr("ff02::1").As16()
	if err = unix.Sendto(fd, unsolicitedNA(a.MAC, a.IP), 0, &unix.SockaddrInet6{Addr: all, ZoneId: uint32(a.Index)}); err != nil { //nolint:gosec // an interface index is small and positive
		return fmt.Errorf("send on %s: %w", a.Interface, err)
	}
	return nil
}

// senderError is why the address cannot be the sender. The kernel refuses a tentative address
// (EADDRNOTAVAIL), so an address in duplicate address detection is not ready yet. When detection
// ends the kernel reports the address again, and the watcher has it announced then.
func senderError(ip netip.Addr, err error) error {
	if errors.Is(err, unix.EADDRNOTAVAIL) {
		return fmt.Errorf("use %s as the sender: %w: %w", ip, errAddressNotReady, err)
	}
	return fmt.Errorf("use %s as the sender: %w", ip, err)
}

// ndHopLimit is the hop limit of neighbour discovery messages, which hosts check to know they came
// from the link.
const ndHopLimit = 255

func htons(v uint16) uint16 { return v<<8 | v>>8 }
