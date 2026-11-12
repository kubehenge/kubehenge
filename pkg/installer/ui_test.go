package installer

import (
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"
)

var (
	spinnerGlyphs = regexp.MustCompile(`[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏⠁⠂⠄⡀⢀⠠⠐⠈]`)
	fingerprintRE = regexp.MustCompile(`([0-9A-F]{2}:){15}[0-9A-F]{2}(:[0-9A-F]{2}){0,16}`)
	trailingSpace = regexp.MustCompile(`(?m)[ \t]+$`)
)

// sanitise strips what changes between runs: styles, spinner frames and the certificate's fingerprint.
func sanitise(s string) string {
	s = ansi.Strip(s)
	s = spinnerGlyphs.ReplaceAllString(s, "*")
	s = fingerprintRE.ReplaceAllString(s, "<fingerprint>")
	return trailingSpace.ReplaceAllString(s, "")
}

// recorder wraps the installer and keeps the last screen it drew for every stage.
type recorder struct {
	inner *Model
	mu    sync.Mutex
	views map[stage]string
	last  string
	// keys counts the key presses the installer has handled, and lastUpdate is when it handled any
	// message last.
	keys       int
	lastUpdate time.Time
}

func (r *recorder) Init() tea.Cmd { return r.inner.Init() }

func (r *recorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn // tea.Model's method returns the interface.
	_, cmd := r.inner.Update(msg)
	screen := sanitise(r.inner.render())
	r.mu.Lock()
	r.last, r.views[r.inner.stage] = screen, screen
	r.lastUpdate = time.Now()
	if _, ok := msg.(tea.KeyPressMsg); ok {
		r.keys++
	}
	r.mu.Unlock()
	return r, cmd
}

func (r *recorder) View() tea.View { return r.inner.View() }

func (r *recorder) keysHandled() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keys
}

// quietFor is how long ago the installer handled a message last.
func (r *recorder) quietFor() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastUpdate)
}

func (r *recorder) screen() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func (r *recorder) viewOf(s stage) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.views[s]
}

// ui is a running installer under test.
type ui struct {
	t   *testing.T
	h   *harness
	s   *Session
	rec *recorder
	tm  *teatest.TestModel
}

func startUI(t *testing.T, h *harness, cfg Config) *ui { //nolint:gocritic // tests pass a Config literal.
	t.Helper()
	s := h.session(cfg)
	model := NewModel(s, func(emit func(Event)) *Runner { return h.runner(s, emit) })
	rec := &recorder{inner: model, views: make(map[stage]string)}
	tm := teatest.NewTestModel(t, rec, teatest.WithInitialTermSize(100, 40))
	return &ui{t: t, h: h, s: s, rec: rec, tm: tm}
}

