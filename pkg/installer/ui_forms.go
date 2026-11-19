package installer

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

// setting is one thing the installer asks, with the form that asks it and what answering does.
type setting struct {
	label string
	// applies says the setting exists for the choices made so far.
	applies func(m *Model) bool
	// open says the options and the node leave it unanswered. Nil means always.
	open   func(m *Model) bool
	value  func(m *Model) string
	fields func(m *Model) []huh.Field
	// apply stores the answers. A command it returns runs before the next question.
	apply func(m *Model) tea.Cmd
}

// needed says the setting still wants an answer: it applies, nothing settled it, and it is open.
func (m *Model) needed(id string) bool {
	st := m.table[id]
	if st == nil || !st.applies(m) || m.answered[id] || m.given[id] {
		return false
	}
	return st.open == nil || st.open(m)
}

// The order the questions come in.
var settingOrder = []string{
	settingAPI, settingTrust, settingFamily, settingNetwork, settingVLAN,
	settingV4Mode, settingV4Addr, settingV4Gateway, settingV6Mode, settingV6Addr, settingV6Gateway,
	settingDNS, settingName, settingCert, settingStorage, settingTemplate, settingEntry,
}

// The settings the summary lets the user change.
var settingList = []string{
	settingFamily, settingNetwork, settingVLAN, settingV4Mode, settingV4Addr, settingV4Gateway,
	settingV6Mode, settingV6Addr, settingV6Gateway, settingDNS, settingMTU, settingCTID, settingHostname,
	settingName, settingCert, settingStorage, settingTemplate, settingAPI, settingProxies, settingEntry,
}

var storageSettings = []string{settingStorage, settingTemplate}

// interfaceSettings are the questions of the Interface step.
var interfaceSettings = []string{settingName, settingCert}

// networkSettings are the settings a kept container takes by a change of its network alone.
var networkSettings = []string{
	settingFamily, settingNetwork, settingVLAN, settingV4Mode, settingV4Addr, settingV4Gateway,
	settingV6Mode, settingV6Addr, settingV6Gateway, settingDNS, settingMTU,
}

// listed says the setting is shown in the list of settings.
func (m *Model) listed(id string) bool {
	st := m.table[id]
	return st != nil && st.applies(m) && st.value != nil
}

func always(*Model) bool { return true }

func input(title string, v *string, placeholder string, validate func(string) error) *huh.Input {
	in := huh.NewInput().Title(title).Value(v).Placeholder(placeholder)
	if validate != nil {
		in = in.Validate(validate)
	}
	return in
}

func choose(title string, v *string, opts ...huh.Option[string]) *huh.Select[string] {
	return huh.NewSelect[string]().Title(title).Options(opts...).Value(v)
}

