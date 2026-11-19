package installer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// The actions of the summary and of a failure.
const (
	actInstall = "install"
	actChange  = "change"
	actCancel  = "cancel"
	actRetry   = "retry"
	actKeep    = "keep"
	actQuit    = "quit"
	actFix     = "fix:"
)

// keyLeave stops a run, or leaves the installer.
const keyLeave = "ctrl+c"

type stage int

const (
	stDetect stage = iota
	stFound
	stAsk
	stList
	stConfirm
	stSummary
	stRun
	stEnd
	stFail
)

// The crumbs of the header and the stages each one covers.
var crumbNames = []string{"Network", "Storage", "Summary", "Install", "Done"}

const (
	crumbNetwork = iota
	crumbStorage
	crumbSummary
	crumbInstall
	crumbDone
)

var hue = lipgloss.Color //nolint:misspell // spellchecker:disable-line, lipgloss spells its constructor Color

var (
	accent  = hue("#E3B04B")
	teal    = hue("#0d9488")
	stone   = hue("#8A8F98")
	good    = hue("#6CC4A1")
	bad     = hue("#E5626B")
	bold    = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Foreground(stone)
	accentS = lipgloss.NewStyle().Foreground(accent)
	tealS   = lipgloss.NewStyle().Foreground(teal)
	goodS   = lipgloss.NewStyle().Foreground(good)
	badS    = lipgloss.NewStyle().Foreground(bad)
)

type (
	detectedMsg  struct{ err error }
	refreshedMsg struct{}
	resolvedMsg  struct{ err error }
	verifiedMsg  struct{ err error }
	eventMsg     Event
	doneMsg      struct {
		res *Result
		err error
	}
	cleanedMsg struct{ then tea.Cmd }
	resumedMsg struct{ err error }
)

// answers are the values the forms edit, copied from the options when a question opens and back
// when it is answered.
type answers struct {
	family, mode4, mode6                 string
	address, gateway, address6, gateway6 string
	dns, bridge, vlan, mtu               string
	ctid, hostname, name                 string
	certFile, keyFile, proxies           string
	storage, templateStorage, apiHost    string
	apiURL, apiCA                        string
	trust, pve, outside, apiCheck        bool
	action                               string
	setting                              string
	gwNote                               string
	// apiPick says the API question lists the addresses found, and apiTyped that the user asked
	// for another.
	apiPick, apiTyped bool
}

// Model is the full-screen installer.
type Model struct {
	S         *Session
	NewRunner func(emit func(Event)) *Runner

	ctx    context.Context //nolint:containedctx // Bubble Tea runs commands outside Update, so the model carries the run's context.
	cancel context.CancelFunc
	stage  stage
	width  int
	height int
	spin   spinner.Model
	bar    progress.Model
	form   *huh.Form
	a      answers
	// note stands under the open question: what the last answer got wrong.
	note string
	// busy says what is being checked while a command runs.
	busy string
	// keys are the keys of the open question, shown in the footer.
	keys string
	// asking is the setting the open question belongs to.
	asking string
	// trail is the settings asked, so that Esc goes back one.
	trail []string
	// answered and given say which settings have an answer from the user and which the options gave.
	answered, given map[string]bool
	// fromSummary and fixing say where a question returns to: the summary, or a failed run's retry.
	fromSummary, fixing bool
	// fixed is the setting a failure sent the user to change.
	fixed      string
	table      map[string]*setting
	leaveAsked bool
	// dry is the user's choice to print the plan instead of installing.
	dry bool

	runner  *Runner
	steps   []StepInfo
	state   map[string]StepState
	detail  map[string]string
	frac    map[string]float64
	started map[string]time.Time
	now     func() time.Time
	events  chan any
	// running closes when the runner's goroutine has returned.
	running chan struct{}
	res     *Result

	fail      error
	cancelled bool
	cleaned   bool
	// keep says the container stays when the installer ends without the user's choice to remove it:
	// a failure screen said it is kept until they choose, or the installer crashed.
	keep bool
}

// NewModel returns the installer for a session. The runner is built when the run starts.
func NewModel(s *Session, newRunner func(emit func(Event)) *Runner) *Model {
	ctx, cancel := context.WithCancel(context.Background())
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(accentS))
	return &Model{
		S: s, NewRunner: newRunner, ctx: ctx, cancel: cancel, width: defaultWidth, height: defaultHeight,
		spin: sp, bar: progress.New(progress.WithWidth(barWidth)), now: time.Now,
		state: map[string]StepState{}, detail: map[string]string{}, frac: map[string]float64{}, started: map[string]time.Time{},
		answered: map[string]bool{}, given: map[string]bool{}, table: newSettings(),
	}
}

