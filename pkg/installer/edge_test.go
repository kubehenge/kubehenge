package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
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
	_ = executeOn(run, env, &cfg, ioStreams{keys, io.Discard}, true, "xterm")
	if h.exec.count("pct destroy 100") != 1 {
		t.Fatalf("the half-made container stayed: %v", h.exec.calls)
	}
}

// On a dumb terminal the installer prints lines, though its questions would otherwise open the
// full-screen guide.
func TestDumbTerminalGetsPlainOutput(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{rootStorages: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := h.session(Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"}).Cfg
	var out strings.Builder
	for _, term := range []string{"dumb", ""} {
		out.Reset()
		if err := executeOn(ctx, h.env(), &cfg, ioStreams{strings.NewReader(""), &out}, true, term); err != nil {
			t.Fatalf("TERM=%q: %v", term, err)
		}
		if !strings.Contains(out.String(), "ok   Create container") || strings.Contains(out.String(), "\x1b") {
			t.Fatalf("TERM=%q gave no plain lines: %q", term, out.String())
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
	h := newHarness(t, opts{})
	h.exec.on("pvesh get /nodes/pve1/network", `[{"type":"bridge","iface":"vmbr1"},{"type":"vlan","iface":"vmbr1.20"}]`)
	h.exec.on("ip -4 route show default", "default via 198.51.100.1 dev vmbr1.20\n")
	if err := h.session(Config{Bridge: "vmbr1", VLAN: "20"}).Prepare(context.Background()); err != nil {
		t.Errorf("a bridge and VLAN named by hand were refused: %v", err)
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
	if strings.Contains(notices, "the best of") || !strings.Contains(notices, "short of room") {
		t.Fatalf("notice %q", notices)
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Cleanup(context.Background())
		}()
	}
	wg.Wait()
	for _, c := range []string{"pct unlock 100", "pct stop 100", "pct destroy 100"} {
		if h.exec.count(c) != 1 {
			t.Errorf("%s ran %d times", c, h.exec.count(c))
		}
	}
}

// A create that fails because another session took the id leaves that session's guest alone.
func TestFailedCreateLeavesTheOtherGuest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail("pct create", 255)
	s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	r := h.runner(s, nil)
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("a failed create went on")
	}
	r.Cleanup(context.Background())
	if n := h.exec.count("pct destroy") + h.exec.count("pct stop") + h.exec.count("pct unlock"); n != 0 {
		t.Fatalf("the other guest was touched: %v", h.exec.calls)
	}

	// A create cut off by a cancel leaves the installer's own locked container, found by its tag.
	for conf, want := range map[string]int{"tags: kubehenge\nlock: create\n": 1, "hostname: other\n": 0} {
		h = newHarness(t, opts{})
		h.exec.fail("pct create", 255)
		h.exec.on("pct config 100", conf)
		s = prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
		r = h.runner(s, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := r.stepCreate(ctx); err == nil {
			t.Fatal("a failed create succeeded")
		}
		r.Cleanup(context.Background())
		if got := h.exec.count("pct destroy 100"); got != want {
			t.Errorf("config %q: destroyed %d times, want %d", conf, got, want)
		}
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

// An upgrade to another API address pushes the API's CA beside its places for the swap.
func TestUpgradePushesTheAPICA(t *testing.T) {
	t.Parallel()
	h, r, out := upgradeHarness(t)
	r.S.Cfg.ProxmoxURL = "https://pve1.example.com:9006"
	if _, err := r.Upgrade(context.Background(), out); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, dest := range []string{"/etc/kubehenge/proxmox-ca.pem.new", "/usr/local/share/ca-certificates/proxmox-ca.crt.new"} {
		if !strings.Contains(strings.Join(h.exec.calls, "\n"), "pct push 100 "+filepath.Join(h.work, filepath.Base(strings.TrimSuffix(dest, ".new")))+" "+dest) {
			t.Errorf("no push to %s: %v", dest, h.exec.calls)
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
