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
	done, ok := s.limiter.reserve(signInCounts(username, r)...)
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too many failed sign-ins. Try again later")
		return
	}
	s.sessions.end(w, r)
	ticket, challenge, err := s.Accounts.SignIn(r.Context(), username, req.Password)
	done(errors.Is(err, pve.ErrAuthentication))
	if err != nil {
		if errors.Is(err, pve.ErrAuthentication) {
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
	done, ok := s.limiter.reserve(signInCounts(challenge.Username, r)...)
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too many failed sign-ins. Try again later")
		return
	}
	ticket, err := s.Accounts.Answer(r.Context(), challenge, req.Kind, strings.TrimSpace(req.Code))
	done(errors.Is(err, pve.ErrAuthentication))
	if err != nil {
		if errors.Is(err, pve.ErrAuthentication) {
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

// signInCounts are what a guess at a user's password or second factor counts against: the user at
// the client's address, and the address whichever user it names. Nothing counts per user alone,
// which would let anyone lock root@pam or any administrator out from anywhere; Proxmox VE's own
// page has no such count either.
func signInCounts(username string, r *http.Request) []count {
	address := clientAddress(r)
	return []count{
		{key: "user:" + strings.ToLower(username) + "\x00" + address, allowed: failuresAllowed},
		{key: "address:" + address, allowed: addressFailuresAllowed},
	}
}

// passphraseCount is what a guess at the appliance's state passphrase counts against: the
// signed-in user who makes it.
func passphraseCount(u user) count {
	return count{key: "passphrase:" + u.ticket.Username, allowed: failuresAllowed}
}

// maxLimiterKeys bounds the memory an attacker can make the limiter use with many keys.
const maxLimiterKeys = 10000

// count is one key a guess counts against, with the failures it allows within the window.
type count struct {
	key     string
	allowed int
}

// limiter refuses a key once it has used up its failures within window, so passwords and codes
// cannot be guessed through the interface faster than an attacker could on Proxmox VE's own page.
type limiter struct {
	window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time
	// pending are the guesses still being checked, by key, which count as failures until they are
	// known: parallel requests would otherwise all pass before the first of them failed.
	pending map[string]int
}

func newLimiter(window time.Duration) *limiter {
	return &limiter{window: window, now: time.Now, failures: map[string][]time.Time{}, pending: map[string]int{}}
}

// reserve takes a guess from every count and returns what to call once the guess is checked, with
// whether it was wrong; false when one of the counts has none left, and none is taken.
func (l *limiter) reserve(counts ...count) (func(failed bool), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, c := range counts {
		if len(l.recent(c.key, now))+l.pending[c.key] >= c.allowed {
			return nil, false
		}
	}
	for _, c := range counts {
		l.pending[c.key]++
	}
	var once sync.Once
	return func(failed bool) {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.settle(counts)
			if failed {
				l.fail(counts)
			}
		})
	}, true
}

// settle gives back the guesses counts took; l.mu is held.
func (l *limiter) settle(counts []count) {
	for _, c := range counts {
		l.pending[c.key]--
		if l.pending[c.key] <= 0 {
			delete(l.pending, c.key)
		}
	}
}

// fail records a wrong guess against counts; l.mu is held.
func (l *limiter) fail(counts []count) {
	now := l.now()
	for _, c := range counts {
		l.failures[c.key] = append(l.recent(c.key, now), now)
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
