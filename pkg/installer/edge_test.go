package installer

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	if err == nil || !strings.Contains(err.Error(), "OpenTofu's download") || !strings.Contains(err.Error(), "TMPDIR") {
		t.Fatalf("want the shortage named with TMPDIR, got %v", err)
	}
}

// install.sh has already checked for 1 GiB and unpacked the release (about 282 MiB) in the same
// TMPDIR, so a directory with 1 GiB free before that still has room for what the installer adds.
func TestWorkDirectoryCheckFollowsInstallSh(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	left := (1024 - 282) * 1024
	h.exec.on("df -Pk "+h.work, fmt.Sprintf("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 8000000 7000000 %d 90%% /\n", left))
	r := h.runner(h.session(Config{}), nil)
	if err := r.stepTofu(context.Background()); err != nil {
		t.Fatalf("a TMPDIR that passed install.sh's check was refused: %v", err)
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
	h := newHarness(t, opts{dhcp: true, rootStorages: 2})
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Enter answers the storage question and confirms the summary. Later presses fall on the run screen.
	keys, typed := io.Pipe()
	go func() {
		for ctx.Err() == nil {
			if _, err := typed.Write([]byte("\r")); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	run, stop := context.WithCancel(ctx)
	go func() {
		for h.exec.count("pct start 100") == 0 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		stop()
		_ = typed.Close()
	}()
	cfg := h.session(Config{IP: "dhcp", PVEEntry: "no"}).Cfg
	// The signal reaches the program as a cancelled context, as the one main makes for SIGTERM.
	env := h.env()
	tune := env.Tune
	env.Tune = func(r *Runner) {
		tune(r)
		r.Sleep = func(time.Duration) { time.Sleep(20 * time.Millisecond) }
	}
	_ = executeOn(run, env, &cfg, ioStreams{in: keys, out: io.Discard, errs: io.Discard}, true, "xterm")
	if h.exec.count("pct destroy 100") != 1 {
		t.Fatalf("the half-made container stayed: %v", h.exec.calls)
	}
}

// On a dumb terminal the installer prints lines, though its questions would otherwise open the
// full-screen guide. A terminal emulator and the Linux console get the guide.
func TestDumbTerminalGetsPlainOutput(t *testing.T) {
	t.Parallel()
	for term, want := range map[string]bool{"xterm": true, "screen": true, "linux": true, "dumb": false, "": false} {
		if CanDrawScreen(term) != want {
			t.Errorf("TERM=%q: full screen %v, want %v", term, !want, want)
		}
	}
	h := newHarness(t, opts{rootStorages: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := h.session(Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"}).Cfg
	var out strings.Builder
	for _, term := range []string{"dumb", ""} {
		out.Reset()
		if err := executeOn(ctx, h.env(), &cfg, ioStreams{in: strings.NewReader(""), out: &out, errs: io.Discard}, true, term); err != nil {
			t.Fatalf("TERM=%q: %v", term, err)
		}
		if !strings.Contains(out.String(), "ok   Create container") || strings.Contains(out.String(), "\x1b") {
			t.Fatalf("TERM=%q gave no plain lines: %q", term, out.String())
		}
	}
}

// pressEnter types Enter every 50 ms until the context ends or until stop says so. The writer types
// more keys.
func pressEnter(ctx context.Context, stop func() bool) (*io.PipeReader, *io.PipeWriter) {
	keys, typed := io.Pipe()
	go func() {
		for ctx.Err() == nil && !stop() {
			if _, err := typed.Write([]byte("\r")); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = typed.Close()
	}()
	return keys, typed
}

// syncBuffer is a screen the program writes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Execute takes the terminal from TERM: lines on a dumb one, the full-screen guide on xterm.
func TestExecuteReadsTERM(t *testing.T) { //nolint:paralleltest // sets TERM and stands in for the machine
	saved := newEnv
	t.Cleanup(func() { newEnv = saved })
	for term, guide := range map[string]bool{"dumb": false, "xterm": true} {
		h := newHarness(t, opts{rootStorages: 2})
		newEnv = func(context.Context) (*Env, error) { return h.env(), nil }
		t.Setenv("TERM", term)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		keys, _ := pressEnter(ctx, func() bool { return false })
		cfg := h.session(Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"}).Cfg
		var out syncBuffer
		err := Execute(ctx, &cfg, keys, &out, true)
		cancel()
		if err != nil {
			t.Fatalf("TERM=%s: %v", term, err)
		}
		screen := out.String()
		if strings.Contains(screen, "\x1b") != guide || strings.Contains(screen, "ok   Create container") == guide || h.exec.count("pct create") != 1 {
			t.Errorf("TERM=%s: full screen %v, want %v: %q", term, !guide, guide, screen)
		}
	}
}

func (h *harness) env() *Env {
	fetch := NewFetcher("")
	fetch.Sleep = func(time.Duration) {}
	return &Env{
		Node: h.node, Exec: h.exec, Fetch: fetch, Dir: h.dir, Work: h.work, LogPath: "/var/log/kubehenge-install.log",
		TofuV: "1.0.0", TofuSH: h.tofuSum, Cleanup: func() {},
		Tune: func(r *Runner) {
			r.Sleep = func(time.Duration) { time.Sleep(time.Millisecond) }
			r.Now = func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) }
			r.Realms = func(context.Context, string, string, []byte) int { return 200 }
			r.Pin = h.pin
		},
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
	h.exec.on("pvesh get /nodes/pve1/network/vmbr1", `{"iface":"vmbr1","type":"bridge","bridge_vlan_aware":1}`)
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

// A tag on a bridge without VLAN filtering makes Proxmox VE create vmbr1v20 and take over the
// port's .20, which would cut the node's own vmbr1.20 network. The installer refuses, and a bridge
// and VLAN named by hand stay the administrator's call.
func TestVLANRouteNeedsAVLANAwareBridge(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		pvesh, interfaces string
		refused           bool
	}{
		"aware by Proxmox VE":     {`{"iface":"vmbr1","bridge_vlan_aware":1}`, "", false},
		"unaware by Proxmox VE":   {`{"iface":"vmbr1","type":"bridge"}`, "", true},
		"aware in the file":       {"", "iface vmbr0 inet manual\n\tbridge-ports eno2\niface vmbr1 inet manual\n\tbridge-vlan-aware yes\n\nauto vmbr2\n", false},
		"aware only in the other": {"", "iface vmbr0 inet manual\n\tbridge-vlan-aware yes\niface vmbr1 inet manual\n\tbridge-ports eno2\n", true},
		"nothing says":            {"", "", true},
	} {
		h := newHarness(t, opts{})
		h.exec.on("pvesh get /nodes/pve1/network", `[{"type":"bridge","iface":"vmbr0"},{"type":"bridge","iface":"vmbr1"},{"type":"vlan","iface":"vmbr1.20"}]`)
		h.exec.on("ip -4 route show default", "default via 198.51.100.1 dev vmbr1.20\n")
		if c.pvesh != "" {
			h.exec.on("pvesh get /nodes/pve1/network/vmbr1", c.pvesh)
		}
		if c.interfaces != "" {
			writeTestFile(t, filepath.Join(h.root, interfaces), []byte(c.interfaces))
		}
		err := h.session(Config{}).Prepare(context.Background())
		if c.refused != (err != nil) {
			t.Errorf("%s: refused=%v, got %v", name, c.refused, err)
		}
		if err != nil && (!strings.Contains(err.Error(), "vmbr1 is not VLAN aware") || !strings.Contains(err.Error(), "Make vmbr1 VLAN aware") || !strings.Contains(err.Error(), "--bridge and --vlan")) {
			t.Errorf("%s: the refusal lacks the cause or a fix: %v", name, err)
		}
	}
	// The same bridge and VLAN named by hand cut the same network. Another VLAN does not.
	for vlan, refused := range map[string]bool{"20": true, "30": false} {
		h := newHarness(t, opts{})
		h.exec.on("pvesh get /nodes/pve1/network", `[{"type":"bridge","iface":"vmbr1"},{"type":"vlan","iface":"vmbr1.20"}]`)
		h.exec.on("ip -4 route show default", "default via 198.51.100.1 dev vmbr1.20\n")
		err := h.session(Config{Bridge: "vmbr1", VLAN: vlan}).Prepare(context.Background())
		if refused != (err != nil) || err != nil && !strings.Contains(err.Error(), "vmbr1 is not VLAN aware") {
			t.Errorf("--bridge vmbr1 --vlan %s: refused=%v, got %v", vlan, refused, err)
		}
	}
}

// An Open vSwitch bridge carries tags on its ports, and an SDN VNet says itself whether it is VLAN
// aware. Both are read from Proxmox VE, else from the interfaces files, sourced ones included.
func TestVLANRouteOnOpenVSwitchAndSDN(t *testing.T) {
	t.Parallel()
	sdn := "auto vn1\niface vn1\n\tbridge_ports vmbr0.7\n\tbridge-vlan-aware yes\n"
	for name, c := range map[string]struct {
		bridge, kind, pvesh, vnet, sourced string
		refused                            bool
	}{
		"Open vSwitch by Proxmox VE": {"vmbr0", "OVSBridge", `{"iface":"vmbr0","type":"OVSBridge"}`, "", "", false},
		"Open vSwitch in the file":   {"vmbr0", "OVSBridge", "", "", "iface vmbr0 inet manual\n\tovs_type OVSBridge\n", false},
		"aware VNet":                 {"vn1", "", "", `{"vnet":"vn1","zone":"z1","vlanaware":1}`, "", false},
		"unaware VNet":               {"vn1", "", "", `{"vnet":"vn1","zone":"z1"}`, "", true},
		"aware VNet in sdn":          {"vn1", "", "", "", sdn, false},
	} {
		h := newHarness(t, opts{})
		network := `[{"type":"eth","iface":"eno1"}]`
		if c.kind != "" {
			network = `[{"type":"` + c.kind + `","iface":"` + c.bridge + `"}]`
		}
		h.exec.on("pvesh get /nodes/pve1/network", network)
		h.exec.on("pvesh get /cluster/sdn/vnets", `[{"vnet":"vn1","zone":"z1"}]`)
		h.exec.fail("pvesh get /nodes/pve1/network/", 2)
		h.exec.fail("pvesh get /cluster/sdn/vnets/", 2)
		if c.pvesh != "" {
			h.exec.on("pvesh get /nodes/pve1/network/"+c.bridge, c.pvesh)
		}
		if c.vnet != "" {
			h.exec.on("pvesh get /cluster/sdn/vnets/"+c.bridge, c.vnet)
		}
		h.exec.on("ip -4 route show default", "default via 198.51.100.1 dev "+c.bridge+".7\n")
		writeTestFile(t, filepath.Join(h.root, interfaces), []byte("auto lo\niface lo inet loopback\n\nsource /etc/network/interfaces.d/*\n"))
		if c.sourced != "" {
			writeTestFile(t, filepath.Join(h.root, "etc", "network", "interfaces.d", "sdn"), []byte(c.sourced))
		}
		s := h.session(Config{})
		err := s.Prepare(context.Background())
		if c.refused != (err != nil) {
			t.Errorf("%s: refused=%v, got %v", name, c.refused, err)
		}
		if err == nil && (s.Cfg.Bridge != c.bridge || s.Cfg.VLAN != "7") {
			t.Errorf("%s: bridge %q VLAN %q", name, s.Cfg.Bridge, s.Cfg.VLAN)
		}
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

// With every template storage short of room, the one with the most free is picked, and the notice
// does not call it the best.
func TestShortTemplateStoragesGoByFreeSpace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvesm status --content vztmpl", "Name Type Status Total Used Available %\naaa dir active 1000 900 1000 90%\nmmm dir active 1000 900 90000 90%\nzzz nfs active 1000 900 5000 90%\n")
	s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	if err := s.FillStorages(PickBest); err != nil {
		t.Fatal(err)
	}
	if s.Cfg.TemplateStorage != "mmm" {
		t.Fatalf("template storage %q, want the one with most free", s.Cfg.TemplateStorage)
	}
	notices := strings.Join(s.Notices, "\n")
	if strings.Contains(notices, "the best of") || !strings.Contains(notices, "the most free of 3") || !strings.Contains(notices, "short of room") {
		t.Fatalf("notice %q", notices)
	}

	h = newHarness(t, opts{})
	h.exec.on("pvesm status --content vztmpl", "Name Type Status Total Used Available %\nlocal dir active 1000 900 90000 90%\n")
	s = prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	if err := s.FillStorages(PickBest); err != nil {
		t.Fatal(err)
	}
	notices = strings.Join(s.Notices, "\n")
	if !strings.Contains(notices, "storage local (dir) for the container template, the only one here that takes it, though under") {
		t.Fatalf("one short storage: notice %q", notices)
	}
}

// apt keeps its own retries and gets a 20 second timeout, on the update and on the install.
func TestAptOptions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	r := h.runner(h.session(Config{CTID: "100"}), nil)
	if err := r.stepUpdates(context.Background()); err != nil {
		t.Fatal(err)
	}
	call := h.exec.last("pct exec 100 -- sh -c apt-get")
	if strings.Contains(call, "Retries") || strings.Count(call, "Acquire::http::Timeout=20") != 2 || strings.Count(call, "Acquire::https::Timeout=20") != 2 {
		t.Fatalf("apt got %q", call)
	}
}

// A failed run's own cleanup and the shutdown after a signal can run at once. The container is
// removed once.
func TestCleanupRunsOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	r := h.runner(h.session(Config{CTID: "100"}), nil)
	r.done = map[string]bool{"create": true}
	r.created = true
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { r.Cleanup(context.Background()) })
	}
	wg.Wait()
	for _, c := range []string{"pct unlock 100", "pct stop 100", "pct destroy 100"} {
		if h.exec.count(c) != 1 {
			t.Errorf("%s ran %d times", c, h.exec.count(c))
		}
	}
}

// A create that fails because another session took the id leaves that session's guest alone, a
// second installer on the same node included. Proxmox VE refuses such a create before it makes
// anything, with one of these two messages.
func TestFailedCreateLeavesTheOtherGuest(t *testing.T) {
	t.Parallel()
	for _, refusal := range []string{
		"unable to create CT 100 - CT 100 already exists on node 'pve1'\n",
		"CT 100 already exists on node 'pve2'\n",
		"unable to create CT 100 - can't lock file '/run/lock/lxc/pve-config-100.lock' - got timeout\n",
	} {
		h := newHarness(t, opts{})
		h.exec.failWith("pct create", refusal)
		s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
		r := h.runner(s, nil)
		_, err := r.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "taken by another guest") {
			t.Fatalf("%q: got %v", refusal, err)
		}
		r.Cleanup(context.Background())
		if n := h.exec.count("pct destroy") + h.exec.count("pct stop") + h.exec.count("pct unlock") + h.exec.count("pct config"); n != 0 {
			t.Fatalf("%q: the other guest was touched: %v", refusal, h.exec.calls)
		}
	}

	// A cancel that lands while the create waits on the other installer's lock changes nothing.
	h := newHarness(t, opts{})
	h.exec.stall("pct create", "unable to create CT 100 - can't lock file '/run/lock/lxc/pve-config-100.lock' - got timeout\n")
	s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	r := h.runner(s, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for h.exec.count("pct create") == 0 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, _ = r.Run(ctx)
	cancel()
	r.Cleanup(context.Background())
	if n := h.exec.count("pct destroy") + h.exec.count("pct stop") + h.exec.count("pct unlock"); n != 0 {
		t.Fatalf("a cancelled create touched the other installer's guest: %v", h.exec.calls)
	}
}

// Any other failed create got past the existence check, so what it left is the installer's own.
// A cancel during the template's extraction is the common case: Proxmox VE writes the tags only
// after it.
func TestCancelledCreateRemovesItsContainer(t *testing.T) {
	t.Parallel()
	for name, failure := range map[string]func(h *harness){
		"cancel during extraction": func(h *harness) { h.exec.stall("pct create") },
		"failed extraction": func(h *harness) {
			h.exec.failWith("pct create", "unable to create CT 100 - command 'lxc-usernsexec ... tar xpf - --zstd' failed: exit code 2\n")
		},
	} {
		h := newHarness(t, opts{})
		failure(h)
		s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
		var out strings.Builder
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for h.exec.count("pct create") == 0 && ctx.Err() == nil {
				time.Sleep(time.Millisecond)
			}
			cancel()
		}()
		err := RunPlain(ctx, s, func(func(Event)) *Runner { return h.runner(s, nil) }, &out)
		cancel()
		if err == nil || h.exec.count("pct destroy 100") != 1 || h.exec.count("pct config") != 0 {
			t.Errorf("%s: err %v, calls %v", name, err, h.exec.calls)
		}
		if !strings.Contains(out.String(), "removing container 100") {
			t.Errorf("%s: the removal was not said: %q", name, out.String())
		}
	}
}

// A stalled pct, such as on a hung cluster file system, holds neither the cancelled run nor its
// cleanup past the cleanup's deadline.
func TestStalledPctEndsAtTheDeadline(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.stall("pct create")
	h.exec.stall("pct config")
	h.exec.stall("pct unlock")
	h.exec.stall("pct stop")
	h.exec.stall("pct destroy")
	s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	r := h.runner(s, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Run(ctx)
		clean, stop := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
		defer stop()
		r.Cleanup(clean)
	}()
	for h.exec.count("pct create") == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the run and its cleanup still wait: %v", h.exec.calls)
	}
	if h.exec.count("pct destroy 100") != 1 {
		t.Fatalf("the cleanup did not try to remove the container: %v", h.exec.calls)
	}
}

// A cancel during the protection step leaves a finished appliance. The message says so and how to
// protect or remove it, and nothing destroys it.
func TestUnprotectedApplianceIsNamed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail("pct set 100 --protection 1", 1)
	s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	r := h.runner(s, nil)
	_, err := r.Run(context.Background())
	for _, want := range []string{"container 100", "pct set 100 --protection 1", "pct destroy 100 --purge"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in %v", want, err)
		}
	}
	if left := r.Left(); !strings.Contains(left, "pct set 100 --protection 1") {
		t.Fatalf("left %q", left)
	}
	r.Cleanup(context.Background())
	if h.exec.count("pct destroy") != 0 {
		t.Fatal("a finished appliance was destroyed")
	}
	m := NewModel(s, nil)
	if m.Left() != "" {
		t.Fatal("a model with no run left something")
	}
	m.runner = r
	if !strings.Contains(m.Left(), "container 100") {
		t.Fatalf("model left %q", m.Left())
	}
}

