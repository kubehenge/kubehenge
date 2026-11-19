package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"
)

var (
	spinnerGlyphs = regexp.MustCompile(`[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏⠁⠂⠄⡀⢀⠠⠐⠈]`)
	fingerprintRE = regexp.MustCompile(`([0-9A-F]{2}:){5,}[0-9A-F]{2}`)
	trailingSpace = regexp.MustCompile(`(?m)[ \t]+$`)
	elapsedRE     = regexp.MustCompile(` \d+(\.\d+)?m?s\b`)
)

// sanitise strips what changes between runs: styles, spinner frames, elapsed times and the
// certificate's fingerprint.
func sanitise(s string) string {
	s = ansi.Strip(s)
	s = spinnerGlyphs.ReplaceAllString(s, "*")
	s = fingerprintRE.ReplaceAllString(s, "<fingerprint>")
	s = elapsedRE.ReplaceAllString(s, " <time>")
	return trailingSpace.ReplaceAllString(s, "")
}

// screens are the two sizes every screen is checked at.
var screens = []struct {
	name string
	w, l int
}{{"80x24", 80, 24}, {"120x40", 120, 40}}

// rendersEnv names the folder a run writes every screen it draws into as text, when it is set.
const rendersEnv = "KUBEHENGE_INSTALLER_RENDERS"

// show writes a screen to the renders folder, when asked for.
func show(t *testing.T, name, screen string) {
	t.Helper()
	dir := os.Getenv(rendersEnv)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // the developer running the test names the folder
		t.Fatal(err)
	}
	file := filepath.Join(dir, strings.NewReplacer("/", "-", " ", "-").Replace(name)+".txt")
	if err := os.WriteFile(file, []byte(screen), 0o600); err != nil { //nolint:gosec // the developer running the test names the folder
		t.Fatal(err)
	}
}

// fits fails a screen that draws past the size of the terminal.
func fits(t *testing.T, screen string, w, h int) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > h {
		t.Errorf("the screen is %d lines, the terminal %d:\n%s", len(lines), h, screen)
	}
	for _, l := range lines {
		if ansi.StringWidth(l) > w {
			t.Errorf("a line is %d columns, the terminal %d: %q", ansi.StringWidth(l), w, l)
		}
	}
}

// recorder wraps the installer and keeps the last screen it drew.
type recorder struct {
	inner *Model
	mu    sync.Mutex
	last  string
	// keys counts the key presses the installer has handled.
	keys int
	// navForm and navField are the form and field an Enter left, until huh's message that moves on
	// from them has arrived.
	navForm  *huh.Form
	navField huh.Field
	// ready says the installer takes a key now: nothing is being checked, no Enter is still moving
	// a form on, and a question's field has its focus.
	ready bool
}

func (r *recorder) Init() tea.Cmd { return r.inner.Init() }

// keyUnless is a key the installer gets only while its screen lacks the text. The check runs as
// the key arrives, so a screen that changed after the test last read it cannot take the key.
type keyUnless struct {
	key    tea.KeyPressMsg
	unless string
}

func (r *recorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn // tea.Model's method returns the interface.
	if k, ok := msg.(keyUnless); ok {
		r.mu.Lock()
		skip := strings.Contains(r.last, k.unless)
		if skip {
			r.keys++
		}
		r.mu.Unlock()
		if skip {
			return r, nil
		}
		msg = k.key
	}
	before := r.inner.form
	var field huh.Field
	if before != nil {
		field = before.GetFocusedField()
	}
	_, cmd := r.inner.Update(msg)
	screen := sanitise(r.inner.render())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = screen
	if key, ok := msg.(tea.KeyPressMsg); ok {
		r.keys++
		// An Enter a field took without an error moves on only when huh's next message arrives.
		if key.String() == "enter" && before != nil && r.inner.form == before && field != nil && field.Error() == nil {
			r.navForm, r.navField = before, field
		}
	}
	if f := r.navForm; f != nil && (r.inner.form != f || f.State != huh.StateNormal || f.GetFocusedField() != r.navField) {
		r.navForm, r.navField = nil, nil
	}
	r.ready = r.takesKeys()
	return r, cmd
}

