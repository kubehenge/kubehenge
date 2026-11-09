package ui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// setupPrivileges are what the management-cluster module needs of the user it runs as: it creates
// pools, users, groups, roles, ACLs, tokens and VMs, downloads the Talos image and deletes it on
// removal. Proxmox VE checks each call again. Asking first saves a half-done run. The list is every
// privilege a run met a refusal for with a token that held nothing else: a VM's configuration needs
// the VM.Config.* ones, the image download Sys.AccessNetwork, listing a bridge SDN.Audit, attaching
// one SDN.Use, reading a pool Pool.Audit and deleting the image Datastore.Allocate.
var setupPrivileges = []string{
	"Sys.Modify", "Permissions.Modify", "User.Modify", "Group.Allocate", "Realm.AllocateUser", "Pool.Allocate",
	"Pool.Audit", "VM.Allocate", "VM.Audit", "VM.PowerMgmt", "VM.Config.CPU", "VM.Config.Cloudinit", "VM.Config.Disk",
	"VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options", "Datastore.Allocate",
	"Datastore.AllocateSpace", "Datastore.AllocateTemplate", "SDN.Audit", "SDN.Use", "Sys.AccessNetwork",
}

type setupView struct {
	// Appliance tells whether the interface runs in the appliance, which sets up the management
	// cluster. Otherwise it was given one.
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
	// renew it on the node with the installer. They name the appliance by its guest ID.
	CertificateExpires *time.Time `json:"certificateExpires,omitempty"`
	ApplianceVMID      int        `json:"applianceVMID,omitempty"`
	// AddressMoved tells that the appliance no longer has the address its certificate is for.
	AddressMoved *addressMove `json:"addressMoved,omitempty"`
	// DiskFull tells that the appliance's disk is nearly full.
	DiskFull *diskFull `json:"diskFull,omitempty"`
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
	// ActivityFollows tells that the interface reads every API server's log as it is written, so
	// a change made now reaches Activity.
	ActivityFollows bool `json:"activityFollows,omitempty"`
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
		s.proxmoxError(w, r, err)
		return false
	}
	v.Allowed = privs.Has(setupPrivileges...)
	// Only those who may run it see how it went. The log names nodes, storages and addresses.
	if !v.Allowed {
		return true
	}
	v.Job, v.Settings = a.Current(), a.SavedSettings().Redacted()
	v.Resumes = !v.Ready && a.HasState()
	if v.Ready {
		v.Upgrade = s.managementUpgrade()
		v.ActivityFollows = s.followsAuditLog()
		if last := a.LastApplied(); last != nil && !last.CredentialsExpire.IsZero() {
			v.CredentialsExpire = &last.CredentialsExpire
		}
	}
	v.ApplianceVMID, v.ProxmoxInsecure = s.applianceID(r.Context(), u), s.ProxmoxInsecure
	if !s.CertificateExpires.IsZero() {
		v.CertificateExpires = &s.CertificateExpires
	}
	v.AddressMoved, v.DiskFull = s.addressMoved(r.Host), s.diskFull(r.Context())
	if !v.Ready || v.ApplianceVMID == 0 || !privs.Has("Sys.Audit") {
		return true
	}
	covered, err := s.backupCovers(r.Context(), u, v.ApplianceVMID)
	if err != nil {
		s.Log.WarnContext(r.Context(), "checking the appliance's backup", "error", err)
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
	vmid    int
	covered bool
	until   time.Time
}

const backupTTL = 5 * time.Minute

// addressMove is the addresses the certificate is for, From, and those the appliance has, To.
type addressMove struct {
	From []string `json:"from"`
	To   []string `json:"to"`
}

