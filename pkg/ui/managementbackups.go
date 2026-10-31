package ui

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// etcdBackupLook is how often the appliance looks whether a snapshot of the management cluster's
// etcd is due.
const etcdBackupLook = time.Minute

// managementBackupsView is the backups of the management cluster's etcd, with the targets they
// may go to.
type managementBackupsView struct {
	*appliance.EtcdBackupStatus

	Targets []string `json:"targets"`
	// KeysStored tells that an access key is kept, which a change of the settings may keep.
	KeysStored bool `json:"keysStored"`
}

// managementSnapshotsView lists the snapshots in the target, or why it cannot.
type managementSnapshotsView struct {
	Snapshots []provisioner.EtcdSnapshot `json:"snapshots"`
	Error     string                     `json:"error,omitempty"`
}

// mayBackUpManagement answers for the appliance's backups: those who may set it up.
func (s *Server) mayBackUpManagement(w http.ResponseWriter, r *http.Request, u user) bool {
	if s.Appliance == nil {
		writeError(w, http.StatusNotFound, "the management cluster's backups are the appliance's")
		return false
	}
	return s.require(w, r, u, "/", setupPrivileges...)
}

func (s *Server) managementBackups(w http.ResponseWriter, r *http.Request, u user) {
	if !s.mayBackUpManagement(w, r, u) {
		return
	}
	writeJSON(w, http.StatusOK, s.managementBackupsOf())
}

func (s *Server) managementBackupsOf() managementBackupsView {
	a := s.Appliance
	v := managementBackupsView{EtcdBackupStatus: a.EtcdBackups(), Targets: []string{}, KeysStored: a.EtcdBackupKeysStored()}
	if saved := a.SavedSettings(); saved != nil {
		for _, t := range saved.BackupTargets {
			v.Targets = append(v.Targets, t.Name)
		}
	}
	return v
}

func (s *Server) managementSnapshots(w http.ResponseWriter, r *http.Request, u user) {
	if !s.mayBackUpManagement(w, r, u) {
		return
	}
	list, err := s.Appliance.ListEtcdSnapshots(r.Context())
	v := managementSnapshotsView{Snapshots: slices.Clip(list)}
	if v.Snapshots == nil {
		v.Snapshots = []provisioner.EtcdSnapshot{}
	}
	if err != nil {
		v.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, v)
}

// checkPassphrase settles the try reservePassphrase held, counting it only when the passphrase was
// wrong, and answers for a failure.
func checkPassphrase(w http.ResponseWriter, done func(failed bool), err error) bool {
	done(errors.Is(err, appliance.ErrPassphrase))
	switch {
	case errors.Is(err, appliance.ErrPassphrase):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, appliance.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		return true
	}
	return false
}

// reservePassphrase holds one of the user's passphrase tries, refused once too many were wrong.
func (s *Server) reservePassphrase(w http.ResponseWriter, u user) (func(failed bool), bool) {
	done, ok := s.limiter.reserve(passphraseCount(u))
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too many wrong passphrases; try again later")
	}
	return done, ok
}

func (s *Server) configureManagementBackups(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Target          string `json:"target"`
		EveryHours      int    `json:"everyHours"`
		Keep            int    `json:"keep"`
		AccessKeyID     string `json:"accessKeyID"`
		SecretAccessKey string `json:"secretAccessKey"`
		Passphrase      string `json:"passphrase"`
	}
	if !readJSON(w, r, &req) || !s.mayBackUpManagement(w, r, u) {
		return
	}
	a := s.Appliance
	if !a.Ready() {
		writeError(w, http.StatusConflict, "there is no management cluster to back up yet")
		return
	}
	settings := appliance.EtcdBackupSettings{
		Target: req.Target, EveryHours: req.EveryHours, Keep: req.Keep, By: u.ticket.Username, Since: time.Now(),
	}
	done, ok := s.reservePassphrase(w, u)
	if !ok {
		return
	}
	err := a.ConfigureEtcdBackups(r.Context(), settings, req.AccessKeyID, req.SecretAccessKey, req.Passphrase)
	if !checkPassphrase(w, done, err) {
		return
	}
	s.Log.Info("management cluster backups set up", "user", u.ticket.Username, "target", req.Target)
	writeJSON(w, http.StatusOK, s.managementBackupsOf())
}

func (s *Server) stopManagementBackups(w http.ResponseWriter, r *http.Request, u user) {
	if !s.mayBackUpManagement(w, r, u) {
		return
	}
	if err := s.Appliance.StopEtcdBackups(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("management cluster backups switched off", "user", u.ticket.Username)
	w.WriteHeader(http.StatusNoContent)
}

// takeManagementSnapshot takes a snapshot now, in the background; the backups' status shows it
// being taken and how it went.
func (s *Server) takeManagementSnapshot(w http.ResponseWriter, r *http.Request, u user) {
	if !s.mayBackUpManagement(w, r, u) {
		return
	}
	err := s.Appliance.StartEtcdSnapshot(r.Context())
	if errors.Is(err, appliance.ErrNoEtcdBackups) || errors.Is(err, appliance.ErrEtcdSnapshotTaking) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.Log.Info("management cluster snapshot asked for", "user", u.ticket.Username)
	writeJSON(w, http.StatusAccepted, s.managementBackupsOf())
}

func (s *Server) snapshotManagement(ctx context.Context) {
	taken, err := s.Appliance.TakeEtcdSnapshot(ctx)
	if err != nil {
		s.Log.Error("management cluster snapshot", "error", err)
		return
	}
	s.Log.Info("management cluster snapshot taken", "snapshot", taken.Name)
}

func (s *Server) restoreManagementEtcd(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if !readJSON(w, r, &req) || !s.mayBackUpManagement(w, r, u) {
		return
	}
	done, ok := s.reservePassphrase(w, u)
	if !ok {
		return
	}
	name := r.PathValue("name")
	err := s.Appliance.RestoreEtcd(r.Context(), name, req.Passphrase, u.ticket)
	if jobConflict(err) {
		done(false)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if !checkPassphrase(w, done, err) {
		return
	}
	s.Log.Warn("management cluster etcd restore started", "user", u.ticket.Username, "snapshot", name)
	writeJSON(w, http.StatusAccepted, s.Appliance.Current())
}

// backUpManagementEtcd takes the management cluster's scheduled snapshots, looking every minute.
func (s *Server) backUpManagementEtcd(ctx context.Context) {
	a := s.Appliance
	if a == nil {
		return
	}
	t := time.NewTicker(etcdBackupLook)
	defer t.Stop()
	for {
		if a.EtcdBackupDue(time.Now()) {
			s.snapshotManagement(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