func (u *ui) waitFor(want string) {
	u.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(u.rec.screen(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	u.t.Fatalf("waited for %q, the screen is:\n%s", want, u.rec.screen())
}

// quietPeriod is how long the installer must have handled no message for the screen to count as
// settled: the messages one key sets off (a form's next field or group, a probe's answer) all
// arrive within it.
const quietPeriod = 50 * time.Millisecond

// settle waits until the installer has handled no message for quietPeriod.
func (u *ui) settle() {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && u.rec.quietFor() < quietPeriod {
		time.Sleep(5 * time.Millisecond)
	}
}

// press sends a key once the screen has settled, and returns once the installer handled it and
// what it set off.
func (u *ui) press(msg tea.KeyPressMsg) {
	u.t.Helper()
	u.settle()
	want := u.rec.keysHandled() + 1
	u.tm.Send(msg)
	deadline := time.Now().Add(15 * time.Second)
	for u.rec.keysHandled() < want {
		if time.Now().After(deadline) {
			u.t.Fatalf("the installer did not handle the key %q, the screen is:\n%s", msg.String(), u.rec.screen())
		}
		time.Sleep(5 * time.Millisecond)
	}
	u.settle()
}

func (u *ui) enter(n int) {
	u.t.Helper()
	for range n {
		u.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	}
}

func (u *ui) down(n int) {
	u.t.Helper()
	for range n {
		u.press(tea.KeyPressMsg{Code: tea.KeyDown})
	}
}

func (u *ui) key(r rune) {
	u.t.Helper()
	u.press(tea.KeyPressMsg{Code: r, Text: string(r)})
}

// golden compares the last screen of a stage once it has settled and, where the stage holds a form,
// shows the form's focus bar: the first frame of a form comes before its field takes focus.
func (u *ui) golden(name string, s stage) {
	u.t.Helper()
	u.settle()
	if s != stEnd && s != stFail && s != stRun {
		u.waitFor("┃")
	}
	u.t.Run(name, func(t *testing.T) { golden.RequireEqual(t, u.rec.viewOf(s)) })
}

func (u *ui) finish() *Model {
	u.t.Helper()
	u.tm.WaitFinished(u.t, teatest.WithFinalTimeout(10*time.Second))
	rec, ok := u.tm.FinalModel(u.t).(*recorder)
	if !ok {
		u.t.Fatal("the final model is not the recorder")
	}
	return rec.inner
}

// networkDone walks the network form of a static road through its five screens.
func (u *ui) networkDone() { u.enter(5) }

func TestUIDHCPRoad(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	u := startUI(t, h, Config{})
	u.waitFor("DHCP")
	u.golden("network", stNetwork)
	u.enter(1)
	u.waitFor("Advanced")
	u.enter(1)
	u.waitFor("Proxmox VE entry")
	u.golden("entry", stPVE)
	u.enter(1)
	u.waitFor("Change network")
	u.golden("summary", stSummary)
	u.enter(1)
	u.waitFor("Kubehenge is running")
	u.golden("end", stEnd)
	u.enter(1)
	u.finish()
	if create := h.exec.last("pct create"); !strings.Contains(create, "ip=dhcp") {
		t.Fatalf("not created with DHCP: %s", create)
	}
}

func TestUIStaticRoadWithPrefill(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pct config 100", "net0: name=eth0,bridge=vmbr0,ip=192.0.2.254/24,gw=192.0.2.1\n")
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("Static")
	u.enter(1)
	u.waitFor("IPv4 address")
	if !strings.Contains(u.rec.screen(), "192.0.2.254/24") || !strings.Contains(u.rec.screen(), "192.0.2.1") {
		t.Fatalf("address and gateway are not prefilled:\n%s", u.rec.screen())
	}
	u.enter(4)
	u.waitFor("Change network")
	if !strings.Contains(u.rec.screen(), "192.0.2.254/24") {
		t.Fatalf("summary lacks the address:\n%s", u.rec.screen())
	}
	u.enter(1)
	u.waitFor("Kubehenge is running")
	u.enter(1)
	u.finish()
	if create := h.exec.last("pct create"); !strings.Contains(create, "ip=192.0.2.254/24,gw=192.0.2.1") {
		t.Fatalf("not created with the static address: %s", create)
	}
}

func TestUIAddressInUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("Static")
	h.exec.fail("arping -c 2 -w 3 -I vmbr0 -D 192.0.2.254", 1)
	u.networkDone()
	u.waitFor("192.0.2.254 answers on this network")
	u.golden("in-use", stNetwork)
	h.exec.on("arping", "")
	u.networkDone()
	u.waitFor("Change network")
	u.down(3)
	u.enter(1)
	u.finish()
	if h.exec.changed() != 0 {
		t.Fatalf("the node was changed: %v", h.exec.calls)
	}
}

func TestUIStorageTiesShown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true, rootStorages: 2, templateStorages: 2})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("DHCP")
	u.enter(2)
	u.waitFor("Disk storage")
	u.golden("choices", stChoices)
	u.enter(2)
	u.waitFor("Change network")
	if u.s.Cfg.Storage != "local-lvm" || u.s.Cfg.TemplateStorage != "local-lvm" {
		t.Fatalf("storages %q and %q", u.s.Cfg.Storage, u.s.Cfg.TemplateStorage)
	}
	u.down(4)
	u.enter(1)
	u.finish()
}

func TestUIStorageTiesNotShown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("DHCP")
	u.enter(2)
	u.waitFor("Change network")
	if strings.Contains(u.rec.viewOf(stChoices), "Disk storage") {
		t.Fatal("a single storage was asked")
	}
	u.down(3)
	u.enter(1)
	u.finish()
}

