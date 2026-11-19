//go:build linux

package installer

import (
	"bytes"
	"context"
	"encoding/binary"
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

// The fields of an Ethernet frame that carries an ARP message.
const (
	arpFrameLen   = 42
	arpEtherType  = 0x0806
	etherTypeIPv4 = 0x0800
	arpHardware   = 1
	arpOpRequest  = 1
	arpOpReply    = 2
	ethHeaderLen  = 14
	arpSenderIP   = 28
	arpOpOffset   = 20
	arpSenderMAC  = 22
	arpTargetIP   = 38
	ipv4Len       = 4
	macLen        = 6
	arpPayloadLen = 28
)

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// arpProbe builds the ARP probe of RFC 5227: a request for target from the unspecified address,
// which a host holding the target answers and which makes no host learn this one.
func arpProbe(mac net.HardwareAddr, target netip.Addr) []byte {
	f := make([]byte, arpFrameLen)
	copy(f[0:macLen], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(f[macLen:2*macLen], mac)
	binary.BigEndian.PutUint16(f[12:14], arpEtherType)
	binary.BigEndian.PutUint16(f[14:16], arpHardware)
	binary.BigEndian.PutUint16(f[16:18], etherTypeIPv4)
	f[18], f[19] = macLen, ipv4Len
	binary.BigEndian.PutUint16(f[arpOpOffset:arpOpOffset+2], arpOpRequest)
	copy(f[arpSenderMAC:arpSenderMAC+macLen], mac)
	t := target.As4()
	copy(f[arpTargetIP:arpTargetIP+ipv4Len], t[:])
	return f
}

// answersProbe reports whether a frame is an ARP reply from target, or another host's probe for it.
func answersProbe(frame []byte, mac net.HardwareAddr, target netip.Addr) bool {
	if len(frame) < ethHeaderLen+arpPayloadLen || binary.BigEndian.Uint16(frame[12:14]) != arpEtherType {
		return false
	}
	t := target.As4()
	op := binary.BigEndian.Uint16(frame[arpOpOffset : arpOpOffset+2])
	sender := frame[arpSenderIP : arpSenderIP+ipv4Len]
	wants := frame[arpTargetIP : arpTargetIP+ipv4Len]
	switch op {
	case arpOpReply:
		return bytes.Equal(sender, t[:])
	case arpOpRequest:
		// Another host probing for the same address from the unspecified one, not this host's own frame.
		return bytes.Equal(wants, t[:]) && !bytes.Equal(frame[arpSenderMAC:arpSenderMAC+macLen], mac) && bytes.Equal(sender, []byte{0, 0, 0, 0})
	}
	return false
}

// arpSocket opens a packet socket on the device that reads in short slices.
func arpSocket(ifi *net.Interface) (int, error) {
	proto := htons(arpEtherType)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return -1, fmt.Errorf("open a packet socket: %w", err)
	}
	if err = unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind to %s: %w", ifi.Name, err)
	}
	tv := unix.NsecToTimeval(int64(arpPoll))
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("set the read timeout: %w", err)
	}
	return fd, nil
}

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
	fd, err := arpSocket(ifi)
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(fd) }()
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