func keysOf(fields []huh.Field) string {
	if len(fields) > 1 {
		return "enter next  esc back  ctrl+c quit"
	}
	if len(fields) == 1 {
		switch f := fields[0].(type) {
		case *huh.Input:
			return "enter confirm  esc back  ctrl+c quit"
		case *huh.Confirm:
			return "←→ choose  enter confirm  esc back  ctrl+c quit"
		case *huh.Select[string]:
			if f.GetFiltering() {
				return "type to filter  ↑↓ choose  enter confirm  esc back  ctrl+c quit"
			}
		case *huh.FilePicker, *huh.MultiSelect[string], *huh.Note, *huh.Text:
		}
	}
	return "↑↓ choose  enter confirm  esc back  ctrl+c quit"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func validateDNS(f *Facts) func(string) error {
	return func(s string) error {
		if s != "" && !dnsListRE.MatchString(s) {
			return errors.New("enter name server addresses, comma-separated")
		}
		if s == "" && f.LoopbackDNSOnly {
			return errors.New("this node's resolver is on loopback, so give name servers")
		}
		return nil
	}
}

func validateRange(message string, low, high int) func(string) error {
	return func(s string) error {
		if s != "" && !inRange(s, low, high) {
			return errors.New(message)
		}
		return nil
	}
}

func storageOptions(list []Storage) []huh.Option[string] {
	opts := make([]huh.Option[string], len(list))
	for i, s := range list {
		opts[i] = huh.NewOption(fmt.Sprintf("%s  %s, %s", s.Name, s.Type, s.FreeText()), s.Name)
	}
	return opts
}

// resetAddressing forgets the addresses and gateways when the network they belong to changed.
func (m *Model) resetAddressing() {
	c := &m.S.Cfg
	for _, id := range []string{settingV4Addr, settingV4Gateway, settingV6Addr, settingV6Gateway} {
		m.answered[id], m.given[id] = false, false
	}
	if modeOf(c.IP) == optStatic {
		c.IP, c.Gateway = "", ""
	}
	if modeOf(c.IP6) == optStatic {
		c.IP6, c.Gateway6 = "", ""
	}
	m.a.gateway, m.a.gwNote = "", ""
}

func hasV4(m *Model) bool { return m.a.family != "v6" }
func hasV6(m *Model) bool { return m.a.family != "v4" }

func (m *Model) static4() bool { return m.a.family != "v6" && m.a.mode4 == optStatic }
func (m *Model) static6() bool { return m.a.family != "v4" && m.a.mode6 == optStatic }

// newSettings builds the table of settings.
func newSettings() map[string]*setting {
	return map[string]*setting{
		settingFamily:    familySetting(),
		settingNetwork:   networkSetting(),
		settingVLAN:      vlanSetting(),
		settingV4Mode:    v4ModeSetting(),
		settingV4Addr:    v4AddrSetting(),
		settingV4Gateway: v4GatewaySetting(),
		settingV6Mode:    v6ModeSetting(),
		settingV6Addr:    v6AddrSetting(),
		settingV6Gateway: v6GatewaySetting(),
		settingDNS:       dnsSetting(),
		settingMTU:       mtuSetting(),
		settingCTID:      ctidSetting(),
		settingHostname:  hostnameSetting(),
		settingName:      nameSetting(),
		settingCert:      certSetting(),
		settingStorage:   storageSetting(),
		settingTemplate:  templateSetting(),
		settingAPI:       apiSetting(),
		settingTrust:     trustSetting(),
		settingProxies:   proxiesSetting(),
		settingEntry:     entrySetting(),
	}
}

func familySetting() *setting {
	return &setting{
		label:   "IP version",
		applies: always,
		value: func(m *Model) string {
			switch m.a.family {
			case "v6":
				return "IPv6 only"
			case "both":
				return "IPv4 and IPv6"
			}
			return "IPv4 only"
		},
		fields: func(m *Model) []huh.Field {
			if m.a.family == "" {
				m.a.family = "v4"
			}
			return []huh.Field{choose("IP version", &m.a.family,
				huh.NewOption("IPv4 only", "v4"), huh.NewOption("IPv6 only", "v6"), huh.NewOption("IPv4 and IPv6", "both"))}
		},
		apply: func(m *Model) tea.Cmd {
			c := &m.S.Cfg
			switch m.a.family {
			case "v4":
				c.IP6, c.Gateway6, m.a.mode6 = "", "", ""
				if c.IP == optNone {
					c.IP = ""
				}
			case "v6":
				c.IP, c.Gateway, m.a.mode4 = optNone, "", ""
			default:
				if c.IP == optNone {
					c.IP = ""
				}
			}
			return nil
		},
	}
}

// networkLabel is the network's line in the list, with the network this node uses marked.
func (m *Model) networkLabel(n *Network) string {
	l := n.Label()
	if n.Name == m.S.Facts.DefaultBridge {
		l += " (this node)"
	}
	return l
}

func networkSetting() *setting {
	return &setting{
		label:   "Network",
		applies: func(m *Model) bool { return len(m.S.Facts.Networks) > 0 },
		open:    func(m *Model) bool { return m.S.Cfg.Bridge == "" || len(m.S.Facts.Networks) > 1 },
		value: func(m *Model) string {
			c := &m.S.Cfg
			if n, ok := m.S.Facts.network(c.Bridge); ok {
				return m.networkLabel(&n)
			}
			return c.Bridge
		},
		fields: func(m *Model) []huh.Field {
			f := m.S.Facts
			m.a.bridge = m.S.Cfg.Bridge
			opts := make([]huh.Option[string], len(f.Networks))
			for i := range f.Networks {
				n := &f.Networks[i]
				opts[i] = huh.NewOption(m.networkLabel(n), n.Name)
			}
			return []huh.Field{m.fitList(choose("Network", &m.a.bridge, opts...), len(opts))}
		},
		apply: func(m *Model) tea.Cmd {
			c := &m.S.Cfg
			changed := c.Bridge != m.a.bridge
			c.Bridge = m.a.bridge
			if n, ok := m.S.Facts.network(c.Bridge); ok && !n.VLANOffered() {
				c.VLAN, m.a.vlan = "", ""
			}
			if changed {
				m.resetAddressing()
				m.answered[settingVLAN], m.given[settingVLAN] = false, false
				c.VLAN, m.a.vlan = "", ""
			}
			if m.needed(settingVLAN) {
				return nil
			}
			return m.refresh()
		},
	}
}

func vlanSetting() *setting {
	return &setting{
		label: "VLAN",
		applies: func(m *Model) bool {
			n, ok := m.S.Facts.network(m.S.Cfg.Bridge)
			return ok && n.VLANOffered()
		},
		value: func(m *Model) string { return orNone(m.S.Cfg.VLAN) },
		fields: func(m *Model) []huh.Field {
			m.a.vlan = m.S.Cfg.VLAN
			return []huh.Field{input("VLAN", &m.a.vlan, "none", func(s string) error {
				if err := validateRange("1 to 4094", 1, vlanMax)(s); err != nil {
					return err
				}
				return m.S.routeVLANError(m.ctx, m.S.Cfg.Bridge, s)
			})}
		},
		apply: func(m *Model) tea.Cmd {
			c := &m.S.Cfg
			changed := c.VLAN != m.a.vlan
			c.VLAN = m.a.vlan
			if changed {
				m.resetAddressing()
			}
			return m.refresh()
		},
	}
}

func v4ModeSetting() *setting {
	return &setting{
		label:   "IPv4 address",
		applies: hasV4,
		value:   func(m *Model) string { return m.S.ipv4Text() },
		fields: func(m *Model) []huh.Field {
			if m.a.mode4 == "" {
				m.a.mode4 = optStatic
				if m.S.Facts.NodeDHCP {
					m.a.mode4 = optDHCP
				}
			}
			return []huh.Field{choose("IPv4 address", &m.a.mode4, huh.NewOption("DHCP", optDHCP), huh.NewOption("Static", optStatic))}
		},
		apply: func(m *Model) tea.Cmd {
			c := &m.S.Cfg
			if m.a.mode4 == optDHCP {
				c.IP, c.Gateway = optDHCP, ""
			} else if c.IP == optDHCP || c.IP == optNone {
				c.IP = ""
			}
			return nil
		},
	}
}

// v4AddrFields asks for the static IPv4 address, filled with the free address found where there is
// one.
func v4AddrFields(m *Model) []huh.Field {
	f := m.S.Facts
	m.a.address = ""
	if modeOf(m.S.Cfg.IP) == optStatic {
		m.a.address = m.S.Cfg.IP
	}
	suggestion := ""
	if f.SuggestedIP != "" && f.Prefix.IsValid() {
		suggestion = fmt.Sprintf("%s/%d", f.SuggestedIP, f.Prefix.Bits())
		if m.a.address == "" {
			m.a.address = suggestion
		}
	}
	in := input("IPv4 address and prefix", &m.a.address, "192.0.2.40/24", validateAddress4(f.Prefix))
	if suggestion != "" {
		in = in.Description("Suggested " + suggestion + ", " + f.SuggestedCheck.Outcome())
	} else if f.Prefix.IsValid() {
		in = in.Description("/" + strconv.Itoa(f.Prefix.Bits()) + " may be left out")
	}
	return []huh.Field{in}
}

// validateAddress4 takes an IPv4 address with its prefix, or without one where the subnet is known.
func validateAddress4(subnet netip.Prefix) func(string) error {
	return func(s string) error {
		if _, ok := ParseAddress4(s, subnet); ok {
			return nil
		}
		if subnet.IsValid() {
			return errors.New("enter an IPv4 address, with or without /prefix")
		}
		return errors.New("enter an address with its prefix, such as 192.0.2.40/24")
	}
}

func v4AddrSetting() *setting {
	return &setting{
		label:   "IPv4 static",
		applies: func(m *Model) bool { return m.static4() },
		value:   func(m *Model) string { return orNone(m.S.Cfg.IP) },
		fields:  v4AddrFields,
		apply: func(m *Model) tea.Cmd {
			c, f := &m.S.Cfg, m.S.Facts
			p, _ := ParseAddress4(m.a.address, f.Prefix)
			c.IP = p.String()
			g, err := netip.ParseAddr(f.Gateway)
			if err == nil && p.Contains(g) && f.GatewayFrom != "" {
				c.Gateway = f.Gateway
				m.answered[settingV4Gateway] = true
				m.a.gwNote = "Found from " + f.GatewayFrom
				return nil
			}
			m.answered[settingV4Gateway] = false
			c.Gateway = ""
			m.a.gateway, m.a.gwNote = GuessGateway(p, f.RouteGateway), ""
			if m.a.gateway != "" {
				m.a.gwNote = "Guessed from the node's gateway " + f.RouteGateway + ". Check it"
			}
			return nil
		},
	}
}

func v4GatewaySetting() *setting {
	return &setting{
		label:   "IPv4 gateway",
		applies: func(m *Model) bool { return m.static4() },
		value:   func(m *Model) string { return orNone(m.S.Cfg.Gateway) },
		fields: func(m *Model) []huh.Field {
			if m.S.Cfg.Gateway != "" {
				m.a.gateway = m.S.Cfg.Gateway
			}
			in := input("IPv4 gateway", &m.a.gateway, "192.0.2.1", ValidateIPv4)
			if m.a.gwNote != "" {
				in = in.Description(m.a.gwNote)
			}
			return []huh.Field{in}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.Gateway = m.a.gateway
			return nil
		},
	}
}

func v6ModeSetting() *setting {
	return &setting{
		label:   "IPv6 address",
		applies: hasV6,
		value:   func(m *Model) string { return m.S.ipv6Text() },
		fields: func(m *Model) []huh.Field {
			if m.a.mode6 == "" {
				m.a.mode6 = optAuto
			}
			return []huh.Field{choose("IPv6 address", &m.a.mode6,
				huh.NewOption("Router advertisements", optAuto), huh.NewOption("DHCPv6", optDHCP), huh.NewOption("Static", optStatic))}
		},
		apply: func(m *Model) tea.Cmd {
			c := &m.S.Cfg
			if m.a.mode6 != optStatic {
				c.IP6, c.Gateway6 = m.a.mode6, ""
			} else if modeOf(c.IP6) != optStatic {
				c.IP6 = ""
			}
			return nil
		},
	}
}

func v6AddrSetting() *setting {
	return &setting{
		label:   "IPv6 static",
		applies: func(m *Model) bool { return m.static6() },
		value:   func(m *Model) string { return orNone(m.S.Cfg.IP6) },
		fields: func(m *Model) []huh.Field {
			m.a.address6 = ""
			if modeOf(m.S.Cfg.IP6) == optStatic {
				m.a.address6 = m.S.Cfg.IP6
			}
			return []huh.Field{input("IPv6 address and prefix", &m.a.address6, "2001:db8::40/64", func(s string) error {
				if !IsIPv6CIDR(s) {
					return errors.New("enter an IPv6 address with its prefix")
				}
				return nil
			})}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.IP6 = m.a.address6
			return nil
		},
	}
}

func v6GatewaySetting() *setting {
	return &setting{
		label:   "IPv6 gateway",
		applies: func(m *Model) bool { return m.static6() },
		value:   func(m *Model) string { return orNone(m.S.Cfg.Gateway6) },
		fields: func(m *Model) []huh.Field {
			m.a.gateway6 = m.S.Cfg.Gateway6
			return []huh.Field{input("IPv6 gateway", &m.a.gateway6, "2001:db8::1", func(s string) error {
				if s != "" && !IsIPv6(s) {
					return errors.New("an IPv6 address")
				}
				return nil
			})}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.Gateway6 = m.a.gateway6
			return nil
		},
	}
}

func dnsSetting() *setting {
	return &setting{
		label:   "DNS",
		applies: always,
		open:    func(m *Model) bool { return m.S.Cfg.DNS == "" && m.S.Facts.LoopbackDNSOnly },
		value: func(m *Model) string {
			if m.S.Cfg.DNS == "" {
				return "this node's: " + strings.Join(m.S.Facts.DNS, ", ")
			}
			return m.S.Cfg.DNS
		},
		fields: func(m *Model) []huh.Field {
			m.a.dns = m.S.Cfg.DNS
			if m.a.dns == "" && !m.S.Facts.LoopbackDNSOnly {
				m.a.dns = strings.Join(m.S.Facts.DNS, ",")
			}
			return []huh.Field{input("DNS servers", &m.a.dns, "192.0.2.53,192.0.2.54", validateDNS(m.S.Facts))}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.DNS = m.a.dns
			return nil
		},
	}
}

// mtuDefault names the MTU the container takes when none is given.
const mtuDefault = "bridge default"

func mtuSetting() *setting {
	return &setting{
		label:   "MTU",
		applies: always,
		open:    func(*Model) bool { return false },
		value: func(m *Model) string {
			if m.S.Cfg.MTU == "" {
				return mtuDefault
			}
			return m.S.Cfg.MTU
		},
		fields: func(m *Model) []huh.Field {
			m.a.mtu = m.S.Cfg.MTU
			return []huh.Field{input("MTU", &m.a.mtu, mtuDefault, validateRange("576 to 65535", mtuMin, mtuMax))}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.MTU = m.a.mtu
			return nil
		},
	}
}

func ctidSetting() *setting {
	return &setting{
		label:   "Container ID",
		applies: always,
		open:    func(*Model) bool { return false },
		value:   func(m *Model) string { return m.S.Cfg.CTID },
		fields: func(m *Model) []huh.Field {
			m.a.ctid = m.S.Cfg.CTID
			return []huh.Field{input("Container ID", &m.a.ctid, m.S.Facts.NextID, func(s string) error {
				if !inRange(s, ctidMin, ctidMax) {
					return errors.New("enter a number from 100 up")
				}
				if s != m.S.Cfg.CTID && !m.S.Node.IDFree(m.ctx, s) {
					return errors.New("ID taken by another guest")
				}
				return nil
			})}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.CTID = m.a.ctid
			return nil
		},
	}
}

func hostnameSetting() *setting {
	return &setting{
		label:   "Host name",
		applies: always,
		open:    func(*Model) bool { return false },
		value:   func(m *Model) string { return m.S.Cfg.Hostname },
		fields: func(m *Model) []huh.Field {
			m.a.hostname = m.S.Cfg.Hostname
			return []huh.Field{input("Host name", &m.a.hostname, DefaultHostname, func(s string) error {
				if !hostnameRE.MatchString(s) {
					return errors.New("enter a host name such as kubehenge")
				}
				return nil
			})}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.Hostname = m.a.hostname
			return nil
		},
	}
}

func storageSetting() *setting {
	return &setting{
		label:   "Disk storage",
		applies: func(m *Model) bool { return len(m.S.Facts.RootStorages) > 1 },
		open:    func(m *Model) bool { return m.S.Cfg.Storage == "" },
		value:   func(m *Model) string { return m.S.Cfg.Storage },
		fields: func(m *Model) []huh.Field {
			m.a.storage = m.S.Cfg.Storage
			if m.a.storage == "" {
				m.a.storage = m.S.Facts.RootStorages[0].Name
			}
			return []huh.Field{choose("Disk storage", &m.a.storage, storageOptions(m.S.Facts.RootStorages)...)}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.Storage = m.a.storage
			return nil
		},
	}
}

func templateSetting() *setting {
	return &setting{
		label:   "Template storage",
		applies: func(m *Model) bool { return len(m.S.Facts.TemplateStorages) > 1 && m.S.Facts.TemplateHeld == "" },
		open:    func(m *Model) bool { return m.S.Cfg.TemplateStorage == "" },
		value:   func(m *Model) string { return m.S.Cfg.TemplateStorage },
		fields: func(m *Model) []huh.Field {
			m.a.templateStorage = m.S.Cfg.TemplateStorage
			if m.a.templateStorage == "" {
				m.a.templateStorage = m.S.Facts.TemplateStorages[0].Name
			}
			return []huh.Field{choose("Template storage", &m.a.templateStorage, storageOptions(m.S.Facts.TemplateStorages)...)}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.TemplateStorage = m.a.templateStorage
			return nil
		},
	}
}

// apiOther is the choice of an API address that is not among those found.
const apiOther = "\x00other"

func apiSetting() *setting {
	return &setting{
		label:   "Proxmox VE API",
		applies: always,
		open: func(m *Model) bool {
			return m.S.Cfg.ProxmoxURL == "" && m.S.Cfg.APIHost == "" && len(TiedCandidates(m.S.Facts.Candidates)) > 1
		},
		value: func(m *Model) string { return m.S.Cfg.ProxmoxURL },
		fields: func(m *Model) []huh.Field {
			tied := TiedCandidates(m.S.Facts.Candidates)
			m.a.apiPick = len(tied) > 1 && !m.a.apiTyped
			if !m.a.apiPick {
				return apiFields(m)
			}
			opts := make([]huh.Option[string], 0, len(tied)+1)
			for _, c := range tied {
				opts = append(opts, huh.NewOption(c.Host+"  "+c.What, c.Host))
			}
			opts = append(opts, huh.NewOption("Another address", apiOther))
			m.a.apiHost = tied[0].Host
			return []huh.Field{choose("Proxmox VE API address", &m.a.apiHost, opts...)}
		},
		apply: func(m *Model) tea.Cmd {
			c := &m.S.Cfg
			switch {
			case m.a.apiPick && m.a.apiHost == apiOther:
				m.a.apiTyped = true
				m.answered[settingAPI] = false
				return m.open(settingAPI)
			case m.a.apiPick:
				c.APIHost, c.ProxmoxURL = m.a.apiHost, ""
			default:
				m.a.apiTyped = false
				c.APIHost, c.ProxmoxURL, c.ProxmoxCA, c.Insecure = "", typedAPIURL(m.a.apiURL), m.a.apiCA, !m.a.apiCheck
				if err := c.CheckOptionFiles(); err != nil {
					m.note = neutral(err.Error())
					c.ProxmoxCA = ""
					m.a.apiTyped, m.answered[settingAPI] = true, false
					return m.open(settingAPI)
				}
			}
			m.S.CA = nil
			m.answered[settingTrust], m.given[settingTrust], c.TrustServedCA = false, false, false
			return m.resolve()
		},
	}
}

// apiFields ask for any API address, a CA file to check it against and whether to check it at all.
func apiFields(m *Model) []huh.Field {
	c := &m.S.Cfg
	m.a.apiURL, m.a.apiCA, m.a.apiCheck = c.ProxmoxURL, c.ProxmoxCA, !c.Insecure
	found := make([]string, 0, len(m.S.Facts.Candidates))
	for _, cand := range m.S.Facts.Candidates {
		found = append(found, APIURL(cand.Host))
	}
	return []huh.Field{
		input("Proxmox VE API address", &m.a.apiURL, "https://pve.example.org:8006", func(s string) error {
			if !apiURLRE.MatchString(typedAPIURL(s)) {
				return errors.New("enter an https:// URL or a host name, such as pve.example.org")
			}
			return nil
		}).Suggestions(found),
		input("API CA file", &m.a.apiCA, "none", nil),
		huh.NewConfirm().Title("API certificate").Affirmative("Check").Negative("Do not check").Value(&m.a.apiCheck).
			Validate(func(check bool) error {
				if !check && m.a.apiCA != "" {
					return errors.New("a CA file needs the certificate check")
				}
				return nil
			}),
	}
}

// typedAPIURL is the API URL a typed address means: https:// and port 8006 where it names none.
func typedAPIURL(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), "/")
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	if u, err := url.Parse(s); err == nil && u.Scheme == "https" && u.Hostname() != "" && u.Port() == "" && u.Path == "" {
		return APIURL(u.Hostname())
	}
	return s
}

