package ui

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func (s *Server) realms(w http.ResponseWriter, r *http.Request) {
	realms, err := s.Accounts.Realms(r.Context())
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, realms)
}

type sessionView struct {
	User string `json:"user"`
	CSRF string `json:"csrf"`
	// SecondFactor is set while the user still has to give one.
	SecondFactor *secondFactorView `json:"secondFactor,omitempty"`
	// Headlamp is the port of the shared Headlamp server on the interface's host name, if one runs.
	Headlamp int `json:"headlamp,omitempty"`
	// CanCreate tells whether the user may create clusters, so the interface need not offer what
	// the server will refuse; unset when not known.
	CanCreate *bool `json:"canCreate,omitempty"`
	// CanTrust tells whether the user holds Sys.Modify on the workload pool, which installing
	// Rancher and trusting identity providers of one's own need; unset when not known.
	CanTrust *bool `json:"canTrust,omitempty"`
	// CanModifyPermissions tells whether the user holds Permissions.Modify on /, which giving a
	// cluster's CSI plugin root@pam for volume snapshots needs; unset when not known.
	CanModifyPermissions *bool `json:"canModifyPermissions,omitempty"`
	// Namespace is where the interface creates clusters unless the creator chooses another of
	// Namespaces, so it can offer the ones a new cluster can be linked with.
	Namespace string `json:"namespace,omitempty"`
	// Namespaces are every namespace the interface creates and changes clusters in, Namespace
	// first; unset when not known.
	Namespaces []string `json:"namespaces,omitempty"`
}

// signedInView describes a signed-in session. Where the interface manages clusters is an addition
// to it: a management cluster that does not answer leaves it unknown rather than the user signed
// out.
func (s *Server) signedInView(ctx context.Context, username string, sess *session) sessionView {
	v := sessionView{User: username, CSRF: sess.csrf, Namespace: s.Namespace}
	if namespaces, err := s.namespaces(ctx, s.currentKube()); err == nil {
		v.Namespace, v.Namespaces = namespaces[0], namespaces
	} else {
		s.Log.Error("reading the namespaces the interface manages", "error", err)
	}
	if s.Headlamp != nil {
		v.Headlamp = s.Headlamp.Port
	}
	return v
}

type secondFactorView struct {
	TOTP     bool  `json:"totp"`
	Yubico   bool  `json:"yubico"`
	Recovery []int `json:"recovery,omitempty"`
}

