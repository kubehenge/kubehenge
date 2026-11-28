package ui

import (
	"errors"
	"net/http"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// addApplianceBackup adds the Proxmox VE backup job for the appliance's container that the
// installer adds, with the signed-in user's rights, for an appliance no job includes.
func (s *Server) addApplianceBackup(w http.ResponseWriter, r *http.Request, u user) {
	if s.Appliance == nil {
		writeError(w, http.StatusNotFound, "the interface runs outside the appliance")
		return
	}
	vmid := s.applianceID(r.Context(), u)
	if vmid == 0 {
		writeError(w, http.StatusConflict, "the appliance's container is not among the guests you see in Proxmox VE")
		return
	}
	storage, err := s.Accounts.AddApplianceBackup(r.Context(), u.ticket, vmid)
	switch {
	case errors.Is(err, pve.ErrNoBackupStorage):
		writeError(w, http.StatusConflict, "no storage of the appliance's node takes backups. Add Backup content under Datacenter, Storage, Edit")
		return
	case err != nil:
		s.proxmoxError(w, r, err)
		return
	}
	s.backups.forget()
	s.Log.InfoContext(r.Context(), "added the appliance's backup job", "user", u.ticket.Username, "vmid", vmid, "storage", storage)
	w.WriteHeader(http.StatusNoContent)
}

// forget drops every answer, after a change to the backup jobs.
func (c *backupCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = nil
}