// Err is the failure the installer ended on, nil after a success or a cancel.
func (m *Model) Err() error { return m.fail }

// Cancelled reports whether the user left before the run.
func (m *Model) Cancelled() bool { return m.cancelled }

// DryRun reports whether the user chose to print the plan and stop.
func (m *Model) DryRun() bool { return m.dry }

// Outcome says what a stopped run undid and what it left, empty when no run started.
func (m *Model) Outcome() string {
	if m.runner == nil {
		return ""
	}
	return m.runner.Outcome()
}

// Finished reports whether the run installed the appliance, which the end screen shows.
func (m *Model) Finished() bool { return m.res != nil }

// Result is what the run installed, nil before it finished.
func (m *Model) Result() *Result { return m.res }

// Left says what a stopped run left behind that the user must look after, empty for nothing.
func (m *Model) Left() string {
	if m.runner == nil {
		return ""
	}
	return m.runner.Left()
}

// Shutdown ends a run still going and removes what it made, unless it is to be kept: with
// --keep-failed, or once a failure screen said the container stays until the user chooses. A signal
// or a lost terminal ends the program without its own cleanup, which would leave a half-made
// container behind.
func (m *Model) Shutdown(ctx context.Context) {
	m.cancel()
	if m.running != nil {
		<-m.running
	}
	if m.runner != nil {
		if m.keeps() {
			m.runner.Keep()
		}
		m.runner.Cleanup(ctx)
	}
}

// Init starts the detection behind one spinner.
func (m *Model) Init() tea.Cmd {
	m.busy = "Detecting this node"
	return tea.Batch(m.spin.Tick, m.detectCmd())
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn // tea.Model's method returns the interface.
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.bar.SetWidth(barWidth)
	case tea.KeyPressMsg:
		if cmd, handled := m.onKey(msg); handled {
			return m, cmd
		}
		m.leaveAsked = false
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
	case detectedMsg:
		return m.afterDetect(msg.err)
	case refreshedMsg:
		return m.advanceModel()
	case resolvedMsg:
		return m.afterResolve(msg.err)
	case verifiedMsg:
		return m.afterVerify(msg.err)
	case resumedMsg:
		return m.afterResume(msg.err)
	case eventMsg:
		m.onEvent(Event(msg))
		cmd := m.wait()
		return m, cmd
	case doneMsg:
		return m.onDone(msg)
	case cleanedMsg:
		m.cleaned = true
		m.state["remove"] = StepDone
		return m, msg.then
	case formMsg:
		cmd := m.onFormMsg(msg)
		return m, cmd
	}
	if m.form != nil {
		cmds = append(cmds, m.updateForm(msg))
	}
	return m, tea.Batch(cmds...)
}

// View implements tea.Model.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// keeps says a stopped run keeps its container: --keep-failed asks it, or a failure screen said so.
func (m *Model) keeps() bool { return m.S.Cfg.KeepFailed || m.keep }

func (m *Model) detectCmd() tea.Cmd {
	return func() tea.Msg { return detectedMsg{m.S.detect(m.ctx)} }
}

func (m *Model) advanceModel() (*Model, tea.Cmd) {
	m.busy = ""
	cmd := m.advance()
	return m, cmd
}

// fitList sizes a list of count options to the terminal, and lets the user filter one that is
// longer than the rows it gets.
func (m *Model) fitList(sel *huh.Select[string], count int) *huh.Select[string] {
	sel = sel.Height(max(m.height-listChrome, minListRows))
	if count > m.height-listChrome {
		sel = sel.Filtering(true)
	}
	return sel
}

// onKey handles the keys that act on the whole installer, and reports whether it did.
func (m *Model) onKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	key := msg.String()
	switch {
	case key == keyLeave && m.stage == stRun:
		m.cancel()
		return nil, true
	case m.stage == stEnd:
		return tea.Quit, true
	case key == keyLeave && m.stage == stFail:
		return m.quitAndKeep(), true
	case key == keyLeave:
		return m.leaveKey(), true
	case key == "esc" && (m.stage == stAsk || m.stage == stList || m.stage == stConfirm):
		return m.back(), true
	case key == "enter" && m.stage == stFound:
		return m.advance(), true
	}
	return nil, false
}