func TestUIAPIAddressTieShown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{
		dhcp:  true,
		names: []string{"a.example.com", "b.example.com"},
		hosts: map[string][]string{"a.example.com": {"192.0.2.10"}, "b.example.com": {"192.0.2.10"}},
	})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("DHCP")
	u.enter(2)
	u.waitFor("Proxmox VE API address")
	u.enter(1)
	u.waitFor("Change network")
	if u.s.Cfg.ProxmoxURL != "https://a.example.com:8006" {
		t.Fatalf("API URL %q", u.s.Cfg.ProxmoxURL)
	}
	u.down(4)
	u.enter(1)
	u.finish()
}

func TestUICATrustShownAndAccepted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true, untrusted: true})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("DHCP")
	u.enter(2)
	u.waitFor("Trust this CA")
	u.golden("trust", stTrust)
	u.key('y')
	u.waitFor("Change network")
	if !u.s.Cfg.TrustServedCA {
		t.Fatal("the yes was not kept")
	}
	u.down(3)
	u.enter(1)
	u.finish()
}

func TestUICATrustRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true, untrusted: true})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("DHCP")
	u.enter(2)
	u.waitFor("Trust this CA")
	u.key('n')
	u.waitFor("Stopped")
	u.golden("refused", stFail)
	u.enter(1)
	m := u.finish()
	if m.Err() == nil || h.exec.changed() != 0 {
		t.Fatalf("refusing the CA must stop with nothing changed: %v %v", m.Err(), h.exec.calls)
	}
}

func TestUIDHCPTimeoutThenStatic(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "")
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("DHCP")
	u.enter(2)
	u.waitFor("Change network")
	u.enter(1)
	u.waitFor("No address from DHCP")
	u.golden("retry", stRetry)
	if h.exec.count("pct destroy 100") != 1 {
		t.Fatalf("the container was not removed: %v", h.exec.calls)
	}
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "2: eth0    inet 192.0.2.254/24 scope global eth0\n")
	u.enter(1)
	u.waitFor("Static")
	u.networkDone()
	u.waitFor("Change network")
	u.enter(1)
	u.waitFor("Kubehenge is running")
	u.enter(1)
	u.finish()
	if h.exec.count("pct create") != 2 || !strings.Contains(h.exec.last("pct create"), "ip=192.0.2.254/24") {
		t.Fatalf("not retried with the static address: %v", h.exec.calls)
	}
	if h.hits != 2 {
		t.Fatalf("the template was fetched again on the retry: %d requests", h.hits)
	}
}

func TestUIGoBackFromSummary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("DHCP")
	u.enter(2)
	u.waitFor("Change network")
	u.down(1)
	u.enter(1)
	u.waitFor("Static")
	u.networkDoneStaticFromDHCP()
	u.waitFor("Change network")
	if !strings.Contains(u.rec.screen(), "192.0.2.254/24") {
		t.Fatalf("summary lacks the address:\n%s", u.rec.screen())
	}
	u.down(3)
	u.enter(1)
	u.finish()
	if h.exec.changed() != 0 {
		t.Fatal("the node was changed")
	}
}

// networkDoneStaticFromDHCP picks Static on a form that opened on DHCP, then walks it.
func (u *ui) networkDoneStaticFromDHCP() {
	u.down(1)
	u.networkDone()
}

func TestUICancelBeforeAnyChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.waitFor("DHCP")
	u.enter(2)
	u.waitFor("Change network")
	u.down(3)
	u.enter(1)
	m := u.finish()
	if !m.Cancelled() || h.exec.changed() != 0 || h.hits != 0 {
		t.Fatalf("cancel changed the node: cancelled=%v calls=%v hits=%d", m.Cancelled(), h.exec.calls, h.hits)
	}
}

// The run, failure and end screens at fixed states.
func TestUIRunScreen(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	s := prepared(t, h, Config{IP: "dhcp", PVEEntry: "no"})
	m := NewModel(s, func(emit func(Event)) *Runner { return h.runner(s, emit) })
	m.stage = stRun
	m.runner = h.runner(s, nil)
	m.steps = m.runner.Steps()
	m.state["tofu"], m.detail["tofu"] = StepDone, "OpenTofu 1.0.0, checksum verified"
	m.state["template"], m.frac["template"] = StepRunning, 0.5
	t.Run("run", func(t *testing.T) {
		t.Parallel()
		golden.RequireEqual(t, sanitise(m.render()))
	})
}
