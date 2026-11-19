package installer

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

// failureOnScreen runs the installer through executeOn until a failure screen says the container
// is kept, and returns the context's cancel, the screen and the run's end.
func failureOnScreen(t *testing.T, h *harness, cfg Config) (context.CancelFunc, *syncBuffer, chan error) { //nolint:gocritic // tests pass a Config literal.
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	keys, _ := pressEnter(ctx, func() bool { return h.exec.count("pct create") > 0 })
	screen := &syncBuffer{}
	c := h.session(cfg).Cfg
	streams := ioStreams{in: keys, out: screen, errs: &strings.Builder{}, size: []tea.ProgramOption{tea.WithWindowSize(100, 40)}}
	ended := make(chan error, 1)
	go func() { ended <- executeOn(ctx, h.env(), &c, streams, true, "xterm") }()
	for !strings.Contains(strings.ReplaceAll(ansi.Strip(screen.String()), " ", ""), "Container100iskeptuntilyouchoose") {
		if ctx.Err() != nil {
			t.Fatalf("the failure screen never said the container is kept: %q", ansi.Strip(screen.String()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cancel, screen, ended
}

// A signal, as a dropped SSH session sends, on a failure screen that said the container is kept
// keeps it, and the way to inspect or remove it is said.
func TestSignalOnTheFailureScreenKeepsTheContainer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.fail("pct start 100", 1)
	cancel, _, ended := failureOnScreen(t, h, Config{IP: "dhcp", PVEEntry: "no"})
	cancel()
	err := <-ended
	if h.exec.count("pct destroy") != 0 {
		t.Fatalf("the signal removed the container: %v", h.exec.calls)
	}
	if err == nil || !strings.Contains(err.Error(), "pct enter 100") || !strings.Contains(err.Error(), "pct destroy 100 --purge") {
		t.Fatalf("the kept container is not said: %v", err)
	}
}

// Leaving from the question a failure sent the user to keeps the container.
func TestUILeavingAFixKeepsTheContainer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "")
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.toSummary()
	u.enter(1)
	u.waitFor("Change IPv4 address")
	u.enter(1)
	u.waitFor("DHCP")
	u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m := u.finish()
	m.Shutdown(context.Background())
	if h.exec.count("pct destroy") != 0 || !strings.Contains(m.Outcome(), "is kept") {
		t.Fatalf("leaving the fix removed the container: %q %v", m.Outcome(), h.exec.calls)
	}
}

// With --keep-failed, Ctrl+C during the run keeps the container, and the run screen says so.
func TestUIKeepFailedHoldsForCtrlCDuringTheRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.stall("pct exec 100 -- ip -4 -o addr show dev eth0")
	u := startUI(t, h, Config{PVEEntry: "no", KeepFailed: true})
	u.toSummary()
	u.enter(1)
	u.waitFor("ctrl+c stops and keeps the container")
	for h.exec.count("pct exec 100 -- ip -4 -o addr show") == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m := u.finish()
	m.Shutdown(context.Background())
	if h.exec.count("pct destroy") != 0 || !strings.Contains(m.Outcome(), "is kept") {
		t.Fatalf("Ctrl+C with --keep-failed removed the container: %q %v", m.Outcome(), h.exec.calls)
	}
}

// On a terminal, a full set of flags without --yes still shows the summary before anything is made.
func TestTerminalAsksForTheSummaryWithoutYes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	keys, _ := pressEnter(ctx, func() bool { return true })
	screen := &syncBuffer{}
	cfg := h.session(Config{IP: "dhcp", PVEEntry: "no"}).Cfg
	streams := ioStreams{in: keys, out: screen, errs: &strings.Builder{}, size: []tea.ProgramOption{tea.WithWindowSize(100, 40)}}
	ended := make(chan error, 1)
	go func() { ended <- executeOn(ctx, h.env(), &cfg, streams, true, "xterm") }()
	for !strings.Contains(ansi.Strip(screen.String()), "enter continue") && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err := <-ended
	if n := h.exec.count("pct create"); n != 0 || !strings.Contains(ansi.Strip(screen.String()), "enter continue") {
		t.Fatalf("installed with no yes: %d creates (%v), screen %q", n, err, ansi.Strip(screen.String()))
	}
}

