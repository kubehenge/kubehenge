package installer

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
)

// Session is one install from detection to the summary: the options, what was detected, and the
// answers so far. It holds no screen, so the terminal and the plain output walk the same road.
type Session struct {
	Node  *Node
	Cfg   Config
	Facts *Facts
	CA    *CAChoice
	// Notices are the one-line decisions made without asking, such as the storage picked.
	Notices []string
	// Cautions are what the person has to know before installing, shown in the summary as well as
	// in the plain output: a choice that leaves part of what they run unable to reach the API.
	Cautions []string

	// Version is the release being installed, for the summary.
	Version string
	// AllowOutside is the user's yes to a static address outside the subnet the node knows of the
	// network.
	AllowOutside bool
	// AddressCheck is the last check of the static address, shown in the summary.
	AddressCheck AddrCheck
	// Restored says the answers were read from an earlier run's file, and Dropped names the saved
	// answers the option checks refused.
	Restored bool
	Dropped  []string
	// NetworkGiven is set when the options already name part of the container's network, which a
	// saved network then does not replace.
	NetworkGiven bool
	// BridgeGiven and VLANGiven say the options name the network and its VLAN.
	BridgeGiven, VLANGiven bool
	// HostnameGiven and CTIDGiven say the options name the host name and the container ID, which
	// saved answers then do not replace.
	HostnameGiven, CTIDGiven bool
	// Saved holds the answers of an earlier run, which detect restores.
	Saved *savedAnswers
	// AnswersPath is where the answers are saved when the install starts, empty for nowhere.
	AnswersPath string

	// vlanAware holds what routeVLANError read of each bridge, as a form checks on every key.
	vlanAware map[string]bool
}

// Needs lists the questions still open.
type Needs struct {
	Network, Bridge, DNS, Storage, TemplateStorage, APIHost, TrustCA, PVEEntry bool
}

// Any reports whether any question is open.
func (n Needs) Any() bool { return n != Needs{} }

// Prepare detects the node and refuses what can never work, before any question.
func (s *Session) Prepare(ctx context.Context) error {
	s.HostnameGiven, s.CTIDGiven = s.Cfg.Hostname != "", s.Cfg.CTID != ""
	if s.Cfg.Hostname == "" {
		s.Cfg.Hostname = DefaultHostname
	}
	if s.Cfg.TrustedProxies == nil {
		s.Cfg.TrustedProxies = []string{}
	}
	c := &s.Cfg
	s.NetworkGiven = c.Bridge != "" || c.VLAN != "" || c.MTU != "" || c.IP != "" || c.Gateway != "" || c.IP6 != "" || c.Gateway6 != ""
	s.BridgeGiven, s.VLANGiven = c.Bridge != "", c.VLAN != ""
	facts, err := s.Node.Detect(ctx, &s.Cfg)
	if err != nil {
		return err
	}
	s.Facts = facts
	if !s.Node.Quorate(ctx) {
		return userErr("this node has no quorum in its Proxmox VE cluster, so it cannot change any container", "Start the other nodes, then run this again")
	}
	if len(facts.Existing) > 0 {
		e := facts.Existing[0]
		return userErr(fmt.Sprintf("an appliance exists as container %s on %s", e.ID, e.Node), "Run this with --upgrade on that node")
	}
	if len(facts.Bridges) == 0 {
		return userErr("this node lists no bridge, Open vSwitch bridge or SDN VNet for the container's network", "Create one in the node's network settings, apply them, then run this again")
	}
	list := strings.Join(facts.Bridges, ", ")
	if s.Cfg.Bridge != "" {
		if !contains(facts.Bridges, s.Cfg.Bridge) {
			return userErr(s.Cfg.Bridge+" is no bridge, Open vSwitch bridge or SDN VNet of this node", "Name one of "+list+" with --bridge")
		}
	} else if facts.DefaultBridge != "" {
		s.Cfg.Bridge = facts.DefaultBridge
	}
	if err = s.checkRouteVLAN(ctx); err != nil {
		return err
	}
	if s.Cfg.CTID == "" {
		s.Cfg.CTID = facts.NextID
	} else if !s.Node.IDFree(ctx, s.Cfg.CTID) {
		return userErr(fmt.Sprintf("--ctid %s is taken by another guest", s.Cfg.CTID), "Name a free ID, or leave --ctid out")
	}
	return nil
}

