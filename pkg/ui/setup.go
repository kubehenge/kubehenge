package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// setupPrivileges are what the management-cluster module needs of the user it runs as: it creates
// pools, users, groups, roles, ACLs, tokens and VMs, and downloads the Talos image. Proxmox VE
// checks each call again; asking first saves a half-done run.
var setupPrivileges = []string{
	"Sys.Modify", "Permissions.Modify", "User.Modify", "Group.Allocate", "Realm.AllocateUser", "Pool.Allocate",
	"VM.Allocate", "Datastore.AllocateSpace", "Datastore.AllocateTemplate",
}

type setupView struct {
	// Appliance tells whether the interface runs in the appliance, which sets up the management
	// cluster; otherwise it was given one.
	Appliance bool `json:"appliance"`
	// Ready tells whether the management cluster is there.
	Ready bool `json:"ready"`
	// Allowed tells whether the user may run the setup.
	Allowed  bool                `json:"allowed"`
	Job      *appliance.JobView  `json:"job,omitempty"`
	Settings *appliance.Settings `json:"settings,omitempty"`
	// Backup tells whether a Proxmox VE backup job includes the appliance, which holds the only
	// copy of the management cluster's state. Unset when that is not known.
	Backup *bool `json:"backup,omitempty"`
	// CertificateExpires is when the interface's certificate runs out, for the administrators who
	// renew it on the node with the installer, naming the appliance by its guest ID.
	CertificateExpires *time.Time `json:"certificateExpires,omitempty"`
	ApplianceVMID      int        `json:"applianceVMID,omitempty"`
	// CredentialsExpire is when the management cluster's admin credentials in the state run out,
	// which an apply in their last month renews.
	CredentialsExpire *time.Time `json:"credentialsExpire,omitempty"`
	// ProxmoxInsecure tells that the Proxmox VE API's certificate goes unverified.
	ProxmoxInsecure bool `json:"proxmoxInsecure,omitempty"`
	// Resumes tells that an earlier run left its encrypted state, so the setup takes that run's
	// passphrase rather than a new one.
	Resumes bool `json:"resumes,omitempty"`
	// Upgrade tells which release last applied the management cluster, and what applying the
	// appliance's own would change.
	Upgrade *managementUpgrade `json:"upgrade,omitempty"`
}

func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request, u user) {
	v := setupView{Ready: s.currentKube() != nil}
	if s.Appliance != nil && !s.applianceStatus(w, r, u, &v) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// applianceStatus fills in what the appliance knows about its setup, or answers the request with
// the error that stopped it.
func (s *Server) applianceStatus(w http.ResponseWriter, r *http.Request, u user, v *setupView) bool {
	a := s.Appliance
	v.Appliance = true
	privs, err := s.Accounts.Privileges(r.Context(), u.ticket, "/")
	if err != nil {
		s.proxmoxError(w, err)
		return false
	}
	v.Allowed = privs.Has(setupPrivileges...)
	// Only those who may run it see how it went; the log names nodes, storages and addresses.
	if !v.Allowed {
		return true
	}
	v.Job, v.Settings = a.Current(), a.SavedSettings()
	v.Resumes = !v.Ready && a.HasState()
	if v.Ready {
		v.Upgrade = s.managementUpgrade()
		if last := a.LastApplied(); last != nil && !last.CredentialsExpire.IsZero() {
			v.CredentialsExpire = &last.CredentialsExpire
		}
	}
	v.ApplianceVMID, v.ProxmoxInsecure = s.ApplianceVMID, s.ProxmoxInsecure
	if !s.CertificateExpires.IsZero() {
		v.CertificateExpires = &s.CertificateExpires
	}
	if !v.Ready || s.ApplianceVMID == 0 || !privs.Has("Sys.Audit") {
		return true
	}
	covered, err := s.backupCovers(r.Context(), u)
	if err != nil {
		s.Log.Warn("checking the appliance's backup", "error", err)
		return true
	}
	v.Backup = &covered
	return true
}

// backupCache remembers, per user, whether a backup job covers the appliance: finding out lists
// every guest of the Proxmox VE cluster, while backup jobs change seldom and the answer only
// warns.
type backupCache struct {
	mu   sync.Mutex
	seen map[string]backupSeen
}

type backupSeen struct {
	covered bool
	until   time.Time
}

const backupTTL = 5 * time.Minute

func (s *Server) backupCovers(ctx context.Context, u user) (bool, error) {
	c, now := &s.backups, time.Now()
	c.mu.Lock()
	e, ok := c.seen[u.ticket.Username]
	c.mu.Unlock()
	if ok && now.Before(e.until) {
		return e.covered, nil
	}
	covered, err := s.Accounts.BackupCovers(ctx, u.ticket, s.ApplianceVMID)
	if err != nil {
		return false, fmt.Errorf("the backup jobs covering VM %d: %w", s.ApplianceVMID, err)
	}
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]backupSeen{}
	}
	for name, e := range c.seen {
		if now.After(e.until) {
			delete(c.seen, name)
		}
	}
	c.seen[u.ticket.Username] = backupSeen{covered: covered, until: now.Add(backupTTL)}
	c.mu.Unlock()
	return covered, nil
}