// leaveKey asks once before leaving with answers given, then leaves.
func (m *Model) leaveKey() tea.Cmd {
	if len(m.answered) > 0 && !m.leaveAsked && m.stage != stDetect {
		m.leaveAsked = true
		return nil
	}
	return m.leave()
}

func (m *Model) leave() tea.Cmd {
	m.cancelled = true
	m.cancel()
	return tea.Quit
}

// afterDetect shows what the detection found, or its failure.
func (m *Model) afterDetect(err error) (*Model, tea.Cmd) {
	m.busy = ""
	if m.S.Facts != nil {
		// A failed check of the API still offers to change it, then goes on from the options.
		m.initFromOptions()
	}
	if err != nil {
		return m.failWith(err)
	}
	m.stage = stFound
	return m, nil
}

// failWith ends on the failure screen, for a failure before any run.
func (m *Model) failWith(err error) (*Model, tea.Cmd) {
	m.fail, m.stage = err, stFail
	cmd := m.failForm()
	return m, cmd
}

// updateForm passes a message to the open form and acts on its end.
func (m *Model) updateForm(msg tea.Msg) tea.Cmd {
	f, cmd := m.form.Update(msg)
	if form, ok := f.(*huh.Form); ok {
		m.form = form
	}
	cmds := []tea.Cmd{tagged(m.form, cmd)}
	switch m.form.State {
	case huh.StateCompleted:
		m.form = nil
		cmds = append(cmds, m.formDone())
	case huh.StateAborted:
		m.form = nil
		return m.leaveKey()
	case huh.StateNormal:
	}
	return tea.Batch(cmds...)
}

func (m *Model) formWidth() int { return min(m.width-formMargin, maxFormWidth) }

func (m *Model) startForm(groups ...*huh.Group) tea.Cmd {
	m.form = huh.NewForm(groups...).WithTheme(huh.ThemeFunc(theme)).WithKeyMap(formKeys()).WithWidth(m.formWidth()).WithShowHelp(false)
	return tagged(m.form, m.form.Init())
}

// formMsg is a message a form's command sent, with the form that sent it.
type formMsg struct {
	form *huh.Form
	msg  tea.Msg
}

var cmdList = reflect.TypeFor[[]tea.Cmd]()

// tagged has every message of a form's command carry the form. huh's own messages, such as the
// one an Enter sends to move to the next field, name no form. One still on its way when its form
// was answered would otherwise reach the next form and answer it too.
func tagged(f *huh.Form, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			return tea.BatchMsg(taggedAll(f, batch))
		}
		// tea.Sequence's message type is unexported, so it is known by its shape.
		if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().ConvertibleTo(cmdList) {
			if list, ok := reflect.TypeAssert[[]tea.Cmd](v.Convert(cmdList)); ok {
				if seq := tea.Sequence(taggedAll(f, list)...); seq != nil {
					return seq()
				}
				return nil
			}
		}
		return formMsg{f, msg}
	}
}

func taggedAll(f *huh.Form, cmds []tea.Cmd) []tea.Cmd {
	out := make([]tea.Cmd, len(cmds))
	for i, c := range cmds {
		out[i] = tagged(f, c)
	}
	return out
}

// onFormMsg passes a form's message on while the form is open, and drops it once the form is gone.
// A message of Bubble Tea's own, such as a request for the window's size, goes back to the program.
func (m *Model) onFormMsg(msg formMsg) tea.Cmd {
	if m.form == nil || msg.form != m.form {
		return nil
	}
	if t := reflect.TypeOf(msg.msg); !strings.HasPrefix(t.PkgPath(), "charm.land/huh/") && !strings.HasPrefix(t.PkgPath(), "charm.land/bubbles/") {
		inner := msg.msg
		return func() tea.Msg { return inner }
	}
	return m.updateForm(msg.msg)
}

func formKeys() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	k.Select.Left.SetEnabled(true)
	k.Select.Right.SetEnabled(true)
	return k
}

func theme(isDark bool) *huh.Styles {
	s := huh.ThemeCharm(isDark)
	s.Focused.Title = s.Focused.Title.Foreground(accent).Bold(true)
	s.Focused.SelectSelector = s.Focused.SelectSelector.Foreground(accent)
	s.Focused.FocusedButton = s.Focused.FocusedButton.Background(accent).Foreground(hue("#1B1B1B"))
	return s
}

