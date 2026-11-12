package installer

import (
	"context"
	"fmt"
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
}

// Needs lists the questions still open.
type Needs struct {
	Network, Bridge, DNS, Storage, TemplateStorage, APIHost, TrustCA, PVEEntry bool
}

// Any reports whether any question is open.
func (n Needs) Any() bool { return n != Needs{} }

// Prepare detects the node and refuses what can never work, before any question.
func (s *Session) Prepare(ctx context.Context) error {
	if s.Cfg.Hostname == "" {
		s.Cfg.Hostname = DefaultHostname
	}
	if s.Cfg.TrustedProxies == nil {
		s.Cfg.TrustedProxies = []string{}
	}
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
	list := strings.Join(facts.Bridges, ", ")
	if list == "" {
		list = optNone
	}
	if s.Cfg.Bridge != "" {
		if !contains(facts.Bridges, s.Cfg.Bridge) {
			return userErr(s.Cfg.Bridge+" is no bridge, Open vSwitch bridge or SDN VNet of this node", "Name one of "+list+" with --bridge")
		}
	} else if facts.DefaultBridge != "" {
		s.Cfg.Bridge = facts.DefaultBridge
	}
	if s.Cfg.CTID == "" {
		s.Cfg.CTID = facts.NextID
	} else if !s.Node.IDFree(ctx, s.Cfg.CTID) {
		return userErr(fmt.Sprintf("--ctid %s is taken by another guest", s.Cfg.CTID), "Name a free id, or leave --ctid out")
	}
	return nil
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

// RefreshForBridge detects the bridge's subnet, address suggestion and API candidates again after
// the user picked another bridge.
func (s *Session) RefreshForBridge(ctx context.Context, bridge string) {
	s.Cfg.Bridge = bridge
	s.Facts.Prefix = s.Node.bridgeAddress4(ctx, bridge)
	s.Facts.NodeDHCP = s.Node.bridgeIsDHCP(bridge)
	s.Facts.SuggestedIP = ""
	if s.Facts.Prefix.IsValid() {
		s.Facts.SuggestedIP = s.Node.SuggestAddress(ctx, bridge, s.Facts)
	}
	s.Facts.Candidates = s.Node.APICandidates(ctx, s.Facts, bridge)
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
			s.noticef("storage %s (%s) for the appliance's disks, %s", list[0].Name, list[0].Type, why(len(list)))
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
			return userErr("no enabled, active storage here takes container templates", "Enable one, or name one with --template-storage")
		case len(list) == 1 || ties == PickBest:
			c.TemplateStorage = list[0].Name
			s.noticef("storage %s (%s) for the container template, %s", list[0].Name, list[0].Type, why(len(list)))
		}
	} else if !hasStorage(s.Facts.TemplateStorages, c.TemplateStorage) {
		return userErr(fmt.Sprintf("storage %s is disabled, inactive or takes no container templates here", c.TemplateStorage), "Name another with --template-storage")
	}
	return nil
}

func why(count int) string {
	if count > 1 {
		return fmt.Sprintf("the best of %d by type and free space", count)
	}
	return "the only one here that takes it"
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
		asked, err := s.pickAPIHost(ties)
		if err != nil || asked {
			return err
		}
		c.ProxmoxURL = APIURL(c.APIHost)
	}
	ca, err := s.Node.DecideCA(ctx, c, c.ProxmoxURL)
	if err != nil {
		return err
	}
	s.CA = ca
	if ca.Kind == CAInsecure {
		s.noticef("the Proxmox VE API at %s is reached without checking its certificate", c.ProxmoxURL)
	}
	if ca.Kind == CASystem {
		s.noticef("the Proxmox VE API's certificate rests on %s in this node's trust store", ca.Subject)
	}
	return nil
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
		return false, userErr(fmt.Sprintf("Proxmox VE's certificate (%s) is valid for no name or address of this cluster's nodes", file),
			"Give the URL the appliance and the clusters reach the API at, by a name or address the certificate is valid for, with --proxmox-url")
	}
	if len(TiedCandidates(cands)) > 1 && ties != PickBest {
		return true, nil
	}
	c.APIHost = cands[0].Host
	if len(cands) > 1 {
		s.noticef("the Proxmox VE API at %s, its certificate is also valid for %s", c.APIHost, joinHosts(cands[1:]))
	}
	return false, nil
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
		return userErr(fmt.Sprintf("the Proxmox VE API at %s serves a certificate neither this cluster's CA nor this node's trust store vouches for, resting on %s with the SHA-256 fingerprint %s",
			c.ProxmoxURL, s.CA.Subject, s.CA.Fingerprint), "Once you have checked that this is your CA, give it with --proxmox-ca <file>")
	}
	return nil
}

func (s *Session) noticef(format string, args ...any) {
	s.Notices = append(s.Notices, fmt.Sprintf(format, args...))
}

// CheckOptionFiles checks the files the options name.
func (c *Config) CheckOptionFiles() error {
	if c.ProxmoxCA != "" {
		data, err := os.ReadFile(c.ProxmoxCA)
		if err != nil || len(data) == 0 {
			return userErr("--proxmox-ca names a readable PEM file of the CA that signed the Proxmox VE API's certificate, alone or among others", "")
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
