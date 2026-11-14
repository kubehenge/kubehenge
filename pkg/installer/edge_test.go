package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A container that gets no DHCP address names the VLAN as the first thing to check.
func TestNoAddressNamesTheVLAN(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		cfg  Config
		call string
	}{
		{"IPv4", Config{IP: "dhcp", PVEEntry: "no"}, "pct exec 100 -- ip -4 -o addr show dev eth0"},
		{"IPv6", Config{IP: "none", IP6: "auto", PVEEntry: "no"}, "pct exec 100 -- ip -6 -o addr show dev eth0"},
	} {
		h := newHarness(t, opts{dhcp: true})
		h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "")
		h.exec.on(c.call, "")
		_, err := plainOn(t, h, c.cfg)
		if err == nil || !strings.Contains(err.Error(), "--vlan is the right one") {
			t.Errorf("%s: the error names no VLAN check: %v", c.name, err)
		}
	}
}

// The downloads need room in the work directory, checked before the first one.
func TestWorkDirectoryNeedsRoom(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("df -Pk "+h.work, "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 8000000 7900000 100000 99% /\n")
	r := h.runner(h.session(Config{}), nil)
	err := r.stepTofu(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the downloaded archives") || !strings.Contains(err.Error(), "TMPDIR") {
		t.Fatalf("want the shortage named with TMPDIR, got %v", err)
	}
}

// A cancelled run stops waiting for the container's address at once instead of after the whole
// two minutes.
func TestCancelStopsTheAddressWait(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "")
	s := prepared(t, h, Config{IP: "dhcp", PVEEntry: "no"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeps := 0
	r := h.runner(s, nil)
	r.Sleep = func(time.Duration) {
		sleeps++
		cancel()
	}
	err := r.stepAddress(ctx)
	if !errors.Is(err, context.Canceled) || sleeps != 1 {
		t.Fatalf("want a cancelled wait after 1 sleep, got %v after %d", err, sleeps)
	}
}

// A program ended by a signal or a lost terminal still removes the container it was making.
func TestShutdownRemovesTheHalfMadeContainer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "")
	s := prepared(t, h, Config{IP: "dhcp", PVEEntry: "no"})
	m := NewModel(s, func(emit func(Event)) *Runner {
		r := h.runner(s, emit)
		r.Sleep = func(time.Duration) { time.Sleep(time.Millisecond) }
		return r
	})
	m.startRun()
	for h.exec.count("pct start 100") == 0 {
		time.Sleep(time.Millisecond)
	}
	m.Shutdown(context.Background())
	if h.exec.count("pct destroy 100") != 1 {
		t.Fatalf("the half-made container stayed: %v", h.exec.calls)
	}
}

func TestCanDrawScreen(t *testing.T) {
	t.Parallel()
	for term, want := range map[string]bool{"xterm": true, "linux": true, "dumb": false, "": false} {
		if got := CanDrawScreen(term); got != want {
			t.Errorf("TERM=%q: %v", term, got)
		}
	}
}

// A node whose management network is a VLAN device on a bridge (vmbr1.20) puts the container on
// that bridge and VLAN, with the address suggestion taken from the device.
func TestDefaultRouteOverAVLANDevice(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvesh get /nodes/pve1/network", `[{"type":"bridge","iface":"vmbr0"},{"type":"bridge","iface":"vmbr1"},{"type":"vlan","iface":"vmbr1.20"}]`)
	h.exec.on("ip -4 route show default", "default via 198.51.100.1 dev vmbr1.20\n")
	h.exec.on("ip -4 -o addr show dev vmbr1.20", "7: vmbr1.20    inet 198.51.100.10/24 scope global vmbr1.20\n")
	s := h.session(Config{})
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if s.Cfg.Bridge != "vmbr1" || s.Cfg.VLAN != "20" {
		t.Fatalf("container on %q VLAN %q", s.Cfg.Bridge, s.Cfg.VLAN)
	}
	if !s.Facts.Prefix.IsValid() || s.Facts.Prefix.Addr().String() != "198.51.100.10" {
		t.Fatalf("prefix %v", s.Facts.Prefix)
	}
	if !strings.Contains(strings.Join(s.Notices, "\n"), "VLAN 20 on vmbr1") {
		t.Fatalf("no notice: %v", s.Notices)
	}
	if got := s.Cfg.containerNet(); !strings.Contains(got, "bridge=vmbr1,tag=20") {
		t.Fatalf("net0 %q", got)
	}

	named := h.session(Config{Bridge: "vmbr1"})
	if err := named.Prepare(context.Background()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if named.Cfg.VLAN != "" {
		t.Fatalf("a bridge named on purpose got VLAN %q", named.Cfg.VLAN)
	}
}

func TestVLANDeviceNeedsABridgeAndATag(t *testing.T) {
	t.Parallel()
	bridges := []string{"vmbr0", "vn1"}
	for dev, want := range map[string]string{"vmbr0.20": "vmbr0/20", "vmbr0": "/", "eno1.20": "/", "vmbr0.0": "/", "vmbr0.5000": "/", "vmbr0.x": "/", ".20": "/", "vn1.7": "vn1/7"} {
		bridge, tag := vlanOfDevice(bridges, dev)
		if got := bridge + "/" + tag; got != want {
			t.Errorf("%s: %s, want %s", dev, got, want)
		}
	}
}

// With no bridge, Open vSwitch bridge or SDN VNet there is nothing to attach the container to.
func TestNoBridgeRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvesh get /nodes/pve1/network", `[{"type":"eth","iface":"eno1"}]`)
	err := h.session(Config{}).Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lists no bridge") {
		t.Fatalf("got %v", err)
	}
}

// A template storage without room for the template is not picked while another has room.
func TestTemplateStorageNeedsRoom(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvesm status --content vztmpl", "Name Type Status Total Used Available %\nlocal dir active 1000 900 100000 90%\nshare nfs active 500000000 1000 400000000 1%\n")
	s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	if err := s.FillStorages(PickBest); err != nil {
		t.Fatal(err)
	}
	if s.Cfg.TemplateStorage != "share" {
		t.Fatalf("template storage %q", s.Cfg.TemplateStorage)
	}

	h = newHarness(t, opts{})
	h.exec.on("pvesm status --content vztmpl", "Name Type Status Total Used Available %\nlocal dir active 1000 900 100000 90%\n")
	s = prepared(t, h, Config{TemplateStorage: "local", IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	if err := s.FillStorages(PickBest); err != nil {
		t.Fatalf("a storage named on purpose was refused for its room: %v", err)
	}
}

// An API that serves its leaf alone leaves nothing to trust but that leaf, which a renewal ends.
func TestServedLeafAloneIsPinned(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{untrusted: true})
	s := h.session(Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	if err := s.detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.CA.Pinned {
		t.Fatal("a served CA was called a pinned leaf")
	}

	h = newHarness(t, opts{untrusted: true})
	h.chain = h.chain[:1]
	s = h.session(Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	if err := s.detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.CA.Pinned {
		t.Fatal("a leaf served alone was not marked")
	}
	err := s.UnattendedCheck()
	if err == nil || !strings.Contains(err.Error(), "fail when that is renewed") {
		t.Fatalf("the refusal does not say the pin breaks at renewal: %v", err)
	}
}