// An upgrade to another API address pushes the API's CA beside its places for the swap, and that
// CA vouches for the certificate the API serves.
func TestUpgradePushesTheAPICA(t *testing.T) {
	t.Parallel()
	h, r, out := upgradeHarness(t)
	r.S.Cfg.ProxmoxURL = "https://pve1.example.com:9006"
	if _, err := r.Upgrade(context.Background(), out); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, dest := range []string{"/etc/kubehenge/proxmox-ca.pem.new", "/usr/local/share/ca-certificates/proxmox-ca.crt.new"} {
		h.exec.mu.Lock()
		pem, pushed := h.exec.pushed[dest]
		h.exec.mu.Unlock()
		pool := x509.NewCertPool()
		if !pushed || !pool.AppendCertsFromPEM(pem) {
			t.Errorf("%s: pushed %v, no certificate in %q", dest, pushed, pem)
			continue
		}
		if _, err := h.chain[0].Verify(x509.VerifyOptions{Roots: pool, DNSName: "pve1.example.com"}); err != nil {
			t.Errorf("%s: the pushed CA does not vouch for the API: %v", dest, err)
		}
	}
}

// Once the appliance is protected, nothing calls it unprotected or offers to destroy it: not the
// runner, not Ctrl+C on the end screen and not a signal there.
func TestFinishedApplianceIsLeftAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	r := h.runner(s, nil)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := r.Left(); left != "" {
		t.Fatalf("a protected appliance left %q", left)
	}

	for _, how := range []string{"ctrl+c", "signal"} {
		h := newHarness(t, opts{dhcp: true, rootStorages: 2})
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		run, signal := context.WithCancel(ctx)
		started := func() bool { return h.exec.count("pct create") > 0 }
		keys, typed := pressEnter(ctx, started)
		screen := &syncBuffer{}
		var errs strings.Builder
		ended := make(chan error, 1)
		cfg := h.session(Config{IP: "dhcp", PVEEntry: "no"}).Cfg
		streams := ioStreams{in: keys, out: screen, errs: &errs, size: []tea.ProgramOption{tea.WithWindowSize(100, 40)}}
		go func() { ended <- executeOn(run, h.env(), &cfg, streams, true, "xterm") }()
		// The renderer moves the cursor over blanks, so the words show without their spaces.
		for !strings.Contains(strings.ReplaceAll(ansi.Strip(screen.String()), " ", ""), "anykeycloses") && ctx.Err() == nil {
			time.Sleep(10 * time.Millisecond)
		}
		if ctx.Err() != nil {
			t.Fatalf("%s: the end screen never showed: %q", how, ansi.Strip(screen.String()))
		}
		if how == "signal" {
			signal()
		} else {
			_, _ = typed.Write([]byte{0x03})
		}
		err := <-ended
		signal()
		cancel()
		if err != nil || errs.Len() != 0 || h.exec.count("pct destroy") != 0 || h.exec.count("pct set 100 --protection 1") != 1 {
			t.Errorf("%s on the end screen: err %v, said %q, calls %v", how, err, errs.String(), h.exec.calls)
		}
	}
}