// A failure of the API's connection takes any URL typed, and the next try runs in the container
// that is there.
func TestUIAPIFailureTakesATypedURL(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.fail(apiConnect, 1)
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.toSummary()
	u.enter(1)
	u.waitFor("Change Proxmox VE API")
	u.golden("api-failure")
	h.exec.on(apiConnect, "")
	u.enter(1)
	u.waitFor("API CA file")
	u.golden("api-form")
	u.press(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	u.typed("198.51.100.7")
	u.enter(2)
	u.key('n')
	u.waitFor("Kubehenge is running")
	u.enter(1)
	u.finish()
	c := u.s.Cfg
	if c.ProxmoxURL != "https://198.51.100.7:8006" || !c.Insecure {
		t.Errorf("the typed URL was not taken: %q insecure %v", c.ProxmoxURL, c.Insecure)
	}
	if h.exec.count("pct create") != 1 || h.exec.count("pct destroy") != 0 {
		t.Fatalf("the container was made again for a new API: %v", h.exec.calls)
	}
	if n := h.exec.count(apiConnect); n < 2 {
		t.Fatalf("the network check did not run again for the new API: %d connects", n)
	}
}

// A container removed after it was switched IPv4-only is made afresh, so its next start switches
// nothing back on.
func TestRemovedIPv4OnlyContainerStartsAfresh(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	r := h.runner(h.session(Config{CTID: "100", IP: optDHCP}), nil)
	ctx := context.Background()
	if err := r.stepCreate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.stepStart(ctx); err != nil {
		t.Fatal(err)
	}
	r.Cleanup(ctx)
	r.S.Cfg.IP6 = optAuto
	if err := r.stepStart(ctx); err != nil {
		t.Fatal(err)
	}
	if n := h.exec.count("pct exec 100 -- sh -c \nset -e\nrm -f /etc/sysctl.d/60-kubehenge-ipv4-only.conf"); n != 0 {
		t.Errorf("a fresh container had IPv6 switched back on: %v", h.exec.calls)
	}
}

// Fixing the network after a failure asks its VLAN and addresses again too.
func TestFixingTheNetworkAsksItsVLANAndAddressesAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	m := NewModel(prepared(t, h, Config{PVEEntry: "no"}), nil)
	follow := []string{settingVLAN, settingV4Addr, settingV4Gateway, settingV6Addr, settingV6Gateway}
	for _, id := range follow {
		m.answered[id], m.given[id] = true, true
	}
	m.a.action = actFix + settingNetwork
	m.failAction()
	for _, id := range follow {
		if m.answered[id] || m.given[id] {
			t.Errorf("%s is not asked again after the network was changed", id)
		}
	}
}

// A DHCP container without a route is told to give a static address, so its network fix asks
// DHCP or static again.
func TestDHCPWithoutARouteAsksTheAddressModeAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.fail(apiConnect, 1)
	h.exec.on("pct exec 100 -- ip -4 route show default", "")
	r, _ := networkRunner(t, h)
	r.S.Cfg.IP = optDHCP
	fault := networkError(t, r.stepNetwork(context.Background()))
	m := NewModel(prepared(t, h, Config{IP: optDHCP, PVEEntry: "no"}), nil)
	for _, id := range settingOrder {
		m.answered[id], m.given[id] = true, true
	}
	m.fail = fault
	id := m.failSetting()
	if id != settingNetwork || !strings.Contains(fault.Fix, "--ip <address>") {
		t.Fatalf("want the network offered with a static address as the fix, got %q: %q", id, fault.Fix)
	}
	m.a.action = actFix + id
	m.failAction()
	for _, mode := range []string{settingV4Mode, settingV6Mode} {
		if m.answered[mode] || m.given[mode] {
			t.Errorf("%s is not asked again after the network was changed", mode)
		}
	}
}

// A sentence keeps the name Proxmox capitalised where a label starts it.
func TestLowerFirstKeepsProxmox(t *testing.T) {
	t.Parallel()
	if got := lowerFirst("Proxmox VE API"); got != "Proxmox VE API" {
		t.Errorf("got %q", got)
	}
	if got := lowerFirst("Network"); got != "network" {
		t.Errorf("got %q", got)
	}
}

// leaves runs cmd and every command its batches and sequences hold, and returns the messages.
func leaves(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice || !v.Type().ConvertibleTo(cmdList) {
		return []tea.Msg{msg}
	}
	inner, _ := reflect.TypeAssert[[]tea.Cmd](v.Convert(cmdList))
	var out []tea.Msg
	for _, c := range inner {
		out = append(out, leaves(c)...)
	}
	return out
}

// The messages a key sets off in a form carry that form, so one still on its way when the form is
// gone is known.
func TestFormKeysSendTaggedMessages(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	m := NewModel(prepared(t, h, Config{PVEEntry: "no"}), nil)
	var a, b string
	m.startForm(huh.NewGroup(huh.NewInput().Value(&a), huh.NewInput().Value(&b)))
	msgs := leaves(m.updateForm(tea.KeyPressMsg{Code: tea.KeyEnter}))
	if len(msgs) == 0 {
		t.Fatal("Enter set nothing off")
	}
	for _, msg := range msgs {
		if f, ok := msg.(formMsg); !ok || f.form != m.form {
			t.Errorf("a message without its form: %#v", msg)
		}
	}
}

