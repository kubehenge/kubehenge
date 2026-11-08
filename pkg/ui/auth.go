package ui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func (s *Server) realms(w http.ResponseWriter, r *http.Request) {
	realms, err := s.Accounts.Realms(r.Context())
	if err != nil {
		s.anonymousProxmoxError(w, r, err)
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
	// the server will refuse. Unset when not known.
	CanCreate *bool `json:"canCreate,omitempty"`
	// CanTrust tells whether the user holds Sys.Modify on the workload pool, which installing
	// Rancher and trusting identity providers of one's own need. Unset when not known.
	CanTrust *bool `json:"canTrust,omitempty"`
	// Namespace is where the interface creates clusters unless the creator chooses another of
	// Namespaces, so it can offer the ones a new cluster can be linked with.
	Namespace string `json:"namespace,omitempty"`
	// Namespaces are every namespace the interface creates and changes clusters in, Namespace
	// first. Unset when not known.
	Namespaces []string `json:"namespaces,omitempty"`
}

// signedInView describes a signed-in session. Where the interface manages clusters is an addition
// to it: a management cluster that does not answer leaves it unknown rather than the user signed
// out.
func (s *Server) signedInView(ctx context.Context, username string, sess *session) sessionView {
	v := sessionView{User: username, Namespace: s.Namespace}
	// An API token's requests have no session, nor a CSRF token to send.
	if sess != nil {
		v.CSRF = sess.csrf
	}
	if namespaces, err := s.namespaces(ctx, s.currentKube()); err == nil {
		v.Namespace, v.Namespaces = namespaces[0], namespaces
	} else {
		s.Log.ErrorContext(ctx, "reading the namespaces the interface manages", "error", err)
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

// withRealm adds the chosen realm to a user name. A name with an @ of its own, such as an Active
// Directory principal name, gets it too, unless what follows its last @ is a realm already.
func (s *Server) withRealm(ctx context.Context, username, realm string) string {
	at := strings.LastIndex(username, "@")
	switch {
	case realm == "" || username == "":
		return username
	case at < 0:
		return username + "@" + realm
	}
	realms, err := s.Accounts.Realms(ctx)
	if err != nil || slices.ContainsFunc(realms, func(r pve.Realm) bool { return r.Realm == username[at+1:] }) {
		return username
	}
	return username + "@" + realm
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
	username := s.withRealm(r.Context(), req.Username, req.Realm)
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "enter a user name and password")
		return
	}
	if utf8.RuneCountInString(username) > maxUsernameLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("The user name and realm together are at most %d characters.", maxUsernameLength))
		return
	}
	done, ok := s.limiter.reserve(signInCounts(username, r)...)
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too many failed sign-ins. Try again later")
		return
	}
	s.sessions.end(w, r)
	ticket, challenge, err := s.Accounts.SignIn(r.Context(), username, req.Password)
	refused := errors.Is(err, pve.ErrAuthentication) || refusedAsClient(err)
	done(refused)
	if err != nil {
		if refused {
			s.Log.InfoContext(r.Context(), "sign-in refused", "user", username, "address", clientAddress(r), "error", err)
			writeError(w, http.StatusUnauthorized, signInRefused)
			return
		}
		s.anonymousProxmoxError(w, r, err)
		return
	}
	if ticket != nil {
		sess := s.sessions.start(w, ticket, nil)
		s.Log.InfoContext(r.Context(), "signed in", "user", ticket.Username, "address", clientAddress(r))
		writeJSON(w, http.StatusOK, s.signedInView(r.Context(), ticket.Username, sess))
		return
	}
	if !challenge.TOTP && !challenge.Yubico && len(challenge.Recovery) == 0 {
		writeError(w, http.StatusUnauthorized, "security keys work only in Proxmox VE. Sign in with an API token instead")
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

// signInWithToken opens a browser session for a Proxmox VE API token, the way in for a user whose
// only second factor is a security key, which answers on Proxmox VE's own page alone. The token
// counts against the same limits as a wrong password, and the session has the same lifetime and
// CSRF token. Its privileges are the token's own, which Proxmox VE already cuts down to its user's
// where the token has privilege separation.
func (s *Server) signInWithToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	id, secret := strings.TrimSpace(req.ID), strings.TrimSpace(req.Secret)
	if id == "" || secret == "" {
		writeError(w, http.StatusBadRequest, "enter the token ID and secret")
		return
	}
	ticket := s.tokenUser(w, r, id+"="+secret)
	if ticket == nil {
		return
	}
	s.sessions.end(w, r)
	sess := s.sessions.start(w, ticket, nil)
	s.Log.InfoContext(r.Context(), "signed in", "user", ticket.Username, "address", clientAddress(r), "via", "token")
	writeJSON(w, http.StatusOK, s.signedInView(r.Context(), ticket.Username, sess))
}

