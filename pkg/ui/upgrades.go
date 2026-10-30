package ui

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// Upgrades are the appliance's own: the interface finds newer signed releases and asks the
// updater, which runs as root, to install one. It never replaces software itself.
type Upgrades struct {
	Spool appliance.Spool
	// Index is the signed release index's URL.
	Index string
	HTTP  *http.Client
	// SettingsPath keeps the automatic upgrade policy.
	SettingsPath string
	Now          func() time.Time
	// Current is the installed release and Key the release key, as the updater has them.
	Current string
	Key     ed25519.PublicKey

	mu      sync.Mutex
	cached  *release.Index
	fetched time.Time
	lastErr string
	lastRun time.Time
	changed chan struct{}

	// lastManagement is when an automatic apply of the management cluster last started.
	lastManagement time.Time
}

// UpgradeSettings is the automatic upgrade policy: how far, and at which hour of the day in which
// time zone.
type UpgradeSettings struct {
	Policy release.Policy `json:"policy"`
	Hour   int            `json:"hour"`
	// TimeZone is an IANA time zone; the appliance's own clock has none anyone chose.
	TimeZone string `json:"timeZone"`
}

const (
	// defaultUpgradeHour is a night hour, when the fewest are at work.
	defaultUpgradeHour = 3
	// shownIndexAge is how old an index may be to show what is available; one to act on is always
	// fetched afresh.
	shownIndexAge = time.Minute
	// privateFile is the mode of the files only the interface's own user reads.
	privateFile os.FileMode = 0o600
	// oncePerDay is the least time between two automatic runs of a kind: a day, less what the hour
	// they start in allows.
	oncePerDay = 20 * time.Hour
)