// formDone acts on the answer of the open form.
func (m *Model) formDone() tea.Cmd {
	m.leaveAsked = false
	switch m.stage {
	case stAsk:
		return m.settingDone(m.asking)
	case stList:
		return m.listDone()
	case stConfirm:
		return m.confirmDone()
	case stSummary:
		return m.summaryAction()
	case stFail:
		return m.failAction()
	case stDetect, stFound, stRun, stEnd:
	}
	return nil
}

// back returns to the question before the open one, or to the summary or the failure the question
// was opened from. The typed API address goes back to the list of addresses found.
func (m *Model) back() tea.Cmd {
	m.note = ""
	switch {
	case m.stage == stList:
		return m.showSummary()
	case m.stage == stAsk && m.asking == settingAPI && m.a.apiTyped && len(TiedCandidates(m.S.Facts.Candidates)) > 1:
		m.a.apiTyped = false
		return m.open(settingAPI)
	case len(m.trail) > 1:
		prev := m.trail[len(m.trail)-2]
		m.trail = m.trail[:len(m.trail)-2]
		return m.open(prev)
	case m.fixing:
		m.fixing = false
		m.fail, m.stage = m.runFailure(), stFail
		return m.failForm()
	case m.fromSummary:
		return m.showSummary()
	}
	return nil
}

// advance opens the first question still open, or checks the answers once none is.
func (m *Model) advance() tea.Cmd {
	for _, id := range settingOrder {
		if m.needed(id) {
			return m.open(id)
		}
	}
	m.busy = "Checking the network settings"
	m.stage = stDetect
	return tea.Batch(m.spin.Tick, func() tea.Msg { return verifiedMsg{m.S.CheckAddressing(m.ctx)} })
}

// afterVerify opens the setting a check refused, or moves on once all pass.
func (m *Model) afterVerify(err error) (*Model, tea.Cmd) {
	m.busy = ""
	if err == nil {
		cmd := m.allAnswered()
		return m, cmd
	}
	var u *UserError
	if !errors.As(err, &u) || u.Setting == "" {
		return m.failWith(err)
	}
	if u.Setting == settingOutside {
		m.stage = stConfirm
		m.note = u.Cause
		cmd := m.confirmForm("Use it anyway?", "Use", "Choose another")
		return m, cmd
	}
	m.note = u.Cause
	m.answered[u.Setting], m.given[u.Setting] = false, false
	cmd := m.open(u.Setting)
	return m, cmd
}

// allAnswered goes on once every question has its answer: back to a failed run, or to the summary.
func (m *Model) allAnswered() tea.Cmd {
	if m.fixing {
		return m.resume(m.fixed)
	}
	if m.S.Cfg.Yes {
		return m.begin()
	}
	return m.showSummary()
}

// begin starts the run, or ends with the plan on a dry run.
func (m *Model) begin() tea.Cmd {
	if m.S.Cfg.DryRun {
		m.dry = true
		return tea.Quit
	}
	return m.startRun()
}

func (m *Model) confirmForm(title, yes, no string) tea.Cmd {
	m.a.outside = false
	m.keys = "←→ choose  enter confirm  esc back  ctrl+c leave"
	return m.startForm(huh.NewGroup(huh.NewConfirm().Title(title).Affirmative(yes).Negative(no).Value(&m.a.outside)))
}

// confirmDone applies the answer to an address outside the network's subnet.
func (m *Model) confirmDone() tea.Cmd {
	if !m.a.outside {
		m.note = ""
		m.answered[settingV4Addr], m.given[settingV4Addr] = false, false
		return m.open(settingV4Addr)
	}
	m.S.AllowOutside = true
	m.note = ""
	return m.advance()
}

// showSummary opens the summary of what is about to be created.
func (m *Model) showSummary() tea.Cmd {
	m.stage, m.fromSummary, m.fixing, m.note = stSummary, false, false, ""
	m.trail = nil
	install := "Install"
	if m.S.Cfg.DryRun {
		install = "Print plan"
	}
	m.a.action = actInstall
	m.keys = "←→ choose  enter confirm  ctrl+c leave"
	return m.startForm(huh.NewGroup(huh.NewSelect[string]().Inline(true).Options(
		huh.NewOption(install, actInstall), huh.NewOption("Change a setting", actChange), huh.NewOption("Cancel", actCancel),
	).Value(&m.a.action)))
}

func (m *Model) summaryAction() tea.Cmd {
	switch m.a.action {
	case actInstall:
		return m.begin()
	case actChange:
		return m.settingList()
	default:
		m.cancelled = true
		return tea.Quit
	}
}