// Ctrl+C on the failure screen keeps the failure and says what was undone and what stays.
func TestCtrlCOnFailureKeepsTheFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true, rootStorages: 2})
	h.exec.fail("pct start 100", 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	keys, typed := pressEnter(ctx, func() bool { return h.exec.count("pct create") > 0 })
	screen := &syncBuffer{}
	var errs strings.Builder
	cfg := h.session(Config{IP: "dhcp", PVEEntry: "no"}).Cfg
	streams := ioStreams{in: keys, out: screen, errs: &errs, size: []tea.ProgramOption{tea.WithWindowSize(100, 40)}}
	ended := make(chan error, 1)
	go func() { ended <- executeOn(ctx, h.env(), &cfg, streams, true, "xterm") }()
	for !strings.Contains(strings.ReplaceAll(ansi.Strip(screen.String()), " ", ""), "Container100wasremoved") && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatalf("the failure screen never said what was undone: %q", ansi.Strip(screen.String()))
	}
	_, _ = typed.Write([]byte{0x03})
	err := <-ended
	if err == nil || !strings.Contains(err.Error(), "did not start") || !strings.Contains(err.Error(), "Container 100 was removed.") ||
		!strings.Contains(err.Error(), "template stays on storage local-lvm") || errs.Len() != 0 {
		t.Fatalf("Ctrl+C on the failure screen: err %v, said %q", err, errs.String())
	}
}

