package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// settingsError is a problem with the settings themselves, which the user fixes on the setup page.
type settingsError struct{ error }

// setupPreflight reads what the settings depend on in Proxmox VE with the user's session and checks
// them against it, so a run that would fail halfway is refused before it starts.
func (s *Server) setupPreflight(ctx context.Context, u user, settings *appliance.Settings) (appliance.Findings, error) {
	a := s.Appliance
	if err := a.Check(settings); err != nil {
		return appliance.Findings{}, settingsError{err}
	}
	owned := a.HasState()
	inv, err := s.inventory(ctx, u, settings, owned)
	if err != nil {
		return appliance.Findings{}, err
	}
	return settings.Preflight(inv, owned), nil
}

// inventory reads Proxmox VE. What the user may not read is left nil, and the checks that need it
// stay out, as Proxmox VE itself judges the run then.
func (s *Server) inventory(ctx context.Context, u user, settings *appliance.Settings, owned bool) (*appliance.Inventory, error) { //nolint:revive // owned picks which reads apply, not a mode of one
	inv := &appliance.Inventory{ProxmoxURL: s.Appliance.ProxmoxURL}
	var err error
	if inv.Storages, err = s.Accounts.Storages(ctx, u.ticket); err != nil {
		return nil, fmt.Errorf("reading the storages: %w", err)
	}
	if inv.Nodes, err = s.Accounts.Nodes(ctx, u.ticket); err != nil {
		return nil, fmt.Errorf("reading the nodes: %w", err)
	}
	names, err := s.Accounts.AccessNames(ctx, u.ticket)
	if err != nil {
		return nil, fmt.Errorf("reading the pools, roles, users and groups: %w", err)
	}
	inv.Pools, inv.Roles, inv.Users, inv.Groups = names.Pools, names.Roles, names.Users, names.Groups
	if err = s.readBridges(ctx, u, inv); err != nil {
		return nil, err
	}
	if inv.Networks, err = s.readNetworks(ctx, u, settings); err != nil {
		return nil, err
	}
	// A cluster reports each node's address, which a node's bridges may not show. A node on its own
	// reports none.
	if reported, err := s.Accounts.NodeAddresses(ctx, u.ticket); err == nil {
		for node, text := range reported {
			if ip, err := netip.ParseAddr(text); err == nil {
				inv.NodeAddresses[node] = append(inv.NodeAddresses[node], ip)
			}
		}
	}
	if !owned {
		inv.Answering = s.probe(ctx, setupAddresses(settings))
	}
	return inv, nil
}

// readBridges fills in each online node's bridges and the addresses it has on them. A node whose
// bridges the user may not read is left out.
func (s *Server) readBridges(ctx context.Context, u user, inv *appliance.Inventory) error {
	inv.Bridges = map[string][]pve.Bridge{}
	inv.NodeAddresses = map[string][]netip.Addr{}
	for _, n := range inv.Nodes {
		if !n.Online {
			continue
		}
		bridges, err := s.Accounts.Bridges(ctx, u.ticket, n.Node)
		if errors.Is(err, pve.ErrForbidden) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading the bridges of %s: %w", n.Node, err)
		}
		inv.Bridges[n.Node] = bridges
		for i := range bridges {
			if own, ok := ownAddress(bridges[i].CIDR); ok {
				inv.NodeAddresses[n.Node] = append(inv.NodeAddresses[n.Node], own.Addr())
			}
		}
	}
	return nil
}

// readNetworks looks up where Proxmox VE has each bridge the settings name. A VNet the user may not
// read is left out, and Proxmox VE decides at the first clone.
func (s *Server) readNetworks(ctx context.Context, u user, settings *appliance.Settings) (map[string]pve.Network, error) {
	networks := map[string]pve.Network{}
	for _, ref := range append([]string{settings.Bridge}, settings.WorkloadBridges...) {
		_, name := pve.SplitBridge(ref)
		if _, done := networks[name]; done || name == "" {
			continue
		}
		network, err := s.Accounts.Network(ctx, u.ticket, name)
		if errors.Is(err, pve.ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading bridge %s: %w", name, err)
		}
		networks[name] = network
	}
	return networks, nil
}

// setupAddresses are the addresses the management cluster takes: its nodes' and its API's.
func setupAddresses(settings *appliance.Settings) []netip.Addr {
	var out []netip.Addr
	for _, text := range append([]string{settings.APIAddress}, settings.NodeAddresses...) {
		host, _, _ := strings.Cut(text, "/")
		if ip, err := netip.ParseAddr(host); err == nil && ip.Is4() {
			out = append(out, ip)
		}
	}
	return out
}

// setupCheck answers what the setup would run into, without starting it: errors that stop the run,
// and warnings that do not.
func (s *Server) setupCheck(w http.ResponseWriter, r *http.Request, u user) {
	if s.Appliance == nil {
		writeError(w, http.StatusNotFound, "this interface does not manage its management cluster")
		return
	}
	var req struct {
		Settings appliance.Settings `json:"settings"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	findings, err := s.setupPreflight(r.Context(), u, &req.Settings)
	if err != nil {
		s.writeSetupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, findings)
}

// writeSetupError answers a settings problem as the user's to fix, and a failed read of Proxmox VE as
// Proxmox VE's.
func (s *Server) writeSetupError(w http.ResponseWriter, r *http.Request, err error) {
	if _, invalid := errors.AsType[settingsError](err); invalid {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.proxmoxError(w, r, err)
}