// settingList opens the list of settings, each with its value.
func (m *Model) settingList() tea.Cmd {
	m.stage, m.fromSummary = stList, true
	opts := make([]huh.Option[string], 0, len(settingList))
	for _, id := range settingList {
		if st := m.table[id]; m.listed(id) {
			opts = append(opts, huh.NewOption(fmt.Sprintf("%-*s %s", summaryLabelWidth, st.label, st.value(m)), id))
		}
	}
	m.a.setting = opts[0].Value
	m.keys = "↑↓ choose  / filter  enter change  esc back  ctrl+c leave"
	sel := m.fitList(huh.NewSelect[string]().Title("Change").Options(opts...).Value(&m.a.setting), len(opts))
	return m.startForm(huh.NewGroup(sel))
}

func (m *Model) listDone() tea.Cmd {
	id := m.a.setting
	m.answered[id], m.given[id] = false, false
	m.trail = nil
	return m.open(id)
}

// open asks one setting.
func (m *Model) open(id string) tea.Cmd {
	m.stage, m.asking = stAsk, id
	if len(m.trail) == 0 || m.trail[len(m.trail)-1] != id {
		m.trail = append(m.trail, id)
	}
	fields := m.table[id].fields(m)
	m.keys = keysOf(fields)
	return m.startForm(huh.NewGroup(fields...))
}

// settingDone stores the answer of a setting and moves on.
func (m *Model) settingDone(id string) tea.Cmd {
	m.note = ""
	m.answered[id] = true
	if cmd := m.table[id].apply(m); cmd != nil {
		return tea.Batch(m.spin.Tick, cmd)
	}
	return m.advance()
}

// refresh reads the network again after its answers changed.
func (m *Model) refresh() tea.Cmd {
	c := &m.S.Cfg
	m.busy, m.stage = "Reading the network "+c.Bridge, stDetect
	return func() tea.Msg {
		m.S.RefreshNetwork(m.ctx, c.Bridge, c.VLAN)
		return refreshedMsg{}
	}
}

// resolve settles the API address and its CA again.
func (m *Model) resolve() tea.Cmd {
	m.busy, m.stage = "Checking the Proxmox VE API", stDetect
	return func() tea.Msg { return resolvedMsg{m.S.ResolveAPI(m.ctx, AskTies)} }
}

func (m *Model) afterResolve(err error) (*Model, tea.Cmd) {
	m.busy = ""
	if err != nil {
		return m.failWith(err)
	}
	cmd := m.advance()
	return m, cmd
}

// initFromOptions notes which settings the options and the restored answers already give.
func (m *Model) initFromOptions() {
	c, s := &m.S.Cfg, m.S
	m.given = map[string]bool{}
	switch {
	case c.IP == optNone:
		m.a.family = "v6"
	case c.IP6 != "":
		m.a.family = "both"
	case c.IP != "":
		m.a.family = "v4"
	}
	m.given[settingFamily] = m.a.family != ""
	m.a.mode4, m.a.mode6 = modeOf(c.IP), modeOf(c.IP6)
	m.given[settingV4Mode], m.given[settingV6Mode] = m.a.mode4 != "", m.a.mode6 != ""
	m.given[settingV4Addr] = m.a.mode4 == optStatic
	m.given[settingV4Gateway] = m.a.mode4 == optDHCP || c.Gateway != ""
	m.given[settingV6Addr] = m.a.mode6 == optStatic
	m.given[settingV6Gateway] = m.a.mode6 != optStatic || c.Gateway6 != ""
	m.given[settingNetwork] = s.BridgeGiven
	m.given[settingVLAN] = s.VLANGiven
	m.given[settingDNS] = c.DNS != ""
}

func modeOf(ip string) string {
	switch ip {
	case "", optNone:
		return ""
	case optDHCP:
		return optDHCP
	case optAuto:
		return optAuto
	}
	return optStatic
}

func (m *Model) runFailure() error {
	if m.fail != nil {
		return m.fail
	}
	return errors.New("the install did not finish")
}

func (m *Model) crumb() int {
	switch m.stage {
	case stDetect, stFound:
		return crumbNetwork
	case stAsk:
		if slices.Contains(storageSettings, m.asking) {
			return crumbStorage
		}
		return crumbNetwork
	case stList, stSummary, stConfirm:
		return crumbSummary
	case stRun:
		return crumbInstall
	case stEnd:
		return crumbDone
	case stFail:
		if m.runner != nil {
			return crumbInstall
		}
		return crumbNetwork
	}
	return crumbNetwork
}
