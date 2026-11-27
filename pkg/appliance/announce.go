package appliance

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

// An address is announced announceRepeat times, announceEvery apart. The first frame can leave
// before the uplink forwards the container's traffic again, so a later one is what the switch learns
// from. RFC 4861 section 10 caps unsolicited neighbour advertisements at three, and section 7.2.6
// spaces them by the retransmission timer, one second. IPv4 follows the same schedule, which is
// arping -U's one-second spacing. RFC 5227 section 2.3 has a host that starts using an address send
// two ARP announcements two seconds apart, so three a second apart reach the network as early.
const (
	announceRepeat = 3
	announceEvery  = time.Second
)

// Announcement is one address of one interface, and what the frames that announce it carry.
type Announcement struct {
	Interface string
	Index     int
	MAC       net.HardwareAddr
	IP        netip.Addr
}

func (n Announcement) key() string {
	return n.MAC.String() + "/" + n.IP.String() + "/" + n.Interface
}

// Announcer tells the network about the appliance's addresses, as keepalived and NetworkManager do
// when they take one over. A container that restarts keeps its address, but the switches and the
// gateway forget it as the link goes down, and learn it again only from a frame the container sends.
// Nothing sends one until the appliance does, so a client waits for its own request to time out.
type Announcer struct {
	Log *slog.Logger
	// List returns the addresses to announce.
	List func() ([]Announcement, error)
	// Send sends one announcement frame for the address.
	Send func(Announcement) error
	// Changes receives a value whenever an address may have been added. A closed channel ends the watch.
	Changes <-chan struct{}

	wait func(context.Context, time.Duration) bool
	warn sync.Once
}

// errAddressNotReady is a Send failure that is no fault: the address cannot send yet, as an IPv6
// address in duplicate address detection cannot. The kernel reports the address again once it can.
var errAddressNotReady = errors.New("the address cannot send yet")

// AnnounceAddresses announces the appliance's addresses when it starts and whenever one is added,
// until ctx ends. A failure is logged once and never stops the appliance.
func (a *Appliance) AnnounceAddresses(ctx context.Context) {
	if n := systemAnnouncer(ctx, a.Log); n != nil {
		n.Run(ctx)
	}
}

// Run announces every address now and each one added later, until ctx ends.
func (n *Announcer) Run(ctx context.Context) {
	announced := map[string]bool{}
	changes := n.Changes
	for {
		announced = n.announceNew(ctx, announced)
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				// The watcher closes the channel when ctx ends too, which is a stop and no fault.
				if ctx.Err() != nil {
					return
				}
				n.warnOnce("watching for added addresses stopped, so ones added later are not announced", nil)
				changes = nil
			}
		}
	}
}

// announceNew announces the addresses not in announced and returns the set that was announced. An
// address whose first frame failed is left out, so the next change tries it again. One that cannot
// send yet is left out without a warning, since the change that makes it ready comes next.
func (n *Announcer) announceNew(ctx context.Context, announced map[string]bool) map[string]bool {
	all, err := n.List()
	if err != nil {
		n.warnOnce("listing the addresses to announce", err)
		return announced
	}
	now := make(map[string]bool, len(all))
	var fresh []Announcement
	for _, a := range all {
		if announced[a.key()] {
			now[a.key()] = true
		} else {
			fresh = append(fresh, a)
		}
	}
	for i := range announceRepeat {
		if i > 0 && !n.pause(ctx) {
			return now
		}
		fresh = n.sendEach(fresh)
		if i == 0 {
			for _, a := range fresh {
				now[a.key()] = true
			}
		}
	}
	return now
}

// sendEach sends one frame for each address and returns those it went out for.
func (n *Announcer) sendEach(addrs []Announcement) []Announcement {
	var sent []Announcement
	for _, a := range addrs {
		if err := n.Send(a); err != nil {
			if !errors.Is(err, errAddressNotReady) {
				n.warnOnce("announcing "+a.IP.String()+" on "+a.Interface, err)
			}
			continue
		}
		sent = append(sent, a)
	}
	return sent
}

func (n *Announcer) pause(ctx context.Context) bool {
	if n.wait != nil {
		return n.wait(ctx, announceEvery)
	}
	t := time.NewTimer(announceEvery)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (n *Announcer) warnOnce(what string, err error) {
	n.warn.Do(func() {
		if n.Log == nil {
			return
		}
		if err == nil {
			n.Log.Warn(what)
			return
		}
		n.Log.Warn("the appliance could not announce its addresses to the network, so clients may time out until it sends something itself: "+what, "error", err)
	})
}

// announcementsOf is the announcement of each of an interface's addresses that is neither loopback
// nor link-local, nothing for an interface that is down, loopback or has no Ethernet address.
func announcementsOf(ifi *net.Interface, addrs []net.Addr) []Announcement {
	if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 || len(ifi.HardwareAddr) != ethernetLen {
		return nil
	}
	var out []Announcement
	for _, s := range sourceAddresses(addrs) {
		if ip, err := netip.ParseAddr(s); err == nil {
			out = append(out, Announcement{Interface: ifi.Name, Index: ifi.Index, MAC: ifi.HardwareAddr, IP: ip})
		}
	}
	return out
}

// The fields of the frames below.
const (
	ethernetLen   = 6
	arpFrameLen   = 42
	etherTypeARP  = 0x0806
	etherTypeIPv4 = 0x0800
	icmpv6NA      = 136
	naLen         = 32
)

// gratuitousARP is the ARP announcement of RFC 5227 section 2.3 as arping -U and keepalived send it,
// a broadcast request whose sender and target address are both the announced one, so every host
// that holds the address in its cache updates it.
func gratuitousARP(mac net.HardwareAddr, ip netip.Addr) []byte {
	f := make([]byte, arpFrameLen)
	for i := range 6 {
		f[i] = 0xff
	}
	copy(f[6:12], mac)
	binary.BigEndian.PutUint16(f[12:14], etherTypeARP)
	binary.BigEndian.PutUint16(f[14:16], 1) // Ethernet
	binary.BigEndian.PutUint16(f[16:18], etherTypeIPv4)
	f[18], f[19] = ethernetLen, 4
	binary.BigEndian.PutUint16(f[20:22], 1) // request
	copy(f[22:28], mac)
	a := ip.As4()
	copy(f[28:32], a[:])
	// The target hardware address, f[32:38], stays zero.
	copy(f[38:42], a[:])
	return f
}

// unsolicitedNA is the ICMPv6 payload of an unsolicited neighbour advertisement (RFC 4861 section
// 7.2.6) with the override flag and the target link-layer address option, so neighbours replace what
// they cache. The checksum is left zero for the kernel, which fills it in on a raw ICMPv6 socket.
func unsolicitedNA(mac net.HardwareAddr, ip netip.Addr) []byte {
	p := make([]byte, naLen)
	p[0] = icmpv6NA
	p[4] = 0x20 // override
	a := ip.As16()
	copy(p[8:24], a[:])
	p[24], p[25] = 2, 1 // target link-layer address, one 8-byte unit
	copy(p[26:32], mac)
	return p
}