// CheckAddressing settles the static addresses of the options against the network they go on,
// before anything is created: a prefix from the network's subnet where none is given, a gateway the
// network supplies where none is given, each address with its gateway, the address inside the
// network's known subnet, and the address free. The terminal installer asks for another address
// where this refuses one.
func (s *Session) CheckAddressing(ctx context.Context) error {
	c, f := &s.Cfg, s.Facts
	if err := s.checkStatic6(); err != nil {
		return err
	}
	if c.IP == "" || c.IP == optDHCP || c.IP == optNone || c.Bridge == "" {
		return nil
	}
	p, ok := ParseAddress4(c.IP, f.Prefix)
	if !ok {
		return settingErr(settingV4Addr, "--ip "+c.IP+" has no prefix length, and the subnet of this network is not known", "Give it as <address>/<prefix>, such as 192.0.2.40/24")
	}
	c.IP = p.String()
	if c.Gateway == "" {
		if g, err := netip.ParseAddr(f.Gateway); err == nil && p.Contains(g) {
			c.Gateway = f.Gateway
		}
	}
	if c.Gateway == "" && !s.apiOnLink(p) {
		return settingErr(settingV4Gateway, "a static address needs a gateway, and this network gives none", "Give it with --gateway")
	}
	if err := checkPair(p, c.Gateway); err != nil {
		return settingErr(settingV4Addr, err.Error(), "Check --ip and --gateway")
	}
	if msg := OutsideSubnet(f.Prefix, p); msg != "" && !c.Yes && !s.AllowOutside {
		return settingErr(settingOutside, msg, "Name the VLAN or network it is on with --vlan or --bridge, or pass --yes to install there anyway")
	}
	check := s.Node.CheckAddress(ctx, p.Addr().String(), Link{f.Device, cmpGateway(c.Gateway, f.Gateway), f.NodeHolds})
	s.AddressCheck = check
	switch check.State {
	case AddrInUse:
		return settingErr(settingV4Addr, p.Addr().String()+" is in use on "+c.Bridge, "Give a free address with --ip")
	case AddrUnchecked:
		s.noticef("could not check that %s is free: %s", p.Addr(), check.Detail)
	case AddrFree:
	}
	return nil
}

