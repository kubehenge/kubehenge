package installer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

type stage int

const (
	stDetect stage = iota
	stNetwork
	stChoices
	stTrust
	stPVE
	stSummary
	stRun
	stRetry
	stEnd
	stFail
)

var stageNames = []struct {
	label  string
	stages []stage
}{
	{"Network", []stage{stDetect, stNetwork}},
	{"Choices", []stage{stChoices, stTrust, stPVE}},
	{"Summary", []stage{stSummary}},
	{"Install", []stage{stRun, stRetry}},
	{"Done", []stage{stEnd, stFail}},
}

var (
	accent  = lipgloss.Color("#E3B04B")
	stone   = lipgloss.Color("#8A8F98")
	good    = lipgloss.Color("#6CC4A1")
	bad     = lipgloss.Color("#E5626B")
	bold    = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Foreground(stone)
	accentS = lipgloss.NewStyle().Foreground(accent)
	goodS   = lipgloss.NewStyle().Foreground(good)
	badS    = lipgloss.NewStyle().Foreground(bad)
)

type (
	detectedMsg struct{ err error }
	resolvedMsg struct{ err error }
	probeMsg    struct {
		address string
		used    bool
	}
	eventMsg Event
	doneMsg  struct {
		res *Result
		err error
	}
	cleanedMsg struct{}
)

// answers are the values the forms edit.
type answers struct {
	mode, address, gateway, dns       string
	more                              bool
	ip6, gateway6, bridge, vlan, mtu  string
	storage, templateStorage, apiHost string
	trust, pve                        bool
	action                            string
	retry                             bool
}

// Model is the full-screen installer.
type Model struct {
	S         *Session
	NewRunner func(emit func(Event)) *Runner

	ctx    context.Context
	cancel context.CancelFunc
	stage  stage
	width  int
	height int
	spin   spinner.Model
	bar    progress.Model
	form   *huh.Form
	a      answers
	netErr string

	runner *Runner
	steps  []StepInfo
	state  map[string]StepState
	detail map[string]string
	frac   map[string]float64
	events chan any
	res    *Result

	urlGiven  bool
	noAddr    *NoAddressError
	fail      error
	cancelled bool
	cleaned   bool
}

// NewModel returns the installer for a session. The runner is built when the run starts.
func NewModel(s *Session, newRunner func(emit func(Event)) *Runner) *Model {
	ctx, cancel := context.WithCancel(context.Background())
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(accentS))
	return &Model{
		S: s, NewRunner: newRunner, ctx: ctx, cancel: cancel, width: 80, height: 24,
		spin: sp, bar: progress.New(progress.WithWidth(24)),
		state: map[string]StepState{}, detail: map[string]string{}, frac: map[string]float64{},
		urlGiven: s.Cfg.ProxmoxURL != "",
	}
}

// Err is the failure the installer ended on, nil after a success or a cancel.
func (m *Model) Err() error { return m.fail }

// Cancelled reports whether the user left before the run.
func (m *Model) Cancelled() bool { return m.cancelled }

// Init starts the detection behind one spinner.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, func() tea.Msg { return detectedMsg{m.S.detect(m.ctx)} })
}

func theme(isDark bool) *huh.Styles {
	s := huh.ThemeCharm(isDark)
	s.Focused.Title = s.Focused.Title.Foreground(accent).Bold(true)
	s.Focused.SelectSelector = s.Focused.SelectSelector.Foreground(accent)
	s.Focused.FocusedButton = s.Focused.FocusedButton.Background(accent).Foreground(lipgloss.Color("#1B1B1B"))
	return s
}

func (m *Model) formWidth() int { return min(m.width-4, 76) }