func trustSetting() *setting {
	return &setting{
		label: "API CA",
		applies: func(m *Model) bool {
			return m.S.CA != nil && m.S.CA.NeedsTrust && m.S.Cfg.ProxmoxCA == ""
		},
		open: func(m *Model) bool { return !m.S.Cfg.TrustServedCA },
		fields: func(m *Model) []huh.Field {
			ca := m.S.CA
			m.a.trust = false
			note := ""
			if ca.Pinned {
				note = "\nThe API serves no CA, so only this certificate is trusted. The appliance stops working when the certificate is renewed."
			}
			return []huh.Field{huh.NewConfirm().Title("Trust this CA for the Proxmox VE API?").
				Description("Subject  " + ca.Subject + "\nIssuer   " + ca.Issuer + "\nSHA-256  " + ca.Fingerprint[:47] + "\n         " + ca.Fingerprint[48:] + note).
				Affirmative("Trust").Negative("Refuse").Value(&m.a.trust)}
		},
		apply: func(m *Model) tea.Cmd {
			c := &m.S.Cfg
			if !m.a.trust {
				_, cmd := m.failWith(settingErr(settingAPI, "you did not trust the CA the Proxmox VE API at "+c.ProxmoxURL+" serves",
					"Give the CA that signed its certificate with --proxmox-ca <file>, or another URL with --proxmox-url"))
				return cmd
			}
			c.TrustServedCA = true
			return nil
		},
	}
}

