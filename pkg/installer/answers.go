package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	TrustedProxies  []string `json:"trustedProxies,omitempty"`
	PVEEntry        string   `json:"pveEntry,omitempty"`
}

// saveAnswers writes the answers the options hold to the file, readable by root alone.
func saveAnswers(path string, c *Config) error {
	data, err := json.MarshalIndent(savedAnswers{
		CTID: c.CTID, Storage: c.Storage, TemplateStorage: c.TemplateStorage, Bridge: c.Bridge, VLAN: c.VLAN, MTU: c.MTU,
		IP: c.IP, Gateway: c.Gateway, IP6: c.IP6, Gateway6: c.Gateway6, DNS: c.DNS, Hostname: c.Hostname, Name: c.Name,
		TrustedProxies: c.TrustedProxies, PVEEntry: c.PVEEntry,
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
	restored := false
	// try applies one group of saved values, and undoes it when the options no longer pass.
	try := func(apply func()) bool {
		before := *c
		apply()
		if c.Validate() != nil {
			*c = before
			return false
		}
		restored = true
		return true
	}
	// take is try that names the group when it is dropped.
	take := func(what string, apply func()) bool {
		if try(apply) {
			return true
		}
		s.Dropped = append(s.Dropped, what)
		return false
	}
	fill := func(what string, dst *string, value string) {
		if *dst == "" && value != "" {
			take(what, func() { *dst = value })
		}
	}
	if !s.NetworkGiven && a.Bridge != "" && slices.Contains(f.Bridges, a.Bridge) && take("network", func() { c.Bridge, c.VLAN = a.Bridge, a.VLAN }) {
		fill("MTU", &c.MTU, a.MTU)
		// The two versions' addresses pass together where one is none, so they are tried together first.
		v4 := func() { c.IP, c.Gateway = a.IP, a.Gateway }
		v6 := func() { c.IP6, c.Gateway6 = a.IP6, a.Gateway6 }
		if a.IP+a.IP6 != "" && !try(func() { v4(); v6() }) {
			if a.IP != "" {
				take("IPv4 address", v4)
			}
			if a.IP6 != "" {
				take("IPv6 address", v6)
			}
		}
		s.RefreshNetwork(ctx, c.Bridge, c.VLAN)
	}
	fill("DNS", &c.DNS, a.DNS)
	fill("DNS name", &c.Name, a.Name)
	if !s.HostnameGiven && a.Hostname != "" && a.Hostname != DefaultHostname {
		take("host name", func() { c.Hostname = a.Hostname })
	}
	if c.Storage == "" && hasStorage(f.RootStorages, a.Storage) {
		c.Storage, restored = a.Storage, true
	}
	if c.TemplateStorage == "" && hasStorage(f.TemplateStorages, a.TemplateStorage) {
		c.TemplateStorage, restored = a.TemplateStorage, true
	}
	switch a.PVEEntry {
	case optYes, "no":
		fill("Proxmox VE entry", &c.PVEEntry, a.PVEEntry)
	case "":
	default:
		s.Dropped = append(s.Dropped, "Proxmox VE entry")
	}
	if len(c.TrustedProxies) == 0 && len(a.TrustedProxies) > 0 {
		take("trusted proxies", func() { c.TrustedProxies = slices.Clone(a.TrustedProxies) })
	}
	if !s.CTIDGiven && a.CTID != "" && a.CTID != c.CTID && inRange(a.CTID, ctidMin, ctidMax) && s.Node.IDFree(ctx, a.CTID) {
		c.CTID, restored = a.CTID, true
	}
	s.Restored = restored
	return restored
}