func (r *recorder) View() tea.View { return r.inner.View() }

// takesKeys is ready, worked out from the installer's state with mu held.
func (r *recorder) takesKeys() bool {
	m := r.inner
	if r.navForm != nil || m.busy != "" {
		return false
	}
	switch m.stage {
	case stDetect:
		return false
	case stFound, stRun, stEnd:
		return true
	case stAsk, stList, stConfirm, stSummary, stFail:
	}
	// A question's stage with no form open is the installer leaving.
	return m.form == nil || strings.Contains(r.last, "┃")
}

func (r *recorder) keysHandled() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keys
}

func (r *recorder) takingKeys() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready
}

func (r *recorder) screen() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// ui is a running installer under test.
type ui struct {
	t    *testing.T
	h    *harness
	s    *Session
	rec  *recorder
	tm   *teatest.TestModel
	w, l int
}

func startUI(t *testing.T, h *harness, cfg Config) *ui { //nolint:gocritic // tests pass a Config literal.
	t.Helper()
	return startUISized(t, h, cfg, 100, 40)
}

func startUISized(t *testing.T, h *harness, cfg Config, w, l int) *ui { //nolint:gocritic // tests pass a Config literal.
	t.Helper()
	s := h.session(cfg)
	model := NewModel(s, func(emit func(Event)) *Runner { return h.runner(s, emit) })
	rec := &recorder{inner: model}
	tm := teatest.NewTestModel(t, rec, teatest.WithInitialTermSize(w, l))
	return &ui{t: t, h: h, s: s, rec: rec, tm: tm, w: w, l: l}
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

// settle waits until the installer takes keys: nothing is being checked, a form an Enter moved on
// has moved, and the question's field has its focus.
func (u *ui) settle() {
	u.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !u.rec.takingKeys() {
		if time.Now().After(deadline) {
			u.t.Fatalf("the installer never took keys, the screen is:\n%s", u.rec.screen())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// press sends a key once the installer takes keys, and returns once it handled the key and what
// the key set off.
func (u *ui) press(msg tea.KeyPressMsg) {
	u.t.Helper()
	u.send(msg, msg.String())
}

// send is press for a key or a keyUnless.
func (u *ui) send(msg tea.Msg, name string) {
	u.t.Helper()
	u.settle()
	want := u.rec.keysHandled() + 1
	u.tm.Send(msg)
	deadline := time.Now().Add(15 * time.Second)
	for u.rec.keysHandled() < want {
		if time.Now().After(deadline) {
			u.t.Fatalf("the installer did not handle the key %q, the screen is:\n%s", name, u.rec.screen())
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

func (u *ui) typed(s string) {
	u.t.Helper()
	for _, r := range s {
		u.key(r)
	}
}

// golden compares the settled screen to its golden file, checks that it fits the terminal and
// writes it to the renders folder when one is named. A form's focus bar is waited for, since the
// first frame of a form comes before its field takes focus.
func (u *ui) golden(name string) {
	u.t.Helper()
	u.settle()
	screen := u.rec.screen()
	show(u.t, fmt.Sprintf("%s-%s-%dx%d", u.t.Name(), name, u.w, u.l), screen)
	fits(u.t, screen, u.w, u.l)
	u.t.Run(name, func(t *testing.T) { golden.RequireEqual(t, screen) })
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

// startUIWith is startUISized with a hook that changes the session before the installer starts.
func startUIWith(t *testing.T, h *harness, cfg Config, w, l int, prep func(*Session)) *ui { //nolint:gocritic // tests pass a Config literal.
	t.Helper()
	s := h.session(cfg)
	prep(s)
	model := NewModel(s, func(emit func(Event)) *Runner { return h.runner(s, emit) })
	rec := &recorder{inner: model}
	tm := teatest.NewTestModel(t, rec, teatest.WithInitialTermSize(w, l))
	return &ui{t: t, h: h, s: s, rec: rec, tm: tm, w: w, l: l}
}

// toSummary answers the questions with their defaults until the summary shows.
func (u *ui) toSummary() {
	u.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for presses := 0; presses < 14 && time.Now().Before(deadline); {
		screen := u.rec.screen()
		switch {
		case strings.Contains(screen, "If it fails"):
			u.settle()
			return
		case strings.Contains(screen, "enter "):
			// A summary drawn after the screen was read would take a plain enter as Install.
			u.send(keyUnless{tea.KeyPressMsg{Code: tea.KeyEnter}, "If it fails"}, "enter")
			presses++
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	u.waitFor("If it fails")
	u.settle()
}

// The static road at both sizes: every screen of it is golden, fits the terminal, and the container
// is made with the address and no IPv6.
func TestUIStaticRoadScreens(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{})
			u := startUIWith(t, h, Config{PVEEntry: "no"}, sz.w, sz.l, func(*Session) {})
			u.waitFor("Node")
			u.golden("found")
			u.enter(1)
			u.waitFor("IP version")
			u.golden("version")
			u.enter(1)
			u.waitFor("VLAN")
			u.golden("vlan")
			u.enter(2)
			u.waitFor("IPv4 address and prefix")
			u.golden("address")
			u.toSummary()
			u.golden("summary")
			u.enter(1)
			u.waitFor("Kubehenge is running")
			u.golden("end")
			u.enter(1)
			u.finish()
			if create := h.exec.last("pct create"); !strings.Contains(create, "ip=192.0.2.254/24,gw=192.0.2.1") || strings.Contains(create, "ip6=") {
				t.Fatalf("not created with the static address alone: %s", create)
			}
		})
	}
}

func TestUIDHCPRoad(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.toSummary()
	if !strings.Contains(u.rec.screen(), "DHCP") {
		t.Fatalf("the summary lacks DHCP:\n%s", u.rec.screen())
	}
	u.enter(1)
	u.waitFor("Kubehenge is running")
	u.enter(1)
	u.finish()
	if create := h.exec.last("pct create"); !strings.Contains(create, "ip=dhcp") {
		t.Fatalf("not created with DHCP: %s", create)
	}
}

// An address that answers is refused on the address question itself, with the reason, and the
// questions before it are not asked again.
func TestUIAddressInUseReturnsToTheAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	u := startUIWith(t, h, Config{PVEEntry: "no"}, 80, 24, func(*Session) {})
	u.waitFor("Node")
	u.enter(4)
	u.waitFor("IPv4 address and prefix")
	h.take("192.0.2.254")
	u.enter(2)
	u.waitFor("is in use on")
	u.golden("in-use")
	if !strings.Contains(u.rec.screen(), "IPv4 address and prefix") || strings.Contains(u.rec.screen(), "IP version") {
		t.Fatalf("the refusal is not on the address question:\n%s", u.rec.screen())
	}
	u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	u.finish()
	if h.exec.changed() != 0 {
		t.Fatalf("the node was changed: %v", h.exec.calls)
	}
}

// A failed step keeps the installer open and the container in place, and offers what to do. Leaving
// with the container kept removes nothing.
func TestUIFailureKeepsTheInstallerOpen(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{dhcp: true})
			h.exec.fail("pct start 100", 1)
			path := filepath.Join(t.TempDir(), "answers.json")
			u := startUIWith(t, h, Config{PVEEntry: "no", Name: "kubehenge.example.org"}, sz.w, sz.l, func(s *Session) { s.AnswersPath = path })
			u.toSummary()
			u.enter(1)
			u.waitFor("Stopped")
			u.golden("failure")
			if got := loadAnswers(path); got == nil || got.Name != "kubehenge.example.org" {
				t.Errorf("the answers were not saved when the install started: %+v", got)
			}
			if h.exec.count("pct destroy") != 0 {
				t.Fatalf("the container was removed before a choice: %v", h.exec.calls)
			}
			for _, want := range []string{"Try again", "Quit and keep the container", "Quit and remove it"} {
				if !strings.Contains(u.rec.screen(), want) {
					t.Errorf("the failure screen lacks %q:\n%s", want, u.rec.screen())
				}
			}
			u.down(1)
			u.enter(1)
			m := u.finish()
			if m.Err() == nil || h.exec.count("pct destroy") != 0 {
				t.Fatalf("keeping the container: err %v, calls %v", m.Err(), h.exec.calls)
			}
			if !strings.Contains(m.Outcome(), "pct enter 100") {
				t.Errorf("the way to inspect the kept container is not said: %q", m.Outcome())
			}
		})
	}
}

// A DHCP run that gets no address offers to change the IPv4 setting. A change of network alone is
// given to the container that is there, which is not created again.
func TestUIDHCPFailureChangesTheSettingAndRetries(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "")
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.toSummary()
	u.enter(1)
	u.waitFor("Change IPv4 address")
	u.golden("no-address")
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "2: eth0    inet 192.0.2.254/24 scope global eth0\n")
	u.enter(1)
	u.waitFor("DHCP")
	u.down(1)
	u.enter(2)
	u.waitFor("Kubehenge is running")
	u.enter(1)
	u.finish()
	if h.exec.count("pct create") != 1 || h.exec.count("pct set 100 --net0") != 1 || h.exec.count("pct destroy") != 0 {
		t.Fatalf("the container was not kept and re-networked: %v", h.exec.calls)
	}
	if !strings.Contains(strings.Join(h.exec.calls, "\n"), "ip=192.0.2.254/24") {
		t.Fatalf("the static address was not given: %v", h.exec.calls)
	}
}