func proxiesSetting() *setting {
	return &setting{
		label:   "Trusted proxies",
		applies: always,
		open:    func(*Model) bool { return false },
		value:   func(m *Model) string { return orNone(strings.Join(m.S.Cfg.TrustedProxies, ", ")) },
		fields: func(m *Model) []huh.Field {
			m.a.proxies = strings.Join(m.S.Cfg.TrustedProxies, ",")
			return []huh.Field{input("Trusted proxies", &m.a.proxies, "none, such as 192.0.2.0/24", func(s string) error {
				for _, p := range splitList(s) {
					if !IsProxyRange(p) {
						return errors.New("enter addresses or ranges, comma-separated")
					}
				}
				return nil
			})}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.TrustedProxies, m.S.Cfg.NoTrustedProxy = splitList(m.a.proxies), m.a.proxies == ""
			return nil
		},
	}
}

func splitList(s string) []string {
	out := []string{}
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func entrySetting() *setting {
	return &setting{
		label:   "Proxmox VE entry",
		applies: always,
		open:    func(m *Model) bool { return m.S.Cfg.PVEEntry == "" },
		value: func(m *Model) string {
			if m.S.Cfg.PVEEntry == optYes {
				return optYes
			}
			return "no"
		},
		fields: func(m *Model) []huh.Field {
			m.a.pve = m.S.Cfg.PVEEntry == optYes
			return []huh.Field{huh.NewConfirm().Title("Proxmox VE entry").
				Description("A Kubernetes button in this node's interface").Affirmative("Add").Negative("Skip").Value(&m.a.pve)}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.PVEEntry = "no"
			if m.a.pve {
				m.S.Cfg.PVEEntry = optYes
			}
			return nil
		},
	}
}

// startRun starts the install, or its next try, and follows its events.
func (m *Model) startRun() tea.Cmd {
	m.stage, m.fail, m.busy = stRun, nil, ""
	if m.S.AnswersPath != "" {
		_ = saveAnswers(m.S.AnswersPath, &m.S.Cfg)
	}
	if m.events == nil {
		m.events = make(chan any, eventBuffer)
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
	running := make(chan struct{})
	m.running = running
	go func() {
		res, err := m.runner.Run(m.ctx)
		close(running)
		m.events <- doneMsg{res, err}
	}()
	return tea.Batch(m.wait(), m.spin.Tick)
}

func (m *Model) wait() tea.Cmd {
	events := m.events
	return func() tea.Msg { return <-events }
}

func (m *Model) onEvent(e Event) {
	if e.State == StepRunning && m.state[e.Step] != StepRunning {
		m.started[e.Step] = m.now()
	}
	m.state[e.Step] = e.State
	if e.Detail != "" {
		m.detail[e.Step] = e.Detail
	}
	if e.Total > 0 {
		m.frac[e.Step] = float64(e.Done) / float64(e.Total)
	}
}

// cleanCmd removes what the run made, then sends then.
func (m *Model) cleanCmd(then tea.Cmd) tea.Cmd {
	m.state["remove"] = StepRunning
	m.started["remove"] = m.now()
	runner := m.runner
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), cleanupTimeout)
		defer cancel()
		runner.Cleanup(ctx)
		return cleanedMsg{then}
	}
}