// An upgraded appliance that cannot be protected gets one warning with pct's own message and the
// one fix, and is never offered for removal, since it holds the management cluster's state.
func TestUpgradeProtectionWarning(t *testing.T) {
	t.Parallel()
	h, r, out := upgradeHarness(t)
	h.exec.failWith("pct set 100 --protection 1", "CT 100 is locked (backup)\n")
	res, err := r.Upgrade(context.Background(), out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	warning := strings.Join(res.Warnings, "\n")
	if !strings.Contains(warning, "CT 100 is locked (backup)") || strings.Count(warning, "Protect it with") != 1 ||
		strings.Contains(warning, "destroy") || strings.Contains(warning, "the appliance is installed") {
		t.Fatalf("warning %q", warning)
	}
}

// One line per step says it removes the container only when there is one of the run's own to
// remove.
func TestPlainSaysWhatItRemoves(t *testing.T) {
	t.Parallel()
	for call, removes := range map[string]bool{"pct set 100 --protection 1": false, "pct start 100": true, "pct create": false} {
		h := newHarness(t, opts{})
		h.exec.fail(call, 1)
		if call == "pct create" {
			h.exec.failWith(call, "CT 100 already exists on node 'pve1'\n")
		}
		out, err := plainOn(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
		said := strings.Contains(out, "removing container 100")
		if err == nil || said != removes || (h.exec.count("pct destroy 100") == 1) != removes {
			t.Errorf("%s failing: said %v, destroyed %d, want %v: %q", call, said, h.exec.count("pct destroy 100"), removes, out)
		}
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