// "Quit and remove it" on the failure screen removes the container, and says so.
func TestUIFailureQuitAndRemove(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.fail("pct start 100", 1)
	u := startUI(t, h, Config{PVEEntry: "no"})
	u.toSummary()
	u.enter(1)
	u.waitFor("Stopped")
	u.down(2)
	u.enter(1)
	m := u.finish()
	if h.exec.count("pct destroy 100") != 1 || !strings.Contains(m.Outcome(), "was removed") {
		t.Fatalf("the container was not removed: %q %v", m.Outcome(), h.exec.calls)
	}
}

// Answers from an earlier run come back, the screen says so, and a finished install deletes them.
func TestUIRestoresAndForgetsAnswers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	path := filepath.Join(t.TempDir(), "answers.json")
	if err := saveAnswers(path, &Config{DNS: "192.0.2.53", Name: "kubehenge.example.org"}); err != nil {
		t.Fatal(err)
	}
	u := startUIWith(t, h, Config{PVEEntry: "no"}, 80, 24, func(s *Session) {
		s.AnswersPath, s.Saved = path, loadAnswers(path)
	})
	u.waitFor("Restored")
	u.golden("restored")
	u.toSummary()
	for _, want := range []string{"192.0.2.53", "kubehenge.example.org"} {
		if !strings.Contains(u.rec.screen(), want) {
			t.Errorf("the summary lacks the restored %q:\n%s", want, u.rec.screen())
		}
	}
	u.enter(1)
	u.waitFor("Kubehenge is running")
	u.enter(1)
	u.finish()
	if loadAnswers(path) != nil {
		t.Error("the answers stayed after the install finished")
	}
}