// credential hands an administrator the management cluster's admin kubeconfig or talosconfig, for
// repairs beyond the interface. It needs the state passphrase on top of the privileges.
func (s *Server) credential(w http.ResponseWriter, r *http.Request, u user) {
	a := s.Appliance
	if a == nil {
		writeError(w, http.StatusNotFound, "this interface does not manage its management cluster")
		return
	}
	var req struct {
		Name       string `json:"name"`
		Passphrase string `json:"passphrase"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	done, ok := s.limiter.reserve(passphraseCount(u))
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too many wrong passphrases. Try again later")
		return
	}
	data, err := a.Credential(r.Context(), req.Passphrase, req.Name)
	done(errors.Is(err, appliance.ErrPassphrase))
	switch {
	case errors.Is(err, appliance.ErrPassphrase):
		writeError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, appliance.ErrBusy), errors.Is(err, appliance.ErrUpgrading):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.Log.Warn("handed out the management cluster's credential", "user", u.ticket.Username, "credential", req.Name)
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="management.`+req.Name+`"`)
	_, _ = w.Write(data)
}

// templatePool is where the management-cluster module keeps the clusters' templates; the
// appliance leaves its template_pool at the default.
const templatePool = "kubehenge-templates"

// remove takes the management cluster down and returns the appliance to its first run. Workload
// clusters have to be deleted first, so none is left without the controller that would clean up
// its Proxmox VE objects.
func (s *Server) remove(w http.ResponseWriter, r *http.Request, u user) {
	a := s.Appliance
	if a == nil {
		writeError(w, http.StatusNotFound, "this interface does not manage its management cluster")
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
		Confirm    string `json:"confirm"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Confirm != "remove" {
		writeError(w, http.StatusUnprocessableEntity, `confirm with "remove"`)
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	// Asked first: while a removal runs, the management cluster is going and answers no question.
	if err := a.Idle(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if kube := s.currentKube(); kube != nil {
		var list clusterv1.ClusterList
		if err := kube.List(r.Context(), &list); err != nil {
			s.kubeError(w, err)
			return
		}
		if n := len(list.Items); n > 0 {
			writeError(w, http.StatusConflict, fmt.Sprintf("delete the %d clusters first", n))
			return
		}
	}
	ticket, err := s.Accounts.Renew(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	err = a.Remove(req.Passphrase, ticket, s.beforeRemoval(ticket), func() { s.activeKube.Store(nil) }) //nolint:contextcheck // the job outlives the request on purpose
	switch {
	case jobConflict(err):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.Log.Warn("management cluster removal started", "user", u.ticket.Username)
	writeJSON(w, http.StatusAccepted, a.Current())
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request, u user) {
	a := s.Appliance
	if a == nil {
		writeError(w, http.StatusNotFound, "this interface does not manage its management cluster")
		return
	}
	var req struct {
		Settings   appliance.Settings `json:"settings"`
		Passphrase string             `json:"passphrase"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	releasing := a.ReleasesHA(&req.Settings)
	privileges := setupPrivileges
	if req.Settings.ProxmoxHA || releasing {
		privileges = append(slices.Clone(privileges), haPrivilege)
	}
	if req.Settings.EtcdRestore {
		privileges = append(slices.Clone(privileges), restorePrivilege)
	}
	if !s.require(w, r, u, "/", privileges...) {
		return
	}
	// The run needs the whole of a ticket's life, so it gets a new one rather than the session's.
	ticket, err := s.Accounts.Renew(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	err = a.Apply(&req.Settings, req.Passphrase, ticket, s.applied(ticket)) //nolint:contextcheck // the job outlives the request on purpose
	switch {
	case jobConflict(err):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.Log.Info("management cluster apply started", "user", u.ticket.Username)
	writeJSON(w, http.StatusAccepted, a.Current())
}

// jobConflict tells whether a job could not start because another one, of any kind, holds the
// appliance.
func jobConflict(err error) bool {
	return errors.Is(err, appliance.ErrBusy) || errors.Is(err, appliance.ErrTemplateBusy) || errors.Is(err, appliance.ErrUpgrading)
}

// beforeRemoval removes, as the ticket's user, what the management-cluster module did not create
// but goes with the management cluster: the clusters' templates, and the user automatic upgrades
// act as.
func (s *Server) beforeRemoval(ticket *pve.Ticket) func(context.Context, *appliance.Job) error {
	templates := s.templateRemover(ticket)
	return func(ctx context.Context, j *appliance.Job) error {
		if err := templates(ctx, j); err != nil {
			return err
		}
		return s.dropUpgrader(ctx, ticket, j)
	}
}

// templateRemover deletes the clusters' templates, which the management-cluster module keeps in
// its pool but did not create, as the ticket's user.
func (s *Server) templateRemover(ticket *pve.Ticket) func(context.Context, *appliance.Job) error {
	return func(ctx context.Context, j *appliance.Job) error {
		members, err := s.Accounts.PoolMembers(ctx, ticket, templatePool)
		if err != nil {
			return fmt.Errorf("listing the templates: %w", err)
		}
		for i := range members {
			m := &members[i]
			j.Add(fmt.Sprintf("Deleting template %s (VM %d)", m.Name, m.VMID))
			if err := s.Accounts.DeleteVM(ctx, ticket, m.Node, m.VMID); err != nil {
				return fmt.Errorf("deleting template %d: %w", m.VMID, err)
			}
		}
		return nil
	}
}

// haPrivilege is what Proxmox VE asks of whoever creates or removes HA resources and rules: the
// administrator who switches Proxmox VE HA for Kubehenge on grants it to Kubehenge's token, and the
// one who switches it off removes what the controller left.
const haPrivilege = "Sys.Console"

// restorePrivilege is what the restore token holds on the workload pool: the administrator who
// turns restores through the guest agent on grants it to Kubehenge, so holds it too.
const restorePrivilege = "VM.GuestAgent.Unrestricted"

// applied is what follows a successful apply: the interface switches to the management cluster,
// and when the apply switched Proxmox VE HA off, the clusters leave HA.
func (s *Server) applied(ticket *pve.Ticket) func(ctx context.Context, kubeconfig []byte) error {
	return func(ctx context.Context, kubeconfig []byte) error {
		if err := s.useKubeconfig(kubeconfig); err != nil {
			return err
		}
		a := s.Appliance
		if saved := a.SavedSettings(); saved == nil || !a.ReleasesHA(saved) {
			return nil
		}
		if err := s.releaseHA(ctx, ticket); err != nil {
			return err
		}
		return a.HAReleased()
	}
}

// releaseHA removes the HA rules and resources the controller made for the clusters, once an apply
// has taken its HA token away: without the token it can no longer remove them, and Proxmox VE
// refuses to destroy a VM that is still an HA resource.
func (s *Server) releaseHA(ctx context.Context, ticket *pve.Ticket) error {
	if err := s.Accounts.ReleaseHA(ctx, ticket, provisioner.HAComment(s.WorkloadPool+"/")); err != nil {
		return fmt.Errorf("removing the clusters from Proxmox VE HA: %w", err)
	}
	return nil
}

// useKubeconfig switches the interface to the management cluster an apply has just made.
func (s *Server) useKubeconfig(kubeconfig []byte) error {
	kube, err := s.NewKube(kubeconfig)
	if err != nil {
		return fmt.Errorf("switching to the new management cluster: %w", err)
	}
	s.setKube(kube)
	return nil
}
