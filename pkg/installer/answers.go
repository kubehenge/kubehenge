package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// savedAnswers are the answers of a run that did not finish, kept on the node for the next run to
// start from. It holds no password, token or passphrase: the installer is never given one, and the
// certificate's key stays a file the options name.
type savedAnswers struct {
	CTID            string   `json:"ctid,omitempty"`
	Storage         string   `json:"storage,omitempty"`
	TemplateStorage string   `json:"templateStorage,omitempty"`
	Bridge          string   `json:"bridge,omitempty"`
	VLAN            string   `json:"vlan,omitempty"`
	MTU             string   `json:"mtu,omitempty"`
	IP              string   `json:"ip,omitempty"`
	Gateway         string   `json:"gateway,omitempty"`
	IP6             string   `json:"ip6,omitempty"`
	Gateway6        string   `json:"gateway6,omitempty"`
	DNS             string   `json:"dns,omitempty"`
	Hostname        string   `json:"hostname,omitempty"`
	Name            string   `json:"name,omitempty"`
	Cert            string   `json:"cert,omitempty"`
	TrustedProxies  []string `json:"trustedProxies,omitempty"`
	PVEEntry        string   `json:"pveEntry,omitempty"`
}

// saveAnswers writes the answers the options hold to the file, readable by root alone.
func saveAnswers(path string, c *Config) error {
	data, err := json.MarshalIndent(savedAnswers{
		CTID: c.CTID, Storage: c.Storage, TemplateStorage: c.TemplateStorage, Bridge: c.Bridge, VLAN: c.VLAN, MTU: c.MTU,
		IP: c.IP, Gateway: c.Gateway, IP6: c.IP6, Gateway6: c.Gateway6, DNS: c.DNS, Hostname: c.Hostname, Name: c.Name,
		Cert: strings.Join(c.ownFiles(), ","), TrustedProxies: c.TrustedProxies, PVEEntry: c.PVEEntry,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal answers: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(path), permDir); err != nil {
		return fmt.Errorf("mkdir all: %w", err)
	}
	if err = os.WriteFile(path, data, permFile); err != nil {
		return fmt.Errorf("write answers: %w", err)
	}
	return nil
}

// loadAnswers reads the saved answers, nil where there are none or the file is unreadable.
func loadAnswers(path string) *savedAnswers {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the installer's own answers file.
	if err != nil {
		return nil
	}
	var a savedAnswers
	if json.Unmarshal(data, &a) != nil {
		return nil
	}
	return &a
}

// removeAnswers deletes the saved answers once an install succeeded.
func removeAnswers(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
}

// Restore fills the options the command line left open from saved answers, keeping only what still
// fits this node: a network, a storage and a container ID that exist and are free. A saved value the
// option checks refuse, as from an older version or a file edited by hand, is dropped and named in
// Dropped. It reports whether it restored anything. Called after the detection, it changes no
// option the flags gave.
func (s *Session) Restore(ctx context.Context, a *savedAnswers) bool {
	if a == nil {
		return false
	}
	c, f := &s.Cfg, s.Facts
	r := &restoring{s: s}
	if !s.NetworkGiven && a.Bridge != "" && slices.Contains(f.Bridges, a.Bridge) && r.take("network", func() { c.Bridge, c.VLAN = a.Bridge, a.VLAN }) {
		r.network(a)
		s.RefreshNetwork(ctx, c.Bridge, c.VLAN)
	}
	r.fill("DNS", &c.DNS, a.DNS)
	r.fill("DNS name", &c.Name, a.Name)
	if c.CertFile == "" && a.Cert != "" {
		r.certificate(a.Cert)
	}
	if !s.HostnameGiven && a.Hostname != "" && a.Hostname != DefaultHostname {
		r.take("host name", func() { c.Hostname = a.Hostname })
	}
	if c.Storage == "" && hasStorage(f.RootStorages, a.Storage) {
		c.Storage, r.restored = a.Storage, true
	}
	if c.TemplateStorage == "" && hasStorage(f.TemplateStorages, a.TemplateStorage) {
		c.TemplateStorage, r.restored = a.TemplateStorage, true
	}
	switch a.PVEEntry {
	case optYes, "no":
		r.fill("Proxmox VE entry", &c.PVEEntry, a.PVEEntry)
	case "":
	default:
		s.Dropped = append(s.Dropped, "Proxmox VE entry")
	}
	if len(c.TrustedProxies) == 0 && len(a.TrustedProxies) > 0 {
		r.take("trusted proxies", func() { c.TrustedProxies = slices.Clone(a.TrustedProxies) })
	}
	if !s.CTIDGiven && a.CTID != "" && a.CTID != c.CTID && inRange(a.CTID, ctidMin, ctidMax) && s.Node.IDFree(ctx, a.CTID) {
		c.CTID, r.restored = a.CTID, true
	}
	s.Restored = r.restored
	return r.restored
}

// restoring applies saved answers to a session's options, one group at a time.
type restoring struct {
	s        *Session
	restored bool
}

// try applies one group of saved values, and undoes it when the options no longer pass.
func (r *restoring) try(apply func()) bool {
	c := &r.s.Cfg
	before := *c
	apply()
	if c.Validate() != nil {
		*c = before
		return false
	}
	r.restored = true
	return true
}

// take is try that names the group in Dropped when it is refused.
func (r *restoring) take(what string, apply func()) bool {
	if r.try(apply) {
		return true
	}
	r.s.Dropped = append(r.s.Dropped, what)
	return false
}

// fill takes a saved value for an option still empty.
func (r *restoring) fill(what string, dst *string, value string) {
	if *dst == "" && value != "" {
		r.take(what, func() { *dst = value })
	}
}

// certificate takes the saved certificate files when they still read and fit the name and address.
// The passphrase was never saved, so encrypted files fit only with --cert-passphrase-file.
func (r *restoring) certificate(files string) {
	c := &r.s.Cfg
	c.CertFile = files
	if err := r.s.CheckCertificate(); err != nil {
		c.CertFile = ""
		r.s.Dropped = append(r.s.Dropped, "certificate")
		return
	}
	r.restored = true
}

// network takes the MTU and the addresses of the saved network, each on its own.
func (r *restoring) network(a *savedAnswers) {
	c := &r.s.Cfg
	r.fill("MTU", &c.MTU, a.MTU)
	// The two versions' addresses pass together where one is none, so they are tried together first.
	v4 := func() { c.IP, c.Gateway = a.IP, a.Gateway }
	v6 := func() { c.IP6, c.Gateway6 = a.IP6, a.Gateway6 }
	if a.IP+a.IP6 == "" || r.try(func() { v4(); v6() }) {
		return
	}
	if a.IP != "" {
		r.take("IPv4 address", v4)
	}
	if a.IP6 != "" {
		r.take("IPv6 address", v6)
	}
}
