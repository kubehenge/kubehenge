package installer

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The columns of the screens: two spaces, a label of this width, then the value.
const (
	summaryLabelWidth = 17
	summaryMargin     = 2
	minValueWidth     = 20
)

// neutralWords says a flag in the words of a screen, where the user is not typing flags.
var neutralWords = strings.NewReplacer(
	"--ip <address>/<prefix> --gateway <address>", "a static IPv4 address and gateway",
	"--ip6 and --gateway6", "a static IPv6 address and gateway",
	"--proxmox-insecure", "the unchecked API certificate",
	"--proxmox-url", "the Proxmox VE API address",
	"--proxmox-ca", "the Proxmox VE API CA",
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
	lines := wrap(value, m.width-summaryLabelWidth-summaryMargin-1)
	var b strings.Builder
	for i, l := range lines {
		name := ""
		if i == 0 {
			name = label
		}
		b.WriteString(strings.Repeat(" ", summaryMargin) + dim.Render(fmt.Sprintf("%-*s", summaryLabelWidth, name)) + l + "\n")
	}
	return b.String()
}

func (m *Model) render() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	switch m.stage {
	case stDetect:
		b.WriteString("  " + m.spin.View() + " " + m.busy + "\n")
	case stFound:
		b.WriteString(m.foundView())
	case stSummary:
		b.WriteString(m.summaryView())
		b.WriteString(m.formView())
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
	}
	return b.String()
}

func (m *Model) noteView() string {
	var b strings.Builder
	for _, l := range wrap(neutral(m.note), m.width-summaryMargin-1) {
		b.WriteString("  " + badS.Render(l) + "\n")
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

func (m *Model) header() string {
	here := m.crumb()
	crumbs := make([]string, 0, len(crumbNames))
	for i, name := range crumbNames {
		if i == here {
			crumbs = append(crumbs, accentS.Bold(true).Render(name))
		} else {
			crumbs = append(crumbs, dim.Render(name))
		}
	}
	right := []string{bold.Render("Kubehenge"), dim.Render("Appliance installer"), strings.Join(crumbs, dim.Render(" · "))}
	var b strings.Builder
	for i, l := range logo {
		b.WriteString(tealS.Render(l) + "  " + right[i] + "\n")
	}
	return b.String()
}

// foundView shows what the detection found, and that earlier answers came back.
func (m *Model) foundView() string {
	c, f := &m.S.Cfg, m.S.Facts
	var b strings.Builder
	b.WriteString(m.row("Node", f.Node))
	b.WriteString(m.row("Networks", fmt.Sprintf("%d, this node uses %s", len(f.Networks), orNone(f.DefaultBridge))))
	b.WriteString(m.row("Disk storage", fmt.Sprintf("%d take container disks", len(f.RootStorages))))
	if c.ProxmoxURL != "" {
		b.WriteString(m.row("Proxmox VE API", c.ProxmoxURL))
	}
	if m.S.CA != nil {
		b.WriteString(m.row("API CA", caText(m.S.CA, c)))
	}
	for _, n := range m.S.Notices {
		b.WriteString(m.row("Note", n))
	}
	if m.S.Restored {
		b.WriteString("\n" + m.row("Restored", "your answers from the last run. Nothing secret is saved. Change any on the summary."))
	}
	b.WriteString("\n" + dim.Render("  enter continue  ctrl+c leave") + "\n")
	return b.String()
}

func (m *Model) summaryView() string {
	var b strings.Builder
	for _, r := range m.S.Rows(!m.S.Cfg.KeepFailed) {
		b.WriteString(m.row(r.Label, r.Value))
	}
	for _, n := range m.S.Cautions {
		b.WriteString(m.row("Note", n))
	}
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
		steps = append(steps[:len(steps):len(steps)], StepInfo{"remove", "Remove container"})
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
		b.WriteString("  " + icon + " " + label + pad + tail + "\n")
	}
	b.WriteString("\n" + dim.Render("  ctrl+c stops and removes the container") + "\n")
	return b.String()
}

func (m *Model) endView() string {
	r := m.res
	var b strings.Builder
	b.WriteString("  " + goodS.Bold(true).Render("Kubehenge is running") + "\n\n")
	b.WriteString(m.row("Open", accentS.Underline(true).Render(r.URL)))
	b.WriteString(m.row("Sign in", "a Proxmox VE administrator"))
	b.WriteString(m.row("Container", r.CTID+", version "+r.Version))
	b.WriteString(m.row("Certificate", "valid until "+r.ValidUntil.UTC().Format(time.DateOnly)))
	b.WriteString(m.row("SHA-256", fingerprintLines(r.Fingerprint)))
	if r.Name != "" {
		b.WriteString(m.row("Next", "point "+r.Name+" at "+r.Address))
	}
	b.WriteString(m.row("Log", r.LogPath))
	for _, w := range r.Warnings {
		b.WriteString("\n")
		for _, l := range wrap(w, m.width-summaryMargin-1) {
			b.WriteString("  " + badS.Render(l) + "\n")
		}
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
	for _, st := range m.steps {
		if m.state[st.ID] == StepFailed {
			b.WriteString(m.row("Step", st.Label))
		}
	}
	b.WriteString(m.row("Cause", neutral(u.Cause)))
	if u.Fix != "" {
		b.WriteString(m.row("Do", neutral(u.Fix)))
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
