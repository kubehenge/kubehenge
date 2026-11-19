//go:build linux

package installer

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

const (
	arpWait    = 1500 * time.Millisecond
	arpResend  = 500 * time.Millisecond
	arpPoll    = 100 * time.Millisecond
	arpMaxRead = 1500
)

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// ProbeARP sends ARP probes for target on the device and reports whether anything answered. It needs
// no arping and no address on the device.
func ProbeARP(ctx context.Context, dev string, target netip.Addr) (bool, error) {
	ifi, err := net.InterfaceByName(dev)
	if err != nil {
		return false, fmt.Errorf("interface %s: %w", dev, err)
	}
	if len(ifi.HardwareAddr) != macLen {
		return false, fmt.Errorf("%s has no hardware address to ask from", dev)
	}
	proto := htons(arpEtherType)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return false, fmt.Errorf("open a packet socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err = unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index}); err != nil {
		return false, fmt.Errorf("bind to %s: %w", dev, err)
	}
	tv := unix.NsecToTimeval(int64(arpPoll))
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return false, fmt.Errorf("set the read timeout: %w", err)
	}
	to := &unix.SockaddrLinklayer{Ifindex: ifi.Index, Halen: macLen, Addr: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	probe := arpProbe(ifi.HardwareAddr, target)
	deadline := time.Now().Add(arpWait)
	var nextSend time.Time
	buf := make([]byte, arpMaxRead)
	for time.Now().Before(deadline) {
		if err = ctx.Err(); err != nil {
			return false, fmt.Errorf("probe cancelled: %w", err)
		}
		if !time.Now().Before(nextSend) {
			if err = unix.Sendto(fd, probe, 0, to); err != nil {
				return false, fmt.Errorf("send on %s: %w", dev, err)
			}
			nextSend = time.Now().Add(arpResend)
		}
		n, _, rerr := unix.Recvfrom(fd, buf, 0)
		if rerr != nil {
			if rerr == unix.EAGAIN || rerr == unix.EINTR {
				continue
			}
			return false, fmt.Errorf("read on %s: %w", dev, rerr)
		}
		if answersProbe(buf[:n], ifi.HardwareAddr, target) {
			return true, nil
		}
	}
	return false, nil
}