func (m *Model) startForm(groups ...*huh.Group) tea.Cmd {
	m.form = huh.NewForm(groups...).WithTheme(huh.ThemeFunc(theme)).WithWidth(m.formWidth()).WithShowHelp(false)
	return m.form.Init()
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.bar.SetWidth(24)
	case tea.KeyPressMsg:
		switch {
		case msg.String() == "ctrl+c" && m.stage == stRun:
			m.cancel()
			return m, nil
		case msg.String() == "ctrl+c":
			return m.leave()
		case m.stage == stEnd || m.stage == stFail:
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
	case detectedMsg:
		if msg.err != nil {
			return m.failWith(msg.err)
		}
		return m, m.next()
	case resolvedMsg:
		if msg.err != nil {
			return m.failWith(msg.err)
		}
		return m, m.next()
	case probeMsg:
		if msg.used {
			m.netErr = msg.address + " answers on this network"
			m.S.Cfg.IP = ""
			m.stage = stNetwork
			return m, m.networkForm()
		}
		m.netErr = ""
		return m, m.next()
	case eventMsg:
		m.onEvent(Event(msg))
		return m, m.wait()
	case doneMsg:
		return m.onDone(msg)
	case cleanedMsg:
		return m.afterCleanup()
	}
	if m.form != nil {
		f, cmd := m.form.Update(msg)
		if form, ok := f.(*huh.Form); ok {
			m.form = form
		}
		cmds = append(cmds, cmd)
		switch m.form.State {
		case huh.StateCompleted:
			m.form = nil
			cmds = append(cmds, m.formDone())
		case huh.StateAborted:
			m.form = nil
			return m.leave()
		case huh.StateNormal:
		}
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) leave() (tea.Model, tea.Cmd) {
	m.cancelled = true
	m.cancel()
	return m, tea.Quit
}

func (m *Model) failWith(err error) (tea.Model, tea.Cmd) {
	m.fail, m.stage, m.form = err, stFail, nil
	return m, nil
}

// next opens the next question, or the summary.
func (m *Model) next() tea.Cmd {
	n := m.S.Questions()
	switch {
	case n.Network || n.Bridge || n.DNS:
		m.stage = stNetwork
		return m.networkForm()
	case n.Storage || n.TemplateStorage || n.APIHost:
		m.stage = stChoices
		return m.choicesForm(n)
	case n.TrustCA:
		m.stage = stTrust
		return m.trustForm()
	case n.PVEEntry:
		m.stage = stPVE
		return m.startForm(huh.NewGroup(huh.NewConfirm().Title("Proxmox VE entry").
			Description("A Kubernetes button in this node's interface").Affirmative("Add").Negative("Skip").Value(&m.a.pve)))
	default:
		m.stage = stSummary
		m.a.action = "install"
		return m.summaryForm()
	}
}

func (m *Model) networkForm() tea.Cmd {
	f, c, a := m.S.Facts, &m.S.Cfg, &m.a
	if a.mode == "" {
		a.mode = "static"
		if f.NodeDHCP {
			a.mode = "dhcp"
		}
		a.address, a.gateway = "", f.Gateway
		if f.SuggestedIP != "" {
			a.address = fmt.Sprintf("%s/%d", f.SuggestedIP, f.Prefix.Bits())
		}
		if !f.LoopbackDNSOnly {
			a.dns = strings.Join(f.DNS, ",")
		}
		a.ip6, a.vlan, a.mtu = c.IP6, c.VLAN, c.MTU
		a.bridge = c.Bridge
	}
	modeGroup := []huh.Field{huh.NewSelect[string]().Title("Address").
		Options(huh.NewOption("DHCP", "dhcp"), huh.NewOption("Static", "static")).Value(&a.mode)}
	if c.Bridge == "" {
		opts := huh.NewOptions(f.Bridges...)
		modeGroup = append(modeGroup, huh.NewSelect[string]().Title("Bridge").Options(opts...).Value(&a.bridge))
	}
	dnsField := func() *huh.Input {
		return huh.NewInput().Title("DNS").Placeholder("192.0.2.53,192.0.2.54").Value(&a.dns).Validate(func(s string) error {
			if s != "" && !dnsListRE.MatchString(s) {
				return errors.New("comma-separated name server addresses")
			}
			if s == "" && f.LoopbackDNSOnly {
				return errors.New("required, this node's resolver is on loopback")
			}
			return nil
		})
	}
	static := huh.NewGroup(
		huh.NewInput().Title("IPv4 address").Placeholder("192.0.2.40/24").Value(&a.address).Validate(ValidateAddressCIDR),
		huh.NewInput().Title("Gateway").Placeholder("192.0.2.1").Value(&a.gateway).Validate(ValidateIPv4),
		dnsField(),
	).WithHideFunc(func() bool { return a.mode != "static" })
	dhcpDNS := huh.NewGroup(dnsField()).WithHideFunc(func() bool { return a.mode != "dhcp" || !f.LoopbackDNSOnly })
	more := huh.NewGroup(huh.NewConfirm().Title("Advanced").Description("IPv6, bridge, VLAN, MTU").Affirmative("Show").Negative("Skip").Value(&a.more))
	adv := huh.NewGroup(
		huh.NewInput().Title("IPv6").Placeholder("auto, dhcp or 2001:db8::40/64").Value(&a.ip6).Validate(func(s string) error {
			if s != "" && s != "auto" && s != "dhcp" && !IsIPv6CIDR(s) {
				return errors.New("auto, dhcp or an address with its prefix")
			}
			return nil
		}),
		huh.NewInput().Title("IPv6 gateway").Value(&a.gateway6).Validate(func(s string) error {
			if s != "" && !IsIPv6(s) {
				return errors.New("an IPv6 address")
			}
			return nil
		}),
		huh.NewSelect[string]().Title("Bridge").Options(huh.NewOptions(f.Bridges...)...).Value(&a.bridge),
		huh.NewInput().Title("VLAN").Placeholder("none").Value(&a.vlan).Validate(func(s string) error {
			if s != "" && !inRange(s, 1, 4094) {
				return errors.New("1 to 4094")
			}
			return nil
		}),
		huh.NewInput().Title("MTU").Placeholder("bridge default").Value(&a.mtu).Validate(func(s string) error {
			if s != "" && !inRange(s, 576, 65535) {
				return errors.New("576 to 65535")
			}
			return nil
		}),
	).WithHideFunc(func() bool { return !a.more })
	return m.startForm(huh.NewGroup(modeGroup...), static, dhcpDNS, more, adv)
}

func (m *Model) choicesForm(n Needs) tea.Cmd {
	f, a := m.S.Facts, &m.a
	var fields []huh.Field
	storageOpts := func(list []Storage) []huh.Option[string] {
		opts := make([]huh.Option[string], len(list))
		for i, s := range list {
			opts[i] = huh.NewOption(fmt.Sprintf("%s  %s, %s", s.Name, s.Type, s.FreeText()), s.Name)
		}
		return opts
	}
	if n.Storage {
		a.storage = f.RootStorages[0].Name
		fields = append(fields, huh.NewSelect[string]().Title("Disk storage").Options(storageOpts(f.RootStorages)...).Value(&a.storage))
	}
	if n.TemplateStorage {
		a.templateStorage = f.TemplateStorages[0].Name
		fields = append(fields, huh.NewSelect[string]().Title("Template storage").Options(storageOpts(f.TemplateStorages)...).Value(&a.templateStorage))
	}
	if n.APIHost {
		tied := TiedCandidates(f.Candidates)
		opts := make([]huh.Option[string], len(tied))
		for i, c := range tied {
			opts[i] = huh.NewOption(c.Host+"  "+c.What, c.Host)
		}
		a.apiHost = tied[0].Host
		fields = append(fields, huh.NewSelect[string]().Title("Proxmox VE API address").Options(opts...).Value(&a.apiHost))
	}
	return m.startForm(huh.NewGroup(fields...))
}

func (m *Model) trustForm() tea.Cmd {
	ca := m.S.CA
	m.a.trust = false
	return m.startForm(huh.NewGroup(huh.NewConfirm().Title("Trust this CA for the Proxmox VE API?").
		Description("Subject  " + ca.Subject + "\nIssuer   " + ca.Issuer + "\nSHA-256  " + ca.Fingerprint[:47] + "\n         " + ca.Fingerprint[48:]).
		Affirmative("Trust").Negative("Refuse").Value(&m.a.trust)))
}

func (m *Model) summaryForm() tea.Cmd {
	opts := []huh.Option[string]{huh.NewOption("Install", "install"), huh.NewOption("Change network", "network")}
	if len(m.S.Facts.RootStorages) > 1 || len(m.S.Facts.TemplateStorages) > 1 {
		opts = append(opts, huh.NewOption("Change storage", "storage"))
	}
	if !m.urlGiven && len(TiedCandidates(m.S.Facts.Candidates)) > 1 {
		opts = append(opts, huh.NewOption("Change API address", "api"))
	}
	opts = append(opts, huh.NewOption("Change Proxmox VE entry", "pve"), huh.NewOption("Cancel", "cancel"))
	return m.startForm(huh.NewGroup(huh.NewSelect[string]().Title("Next").Options(opts...).Value(&m.a.action)))
}

// formDone applies a finished form and moves on.
func (m *Model) formDone() tea.Cmd {
	c, a := &m.S.Cfg, &m.a
	switch m.stage {
	case stNetwork:
		if a.mode == "dhcp" {
			c.IP, c.Gateway = "dhcp", ""
		} else {
			c.IP, c.Gateway = a.address, a.gateway
		}
		if a.dns != "" {
			c.DNS = a.dns
		}
		c.IP6, c.Gateway6, c.VLAN, c.MTU = a.ip6, a.gateway6, a.vlan, a.mtu
		bridge := c.Bridge
		if a.bridge != "" {
			bridge = a.bridge
		}
		return func() tea.Msg {
			if bridge != c.Bridge || c.Bridge == "" {
				m.S.RefreshForBridge(m.ctx, bridge)
			}
			if a.mode == "static" {
				addr := strings.SplitN(c.IP, "/", 2)[0]
				return probeMsg{address: addr, used: m.S.Node.AddressInUse(m.ctx, c.Bridge, addr)}
			}
			return probeMsg{}
		}
	case stChoices:
		if a.storage != "" {
			c.Storage = a.storage
		}
		if a.templateStorage != "" {
			c.TemplateStorage = a.templateStorage
		}
		if a.apiHost != "" {
			c.APIHost = a.apiHost
			return m.resolve()
		}
		return m.next()
	case stTrust:
		if !a.trust {
			m.fail = apiFailure(true, c.ProxmoxURL, "8006", "")
			m.stage = stFail
			return nil
		}
		c.TrustServedCA = true
		return m.next()
	case stPVE:
		c.PVEEntry = "no"
		if a.pve {
			c.PVEEntry = "yes"
		}
		return m.next()
	case stSummary:
		return m.summaryAction()
	case stRetry:
		if !a.retry {
			m.fail = m.runFailure()
			m.stage = stFail
			return nil
		}
		c.IP, a.mode = "", "static"
		return m.next()
	case stDetect, stRun, stEnd, stFail:
	}
	return nil
}

func (m *Model) resolve() tea.Cmd {
	return func() tea.Msg { return resolvedMsg{m.S.ResolveAPI(m.ctx, false)} }
}

func (m *Model) summaryAction() tea.Cmd {
	c := &m.S.Cfg
	switch m.a.action {
	case "install":
		return m.startRun()
	case "network":
		c.IP = ""
	case "storage":
		c.Storage, c.TemplateStorage = "", ""
		if err := m.S.FillStorages(false); err != nil {
			m.fail, m.stage = err, stFail
			return nil
		}
	case "api":
		c.APIHost, c.ProxmoxURL, m.S.CA = "", "", nil
		return m.resolve()
	case "pve":
		c.PVEEntry = ""
	case "cancel":
		m.cancelled = true
		return tea.Quit
	}
	return m.next()
}

func (m *Model) startRun() tea.Cmd {
	m.stage = stRun
	if m.events == nil {
		m.events = make(chan any, 256)
	}
	if m.runner == nil {
		m.runner = m.NewRunner(func(e Event) {
			select {
			case m.events <- eventMsg(e):
			case <-m.ctx.Done():
			}
		})
	}
	m.steps = m.runner.Steps()
	for _, st := range m.steps {
		if m.state[st.ID] != StepDone {
			m.state[st.ID] = StepPending
		}
	}
	go func() {
		res, err := m.runner.Run(m.ctx)
		m.events <- doneMsg{res, err}
	}()
	return tea.Batch(m.wait(), m.spin.Tick)
}

func (m *Model) wait() tea.Cmd {
	events := m.events
	return func() tea.Msg { return <-events }
}

func (m *Model) onEvent(e Event) {
	m.state[e.Step] = e.State
	if e.Detail != "" {
		m.detail[e.Step] = e.Detail
	}
	if e.Total > 0 {
		m.frac[e.Step] = float64(e.Done) / float64(e.Total)
	}
}

func (m *Model) runFailure() error {
	if m.fail != nil {
		return m.fail
	}
	return errors.New("the install did not finish")
}

func (m *Model) onDone(d doneMsg) (tea.Model, tea.Cmd) {
	if d.err == nil {
		m.res, m.stage = d.res, stEnd
		return m, nil
	}
	m.fail = d.err
	var na *NoAddressError
	m.noAddr = nil
	if errors.As(d.err, &na) && na.DHCP && m.ctx.Err() == nil {
		m.noAddr = na
	}
	if m.ctx.Err() != nil {
		m.fail = errors.New("cancelled")
	}
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), 2*time.Minute)
		defer cancel()
		m.runner.Cleanup(ctx)
		return cleanedMsg{}
	}
}