func (u *Upgrades) settings() UpgradeSettings {
	s := UpgradeSettings{Policy: release.PolicyOff, Hour: defaultUpgradeHour, TimeZone: "UTC"}
	if raw, err := os.ReadFile(u.SettingsPath); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// location is the settings' time zone, UTC when it no longer loads.
func (s UpgradeSettings) location() *time.Location {
	if loc, err := time.LoadLocation(s.TimeZone); err == nil && s.TimeZone != "" {
		return loc
	}
	return time.UTC
}

// index returns the verified release index, fetched again when the one at hand is maxAge old.
func (u *Upgrades) index(ctx context.Context, maxAge time.Duration) (*release.Index, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cached != nil && u.Now().Sub(u.fetched) < maxAge {
		return u.cached, ""
	}
	ix, err := appliance.FetchIndex(ctx, u.HTTP, u.Index, u.Key)
	if err != nil {
		u.lastErr = err.Error()
		return u.cached, u.lastErr
	}
	u.cached, u.fetched, u.lastErr = ix, u.Now(), ""
	return ix, ""
}

type upgradesView struct {
	Version   string                  `json:"version"`
	Available []string                `json:"available"`
	Error     string                  `json:"error,omitempty"`
	Settings  UpgradeSettings         `json:"settings"`
	Status    *appliance.UpdateStatus `json:"status,omitempty"`
	// Management is whether the management cluster follows the appliance's upgrades on its own.
	Management *managementAutomatic `json:"management,omitempty"`
}

// aboutRelease keeps the updater's last result only while it speaks of the installed release: an
// upgrade from the shell (install.sh --upgrade) leaves the updater's record of an older one behind.
func aboutRelease(st *appliance.UpdateStatus, current string) *appliance.UpdateStatus {
	if st == nil {
		return nil
	}
	if st.State == "succeeded" && st.To == current || st.State != "succeeded" && st.From == current {
		return st
	}
	return nil
}

func (s *Server) upgradesStatus(w http.ResponseWriter, r *http.Request, u user) {
	up := s.Upgrades
	if up == nil {
		writeError(w, http.StatusNotFound, "upgrades need the appliance")
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	v := upgradesView{
		Version: up.Current, Available: []string{}, Settings: up.settings(), Status: aboutRelease(up.Spool.Status(), up.Current),
		Management: s.managementAutomatic(),
	}
	ix, errText := up.index(r.Context(), shownIndexAge)
	v.Error = errText
	if cur, err := release.ParseVersion(up.Current); ix != nil && err == nil {
		var newer []release.SemVer
		for _, rel := range ix.Releases {
			if rv, parseErr := release.ParseVersion(rel.Version); parseErr == nil && cur.Less(rv) {
				newer = append(newer, rv)
			}
		}
		slices.SortFunc(newer, func(a, b release.SemVer) int {
			switch {
			case a.Less(b):
				return -1
			case b.Less(a):
				return 1
			default:
				return 0
			}
		})
		for _, rv := range newer {
			v.Available = append(v.Available, rv.String())
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) upgrade(w http.ResponseWriter, r *http.Request, u user) {
	up := s.Upgrades
	if up == nil {
		writeError(w, http.StatusNotFound, "upgrades need the appliance")
		return
	}
	var req struct {
		Version string `json:"version"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	if s.applying() {
		writeError(w, http.StatusConflict, "the management cluster is being applied. Upgrade once it finishes")
		return
	}
	ix, errText := up.index(r.Context(), 0)
	if ix == nil {
		writeError(w, http.StatusBadGateway, "the release index: "+errText)
		return
	}
	if _, err := ix.Find(up.Current, req.Version); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := up.Spool.Request(appliance.UpdateRequest{Version: req.Version}); err != nil {
		s.Log.ErrorContext(r.Context(), "upgrade request", "error", err)
		writeError(w, http.StatusInternalServerError, "the upgrade could not start")
		return
	}
	s.Log.InfoContext(r.Context(), "appliance upgrade requested", "user", u.ticket.Username, "from", up.Current, "to", req.Version)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) upgradeSettings(w http.ResponseWriter, r *http.Request, u user) {
	up := s.Upgrades
	if up == nil {
		writeError(w, http.StatusNotFound, "upgrades need the appliance")
		return
	}
	var req UpgradeSettings
	if !readJSON(w, r, &req) {
		return
	}
	switch req.Policy {
	case release.PolicyOff, release.PolicyPatch, release.PolicyMinor:
	default:
		writeError(w, http.StatusUnprocessableEntity, "policy: off, patch or minor")
		return
	}
	if req.Hour < 0 || req.Hour > 23 {
		writeError(w, http.StatusUnprocessableEntity, "hour: 0 to 23")
		return
	}
	if req.TimeZone == "" {
		req.TimeZone = "UTC"
	}
	// Local names the appliance's clock, which is what the setting exists to avoid.
	if _, err := time.LoadLocation(req.TimeZone); err != nil || req.TimeZone == "Local" {
		writeError(w, http.StatusUnprocessableEntity, "timeZone: an IANA time zone, such as Europe/Berlin")
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	raw, err := json.Marshal(req)
	if err == nil {
		err = os.WriteFile(up.SettingsPath, raw, privateFile)
	}
	if err != nil {
		s.Log.ErrorContext(r.Context(), "upgrade settings", "error", err)
		writeError(w, http.StatusInternalServerError, "could not save the settings")
		return
	}
	s.Log.InfoContext(r.Context(), "appliance upgrade policy set", "user", u.ticket.Username, "policy", req.Policy, "hour", req.Hour, "timeZone", req.TimeZone)
	up.wake()
	writeJSON(w, http.StatusOK, req)
}

// wake tells autoUpgrade that what it waits for changed.
func (u *Upgrades) wake() {
	select {
	case u.changes() <- struct{}{}:
	default:
	}
}

// applying tells whether the management cluster is being applied or a template built, which a
// restart would cut off.
func (s *Server) applying() bool {
	return s.Appliance != nil && s.Appliance.Busy()
}

// autoUpgrade asks the updater, once a day at the chosen hour, for the newest release the policy
// allows; the updater decides whether there is one. In the same hour, once the appliance runs a
// newer release than the one that applied the management cluster, it applies that too where an
// administrator switched that on. It sleeps until that hour, then looks once a minute within it, as
// an apply going on holds the upgrade back, and an upgrade of the appliance the apply; with both
// off it sleeps until the settings change.
func (s *Server) autoUpgrade(ctx context.Context) {
	up := s.Upgrades
	if up == nil {
		return
	}
	for {
		wait, on := up.untilHour(up.Now(), s.unattendedOn())
		if on && wait == 0 {
			// The management cluster waits for an upgrade asked for now, which brings what it moves to.
			if !s.requestDueUpgrade() {
				s.applyDueManagement() //nolint:contextcheck // the run outlives the loop's wait on purpose, as one a request starts does
			}
			wait = time.Minute
		}
		var fire <-chan time.Time
		var t *time.Timer
		if on {
			t = time.NewTimer(wait)
			fire = t.C
		}
		select {
		case <-ctx.Done():
		case <-up.changes():
		case <-fire:
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// requestDueUpgrade asks the updater for an automatic upgrade when one is due, and tells whether it
// did.
func (s *Server) requestDueUpgrade() bool {
	req, ok := s.Upgrades.due(s.applying())
	if !ok {
		return false
	}
	if err := s.Upgrades.Spool.Request(req); err != nil {
		s.Log.Error("automatic upgrade request", "error", err)
		return false
	}
	s.Log.Info("automatic upgrade requested", "policy", req.Policy)
	return true
}

// unattendedOn tells whether the management cluster follows the appliance's upgrades on its own.
func (s *Server) unattendedOn() bool {
	return s.Appliance != nil && s.Appliance.UnattendedOn() != nil
}

// untilHour is how long until the chosen hour starts, 0 within it; not on while the policy is off,
// unless the management cluster's automatic upgrades are on (management).
func (u *Upgrades) untilHour(now time.Time, management bool) (time.Duration, bool) { //nolint:revive // flag-parameter: management is a state the decision reads, not a mode
	st := u.settings()
	if st.Policy == release.PolicyOff && !management {
		return 0, false
	}
	loc := st.location()
	t := now.In(loc)
	start := time.Date(t.Year(), t.Month(), t.Day(), st.Hour, 0, 0, 0, loc)
	switch {
	case t.Before(start):
		return start.Sub(t), true
	case t.Before(start.Add(time.Hour)):
		return 0, true
	}
	return time.Date(t.Year(), t.Month(), t.Day()+1, st.Hour, 0, 0, 0, loc).Sub(t), true
}

// changes tells autoUpgrade that the settings changed.
func (u *Upgrades) changes() chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.changed == nil {
		u.changed = make(chan struct{}, 1)
	}
	return u.changed
}

// due returns the automatic request to make now, if any: in the chosen hour, once a day, never
// during an apply.
func (u *Upgrades) due(applying bool) (appliance.UpdateRequest, bool) { //nolint:revive // flag-parameter: applying is the state the decision reads, not a mode
	st := u.settings()
	now := u.Now().In(st.location())
	u.mu.Lock()
	defer u.mu.Unlock()
	// Kept on disk, since an upgrade restarts the interface within the same hour.
	if u.lastRun.IsZero() {
		if raw, err := os.ReadFile(u.SettingsPath + ".last"); err == nil {
			_ = u.lastRun.UnmarshalText(raw)
		}
	}
	if st.Policy == release.PolicyOff || applying || now.Hour() != st.Hour || now.Sub(u.lastRun) < oncePerDay {
		return appliance.UpdateRequest{}, false
	}
	u.lastRun = now
	if raw, err := now.MarshalText(); err == nil {
		_ = os.WriteFile(u.SettingsPath+".last", raw, privateFile)
	}
	return appliance.UpdateRequest{Policy: st.Policy}, true
}

// managementPath records when an automatic apply of the management cluster last started, kept on
// disk so that a restart within the hour does not run it again.
func (u *Upgrades) managementPath() string { return u.SettingsPath + ".management" }

// managementDue tells whether an automatic apply of the management cluster may start now: in the
// chosen hour, once a day, never beside another job.
func (u *Upgrades) managementDue(applying bool) bool {
	st := u.settings()
	now := u.Now().In(st.location())
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.lastManagement.IsZero() {
		if raw, err := os.ReadFile(u.managementPath()); err == nil {
			_ = u.lastManagement.UnmarshalText(raw)
		}
	}
	return !applying && now.Hour() == st.Hour && now.Sub(u.lastManagement) >= oncePerDay
}

// managementStarted records that an automatic apply of the management cluster started, or failed
// to, so that the next is tried the next day.
func (u *Upgrades) managementStarted() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastManagement = u.Now()
	if raw, err := u.lastManagement.MarshalText(); err == nil {
		_ = os.WriteFile(u.managementPath(), raw, privateFile)
	}
}
