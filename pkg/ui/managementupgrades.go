package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// The Proxmox VE user automatic upgrades of the management cluster act as. Its roles let it change
// the management cluster's VMs and read the rest (provisioner.UpgraderRoles). It is in the group of
// the module's token users (management-cluster/proxmox.tf, tokens), so that it reads their tokens
// and revokes its own.
const (
	upgraderUser      = "kubehenge-upgrader@pve"
	upgraderTokenName = "apply"
	upgraderComment   = "Kubehenge's appliance: upgrades the management cluster after each Kubehenge upgrade"
	upgraderGroup     = "kubehenge-tokens"
)

// Upgraders make, narrow and delete the Proxmox VE user automatic upgrades act as.
type Upgraders interface {
	CheckAccessControl(ctx context.Context, t *pve.Ticket) error
	CreateUpgrader(ctx context.Context, t *pve.Ticket, u *pve.Upgrader) (string, error)
	GrantUpgrader(ctx context.Context, t *pve.Ticket, u *pve.Upgrader) error
	RevokeToken(ctx context.Context, token string) error
	DeleteUser(ctx context.Context, t *pve.Ticket, userid string) error
}

// upgraderOf is the upgrader for the management cluster the settings describe.
func upgraderOf(s *appliance.Settings) *pve.Upgrader {
	network := "/sdn/zones/localnetwork/" + s.Bridge
	if zone, vnet, ok := strings.Cut(s.Bridge, "/"); ok {
		network = "/sdn/zones/" + zone + "/" + vnet
	}
	places := &provisioner.UpgraderPlaces{
		ManagementPool: managementPool, VMDatastore: s.VMDatastore, ImageDatastore: s.ImageDatastore,
		Network: network, TokenGroup: upgraderGroup, Nodes: s.Nodes,
	}
	grants := provisioner.UpgraderGrants(places)
	u := &pve.Upgrader{User: upgraderUser, Token: upgraderTokenName, Comment: upgraderComment, Group: upgraderGroup, Roles: provisioner.UpgraderRoles}
	for _, g := range grants {
		u.Grants = append(u.Grants, pve.Grant{Path: g.Path, Role: g.Role, Propagate: g.Propagate})
	}
	return u
}

// managementUpgrade tells whether the management cluster runs what an older release of the
// appliance applied, and what applying this one changes.
type managementUpgrade struct {
	// Applied is the release that last applied the management cluster, Carried the appliance's own.
	Applied string `json:"applied"`
	Carried string `json:"carried"`
	Behind  bool   `json:"behind"`
	// Changes are the components whose versions applying this release changes, in the order the
	// module lists them. A side the record does not know is left out.
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

// newerRelease tells whether carried is a later release than applied. A build that is no release,
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
	// KeyLost tells that the runs cannot open what was kept, as after a restore, until renewed.
	KeyLost bool `json:"keyLost,omitempty"`
	// Failed says why the last apply could not switch them on.
	Failed string `json:"failed,omitempty"`
	// ImageStorage is the storage whose files the user may delete, beyond the management cluster's
	// own VMs.
	ImageStorage string `json:"imageStorage,omitempty"`
}

func (s *Server) managementAutomatic() *managementAutomatic {
	a := s.Appliance
	if a == nil {
		return nil
	}
	v := &managementAutomatic{Possible: a.Ready(), User: upgraderUser, Failed: a.UnattendedChoice().Failed}
	if saved := a.SavedSettings(); saved != nil {
		v.ImageStorage = saved.ImageDatastore
	}
	if kept := a.UnattendedOn(); kept != nil && !a.UnattendedChoice().Off {
		v.On, v.By, v.Since, v.User, v.KeyLost, v.Failed = true, kept.By, &kept.Since, kept.User, kept.KeyLost, ""
	}
	return v
}

// switchOnUpgrades makes the user automatic upgrades act as, with the administrator's session, and
// keeps its token, and the passphrase where the state requires one. It does nothing before every
// node of Proxmox VE has a libpve-access-control that keeps a group's user-modifiers from making
// tokens that act as its members. A user kept already is given
// the upgrader's grants alone, as one made with Administrator on / needs. One whose key is lost is
// made anew.
func (s *Server) switchOnUpgrades(ctx context.Context, t *pve.Ticket, passphrase string) error {
	a := s.Appliance
	saved := a.SavedSettings()
	if saved == nil {
		return errors.New("no management cluster to upgrade yet")
	}
	spec := upgraderOf(saved)
	if kept := a.UnattendedOn(); kept != nil && !kept.KeyLost {
		err := s.Accounts.GrantUpgrader(ctx, t, spec)
		if err == nil && kept.Narrow {
			return nil
		}
		if err == nil {
			if markErr := a.MarkUnattendedNarrow(); markErr != nil {
				return fmt.Errorf("recording the narrowed upgrader: %w", markErr)
			}
			return nil
		}
		// The user is gone or refuses, so it is made anew.
		s.Log.WarnContext(ctx, "the automatic upgrades' Proxmox VE user is made anew", "error", err)
	}
	token, err := s.Accounts.CreateUpgrader(ctx, t, spec)
	if err != nil {
		return fmt.Errorf("making %s: %w", upgraderUser, err)
	}
	err = a.KeepUnattended(appliance.Unattended{Passphrase: passphrase, Token: token, By: t.Username, Since: s.now(), Narrow: true})
	if err != nil {
		return errors.Join(err, s.Accounts.DeleteUser(ctx, t, upgraderUser))
	}
	return nil
}