// A saved answer the checks refuse is named on the found screen, and the rest of its network comes
// back.
func TestUIRestoreNamesWhatItDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	path := filepath.Join(t.TempDir(), "answers.json")
	saved := &Config{Bridge: "vmbr0", MTU: "abc", IP: "192.0.2.40/24", Gateway: "192.0.2.1", Hostname: "bad_host!", DNS: "192.0.2.53"}
	if err := saveAnswers(path, saved); err != nil {
		t.Fatal(err)
	}
	u := startUIWith(t, h, Config{PVEEntry: "no"}, 80, 24, func(s *Session) {
		s.AnswersPath, s.Saved = path, loadAnswers(path)
	})
	u.waitFor("which fail the checks")
	u.golden("dropped")
	if screen := u.rec.screen(); !strings.Contains(screen, "the saved MTU and host name") {
		t.Errorf("the dropped answers are not named:\n%s", screen)
	}
	u.press(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	u.finish()
	if c := u.s.Cfg; c.IP != "192.0.2.40/24" || c.MTU != "" || c.Hostname != DefaultHostname {
		t.Errorf("want the address without the MTU and host name, got %q %q %q", c.IP, c.MTU, c.Hostname)
	}
}

// --dry-run ends with the plan and changes nothing. --yes skips the confirmation.
func TestUIDryRunAndYes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	u := startUI(t, h, Config{PVEEntry: "no", DryRun: true})
	u.toSummary()
	u.enter(1)
	m := u.finish()
	if !m.DryRun() || h.exec.changed() != 0 {
		t.Fatalf("dry run: %v, calls %v", m.DryRun(), h.exec.calls)
	}

	h2 := newHarness(t, opts{dhcp: true})
	u2 := startUI(t, h2, Config{PVEEntry: "no", Yes: true})
	u2.waitFor("Node")
	for range 8 {
		if strings.Contains(u2.rec.screen(), "Kubehenge is running") {
			break
		}
		u2.enter(1)
	}
	u2.waitFor("Kubehenge is running")
	if strings.Contains(u2.rec.screen(), "If it fails") {
		t.Error("the summary was shown with --yes")
	}
	u2.enter(1)
	u2.finish()
}