// The failure screen offers a setting only where it exists, and the API question opens with no
// address found.
func TestFailSettingOffersOnlyWhatApplies(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	s := prepared(t, h, Config{PVEEntry: "no"})
	s.Facts.Candidates = nil
	m := NewModel(s, nil)
	m.fail = settingErr(settingV4Gateway, "no route", "")
	if id := m.failSetting(); id != "" {
		t.Errorf("a DHCP install was offered %q", id)
	}
	m.fail = settingErr(settingAPI, "cannot connect", "")
	if id := m.failSetting(); id != settingAPI {
		t.Fatalf("the API is not offered: %q", id)
	}
	if fields := m.table[settingAPI].fields(m); len(fields) != 3 {
		t.Errorf("want the URL, CA file and check, got %d fields", len(fields))
	}
}

// A certificate valid for no name of the cluster offers the API's question, where a URL fixes it.
func TestCertificateForNoClusterNameOffersTheAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	s := prepared(t, h, Config{PVEEntry: "no"})
	s.Facts.Candidates, s.Cfg.APIHost, s.Cfg.ProxmoxURL = nil, "", ""
	err := s.ResolveAPI(context.Background(), PickBest)
	if err == nil || !strings.Contains(err.Error(), "valid for no name") {
		t.Fatalf("want the certificate named, got %v", err)
	}
	m := NewModel(s, nil)
	m.fail = err
	if id := m.failSetting(); id != settingAPI {
		t.Fatalf("the API is not offered: %q", id)
	}
}

// Refusing the CA leads to the API's fields, not the same question again.
func TestUIRefusedCAOffersTheAPIFields(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true, untrusted: true})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("Node")
	u.enter(1)
	u.waitFor("Trust this CA")
	u.key('n')
	u.waitFor("Change Proxmox VE API")
	u.enter(1)
	u.waitFor("API CA file")
	if strings.Contains(u.rec.screen(), "Trust this CA") {
		t.Fatalf("the same question came again:\n%s", u.rec.screen())
	}
}

// A form's message that arrives after the form is gone moves the form open now on to nothing, and
// a form's commands carry which form sent them.
func TestUIStaleFormMessageIsDropped(t *testing.T) {
	t.Parallel()
	gone := huh.NewForm(huh.NewGroup(huh.NewNote()))
	if msg, ok := tagged(gone, huh.NextField)().(formMsg); !ok || msg.form != gone {
		t.Fatalf("the form's command is not tagged with its form: %#v", msg)
	}
	h := newHarness(t, opts{dhcp: true})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.toSummary()
	u.tm.Send(formMsg{form: gone, msg: huh.NextField()})
	u.press(tea.KeyPressMsg{Code: tea.KeyDown})
	if !strings.Contains(u.rec.screen(), "If it fails") || h.exec.count("pct create") != 0 {
		t.Fatalf("a message of a form that is gone submitted the summary:\n%s", u.rec.screen())
	}
	u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	u.finish()
}

// A kept IPv4-only container whose network gains IPv6 gets IPv6 back at its next start.
func TestIPv4OnlyContainerGetsIPv6Back(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	r := h.runner(h.session(Config{CTID: "100", IP: optDHCP}), nil)
	ctx := context.Background()
	if err := r.stepStart(ctx); err != nil {
		t.Fatal(err)
	}
	r.S.Cfg.IP6 = optAuto
	if err := r.stepStart(ctx); err != nil {
		t.Fatal(err)
	}
	if n := h.exec.count("pct exec 100 -- sh -c \nset -e\nrm -f /etc/sysctl.d/60-kubehenge-ipv4-only.conf"); n != 1 {
		t.Errorf("want IPv6 switched on once, got %d: %v", n, h.exec.calls)
	}
}

// Clearing the name servers of a kept container gives it the node's again, rather than leaving the
// ones it was made with.
func TestRenetWithoutDNSRemovesTheOldNameServers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	r := h.runner(h.session(Config{CTID: "100", IP: optDHCP, DNS: "192.0.2.99"}), nil)
	ctx := context.Background()
	if err := r.stepCreate(ctx); err != nil {
		t.Fatal(err)
	}
	r.S.Cfg.DNS = ""
	if err := r.Renet(ctx); err != nil {
		t.Fatal(err)
	}
	if set := h.exec.last("pct set 100 --net0"); !strings.HasSuffix(set, " --delete nameserver") {
		t.Fatalf("the old name servers stay: %q", set)
	}
}

// A network change Proxmox VE refuses leaves the container stopped, so the next try starts it.
func TestRefusedNetworkChangeStartsTheContainerOnTheNextTry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.on("pct exec 100 -- ip -4 route show default", "")
	h.exec.fail(apiConnect, 1)
	s := prepared(t, h, Config{IP: optDHCP, PVEEntry: "no"})
	r := h.runner(s, nil)
	ctx := context.Background()
	if _, err := r.Run(ctx); err == nil {
		t.Fatal("the run without a route passed")
	}
	h.exec.fail("pct set 100 --net0", 255)
	if err := r.Renet(ctx); err == nil {
		t.Fatal("the refused change passed")
	}
	_, _ = r.Run(ctx)
	if n := h.exec.count("pct start 100"); n != 2 {
		t.Fatalf("want the container started again on the next try, got %d starts: %v", n, h.exec.calls)
	}
}