func (m *Model) afterCleanup() (tea.Model, tea.Cmd) {
	m.cleaned = true
	if m.fail != nil && m.fail.Error() == "cancelled" {
		m.cancelled = true
		return m, tea.Quit
	}
	if m.noAddr != nil {
		m.stage = stRetry
		m.a.retry = true
		return m, m.startForm(huh.NewGroup(huh.NewConfirm().Title("No address from DHCP").
			Description("The container is removed. Use a static address?").Affirmative("Use static").Negative("Stop").Value(&m.a.retry)))
	}
	m.stage = stFail
	return m, nil
}

// View implements tea.Model.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	return v
}

func (m *Model) render() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	switch m.stage {
	case stDetect:
		b.WriteString("  " + m.spin.View() + " Detecting this node\n")
	case stSummary:
		b.WriteString(m.summary())
		b.WriteString(m.formView())
	case stRun:
		b.WriteString(m.runView())
	case stEnd:
		b.WriteString(m.endView())
	case stFail:
		b.WriteString(m.failView())
	case stNetwork, stChoices, stTrust, stPVE, stRetry:
		if m.netErr != "" && m.stage == stNetwork {
			b.WriteString("  " + badS.Render(m.netErr) + "\n\n")
		}
		b.WriteString(m.formView())
	}
	return b.String()
}

