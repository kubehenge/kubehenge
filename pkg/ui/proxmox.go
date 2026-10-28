package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// What a cluster's VMs need of Proxmox VE beyond their pool, checked as Proxmox VE itself checks a
// user creating a VM there.
const (
	privStorage = "Datastore.AllocateSpace"
	privBridge  = "SDN.Use"
	// privDatastoreAllocate is what a user gives a cluster's token on its storage when the cluster
	// runs the CSI plugin's newest release, so it takes the user's own.
	privDatastoreAllocate = "Datastore.Allocate"
	// privPermissionsModify on / is what a user needs to give a cluster's CSI plugin root@pam for
	// volume snapshots: a user who holds it can already give themselves anything root@pam can do.
	privPermissionsModify = "Permissions.Modify"
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
	// Allocatable tells whether the user may give a cluster's token Datastore.Allocate there.
	Allocatable bool `json:"allocatable"`
}

func (s *Server) storages(w http.ResponseWriter, r *http.Request, u user) {
	storages, err := s.Accounts.Storages(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	held := map[string]pve.Privileges{}
	views := make([]storageView, 0, len(storages))
	for _, st := range storages {
		privs, seen := held[st.Storage]
		if !seen {
			var err error
			if privs, err = s.Accounts.Privileges(r.Context(), u.ticket, storagePath(st.Storage)); err != nil {
				s.proxmoxError(w, err)
				return
			}
			held[st.Storage] = privs
		}
		usable := privs.Has(privStorage) && slices.Contains(st.Content, "images")
		views = append(views, storageView{Storage: st, Usable: usable, Allocatable: usable && privs.Has(privDatastoreAllocate)})
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
	// Readable tells whether the user may read the rules, which takes Sys.Audit on /, or is offered
	// them.
	Readable bool `json:"readable"`
	// Offered tells that the rules come from Kubehenge's controller, which an administrator lets
	// offer them to those who may create or change a cluster: their names and nodes, nothing more.
	Offered bool            `json:"offered,omitempty"`
	Rules   []placementRule `json:"rules"`
}

type placementRule struct {
	pve.HARule

	// Allowed are the nodes the user sees that the rule keeps to: its own, or for a negative rule
	// every other one.
	Allowed []string `json:"allowed"`
}

func (s *Server) haRules(w http.ResponseWriter, r *http.Request, u user) {
	view := haRulesView{Readable: true, Rules: []placementRule{}}
	rules, err := s.Accounts.NodeAffinityRules(r.Context(), u.ticket)
	if errors.Is(err, pve.ErrForbidden) {
		rules, view.Offered, err = s.offeredRules(r.Context(), u)
		view.Readable = view.Offered
	}
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	if !view.Readable {
		writeJSON(w, http.StatusOK, view)
		return
	}
	nodes, err := s.Accounts.Nodes(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Node)
	}
	for _, rule := range rules {
		// A rule that leaves the user no node is no place for a cluster.
		if allowed := rule.Allowed(names); len(allowed) > 0 {
			view.Rules = append(view.Rules, placementRule{HARule: rule, Allowed: allowed})
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// offeredRules are the rules Kubehenge's controller offers, where an administrator lets it and the
// user may create a cluster or change one, and whether they are offered. A management cluster that
// cannot be asked offers none: a cluster can still be placed without them.
func (s *Server) offeredRules(ctx context.Context, u user) ([]pve.HARule, bool, error) {
	kube := s.currentKube()
	if kube == nil {
		return nil, false, nil
	}
	cm := &corev1.ConfigMap{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: provisioner.HARulesName}, cm)
	switch {
	// Without the setting the interface may not read it either.
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		return nil, false, nil
	case err != nil:
		s.Log.Warn("reading the HA rules offered", "error", err)
		return nil, false, nil
	}
	offered, err := provisioner.ParseOfferedRules(cm)
	if err != nil {
		s.Log.Warn("reading the HA rules offered", "error", err)
		return nil, false, nil
	}
	if may, err := s.placesClusters(ctx, u); err != nil || !may {
		return nil, false, err
	}
	rules := make([]pve.HARule, 0, len(offered))
	for _, o := range offered {
		rules = append(rules, pve.HARule{Rule: o.Rule, Nodes: o.Nodes})
	}
	return rules, true, nil
}

// placesClusters tells whether the user may choose where a cluster's VMs go: VM.Allocate on the
// workload pool, to create one, or on a cluster's pool beneath it, to change that one.
func (s *Server) placesClusters(ctx context.Context, u user) (bool, error) {
	all, err := s.Accounts.AllPrivileges(ctx, u.ticket)
	if err != nil {
		return false, fmt.Errorf("reading the user's privileges: %w", err)
	}
	pool := s.workloadPoolPath()
	for path, privs := range all {
		if (path == pool || strings.HasPrefix(path, pool+"/")) && privs.Has(privChange) {
			return true, nil
		}
	}
	// Inherited from above, which Proxmox VE lists only for paths with an entry of their own.
	privs, err := s.Accounts.Privileges(ctx, u.ticket, pool)
	if err != nil {
		return false, fmt.Errorf("reading the user's privileges on %s: %w", pool, err)
	}
	return privs.Has(privChange), nil
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