func (m *Model) onDone(d doneMsg) (*Model, tea.Cmd) {
	if d.err == nil {
		m.res, m.stage = d.res, stEnd
		if m.S.AnswersPath != "" {
			removeAnswers(m.S.AnswersPath)
		}
		return m, nil
	}
	if m.ctx.Err() != nil {
		m.cancelled = true
		if m.keeps() {
			m.runner.Keep()
		}
		cmd := m.cleanCmd(tea.Quit)
		return m, cmd
	}
	return m.failWith(d.err)
}

// quitAndKeep leaves the container of a failed run where it is.
func (m *Model) quitAndKeep() tea.Cmd {
	if m.runner != nil {
		m.runner.Keep()
	}
	return tea.Quit
}

// quitAndRemove leaves after removing the container, if the run made one.
func (m *Model) quitAndRemove() tea.Cmd {
	if m.runner == nil || !m.runner.Made() {
		return tea.Quit
	}
	m.stage, m.form = stRun, nil
	return tea.Batch(m.spin.Tick, m.cleanCmd(tea.Quit))
}

// lowerFirst lowers the first letter of a label in a sentence, keeping an acronym such as IPv4 and
// the name Proxmox.
func lowerFirst(label string) string {
	r := []rune(label)
	if len(r) > 1 && unicode.IsUpper(r[0]) && unicode.IsLower(r[1]) && !strings.HasPrefix(label, "Proxmox") {
		r[0] = unicode.ToLower(r[0])
	}
	return string(r)
}