func (m *Model) formView() string {
	if m.form == nil {
		return ""
	}
	return indent(m.form.View(), "  ") + "\n" + dim.Render("  enter confirm  ctrl+c leave") + "\n"
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

func (m *Model) header() string {
	mark := []string{" ▟▀▜ ▟▀▜ ", " █ █ █ █ ", " ▀▀▘ ▀▀▘ "}
	var crumbs []string
	for _, s := range stageNames {
		label := s.label
		active := false
		for _, st := range s.stages {
			active = active || st == m.stage
		}
		if active {
			crumbs = append(crumbs, accentS.Bold(true).Render(label))
		} else {
			crumbs = append(crumbs, dim.Render(label))
		}
	}
	right := []string{bold.Render("Kubehenge"), dim.Render("Appliance installer"), strings.Join(crumbs, dim.Render("  ·  "))}
	var b strings.Builder
	for i := range mark {
		b.WriteString(accentS.Render(mark[i]) + "  " + right[i] + "\n")
	}
	return b.String()
}

// row renders a label and a value in two columns.
func row(label, value string) string {
	return "  " + dim.Render(fmt.Sprintf("%-18s", label)) + value + "\n"
}

func (m *Model) summary() string {
	c, f := &m.S.Cfg, m.S.Facts
	var b strings.Builder
	address := "DHCP"
	if c.IP != "dhcp" {
		address = c.IP
	}
	b.WriteString(row("Address", address))
	if c.Gateway != "" {
		b.WriteString(row("Gateway", c.Gateway))
	}
	dns := c.DNS
	if dns == "" {
		dns = "this node's: " + strings.Join(f.DNS, ", ")
	}
	b.WriteString(row("DNS", dns))
	net := c.Bridge
	if c.VLAN != "" {
		net += ", VLAN " + c.VLAN
	}
	if c.MTU != "" {
		net += ", MTU " + c.MTU
	}
	b.WriteString(row("Bridge", net))
	if c.IP6 != "" {
		b.WriteString(row("IPv6", c.IP6+" "+c.Gateway6))
	}
	b.WriteString(row("Container", c.CTID+", "+c.Hostname))
	b.WriteString(row("Disk storage", c.Storage))
	b.WriteString(row("Template storage", c.TemplateStorage))
	b.WriteString(row("Proxmox VE API", c.ProxmoxURL))
	if m.S.CA != nil {
		b.WriteString(row("API CA", caText(m.S.CA, c)))
	}
	if c.Name != "" {
		b.WriteString(row("DNS name", c.Name))
	}
	entry := "no"
	if c.PVEEntry == "yes" {
		entry = "yes"
	}
	b.WriteString(row("Proxmox VE entry", entry))
	b.WriteString("\n")
	return b.String()
}

func caText(ca *CAChoice, c *Config) string {
	switch ca.Kind {
	case CAInsecure:
		return "not checked"
	case CACluster:
		return "this cluster's CA"
	case CASystem:
		return "trust store: " + ca.Subject
	case CAServed:
		return "served, trusted by you: " + ca.Subject
	case CAGiven:
		return c.ProxmoxCA
	}
	return ""
}

func (m *Model) runView() string {
	var b strings.Builder
	for _, st := range m.steps {
		icon, label := dim.Render("·"), dim.Render(st.Label)
		switch m.state[st.ID] {
		case StepRunning:
			icon, label = m.spin.View(), bold.Render(st.Label)
		case StepDone:
			icon, label = goodS.Render("✓"), st.Label
		case StepFailed:
			icon, label = badS.Render("✗"), st.Label
		case StepSkipped:
			icon, label = dim.Render("-"), st.Label
		case StepPending:
		}
		tail := dim.Render(m.detail[st.ID])
		if f, ok := m.frac[st.ID]; ok && m.state[st.ID] == StepRunning {
			tail = m.bar.ViewAs(f)
		}
		b.WriteString(fmt.Sprintf("  %s %-20s %s\n", icon, label, tail))
	}
	b.WriteString("\n" + dim.Render("  ctrl+c stops and removes the container") + "\n")
	return b.String()
}

func (m *Model) endView() string {
	r := m.res
	var b strings.Builder
	b.WriteString("  " + goodS.Bold(true).Render("Kubehenge is running") + "\n\n")
	b.WriteString(row("Open", accentS.Underline(true).Render(r.URL)))
	b.WriteString(row("Sign in", "a Proxmox VE administrator"))
	b.WriteString(row("Container", r.CTID))
	b.WriteString(row("Version", r.Version))
	b.WriteString(row("Fingerprint", r.Fingerprint))
	b.WriteString(row("Valid until", r.ValidUntil.UTC().Format("2006-01-02")))
	if r.Name != "" {
		b.WriteString(row("DNS", "point "+r.Name+" at "+r.Address))
	}
	b.WriteString(row("Log", r.LogPath))
	for _, w := range r.Warnings {
		b.WriteString("\n  " + badS.Render(w) + "\n")
	}
	b.WriteString("\n" + dim.Render("  any key closes") + "\n")
	return b.String()
}

func (m *Model) failView() string {
	var b strings.Builder
	b.WriteString("  " + badS.Bold(true).Render("Stopped") + "\n\n")
	var u *UserError
	var na *NoAddressError
	switch {
	case errors.As(m.fail, &na):
		u = &na.UserError
	case errors.As(m.fail, &u):
	default:
		u = &UserError{Cause: m.fail.Error()}
	}
	b.WriteString(row("Cause", u.Cause))
	if u.Fix != "" {
		b.WriteString(row("Do", u.Fix))
	}
	if m.runner != nil {
		b.WriteString(row("Log", m.runner.LogPath))
		if m.cleaned {
			b.WriteString(row("Container", "removed"))
		}
	} else {
		b.WriteString(row("Changed", "nothing"))
	}
	b.WriteString("\n" + dim.Render("  any key closes") + "\n")
	return b.String()
}
