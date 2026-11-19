package installer

import (
	"crypto"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The columns of the screens: two spaces, a label of this width, then the value. A summary too
// tall for the screen gives its notes the narrower label column.
const (
	summaryLabelWidth = 18
	noteLabelWidth    = 6
	summaryMargin     = 2
	minValueWidth     = 20
)

// neutralWords says a flag in the words of a screen, where the user is not typing flags.
var neutralWords = strings.NewReplacer(
	"--ip <address>/<prefix> --gateway <address>", "a static IPv4 address and gateway",
	"--ip6 and --gateway6", "a static IPv6 address and gateway",
	"--proxmox-insecure", "the unchecked API certificate",
	"--proxmox-url", "the Proxmox VE API address",
	"--proxmox-ca <file>", "the API CA file",
	"--proxmox-ca", "the API CA file",
	"--template-storage", "the template storage",
	"--trusted-proxy", "the trusted proxies",
	"--gateway6", "the IPv6 gateway",
	"--gateway", "the IPv4 gateway",
	"--hostname", "the host name",
	"--storage", "the disk storage",
	"--bridge", "the network",
	"--ctid", "the container ID",
	"--ip6", "the IPv6 address",
	"--ip", "the IPv4 address",
	"--vlan", "the VLAN",
	"--mtu", "the MTU",
	"--dns", "the DNS servers",
	"--name", "the DNS name",
	"--cert", "the certificate",
	"--key", "the key",
	"--yes", "the yes at the summary",
)

func neutral(text string) string { return neutralWords.Replace(text) }

// fingerprintLines splits a SHA-256 fingerprint in two halves, so that it is never cut mid-byte.
func fingerprintLines(fp string) string {
	const half = 47
	if len(fp) <= half+1 {
		return fp
	}
	return fp[:half] + "\n" + fp[half+1:]
}

// wrap breaks text to the width.
func wrap(text string, width int) []string {
	return strings.Split(ansi.Wrap(text, max(width, minValueWidth), ""), "\n")
}

// row renders a label and a value in two columns, wrapping the value under itself.
func (m *Model) row(label, value string) string {
	return m.rowIn(summaryLabelWidth, label, value)
}

// rowIn is row with a label column of the width given.
func (m *Model) rowIn(labelWidth int, label, value string) string {
	lines := wrap(value, m.width-labelWidth-summaryMargin-1)
	var b strings.Builder
	for i, l := range lines {
		name := ""
		if i == 0 {
			name = label
		}
		b.WriteString(strings.Repeat(" ", summaryMargin))
		b.WriteString(dim.Render(fmt.Sprintf("%-*s", labelWidth, name)))
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Model) render() string {
	if m.stage == stSummary {
		return m.summaryScreen()
	}
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	switch m.stage {
	case stDetect:
		b.WriteString("  ")
		b.WriteString(m.spin.View())
		b.WriteString(" ")
		b.WriteString(m.busy)
		b.WriteString("\n")
	case stFound:
		b.WriteString(m.foundView())
	case stRun:
		b.WriteString(m.runView())
	case stEnd:
		b.WriteString(m.endView())
	case stFail:
		b.WriteString(m.failView())
		b.WriteString(m.formView())
	case stAsk, stList, stConfirm:
		if m.note != "" {
			b.WriteString(m.noteView())
		}
		b.WriteString(m.formView())
	case stSummary:
	}
	return b.String()
}

func (m *Model) noteView() string {
	var b strings.Builder
	for _, l := range wrap(neutral(m.note), m.width-summaryMargin-1) {
		b.WriteString("  ")
		b.WriteString(badS.Render(l))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

func (m *Model) formView() string {
	if m.form == nil {
		return ""
	}
	keys := m.keys
	if m.leaveAsked {
		keys = "ctrl+c again leaves, your answers are kept for the next run"
	}
	return indent(m.form.View(), "  ") + "\n" + dim.Render("  "+keys) + "\n"
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

// The trilithon, three lines, beside the name and the crumbs.
var logo = []string{" ▄▄▄▄▄▄▄▄▄ ", " ▀██▀▀▀██▀ ", "  ██   ██  "}

// crumbLine is the crumbs with the stage the installer is at lit.
func (m *Model) crumbLine() string {
	here := m.crumb()
	crumbs := make([]string, 0, len(crumbNames))
	for i, name := range crumbNames {
		if i == here {
			crumbs = append(crumbs, accentS.Bold(true).Render(name))
		} else {
			crumbs = append(crumbs, dim.Render(name))
		}
	}
	return strings.Join(crumbs, dim.Render(" · "))
}

func (m *Model) header() string {
	right := []string{bold.Render("Kubehenge"), dim.Render("Appliance installer"), m.crumbLine()}
	var b strings.Builder
	for i, l := range logo {
		b.WriteString(tealS.Render(l))
		b.WriteString("  ")
		b.WriteString(right[i])
		b.WriteString("\n")
	}
	return b.String()
}

// compactHeader is the header in one line: the name and the crumbs.
func (m *Model) compactHeader() string {
	return "  " + bold.Render("Kubehenge") + "  " + m.crumbLine() + "\n"
}

// foundView shows what the detection found, and that earlier answers came back.
func (m *Model) foundView() string {
	c, f := &m.S.Cfg, m.S.Facts
	var b strings.Builder
	b.WriteString(m.row("Node", f.Node))
	b.WriteString(m.row("Networks", fmt.Sprintf("%d, this node uses %s", len(f.Networks), orNone(f.DefaultBridge))))
	b.WriteString(m.row("Storage", storageText(f.RootStorages)))
	if c.ProxmoxURL != "" {
		b.WriteString(m.row("Proxmox VE API", c.ProxmoxURL))
	}
	if m.S.CA != nil {
		b.WriteString(m.row("API CA", caText(m.S.CA, c)))
	}
	for _, n := range m.S.Notices {
		// The Storage row says what the storage notices say.
		if !strings.HasPrefix(n, "storage ") {
			b.WriteString(m.row("Note", n))
		}
	}
	if m.S.Restored || len(m.S.Dropped) > 0 {
		b.WriteString("\n")
	}
	if m.S.Restored {
		b.WriteString(m.row("Restored", "your answers from the last run. Nothing secret is saved. Change any on the summary."))
	}
	if d := m.S.Dropped; len(d) > 0 {
		list := d[len(d)-1]
		if len(d) > 1 {
			list = strings.Join(d[:len(d)-1], ", ") + " and " + list
		}
		b.WriteString(m.row("Dropped", "the saved "+list+", which this version refuses"))
	}
	b.WriteString("\n")
	b.WriteString(dim.Render("  enter continue  ctrl+c leave"))
	b.WriteString("\n")
	return b.String()
}

// storageText says which storages take container disks.
func storageText(list []Storage) string {
	if len(list) == 1 {
		return list[0].Name + ", the only one for container disks"
	}
	return fmt.Sprintf("%d take container disks", len(list))
}

// summaryScreen draws the summary with the form under it. A screen too short for it loses first
// the logo and the blank lines, then the width of the cautions' label column. Each caution stays
// whole.
func (m *Model) summaryScreen() string {
	const lastLevel = 2
	var out string
	for level := 0; level <= lastLevel; level++ {
		out = m.summaryAt(level)
		if strings.Count(strings.TrimRight(out, "\n"), "\n")+1 <= m.height {
			break
		}
	}
	return out
}

func (m *Model) summaryAt(level int) string {
	var b strings.Builder
	sep, head, noteWidth := "\n", m.header(), summaryLabelWidth
	if level > 0 {
		sep, head = "", m.compactHeader()
	}
	if level > 1 {
		noteWidth = noteLabelWidth
	}
	b.WriteString(head)
	b.WriteString(sep)
	for _, r := range m.S.Rows(!m.S.Cfg.KeepFailed) {
		b.WriteString(m.row(r.Label, r.Value))
	}
	for _, n := range m.S.Cautions {
		b.WriteString(m.rowIn(noteWidth, "Note", causeThenFix(n)))
	}
	b.WriteString(sep)
	b.WriteString(m.formView())
	return b.String()
}

// causeThenFix starts a caution's fix, its text after the first sentence, on a line of its own.
func causeThenFix(text string) string {
	if cause, fix, ok := strings.Cut(text, ". "); ok {
		return cause + ".\n" + fix
	}
	return text
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

func (m *Model) elapsed(id string) string {
	start, ok := m.started[id]
	if !ok {
		return ""
	}
	return dim.Render(" " + m.now().Sub(start).Round(time.Second).String())
}

func (m *Model) runView() string {
	var b strings.Builder
	steps := m.steps
	if _, ok := m.state["remove"]; ok {
		steps = append(slices.Clip(steps), StepInfo{"remove", "Remove container"})
	}
	const labelWidth = 20
	for _, st := range steps {
		icon, label := dim.Render("·"), dim.Render(st.Label)
		pad := strings.Repeat(" ", max(labelWidth-len([]rune(st.Label)), 1))
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
		if m.state[st.ID] == StepRunning {
			tail += m.elapsed(st.ID)
		}
		for _, s := range []string{"  ", icon, " ", label, pad, tail, "\n"} {
			b.WriteString(s)
		}
	}
	stop := "  ctrl+c quit and remove"
	switch {
	case m.state["remove"] == StepRunning:
		// The removal runs to its end whatever the key, and the installer closes after it.
		stop = "  removing container " + m.S.Cfg.CTID + ", ctrl+c waits for it"
	case m.keeps():
		stop = "  ctrl+c quit and keep"
	}
	b.WriteString("\n")
	b.WriteString(dim.Render(stop))
	b.WriteString("\n")
	return b.String()
}

func (m *Model) endView() string {
	r := m.res
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(goodS.Bold(true).Render("Kubehenge is running"))
	b.WriteString("\n\n")
	b.WriteString(m.row("Open", accentS.Underline(true).Render(r.URL)))
	b.WriteString(m.row("Sign in", "a Proxmox VE administrator"))
	b.WriteString(m.row("Container", r.CTID+", version "+r.Version))
	b.WriteString(m.row("Certificate", "valid until "+r.ValidUntil.UTC().Format(time.DateOnly)))
	b.WriteString(m.row(crypto.SHA256.String(), fingerprintLines(r.Fingerprint)))
	if r.Name != "" {
		b.WriteString(m.row("Next", "point "+r.Name+" at "+r.Address))
	}
	b.WriteString(m.row("Log", r.LogPath))
	for _, w := range r.Warnings {
		b.WriteString("\n")
		for _, l := range wrap(w, m.width-summaryMargin-1) {
			b.WriteString("  ")
			b.WriteString(badS.Render(l))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(dim.Render("  any key closes"))
	b.WriteString("\n")
	return b.String()
}

func (m *Model) failView() string {
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(badS.Bold(true).Render("Stopped"))
	b.WriteString("\n\n")
	var u *UserError
	var na *NoAddressError
	switch {
	case errors.As(m.fail, &na):
		u = &na.UserError
	case errors.As(m.fail, &u):
	default:
		u = &UserError{Cause: m.fail.Error()}
	}
	for _, st := range m.steps {
		if m.state[st.ID] == StepFailed {
			b.WriteString(m.row("Step", st.Label))
		}
	}
	b.WriteString(m.row("Cause", neutral(u.Cause)))
	if u.Fix != "" {
		fix := u.Fix
		if m.runner != nil && m.runner.LogPath != "" {
			// The Log row below gives the path, so the action only names the log.
			fix = strings.ReplaceAll(fix, "See "+m.runner.LogPath, "Read the log")
		}
		b.WriteString(m.row("Do", neutral(fix)))
	}
	if m.runner != nil {
		b.WriteString(m.row("Log", m.runner.LogPath))
		if m.runner.Made() {
			b.WriteString(m.row("Container", m.S.Cfg.CTID+" is kept until you choose"))
		}
	} else {
		b.WriteString(m.row("Changed", "nothing"))
	}
	b.WriteString("\n")
	return b.String()
}
