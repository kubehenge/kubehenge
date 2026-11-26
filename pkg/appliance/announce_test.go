package appliance

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	announceMAC = net.HardwareAddr{0xbc, 0x24, 0x11, 0x01, 0x02, 0x03}
	announceV4  = netip.MustParseAddr("10.16.248.5")
	announceV6  = netip.MustParseAddr("2001:db8::5")
)

func TestGratuitousARPFrame(t *testing.T) {
	t.Parallel()
	want := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // broadcast
		0xbc, 0x24, 0x11, 0x01, 0x02, 0x03, // source
		0x08, 0x06, // ARP
		0x00, 0x01, 0x08, 0x00, 6, 4, // Ethernet, IPv4, address lengths
		0x00, 0x01, // request
		0xbc, 0x24, 0x11, 0x01, 0x02, 0x03, 10, 16, 248, 5, // sender
		0, 0, 0, 0, 0, 0, 10, 16, 248, 5, // target: no hardware address, the same IP
	}
	if got := gratuitousARP(announceMAC, announceV4); !bytes.Equal(got, want) {
		t.Errorf("frame = % x, want % x", got, want)
	}
}

func TestUnsolicitedNAPayload(t *testing.T) {
	t.Parallel()
	want := []byte{
		136, 0, 0, 0, // neighbour advertisement, no code, checksum for the kernel
		0x20, 0, 0, 0, // override only: not a router, not solicited
		0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, // target
		2, 1, 0xbc, 0x24, 0x11, 0x01, 0x02, 0x03, // target link-layer address
	}
	if got := unsolicitedNA(announceMAC, announceV6); !bytes.Equal(got, want) {
		t.Errorf("payload = % x, want % x", got, want)
	}
}

type announceRun struct {
	mu     sync.Mutex
	list   []Announcement
	sent   []string
	waits  []time.Duration
	failOn error
}

func (r *announceRun) announcer(log *slog.Logger) *Announcer {
	return &Announcer{
		Log: log,
		List: func() ([]Announcement, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return slices.Clone(r.list), nil
		},
		Send: func(a Announcement) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.failOn != nil {
				return r.failOn
			}
			r.sent = append(r.sent, a.IP.String())
			return nil
		},
		wait: func(_ context.Context, d time.Duration) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.waits = append(r.waits, d)
			return true
		},
	}
}

func (r *announceRun) sentSoFar() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sent)
}

func announcementOf(ip netip.Addr) Announcement {
	return Announcement{Interface: "eth0", Index: 2, MAC: announceMAC, IP: ip}
}

func TestAnnounceOnStartRepeatsEachAddress(t *testing.T) {
	t.Parallel()
	r := &announceRun{list: []Announcement{announcementOf(announceV4), announcementOf(announceV6)}}
	r.announcer(nil).announceNew(t.Context(), map[string]bool{})
	want := []string{"10.16.248.5", "2001:db8::5", "10.16.248.5", "2001:db8::5", "10.16.248.5", "2001:db8::5"}
	if got := r.sentSoFar(); !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
	if want := []time.Duration{time.Second, time.Second}; !slices.Equal(r.waits, want) {
		t.Errorf("waits = %v, want %v", r.waits, want)
	}
}

func TestAnnounceAnAddressAddedWhileRunning(t *testing.T) {
	t.Parallel()
	r := &announceRun{list: []Announcement{announcementOf(announceV4)}}
	changes := make(chan struct{})
	n := r.announcer(nil)
	n.Changes = changes
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	waitUntil(t, func() bool { return len(r.sentSoFar()) == announceRepeat })
	r.mu.Lock()
	r.list = append(r.list, announcementOf(announceV6))
	r.mu.Unlock()
	changes <- struct{}{}
	waitUntil(t, func() bool { return len(r.sentSoFar()) == 2*announceRepeat })
	cancel()
	<-done
	got := r.sentSoFar()
	if v6 := slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s != "2001:db8::5" }); len(v6) != announceRepeat {
		t.Errorf("the added address was announced %d times in %v, want %d", len(v6), got, announceRepeat)
	}
	if v4 := slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s != "10.16.248.5" }); len(v4) != announceRepeat {
		t.Errorf("the first address was announced %d times in %v, want %d", len(v4), got, announceRepeat)
	}
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for range 2000 {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out")
}

// warnings is a log's warning lines, written under a lock since the announcer logs from its own goroutine.
type warnings struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *warnings) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *warnings) lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.DeleteFunc(strings.Split(strings.TrimSpace(w.buf.String()), "\n"), func(s string) bool { return !strings.Contains(s, "level=WARN") })
}

func TestAnnounceWithoutPermissionWarnsOnceAndKeepsRunning(t *testing.T) {
	t.Parallel()
	h := &warnings{}
	r := &announceRun{list: []Announcement{announcementOf(announceV4), announcementOf(announceV6)}, failOn: errors.New("operation not permitted")}
	n := r.announcer(slog.New(slog.NewTextHandler(h, nil)))
	changes := make(chan struct{})
	n.Changes = changes
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	changes <- struct{}{}
	changes <- struct{}{}
	cancel()
	<-done
	got := h.lines()
	if len(got) != 1 {
		t.Fatalf("warnings = %q, want exactly one", got)
	}
	if !strings.Contains(got[0], "could not announce") {
		t.Errorf("warning = %q", got[0])
	}
}

func TestAnnouncementsOfSkipLoopbackAndLinkLocal(t *testing.T) {
	t.Parallel()
	cidr := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	addrs := []net.Addr{cidr("127.0.0.1/8"), cidr("::1/128"), cidr("169.254.1.1/16"), cidr("fe80::1/64"), cidr("10.16.248.5/24"), cidr("2001:db8::5/64")}
	up := &net.Interface{Index: 2, Name: "eth0", HardwareAddr: announceMAC, Flags: net.FlagUp}
	var got []string
	for _, a := range announcementsOf(up, addrs) {
		got = append(got, a.IP.String())
	}
	if want := []string{"10.16.248.5", "2001:db8::5"}; !slices.Equal(got, want) {
		t.Errorf("announced %v, want %v", got, want)
	}
	for name, ifi := range map[string]*net.Interface{
		"loopback":    {Name: "lo", Flags: net.FlagUp | net.FlagLoopback, HardwareAddr: announceMAC},
		"down":        {Name: "eth1", Flags: 0, HardwareAddr: announceMAC},
		"no hardware": {Name: "tun0", Flags: net.FlagUp},
	} {
		if got := announcementsOf(ifi, addrs); len(got) != 0 {
			t.Errorf("%s: announced %v", name, got)
		}
	}
	if got := announcementsOf(&net.Interface{Name: "eth2", Flags: net.FlagUp, HardwareAddr: announceMAC}, []net.Addr{cidr("fe80::1/64"), cidr("127.0.0.1/8")}); len(got) != 0 {
		t.Errorf("link-local only: announced %v", got)
	}
}

func TestServiceUnitKeepsTheRawSocketsAnnouncementsNeed(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "appliance", "kubehenge.service"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string][]string{}
	for line := range strings.Lines(string(raw)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
			values[k] = append(values[k], strings.Fields(v)...)
		}
	}
	for _, k := range []string{"AmbientCapabilities", "CapabilityBoundingSet"} {
		if !slices.Contains(values[k], "CAP_NET_RAW") {
			t.Errorf("%s = %q, without CAP_NET_RAW", k, values[k])
		}
	}
	if !slices.Contains(values["RestrictAddressFamilies"], "AF_PACKET") {
		t.Errorf("RestrictAddressFamilies = %q, without AF_PACKET", values["RestrictAddressFamilies"])
	}
}