// failSetting is the setting a failure names, if the installer can ask it again.
func (m *Model) failSetting() string {
	var u *UserError
	var na *NoAddressError
	switch {
	case errors.As(m.fail, &na):
		u = &na.UserError
	case !errors.As(m.fail, &u):
		return ""
	}
	if st := m.table[u.Setting]; st == nil || m.S.Facts == nil || !st.applies(m) {
		return ""
	}
	return u.Setting
}

// failForm asks what to do about the failure: change the setting at fault, try again, or leave.
func (m *Model) failForm() tea.Cmd {
	var opts []huh.Option[string]
	if id := m.failSetting(); id != "" {
		opts = append(opts, huh.NewOption("Change "+lowerFirst(m.table[id].label), actFix+id))
	}
	made := m.runner != nil && m.runner.Made()
	// The screen says the container is kept until the user chooses, so only that choice removes it.
	m.keep = m.keep || made
	if m.runner != nil {
		opts = append(opts, huh.NewOption("Try again", actRetry))
	}
	if made {
		opts = append(opts, huh.NewOption("Quit and keep the container", actKeep), huh.NewOption("Quit and remove it", actQuit))
	} else {
		opts = append(opts, huh.NewOption("Quit", actQuit))
	}
	m.a.action = opts[0].Value
	m.keys = "↑↓ choose  enter confirm  ctrl+c quit"
	if made {
		m.keys = "↑↓ choose  enter confirm  ctrl+c quit and keep"
	}
	return m.startForm(huh.NewGroup(huh.NewSelect[string]().Options(opts...).Value(&m.a.action)))
}