// cmpGateway is the gateway the address check tests its probe against.
func cmpGateway(given, found string) string {
	if given != "" {
		return given
	}
	return found
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

// Questions says which questions the options and the node leave open.
func (s *Session) Questions() Needs {
	f, c := s.Facts, &s.Cfg
	return Needs{
		Network:         c.IP == "",
		Bridge:          c.Bridge == "",
		DNS:             c.DNS == "" && f.LoopbackDNSOnly,
		Storage:         c.Storage == "" && len(f.RootStorages) > 1,
		TemplateStorage: c.TemplateStorage == "" && f.TemplateHeld == "" && len(f.TemplateStorages) > 1,
		APIHost:         c.ProxmoxURL == "" && c.APIHost == "" && len(TiedCandidates(f.Candidates)) > 1,
		TrustCA:         s.CA != nil && s.CA.NeedsTrust && !c.TrustServedCA && c.ProxmoxCA == "",
		PVEEntry:        c.PVEEntry == "",
	}
}

// Ties says what a choice between equal options does: wait for a question, or take the best ranked.
type Ties int

// What a tie does.
const (
	AskTies Ties = iota
	PickBest
)

// FillStorages sets each storage that needs no question: the only one that fits, or with PickBest
// the best ranked.
func (s *Session) FillStorages(ties Ties) error {
	c := &s.Cfg
	if c.Storage == "" {
		list := s.Facts.RootStorages
		switch {
		case len(list) == 0:
			if s.Facts.ShortRoot != "" {
				return userErr("no storage that takes container disks has the 9 GiB free that the appliance needs: "+s.Facts.ShortRoot, "Free space, or name another with --storage")
			}
			return userErr("no enabled, active storage here takes container disks", "Enable one, or name one with --storage")
		case len(list) == 1 || ties == PickBest:
			c.Storage = list[0].Name
			s.noticef("storage %s (%s) for the appliance's disks, %s", list[0].Name, list[0].Type, why(len(list), false))
		}
	} else if !hasStorage(s.Facts.RootStorages, c.Storage) {
		return userErr(fmt.Sprintf("storage %s is disabled, inactive, takes no container disks here or has under 9 GiB free for them", c.Storage), "Name another with --storage")
	}
	if c.TemplateStorage == "" {
		list := s.Facts.TemplateStorages
		switch {
		case s.Facts.TemplateHeld != "":
			c.TemplateStorage = s.Facts.TemplateHeld
		case len(list) == 0:
			return userErr("no enabled, active storage here takes container templates", "Give a directory, NFS or CIFS storage the content type Container template, or name one with --template-storage")
		case len(list) == 1 || ties == PickBest:
			c.TemplateStorage = list[0].Name
			room := ""
			if s.Facts.TemplateShort {
				room = fmt.Sprintf(", though under %d MiB and short of room for it", templateKiB/kibPerMiB)
			}
			s.noticef("storage %s (%s) for the container template, %s%s", list[0].Name, list[0].Type, why(len(list), s.Facts.TemplateShort), room)
		}
	} else if !hasStorage(s.Facts.TemplateStorages, c.TemplateStorage) && !hasStorage(s.Facts.TemplateTight, c.TemplateStorage) {
		return userErr(fmt.Sprintf("storage %s is disabled, inactive or takes no container templates here", c.TemplateStorage), "Name another with --template-storage")
	}
	return nil
}

// why says how the storage was picked from the count that take the content. With all of them
// short of room, the pick is the one with the most free, not the best.
func why(count int, short bool) string {
	switch {
	case count == 1:
		return "the only one here that takes it"
	case short:
		return fmt.Sprintf("the most free of %d", count)
	}
	return fmt.Sprintf("the best of %d by type and free space", count)
}

func hasStorage(list []Storage, name string) bool {
	for _, s := range list {
		if s.Name == name {
			return true
		}
	}
	return false
}

// ResolveAPI settles the Proxmox VE API's address and the CA it is checked against. With PickBest,
// a tie goes to the best-ranked candidate. It returns early when the user must pick the address.
func (s *Session) ResolveAPI(ctx context.Context, ties Ties) error {
	c := &s.Cfg
	if c.ProxmoxURL == "" {
		s.Cautions = nil
		asked, err := s.pickAPIHost(ties)
		if err != nil || asked {
			return err
		}
		c.ProxmoxURL = APIURL(c.APIHost)
		s.noteAPIFamily()
	}
	ca, err := s.Node.DecideCA(ctx, c, c.ProxmoxURL)
	if err != nil {
		if u, ok := errors.AsType[*UserError](err); ok && u.Setting == "" {
			u.Setting = settingAPI
		}
		return err
	}
	s.CA = ca
	if ca.Kind == CAInsecure {
		s.noticef("the Proxmox VE API at %s is reached without checking its certificate", c.ProxmoxURL)
	}
	if ca.Kind == CASystem {
		s.noticef("the Proxmox VE API's certificate is signed by %s, from this node's trust store", ca.Subject)
	}
	return nil
}

func joinHosts(list []APICandidate) string {
	hosts := make([]string, len(list))
	for i, c := range list {
		hosts[i] = c.Host
	}
	return strings.Join(hosts, ", ")
}

// UnattendedCheck fails for what only a question could settle: an address on a node without DHCP,
// and a CA nothing vouches for.
func (s *Session) UnattendedCheck() error {
	c, f := &s.Cfg, s.Facts
	if c.Bridge == "" {
		return userErr("no bridge carries this node's default route and there is no vmbr0. Name one of "+strings.Join(f.Bridges, ", "), "Give it with --bridge")
	}
	if c.DNS == "" && f.LoopbackDNSOnly {
		return userErr("/etc/resolv.conf names only name servers on this node's loopback, which the container cannot reach", "Give the name servers it asks with --dns, such as 192.0.2.53,192.0.2.54")
	}
	if c.IP == "" {
		if !f.NodeDHCP {
			return userErr("this node's network has no sign of DHCP, so the container would get no address",
				"Give one with --ip <address>/<prefix> --gateway <address>, or --ip dhcp where a DHCP server answers")
		}
		c.IP = optDHCP
	}
	if s.CA != nil && s.CA.NeedsTrust && !c.TrustServedCA && c.ProxmoxCA == "" {
		fix := "Check that it is your CA, then give it with --proxmox-ca <file>"
		if s.CA.Pinned {
			fix = "Give the CA that signed it with --proxmox-ca <file>. Without it the appliance fails when the certificate is renewed"
		}
		return userErr(fmt.Sprintf("the Proxmox VE API at %s serves a certificate from %s (SHA-256 fingerprint %s) that this node does not trust",
			c.ProxmoxURL, s.CA.Subject, s.CA.Fingerprint), fix)
	}
	return nil
}

// checkStatic6 checks a static IPv6 address against its gateway.
func (s *Session) checkStatic6() error {
	c := &s.Cfg
	if !IsIPv6CIDR(c.IP6) {
		return nil
	}
	p, err := netip.ParsePrefix(c.IP6)
	if err != nil {
		return nil //nolint:nilerr // Validate names a malformed address.
	}
	if err := checkPair6(p, c.Gateway6); err != nil {
		return settingErr(settingV6Addr, err.Error(), "Check --ip6 and --gateway6")
	}
	return nil
}

// apiOnLink reports whether the Proxmox VE API the container will use is in its own subnet, which
// needs no gateway.
func (s *Session) apiOnLink(p netip.Prefix) bool {
	host, _, _ := splitAPI(s.Cfg.ProxmoxURL)
	if host == "" && len(s.Facts.Candidates) > 0 {
		host = s.Facts.Candidates[0].Host
	}
	a, err := netip.ParseAddr(host)
	return err == nil && p.Contains(a)
}

// pickAPIHost sets the API host from the candidates. It reports true when a tie is left for the
// user to settle.
func (s *Session) pickAPIHost(ties Ties) (bool, error) {
	c := &s.Cfg
	if c.APIHost != "" {
		return false, nil
	}
	cands := s.Facts.Candidates
	if len(cands) == 0 {
		_, file, _ := s.Node.servedCertificate()
		return false, settingErr(settingAPI, fmt.Sprintf("the certificate in %s covers no name or address of this cluster's nodes", file),
			"Give a name or address the certificate covers with --proxmox-url")
	}
	if len(TiedCandidates(cands)) > 1 && ties != PickBest {
		return true, nil
	}
	c.APIHost = cands[0].Host
	if len(cands) > 1 {
		s.noticef("the API's certificate also covers %s", joinHosts(cands[1:]))
	}
	return false, nil
}

// noteAPIFamily notes that the API is reached at an IPv6 address because Proxmox VE's own
// certificate lacks this node's IPv4 address on the bridge, as on a dual-stack node whose name
// resolves to its IPv6 address first. Clusters without IPv6 cannot reach it there. A certificate of
// the administrator's is theirs to choose.
func (s *Session) noteAPIFamily() {
	bridge := s.Facts.Prefix.Addr()
	host, err := netip.ParseAddr(s.Cfg.APIHost)
	if err != nil || !host.Unmap().Is6() || !bridge.Is4() {
		return
	}
	cert, file, err := s.Node.servedCertificate()
	if err != nil || file != pveNodePEM {
		return
	}
	if _, ips := certNames(cert); slices.Contains(ips, bridge.String()) {
		return
	}
	s.Cautions = append(s.Cautions, apiFamilyCaution(bridge.String()))
}

// apiFamilyCaution is the text of that note: the cause, then the fix, each short enough for one
// line of the summary at 80 columns.
func apiFamilyCaution(ipv4 string) string {
	return fmt.Sprintf("Clusters without IPv6 need %[1]s in the API certificate. Add it, then pass --proxmox-url https://%[1]s:8006", ipv4)
}

// checkRouteVLAN refuses the VLAN of the node's default route on its bridge, taken from the route
// or named by hand, unless the bridge is VLAN aware. On any other bridge the tag makes Proxmox VE
// create a second bridge and take over the port's VLAN device, which would cut the node's own
// network.
func (s *Session) checkRouteVLAN(ctx context.Context) error {
	f := s.Facts
	b, v := f.DefaultBridge, f.DefaultVLAN
	if err := s.routeVLANError(ctx, s.Cfg.Bridge, s.Cfg.VLAN); err != nil || v == "" || s.Cfg.Bridge != b || s.Cfg.VLAN != v {
		return err
	}
	if f.VLANFromRoute {
		s.noticef("VLAN %s on %s, since the node's default route leaves by %s.%s (name --bridge %s for no VLAN)", v, b, b, v, b)
	}
	return nil
}

// routeVLANError is the refusal of a bridge and VLAN that are the node's default route device on
// a bridge that is not VLAN aware, nil for any other pair. The flags, the detected route and the
// terminal's VLAN field all go through it. Each bridge is read once.
func (s *Session) routeVLANError(ctx context.Context, bridge, vlan string) error {
	f := s.Facts
	b, v := f.DefaultBridge, f.DefaultVLAN
	if v == "" || bridge != b || vlan != v {
		return nil
	}
	aware, read := s.vlanAware[b]
	if !read {
		aware = s.Node.bridgeVLANAware(ctx, b)
		if s.vlanAware == nil {
			s.vlanAware = map[string]bool{}
		}
		s.vlanAware[b] = aware
	}
	if aware {
		return nil
	}
	return userErr(fmt.Sprintf("the node's default route leaves by %s.%s, but %s is not VLAN aware. A tag on it would cut the node's own network", b, v, b),
		fmt.Sprintf("Make %s VLAN aware in the node's network settings, or name another bridge or VLAN with --bridge and --vlan", b))
}

func (s *Session) noticef(format string, args ...any) {
	s.Notices = append(s.Notices, fmt.Sprintf(format, args...))
}

// CheckOptionFiles checks the files the options name.
func (c *Config) CheckOptionFiles() error {
	if c.ProxmoxCA != "" {
		data, err := os.ReadFile(c.ProxmoxCA)
		if err != nil || len(data) == 0 {
			return userErr("--proxmox-ca needs a readable PEM file with the CA that signed the Proxmox VE API's certificate", "")
		}
	}
	if c.Tofu != "" {
		if _, err := os.Stat(c.Tofu); err != nil {
			return userErr("--tofu names a readable OpenTofu release archive", "")
		}
	}
	for _, f := range []string{c.CertFile, c.KeyFile} {
		if f != "" {
			if _, err := os.Stat(f); err != nil {
				return userErr("--cert and --key name a readable PEM certificate and its key", "")
			}
		}
	}
	return nil
}
