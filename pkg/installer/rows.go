package installer

import (
	"fmt"
	"strings"
)

// What the container is created with, as the summary says and stepCreate asks for.
const (
	ctCores      = "2"
	ctMemoryMiB  = "1024"
	ctSwapMiB    = "512"
	ctDiskGiB    = "8"
	ctKeysGiB    = "1"
	resourceText = ctCores + " cores, 1 GiB memory, " + ctSwapMiB + " MiB swap, " + ctDiskGiB + " GiB disk, " + ctKeysGiB + " GiB keys"
)

// Row is one line of the summary: what is settled, and its value.
type Row struct{ Label, Value string }

// Rows is the plan as the summary shows it, for the full-screen installer, the plain output and
// the dry run alike. Interactive says whether a failure asks what to do with the container.
func (s *Session) Rows(interactive bool) []Row {
	c, f := &s.Cfg, s.Facts
	var rows []Row
	add := func(label, value string) { rows = append(rows, Row{label, value}) }
	release := "Kubehenge"
	if s.Version != "" {
		release += " " + s.Version
	}
	add("Install", release+" on node "+f.Node)
	net := c.Bridge
	if n, ok := f.network(c.Bridge); ok {
		net += " (" + n.Kind + ")"
	}
	if c.VLAN != "" {
		net += ", VLAN " + c.VLAN
	}
	if c.MTU != "" {
		net += ", MTU " + c.MTU
	}
	add("Network", net)
	add("IPv4", s.ipv4Text())
	add("IPv6", s.ipv6Text())
	dns := c.DNS
	if dns == "" {
		dns = "this node's: " + strings.Join(f.DNS, ", ")
	}
	add("DNS", dns)
	add("Container ID", c.CTID)
	add("Host name", c.Hostname)
	add("Resources", resourceText)
	add("Storage", "disk "+c.Storage+", template "+c.TemplateStorage)
	add("Proxmox VE API", c.ProxmoxURL)
	if s.CA != nil {
		add("API CA", caText(s.CA, c))
	}
	if c.Name != "" {
		add("DNS name", c.Name)
	}
	if c.CertFile != "" {
		add("Certificate", "yours, "+c.CertFile)
	}
	if len(c.TrustedProxies) > 0 {
		add("Trusted proxies", strings.Join(c.TrustedProxies, ", "))
	}
	entry := "no"
	if c.PVEEntry == optYes {
		entry = optYes
	}
	add("Proxmox VE entry", entry)
	switch {
	case c.KeepFailed:
		add("If it fails", "the container is kept")
	case interactive:
		add("If it fails", "you choose to keep or remove the container")
	default:
		add("If it fails", "the container is removed")
	}
	return rows
}

func (s *Session) ipv4Text() string {
	c := &s.Cfg
	switch c.IP {
	case optNone:
		return "off"
	case optDHCP:
		return "DHCP"
	case "":
		return "not chosen"
	}
	text := c.IP
	if c.Gateway != "" {
		text += " via " + c.Gateway
	}
	switch {
	case s.AddressCheck.Detail == "":
	case s.AddressCheck.State == AddrFree:
		text += ", free"
	case s.AddressCheck.State == AddrUnchecked:
		text += ", not checked"
	case s.AddressCheck.State == AddrInUse:
		text += ", in use"
	}
	return text
}

func (s *Session) ipv6Text() string {
	c := &s.Cfg
	switch c.IP6 {
	case "":
		return "off, the container takes none"
	case optAuto:
		return "router advertisements"
	case optDHCP:
		return "DHCPv6"
	}
	if c.Gateway6 != "" {
		return c.IP6 + " via " + c.Gateway6
	}
	return c.IP6
}

// PlanText is the rows as lines for the plain output.
func (s *Session) PlanText(interactive bool) string {
	var b strings.Builder
	for _, r := range s.Rows(interactive) {
		fmt.Fprintf(&b, "  %-*s %s\n", summaryLabelWidth, r.Label, r.Value)
	}
	return b.String()
}