func (m *Model) failAction() tea.Cmd {
	action := m.a.action
	switch {
	case strings.HasPrefix(action, actFix):
		id := strings.TrimPrefix(action, actFix)
		m.fixed, m.fixing = id, m.runner != nil
		ids := []string{id}
		if id == settingNetwork {
			// A network that does not carry the address may be right with another VLAN, address or
			// address mode, such as a static address where DHCP sends no router.
			ids = append(ids, settingVLAN, settingV4Mode, settingV4Addr, settingV4Gateway, settingV6Mode, settingV6Addr, settingV6Gateway)
		}
		for _, id := range ids {
			m.answered[id], m.given[id] = false, false
		}
		m.trail = nil
		return m.open(id)
	case action == actRetry:
		return m.resume("")
	case action == actKeep:
		return m.quitAndKeep()
	}
	return m.quitAndRemove()
}

// resume starts the run again after the setting fixed was changed. A change of network is given to
// the container that is there, and a change of the API has the steps from the network check on run
// again in it. Another setting starts the container afresh.
func (m *Model) resume(fixed string) tea.Cmd {
	m.fixing = false
	m.stage, m.busy, m.form = stDetect, "Retrying", nil
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		ctx := m.ctx
		switch {
		case fixed == "":
		case slices.Contains(networkSettings, fixed) && m.runner.Made():
			return resumedMsg{m.runner.Renet(ctx)}
		case (fixed == settingAPI || fixed == settingTrust) && m.runner.Made():
			m.runner.Redo(stepNetworkName)
		default:
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			m.runner.Cleanup(cctx)
		}
		return resumedMsg{nil}
	})
}

func (m *Model) afterResume(err error) (*Model, tea.Cmd) {
	m.busy = ""
	if err != nil {
		return m.failWith(err)
	}
	cmd := m.startRun()
	return m, cmd
}