// The summary lists the settings, each with its value, and a change reopens only that setting.
func TestUIChangeASettingFromTheSummary(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{dhcp: true})
			u := startUIWith(t, h, Config{PVEEntry: "no"}, sz.w, sz.l, func(*Session) {})
			u.toSummary()
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.enter(1)
			u.waitFor("enter change")
			u.golden("list")
			u.press(tea.KeyPressMsg{Code: tea.KeyEscape})
			u.waitFor("If it fails")
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.press(tea.KeyPressMsg{Code: tea.KeyRight})
			u.enter(1)
			m := u.finish()
			if !m.Cancelled() || h.exec.changed() != 0 {
				t.Fatalf("cancel changed the node: %v", h.exec.calls)
			}
		})
	}
}

// The failure screen of a refused CA and the question of an address outside the subnet.
func TestUICATrustScreens(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{dhcp: true, untrusted: true})
			u := startUIWith(t, h, Config{PVEEntry: "no"}, sz.w, sz.l, func(*Session) {})
			u.waitFor("Node")
			u.enter(1)
			u.waitFor("Trust this CA")
			u.golden("trust")
			u.key('n')
			u.waitFor("Stopped")
			u.golden("refused")
			u.down(1)
			u.enter(1)
			m := u.finish()
			if m.Err() == nil || h.exec.changed() != 0 {
				t.Fatalf("refusing the CA must stop with nothing changed: %v %v", m.Err(), h.exec.calls)
			}
		})
	}
}

// The run screen at fixed states, at both sizes.
func TestUIRunScreen(t *testing.T) {
	t.Parallel()
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, opts{dhcp: true})
			s := prepared(t, h, Config{IP: "dhcp", PVEEntry: "no"})
			m := NewModel(s, func(emit func(Event)) *Runner { return h.runner(s, emit) })
			m.width, m.height = sz.w, sz.l
			m.stage = stRun
			m.runner = h.runner(s, nil)
			m.steps = m.runner.Steps()
			m.state["tofu"], m.detail["tofu"] = StepDone, "OpenTofu 1.0.0, checksum verified"
			m.state["template"], m.frac["template"] = StepRunning, 0.5
			screen := sanitise(m.render())
			show(t, "TestUIRunScreen-run-"+sz.name, screen)
			fits(t, screen, sz.w, sz.l)
			golden.RequireEqual(t, screen)
		})
	}
}