// maxUsernameLength is the longest sign-in name Proxmox VE takes, realm included. It answers a
// longer one with a 400 that has nothing to do with the password.
const maxUsernameLength = 64

// signInRefused is the one answer to every refused password. Proxmox VE answers a wrong password,
// an unknown or disabled user and a missing second factor all the same, so this text does too.
const signInRefused = "Sign-in failed. Check the user name, password and realm, or set up the required second factor."

// secondFactorRefused says what a refused second factor can mean. Proxmox VE answers a wrong code,
// a factor locked after too many tries and a cluster without quorum (which cannot write the
// factor's state, a used recovery key or a TOTP code's counter) with the same 401. The user has given the right password, so naming them tells
// no stranger anything.
func secondFactorRefused(kind string, c *pve.Challenge) string {
	switch {
	case kind == "recovery":
		return "Wrong recovery key, or the cluster has no quorum. Without quorum, use a node's console."
	case len(c.Recovery) > 0:
		return "Wrong code, a locked factor or a cluster without quorum. A recovery key unlocks a locked factor. Without quorum, use a node's console."
	default:
		return "Wrong code, a locked factor or a cluster without quorum. An administrator resets a locked factor. Without quorum, use a node's console."
	}
}

// refusedAsClient tells whether Proxmox VE refused the request itself as malformed or not allowed.
// On a sign-in that must read as a wrong password, since a stranger learns from nothing else.
func refusedAsClient(err error) bool {
	apiErr, ok := errors.AsType[*pve.APIError](err)
	return ok && strings.HasPrefix(apiErr.Status, "4")
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
	refused := errors.Is(err, pve.ErrAuthentication) || refusedAsClient(err)
	done(refused)
	if err != nil {
		if refused {
			s.Log.InfoContext(r.Context(), "second factor refused", "user", challenge.Username, "address", clientAddress(r), "error", err)
			writeError(w, http.StatusUnauthorized, secondFactorRefused(req.Kind, challenge))
			return
		}
		s.anonymousProxmoxError(w, r, err)
		return
	}
	s.sessions.end(w, r)
	next := s.sessions.start(w, ticket, nil)
	s.Log.InfoContext(r.Context(), "signed in", "user", ticket.Username, "address", clientAddress(r), "secondFactor", req.Kind)
	writeJSON(w, http.StatusOK, s.signedInView(r.Context(), ticket.Username, next))
}

func (s *Server) whoAmI(w http.ResponseWriter, r *http.Request, u user) {
	v := s.signedInView(r.Context(), u.ticket.Username, u.session)
	if held, err := s.Accounts.Privileges(r.Context(), u.ticket, s.workloadPoolPath()); err == nil {
		create, trust := held.Has(privChange), held.Has(privKubeconfig)
		v.CanCreate, v.CanTrust = &create, &trust
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request, u user) {
	s.sessions.end(w, r)
	s.Log.InfoContext(r.Context(), "signed out", "user", u.ticket.Username)
	w.WriteHeader(http.StatusNoContent)
}

// clientAddress is the connection's peer, or behind a trusted proxy (Server.TrustedProxies) the
// client it forwards for. Forwarding headers from any other peer are not believed.
func clientAddress(r *http.Request) string {
	if client, ok := r.Context().Value(clientAddressKey{}).(string); ok {
		return client
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// signInCounts are what a guess at a user's password or second factor counts against: the user at
// the client's address, and the address whichever user it names. Nothing counts per user alone,
// which would let anyone lock root@pam or any administrator out from anywhere. Proxmox VE's own
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
// whether it was wrong. False when one of the counts has none left, and none is taken.
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

// settle gives back the guesses counts took. l.mu is held.
func (l *limiter) settle(counts []count) {
	for _, c := range counts {
		l.pending[c.key]--
		if l.pending[c.key] <= 0 {
			delete(l.pending, c.key)
		}
	}
}

// fail records a wrong guess against counts. l.mu is held.
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
