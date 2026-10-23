package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// The Proxmox VE user automatic upgrades of the management cluster act as, made only when an
// administrator switches them on: an apply creates users, roles, ACLs and VMs, so it holds the
// Administrator role on /.
const (
	upgraderUser      = "kubehenge-upgrader@pve"
	upgraderTokenName = "apply"
	upgraderComment   = "Kubehenge's appliance: applies the management cluster in its automatic upgrades"
)

// Upgraders make and delete the Proxmox VE user automatic upgrades act as, with an administrator's
// session.
type Upgraders interface {
	CreateAdministratorToken(ctx context.Context, t *pve.Ticket, userid, name, comment string) (string, error)
	DeleteUser(ctx context.Context, t *pve.Ticket, userid string) error
}

// managementUpgrade tells whether the management cluster runs what an older release of the
// appliance applied, and what applying this one changes.
type managementUpgrade struct {
	// Applied is the release that last applied the management cluster, Carried the appliance's own.
	Applied string `json:"applied"`
	Carried string `json:"carried"`
	Behind  bool   `json:"behind"`
	// Changes are the components whose versions applying this release changes, in the order the
	// module lists them; a side the record does not know is left out.
	Changes []versionChange `json:"changes,omitempty"`
}

type versionChange struct {
	Component string `json:"component"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
}

// componentOrder is how the module's tested set lists its components, the management cluster's
// own first.
var componentOrder = []string{
	"talos", "kubernetes", "cluster_api", "capmox", "rke2", "k3s", "ipam_in_cluster", "helm_addon",
	"talos_bootstrap", "talos_control_plane", "cert_manager", "capi_operator", "kubehenge",
}

// managementUpgrade is nil until an apply of a release that records itself has succeeded.
func (s *Server) managementUpgrade() *managementUpgrade {
	a := s.Appliance
	last := a.LastApplied()
	if last == nil || !a.Ready() {
		return nil
	}
	v := &managementUpgrade{Applied: last.Release, Carried: a.Release, Behind: newerRelease(last.Release, a.Release)}
	if v.Behind {
		v.Changes = versionChanges(last.Versions, a.ModuleVersions())
	}
	return v
}

// newerRelease tells whether carried is a later release than applied; a build that is no release,
// such as dev, is never.
func newerRelease(applied, carried string) bool {
	was, err := release.ParseVersion(applied)
	if err != nil {
		return false
	}
	now, err := release.ParseVersion(carried)
	return err == nil && was.Less(now)
}

func versionChanges(from, to map[string]string) []versionChange {
	keys := slices.Clone(componentOrder)
	var others []string
	for _, m := range []map[string]string{from, to} {
		for k := range m {
			if !slices.Contains(keys, k) && !slices.Contains(others, k) {
				others = append(others, k)
			}
		}
	}
	slices.Sort(others)
	var out []versionChange
	for _, k := range append(keys, others...) {
		if f, t := from[k], to[k]; f != t {
			out = append(out, versionChange{Component: k, From: f, To: t})
		}
	}
	return out
}

// managementAutomatic is whether the appliance applies the management cluster on its own after
// upgrading itself, never with the secrets it keeps for that.
type managementAutomatic struct {
	// Possible tells that there is a management cluster to upgrade.
	Possible bool       `json:"possible"`
	On       bool       `json:"on"`
	By       string     `json:"by,omitempty"`
	Since    *time.Time `json:"since,omitempty"`
	// User is the Proxmox VE user the runs act as.
	User string `json:"user"`
}

func (s *Server) managementAutomatic() *managementAutomatic {
	a := s.Appliance
	if a == nil {
		return nil
	}
	v := &managementAutomatic{Possible: a.Ready(), User: upgraderUser}
	if kept := a.UnattendedOn(); kept != nil {
		v.On, v.By, v.Since, v.User = true, kept.By, &kept.Since, kept.User
	}
	return v
}

// keepUnattended switches automatic upgrades of the management cluster on. It checks the
// passphrase against the state, makes the Proxmox VE user and token the runs act as with the
// administrator's session, and keeps both on the appliance. The token holds the Administrator role,
// Sys.Console with it, so whoever switches it on needs that too, as for Proxmox VE HA.
func (s *Server) keepUnattended(w http.ResponseWriter, r *http.Request, u user) {
	a, up := s.Appliance, s.Upgrades
	if a == nil || up == nil {
		writeError(w, http.StatusNotFound, "automatic upgrades are the appliance's")
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.require(w, r, u, "/", append(slices.Clone(setupPrivileges), haPrivilege)...) {
		return
	}
	if !a.Ready() {
		writeError(w, http.StatusConflict, "there is no management cluster to upgrade yet")
		return
	}
	key := "passphrase:" + u.ticket.Username
	if !s.limiter.allow(key) {
		writeError(w, http.StatusTooManyRequests, "too many wrong passphrases; try again later")
		return
	}
	switch err := a.CheckPassphrase(r.Context(), req.Passphrase); {
	case errors.Is(err, appliance.ErrPassphrase):
		s.limiter.fail(key)
		writeError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, appliance.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	token, err := s.Accounts.CreateAdministratorToken(r.Context(), u.ticket, upgraderUser, upgraderTokenName, upgraderComment)
	if err != nil {
		s.proxmoxChangeError(w, err)
		return
	}
	err = a.KeepUnattended(appliance.Unattended{Passphrase: req.Passphrase, Token: token, By: u.ticket.Username, Since: up.Now()})
	if err != nil {
		s.Log.Error("keeping what automatic upgrades need", "error", err)
		if delErr := s.Accounts.DeleteUser(r.Context(), u.ticket, upgraderUser); delErr != nil {
			s.Log.Error("deleting the automatic upgrades' Proxmox VE user again", "error", delErr)
		}
		writeError(w, http.StatusInternalServerError, "could not keep what automatic upgrades need")
		return
	}
	s.Log.Warn("automatic upgrades of the management cluster switched on: the appliance keeps the state passphrase and an administrator's API token",
		"user", u.ticket.Username, "proxmoxUser", upgraderUser)
	up.wake()
	writeJSON(w, http.StatusOK, s.managementAutomatic())
}

// forgetUnattended switches automatic upgrades of the management cluster off: what the appliance
// kept goes first, so that nothing on it reaches the state or Proxmox VE even when Proxmox VE then
// refuses to delete the user.
func (s *Server) forgetUnattended(w http.ResponseWriter, r *http.Request, u user) {
	a, up := s.Appliance, s.Upgrades
	if a == nil || up == nil {
		writeError(w, http.StatusNotFound, "automatic upgrades are the appliance's")
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	if err := a.ForgetUnattended(); err != nil {
		s.Log.Error("switching automatic upgrades of the management cluster off", "error", err)
		writeError(w, http.StatusInternalServerError, "could not delete what automatic upgrades kept")
		return
	}
	s.Log.Warn("automatic upgrades of the management cluster switched off", "user", u.ticket.Username)
	up.wake()
	if err := s.Accounts.DeleteUser(r.Context(), u.ticket, upgraderUser); err != nil {
		s.Log.Error("deleting the automatic upgrades' Proxmox VE user", "error", err)
		writeError(w, http.StatusBadGateway, "the appliance keeps nothing any more, but Proxmox VE did not delete the user "+upgraderUser+": "+err.Error()+"; delete it in Proxmox VE")
		return
	}
	writeJSON(w, http.StatusOK, s.managementAutomatic())
}

// proxmoxChangeError answers a change Proxmox VE refused with what it said.
func (s *Server) proxmoxChangeError(w http.ResponseWriter, err error) {
	if errors.Is(err, pve.ErrAuthentication) || errors.Is(err, pve.ErrForbidden) {
		s.proxmoxError(w, err)
		return
	}
	s.Log.Error("proxmox VE refused a change", "error", err)
	writeError(w, http.StatusBadGateway, "Proxmox VE: "+err.Error())
}

// dropUpgrader is part of removing the management cluster: automatic upgrades go with it, the
// Proxmox VE user included, whether or not they were on.
func (s *Server) dropUpgrader(ctx context.Context, t *pve.Ticket, j *appliance.Job) error {
	a := s.Appliance
	if a.UnattendedOn() != nil {
		j.Add("Switching automatic upgrades of the management cluster off")
	}
	if err := a.ForgetUnattended(); err != nil {
		return fmt.Errorf("switching automatic upgrades off: %w", err)
	}
	if err := s.Accounts.DeleteUser(ctx, t, upgraderUser); err != nil {
		return fmt.Errorf("switching automatic upgrades off: %w", err)
	}
	return nil
}

// applyDueManagement starts an automatic apply of the management cluster when it is due: switched
// on, behind the appliance's release, in the upgrade hour, once a day, never beside another job or
// an upgrade of the appliance asked for and not yet begun. A run that fails is tried again the next
// day, not before.
func (s *Server) applyDueManagement() {
	a, up := s.Appliance, s.Upgrades
	if a == nil || up == nil {
		return
	}
	if a.UnattendedOn() == nil {
		return
	}
	mu := s.managementUpgrade()
	if mu == nil || !mu.Behind || up.Spool.Pending() || !up.managementDue(s.applying()) {
		return
	}
	err := a.ApplyUnattended(func(_ context.Context, kubeconfig []byte) error { return s.useKubeconfig(kubeconfig) })
	switch {
	case jobConflict(err):
		// Another job took the appliance meanwhile; the next minute looks again.
		return
	case err != nil:
		s.Log.Error("automatic management-cluster upgrade", "error", err)
	default:
		s.Log.Info("automatic management-cluster upgrade started", "from", mu.Applied, "to", mu.Carried)
	}
	up.managementStarted()
}