func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Realm    string `json:"realm"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	username := req.Username
	if req.Realm != "" && !strings.Contains(username, "@") {
		username += "@" + req.Realm
	}
	if username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "enter a user name and password")
		return
	}
	keys := []string{"user:" + strings.ToLower(username), "address:" + clientAddress(r)}
	if !s.limiter.allow(keys...) {
		writeError(w, http.StatusTooManyRequests, "too many failed sign-ins. Try again later")
		return
	}
	s.sessions.end(w, r)
	ticket, challenge, err := s.Accounts.SignIn(r.Context(), username, req.Password)
	if err != nil {
		if errors.Is(err, pve.ErrAuthentication) {
			s.limiter.fail(keys...)
			s.Log.Info("sign-in refused", "user", username, "address", clientAddress(r))
			writeError(w, http.StatusUnauthorized, "sign-in failed: check the user name, password and realm")
			return
		}
		s.proxmoxError(w, err)
		return
	}
	if ticket != nil {
		sess := s.sessions.start(w, ticket, nil)
		s.Log.Info("signed in", "user", ticket.Username, "address", clientAddress(r))
		writeJSON(w, http.StatusOK, s.signedInView(r.Context(), ticket.Username, sess))
		return
	}
	if !challenge.TOTP && !challenge.Yubico && len(challenge.Recovery) == 0 {
		writeError(w, http.StatusUnauthorized, "security keys work only in Proxmox VE. Use a TOTP code, recovery key or OpenID realm")
		return
	}
	sess := s.sessions.start(w, nil, challenge)
	writeJSON(w, http.StatusOK, sessionView{
		User: challenge.Username,
		CSRF: sess.csrf,
		SecondFactor: &secondFactorView{
			TOTP: challenge.TOTP, Yubico: challenge.Yubico, Recovery: challenge.Recovery,
		},
	})
}

func (s *Server) secondFactor(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.lookup(r)
	if sess == nil || !sess.checkCSRF(r) {
		writeError(w, http.StatusUnauthorized, "sign in with your password first")
		return
	}
	sess.mu.Lock()
	challenge := sess.challenge
	sess.mu.Unlock()
	if challenge == nil {
		writeError(w, http.StatusConflict, "already signed in")
		return
	}
	var req struct {
		Kind string `json:"kind"`
		Code string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	switch req.Kind {
	case "totp", "yubico", "recovery":
	default:
		writeError(w, http.StatusBadRequest, "kind is totp, yubico or recovery")
		return
	}
	keys := []string{"user:" + strings.ToLower(challenge.Username), "address:" + clientAddress(r)}
	if !s.limiter.allow(keys...) {
		writeError(w, http.StatusTooManyRequests, "too many failed sign-ins. Try again later")
		return
	}
	ticket, err := s.Accounts.Answer(r.Context(), challenge, req.Kind, strings.TrimSpace(req.Code))
	if err != nil {
		if errors.Is(err, pve.ErrAuthentication) {
			s.limiter.fail(keys...)
			s.Log.Info("second factor refused", "user", challenge.Username, "address", clientAddress(r))
			writeError(w, http.StatusUnauthorized, "wrong code. Try again")
			return
		}
		s.proxmoxError(w, err)
		return
	}
	s.sessions.end(w, r)
	next := s.sessions.start(w, ticket, nil)
	s.Log.Info("signed in", "user", ticket.Username, "address", clientAddress(r), "secondFactor", req.Kind)
	writeJSON(w, http.StatusOK, s.signedInView(r.Context(), ticket.Username, next))
}

func (s *Server) whoAmI(w http.ResponseWriter, r *http.Request, u user) {
	v := s.signedInView(r.Context(), u.ticket.Username, u.session)
	if held, err := s.Accounts.Privileges(r.Context(), u.ticket, s.workloadPoolPath()); err == nil {
		create, trust := held.Has(privChange), held.Has(privKubeconfig)
		v.CanCreate, v.CanTrust = &create, &trust
	}
	if held, err := s.Accounts.Privileges(r.Context(), u.ticket, "/"); err == nil {
		permissions := held.Has(privPermissionsModify)
		v.CanModifyPermissions = &permissions
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request, u user) {
	s.sessions.end(w, r)
	s.Log.Info("signed out", "user", u.ticket.Username)
	w.WriteHeader(http.StatusNoContent)
}

// clientAddress is the connection's peer. Forwarding headers are not trusted: the interface is
// reached directly.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// maxLimiterKeys bounds the memory an attacker can make the limiter use with many keys.
const maxLimiterKeys = 10000

// limiter refuses a key after limit failures within window, so passwords and codes cannot be
// guessed through the interface faster than an attacker could on Proxmox VE's own page.
type limiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, now: time.Now, failures: map[string][]time.Time{}}
}

func (l *limiter) allow(keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, k := range keys {
		if len(l.recent(k, now)) >= l.limit {
			return false
		}
	}
	return true
}

func (l *limiter) fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, k := range keys {
		l.failures[k] = append(l.recent(k, now), now)
	}
	if len(l.failures) > maxLimiterKeys {
		for k := range l.failures {
			if len(l.recent(k, now)) == 0 {
				delete(l.failures, k)
			}
		}
	}
}

func (l *limiter) recent(key string, now time.Time) []time.Time {
	times := l.failures[key]
	i := 0
	for i < len(times) && now.Sub(times[i]) >= l.window {
		i++
	}
	if i == len(times) {
		delete(l.failures, key)
		return nil
	}
	l.failures[key] = times[i:]
	return times[i:]
}