// upgradesAfterApply switches automatic upgrades on after a successful apply with an
// administrator's session, with what the apply had, unless an administrator switched them off. A
// failure leaves the apply as it is, shows on the setting, and the next apply tries again.
func (s *Server) upgradesAfterApply(ctx context.Context, t *pve.Ticket, passphrase string) {
	a := s.Appliance
	c := a.UnattendedChoice()
	if c.Off || s.Upgrades == nil {
		return
	}
	c.Failed = ""
	if err := s.switchOnUpgrades(ctx, t, passphrase); err != nil {
		s.Log.ErrorContext(ctx, "switching automatic upgrades of the management cluster on", "error", err)
		c.Failed = fmt.Sprintf("Off: %s could not be made (%v). Apply again as a Proxmox VE administrator.", upgraderUser, err)
		if errors.Is(err, pve.ErrAccessControlOld) {
			c.Failed = fmt.Sprintf("Off: %v. Apply again after.", errors.Unwrap(err))
		}
	}
	if err := a.ChooseUnattended(c); err != nil {
		s.Log.ErrorContext(ctx, "recording automatic upgrades of the management cluster", "error", err)
	}
}

// keepUnattended switches automatic upgrades of the management cluster on, or renews them after a
// restore lost their key. A passphrase is asked for only where the state requires one.
func (s *Server) keepUnattended(w http.ResponseWriter, r *http.Request, u user) {
	a, up := s.Appliance, s.Upgrades
	if a == nil || up == nil {
		writeError(w, http.StatusNotFound, "automatic upgrades need the appliance")
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	if !a.Ready() {
		writeError(w, http.StatusConflict, "no management cluster to upgrade yet")
		return
	}
	if a.PassphraseRequired() {
		done, ok := s.limiter.reserve(passphraseCount(u))
		if !ok {
			writeError(w, http.StatusTooManyRequests, "too many wrong passphrases. Try again later")
			return
		}
		err := a.CheckPassphrase(r.Context(), req.Passphrase)
		done(errors.Is(err, appliance.ErrPassphrase))
		switch {
		case errors.Is(err, appliance.ErrPassphrase):
			writeError(w, http.StatusForbidden, err.Error())
			return
		case errors.Is(err, appliance.ErrBusy):
			writeError(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	if err := s.switchOnUpgrades(r.Context(), u.ticket, req.Passphrase); err != nil {
		s.proxmoxChangeError(w, r, err)
		return
	}
	if err := a.ChooseUnattended(appliance.UnattendedChoice{}); err != nil {
		s.Log.ErrorContext(r.Context(), "recording automatic upgrades of the management cluster", "error", err)
		writeError(w, http.StatusInternalServerError, "automatic upgrades could not be switched on")
		return
	}
	s.Log.InfoContext(r.Context(), "automatic upgrades of the management cluster switched on", "user", u.ticket.Username, "proxmoxUser", upgraderUser)
	up.wake()
	writeJSON(w, http.StatusOK, s.managementAutomatic())
}

// forgetUnattended switches automatic upgrades of the management cluster off for good: the choice
// is recorded first, so that no apply switches them on again and a crash midway is finished by the
// next minute's check (finishSwitchOff).
func (s *Server) forgetUnattended(w http.ResponseWriter, r *http.Request, u user) {
	a, up := s.Appliance, s.Upgrades
	if a == nil || up == nil {
		writeError(w, http.StatusNotFound, "automatic upgrades need the appliance")
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	if err := a.ChooseUnattended(appliance.UnattendedChoice{Off: true}); err != nil {
		s.Log.ErrorContext(r.Context(), "switching automatic upgrades of the management cluster off", "error", err)
		writeError(w, http.StatusInternalServerError, "automatic upgrades could not be switched off")
		return
	}
	s.Log.WarnContext(r.Context(), "automatic upgrades of the management cluster switched off", "user", u.ticket.Username)
	up.wake()
	err := s.forgetUpgrader(r.Context(), u.ticket)
	var left *upgraderLeftError
	switch {
	case errors.As(err, &left):
		s.Log.ErrorContext(r.Context(), "deleting the automatic upgrades' Proxmox VE user", "error", err)
		writeError(w, http.StatusBadGateway, "automatic upgrades are off, but user "+upgraderUser+" is left. Delete it in Proxmox VE: "+left.err.Error())
		return
	case err != nil:
		s.Log.ErrorContext(r.Context(), "revoking the automatic upgrades' token", "error", err)
		writeError(w, http.StatusBadGateway, "automatic upgrades are off. Their token is revoked within a minute: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.managementAutomatic())
}

// upgraderLeftError is a switch-off that left the upgrader's user, without its token.
type upgraderLeftError struct{ err error }

func (e *upgraderLeftError) Error() string {
	return "deleting the user " + upgraderUser + ": " + e.err.Error()
}

func (e *upgraderLeftError) Unwrap() error { return e.err }

// forgetUpgrader deletes what automatic upgrades keep and what they act as: the passphrase, then
// the token in Proxmox VE, then the key and the sealed token. The key goes after the token, so that a
// crash at any step leaves the token revocable with itself. The session, when there is one,
// deletes the user with its token. Otherwise, or when it may not, the token revokes itself.
func (s *Server) forgetUpgrader(ctx context.Context, t *pve.Ticket) error {
	a := s.Appliance
	if err := a.ForgetUnattendedPassphrase(); err != nil {
		return fmt.Errorf("deleting the kept passphrase: %w", err)
	}
	var userErr error
	if t != nil {
		userErr = s.Accounts.DeleteUser(ctx, t, upgraderUser)
	}
	if t == nil || userErr != nil {
		token, err := a.UnattendedToken()
		switch {
		case err == nil:
			if revokeErr := s.Accounts.RevokeToken(ctx, token); revokeErr != nil {
				return errors.Join(userErr, revokeErr)
			}
		// A token whose key is gone opens for no one.
		case !errors.Is(err, appliance.ErrNotUnattended) && !errors.Is(err, appliance.ErrUnattendedKey):
			return fmt.Errorf("opening the kept token: %w", err)
		}
	}
	if err := a.ForgetUnattended(); err != nil {
		return fmt.Errorf("deleting the kept token and its key: %w", err)
	}
	if userErr != nil {
		return &upgraderLeftError{userErr}
	}
	return nil
}

// finishSwitchOff ends a switch-off that a crash cut short: the token revokes itself.
func (s *Server) finishSwitchOff(ctx context.Context) {
	a := s.Appliance
	if !a.UnattendedChoice().Off || a.UnattendedOn() == nil {
		return
	}
	if err := s.forgetUpgrader(ctx, nil); err != nil {
		s.Log.ErrorContext(ctx, "finishing switching automatic upgrades of the management cluster off", "error", err)
		return
	}
	s.Log.InfoContext(ctx, "automatic upgrades of the management cluster: their token is revoked, and user "+upgraderUser+" is left without one")
}

// narrowUpgrader moves a user made with Administrator on / to the upgrader's grants alone, with its
// own token, once an automatic run with it has applied the release that brings them.
func (s *Server) narrowUpgrader(ctx context.Context) {
	a := s.Appliance
	kept, saved := a.UnattendedOn(), a.SavedSettings()
	if kept == nil || kept.Narrow || saved == nil {
		return
	}
	token, err := a.UnattendedToken()
	if err == nil {
		id, _, _ := strings.Cut(token, "=")
		err = s.Accounts.GrantUpgrader(ctx, &pve.Ticket{Username: id, Token: token}, upgraderOf(saved))
	}
	if err == nil {
		err = a.MarkUnattendedNarrow()
	}
	if err != nil {
		s.Log.ErrorContext(ctx, "moving the automatic upgrades' Proxmox VE user to its own roles", "error", err)
	}
}

// proxmoxChangeError answers a change Proxmox VE refused with what it said.
func (s *Server) proxmoxChangeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pve.ErrAuthentication) || errors.Is(err, pve.ErrForbidden) {
		s.proxmoxError(w, r, err)
		return
	}
	s.Log.ErrorContext(r.Context(), "proxmox VE refused a change", "error", err)
	writeError(w, http.StatusBadGateway, "Proxmox VE: "+err.Error())
}

// dropUpgrader is part of removing the management cluster: automatic upgrades go with it, the
// Proxmox VE user included, whether or not they were on.
func (s *Server) dropUpgrader(ctx context.Context, t *pve.Ticket, j *appliance.Job) error {
	a := s.Appliance
	if a.UnattendedOn() != nil {
		j.Add("Switching automatic upgrades off")
	}
	if err := s.forgetUpgrader(ctx, t); err != nil {
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
	s.finishSwitchOff(context.Background())
	if a.UnattendedOn() == nil || a.UnattendedChoice().Off {
		return
	}
	mu := s.managementUpgrade()
	if mu == nil || !mu.Behind || up.Spool.Pending() || !up.managementDue(s.applying()) {
		return
	}
	err := a.ApplyUnattended(func(ctx context.Context, kubeconfig []byte) error {
		if err := s.useKubeconfig(kubeconfig); err != nil {
			return err
		}
		s.narrowUpgrader(ctx)
		return nil
	})
	switch {
	case jobConflict(err):
		// Another job took the appliance meanwhile. The next minute looks again.
		return
	case err != nil:
		s.Log.Error("automatic management-cluster upgrade", "error", err)
	default:
		s.Log.Info("automatic management-cluster upgrade started", "from", mu.Applied, "to", mu.Carried)
	}
	up.managementStarted()
}