// addressMoved tells that none of the addresses the certificate is for is the appliance's any
// more, as after its DHCP lease changed or a move to another network. Nil while one still is, or
// while the user reaches the appliance by a DNS name the certificate is for.
func (s *Server) addressMoved(host string) *addressMove {
	if len(s.CertificateIPs) == 0 || s.OwnAddresses == nil {
		return nil
	}
	own := s.OwnAddresses()
	if slices.ContainsFunc(s.CertificateIPs, func(ip netip.Addr) bool { return slices.Contains(own, ip) }) {
		return nil
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if slices.ContainsFunc(s.CertificateNames, func(n string) bool { return strings.EqualFold(n, host) }) {
		return nil
	}
	m := &addressMove{From: []string{}, To: []string{}}
	for _, ip := range s.CertificateIPs {
		m.From = append(m.From, ip.String())
	}
	for _, ip := range own {
		m.To = append(m.To, ip.String())
	}
	return m
}

// diskWarning is the share of the appliance's disk in use, in per cent, from which administrators
// are told to grow it. An upgrade needs room for three copies of the software, and OpenTofu
// rewrites the state in place.
const (
	diskWarning = 80
	mebibyte    = 1 << 20
)

// diskFull is the share of the appliance's disk in use, in per cent, and the mebibytes left.
type diskFull struct {
	UsedPercent uint64 `json:"usedPercent"`
	FreeMiB     uint64 `json:"freeMiB"`
}

// diskFull tells how full the appliance's disk is, once it is past diskWarning. Nil below that, or
// when the use is not known.
func (s *Server) diskFull(ctx context.Context) *diskFull {
	if s.DiskUse == nil {
		return nil
	}
	used, free, err := s.DiskUse()
	if err != nil {
		s.Log.WarnContext(ctx, "reading the appliance's disk use", "error", err)
		return nil
	}
	total := used + free
	if total == 0 {
		return nil
	}
	// Rounded up, as df rounds it.
	percent := (used*100 + total - 1) / total
	if percent < diskWarning {
		return nil
	}
	return &diskFull{UsedPercent: percent, FreeMiB: free / mebibyte}
}

// applianceID is the appliance's guest ID as the user sees it, found by its MAC addresses where
// they are known, or the installed one. 0 when no guest the user sees has its MAC: a restore or a
// clone took it out of their sight, and the installed ID would name another guest or none.
func (s *Server) applianceID(ctx context.Context, u user) int {
	if len(s.ApplianceMACs) == 0 {
		return s.ApplianceVMID
	}
	id, err := s.applianceIDs.get(ctx, u.ticket.Username, backupTTL, time.Now, func(ctx context.Context) int {
		id, err := s.Accounts.ApplianceID(ctx, u.ticket, s.ApplianceVMID, s.ApplianceMACs)
		switch {
		case errors.Is(err, pve.ErrNoAppliance):
			return 0
		case err != nil:
			s.Log.WarnContext(ctx, "finding the appliance's guest ID", "error", err)
			return s.ApplianceVMID
		}
		return id
	})
	if err != nil {
		return s.ApplianceVMID
	}
	return id
}

func (s *Server) backupCovers(ctx context.Context, u user, vmid int) (bool, error) {
	c, now := &s.backups, time.Now()
	c.mu.Lock()
	e, ok := c.seen[u.ticket.Username]
	c.mu.Unlock()
	if ok && e.vmid == vmid && now.Before(e.until) {
		return e.covered, nil
	}
	covered, err := s.Accounts.BackupCovers(ctx, u.ticket, vmid)
	if err != nil {
		return false, fmt.Errorf("the backup jobs covering VM %d: %w", vmid, err)
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
	c.seen[u.ticket.Username] = backupSeen{vmid: vmid, covered: covered, until: now.Add(backupTTL)}
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
	s.Log.WarnContext(r.Context(), "handed out the management cluster's credential", "user", u.ticket.Username, "credential", req.Name)
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="management.`+req.Name+`"`)
	_, _ = w.Write(data)
}

// takeOver makes this appliance's state the one the management cluster follows, after an apply
// was refused because a newer state changed the cluster. It needs the state passphrase on top of
// the privileges, as the next apply undoes what that newer state made.
func (s *Server) takeOver(w http.ResponseWriter, r *http.Request, u user) {
	a := s.Appliance
	if a == nil {
		writeError(w, http.StatusNotFound, "this interface does not manage its management cluster")
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
	done, ok := s.limiter.reserve(passphraseCount(u))
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too many wrong passphrases. Try again later")
		return
	}
	err := a.TakeOver(r.Context(), req.Passphrase)
	done(errors.Is(err, appliance.ErrPassphrase))
	switch {
	case errors.Is(err, appliance.ErrPassphrase):
		writeError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, appliance.ErrBusy), errors.Is(err, appliance.ErrUpgrading):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.Log.WarnContext(r.Context(), "took the management cluster over", "user", u.ticket.Username)
	w.WriteHeader(http.StatusNoContent)
}

// templatePool is where the management-cluster module keeps the clusters' templates. The
// appliance leaves its template_pool at the default.
const templatePool = "kubehenge-templates"

// managementPool is where the management-cluster module puts the management cluster's VMs.
const managementPool = "kubehenge-management"

// removalMayGoOn answers a removal whose management cluster did not answer. It returns true,
// having written nothing, when Proxmox VE holds none of the cluster's VMs, so the removal may go on
// and delete the rest. With VMs left it refuses, naming them, since removing the cluster under a VM
// that may still run workload clusters would orphan them.
func (s *Server) removalMayGoOn(w http.ResponseWriter, r *http.Request, u user, asked error) bool {
	members, err := s.Accounts.PoolMembers(r.Context(), u.ticket, managementPool)
	if err != nil {
		s.proxmoxError(w, r, err)
		return false
	}
	var ids []string
	for i := range members {
		if members[i].Template == 0 {
			ids = append(ids, strconv.Itoa(members[i].VMID))
		}
	}
	if len(ids) == 0 {
		s.Log.WarnContext(r.Context(), "removing a management cluster that no longer answers", "error", asked)
		return true
	}
	writeError(w, http.StatusConflict, "the management cluster does not answer and Proxmox VE still holds its VMs "+
		strings.Join(ids, ", ")+". Start them so it can answer, or remove them in Proxmox VE, then remove again.")
	return false
}

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
			// A removal that stopped part way left a management cluster that cannot answer. It goes on
			// once Proxmox VE holds none of its VMs, or no retry could ever finish it.
			if !s.removalMayGoOn(w, r, u, err) {
				return
			}
		} else if n := len(list.Items); n > 0 {
			writeError(w, http.StatusConflict, fmt.Sprintf("delete the %d clusters first", n))
			return
		}
	}
	ticket, err := s.Accounts.Renew(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, r, err)
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
	s.Log.WarnContext(r.Context(), "management cluster removal started", "user", u.ticket.Username)
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
	// The interface never had the proxy's password, so it comes back empty unless typed anew.
	req.Settings.KeepsProxySecret(a.SavedSettings())
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
	findings, err := s.setupPreflight(r.Context(), u, &req.Settings)
	if err != nil {
		s.writeSetupError(w, r, err)
		return
	}
	if len(findings.Errors) > 0 {
		writeError(w, http.StatusUnprocessableEntity, strings.Join(findings.Errors, " "))
		return
	}
	// The run needs the whole of a ticket's life, so it gets a new one rather than the session's.
	ticket, err := s.Accounts.Renew(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, r, err)
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
	s.Log.InfoContext(r.Context(), "management cluster apply started", "user", u.ticket.Username)
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
