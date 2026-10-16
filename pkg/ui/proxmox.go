package ui

import (
	"errors"
	"net/http"
	"slices"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// What a cluster's VMs need of Proxmox VE beyond their pool, checked as Proxmox VE itself checks a
// user creating a VM there.
const (
	privStorage = "Datastore.AllocateSpace"
	privBridge  = "SDN.Use"
)

func storagePath(storage string) string { return "/storage/" + storage }

// bridgePath is where Proxmox VE grants the use of a bridge or VNet in a zone, and of one VLAN on
// it.
func bridgePath(zone, bridge, vlan string) string {
	p := "/sdn/zones/" + zone + "/" + bridge
	if vlan != "" {
		p += "/" + vlan
	}
	return p
}

type storageView struct {
	pve.Storage

	// Usable tells whether it takes VM disks and the user may put a cluster's disks there.
	Usable bool `json:"usable"`
}

func (s *Server) storages(w http.ResponseWriter, r *http.Request, u user) {
	storages, err := s.Accounts.Storages(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	usable := map[string]bool{}
	views := make([]storageView, 0, len(storages))
	for _, st := range storages {
		ok, seen := usable[st.Storage]
		if !seen {
			privs, err := s.Accounts.Privileges(r.Context(), u.ticket, storagePath(st.Storage))
			if err != nil {
				s.proxmoxError(w, err)
				return
			}
			ok = privs.Has(privStorage)
			usable[st.Storage] = ok
		}
		views = append(views, storageView{Storage: st, Usable: ok && slices.Contains(st.Content, "images")})
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request, u user) {
	nodes, err := s.Accounts.Nodes(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

// haRulesView offers Proxmox VE's HA node-affinity rules as places for a cluster's VMs.
type haRulesView struct {
	// Readable tells whether the user may read the rules, which takes Sys.Audit on /.
	Readable bool            `json:"readable"`
	Rules    []placementRule `json:"rules"`
}

type placementRule struct {
	pve.HARule

	// Allowed are the nodes the user sees that the rule keeps to: its own, or for a negative rule
	// every other one.
	Allowed []string `json:"allowed"`
}

func (s *Server) haRules(w http.ResponseWriter, r *http.Request, u user) {
	rules, err := s.Accounts.NodeAffinityRules(r.Context(), u.ticket)
	if errors.Is(err, pve.ErrForbidden) {
		writeJSON(w, http.StatusOK, haRulesView{Rules: []placementRule{}})
		return
	}
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	nodes, err := s.Accounts.Nodes(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	view := haRulesView{Readable: true, Rules: []placementRule{}}
	for _, rule := range rules {
		var allowed []string
		for _, n := range nodes {
			if slices.Contains(rule.Nodes, n.Node) != rule.Negative {
				allowed = append(allowed, n.Node)
			}
		}
		// A rule that leaves the user no node is no place for a cluster.
		if len(allowed) > 0 {
			view.Rules = append(view.Rules, placementRule{HARule: rule, Allowed: allowed})
		}
	}
	writeJSON(w, http.StatusOK, view)
}

type bridgeView struct {
	pve.Bridge

	// Usable tells whether the user may connect a cluster's VMs to it.
	Usable bool `json:"usable"`
}

func (s *Server) bridges(w http.ResponseWriter, r *http.Request, u user) {
	bridges, err := s.Accounts.Bridges(r.Context(), u.ticket, r.PathValue("node"))
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	views := make([]bridgeView, 0, len(bridges))
	for _, b := range bridges {
		// A node's own bridges are all in the local network zone.
		privs, err := s.Accounts.Privileges(r.Context(), u.ticket, bridgePath("localnetwork", b.Bridge, ""))
		if err != nil {
			s.proxmoxError(w, err)
			return
		}
		views = append(views, bridgeView{Bridge: b, Usable: privs.Has(privBridge)})
	}
	writeJSON(w, http.StatusOK, views)
}
