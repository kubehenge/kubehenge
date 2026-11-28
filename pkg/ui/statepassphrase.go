package ui

import "net/http"

// statePassphrase switches the passphrase that opens the management cluster's state on or off.
// The appliance keeps the state's key by default. On, the key is kept only sealed with the new
// passphrase, and every action that opens the state asks for it. passphrase is the one required so
// far, or the one of a state made before the appliance kept its key.
func (s *Server) statePassphrase(w http.ResponseWriter, r *http.Request, u user) {
	a := s.Appliance
	if a == nil {
		writeError(w, http.StatusNotFound, "this interface does not manage its management cluster")
		return
	}
	var req struct {
		Required      bool   `json:"required"`
		Passphrase    string `json:"passphrase"`
		NewPassphrase string `json:"newPassphrase"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	done, ok := s.reservePassphrase(w, u)
	if !ok {
		return
	}
	var err error
	if req.Required {
		err = a.RequirePassphrase(r.Context(), req.Passphrase, req.NewPassphrase)
	} else {
		err = a.KeepStateKey(r.Context(), req.Passphrase)
	}
	if !checkPassphrase(w, done, err) {
		return
	}
	s.Log.WarnContext(r.Context(), "changed whether the management cluster's state needs a passphrase", "user", u.ticket.Username, "required", req.Required)
	writeJSON(w, http.StatusOK, map[string]bool{"required": a.PassphraseRequired()})
}
