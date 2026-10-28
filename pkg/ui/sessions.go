package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// sessionCookie carries the session ID. The __Host- prefix makes browsers refuse it unless it is
// Secure, host-only and for the whole site, so no other host or plain-HTTP page can plant one.
const sessionCookie = "__Host-kubehenge-session"

const (
	// A second factor has to follow the password within this time.
	challengeLifetime = 5 * time.Minute
	// Renewing a ticket this old leaves room for requests that run long before it expires.
	renewAfter = pve.TicketLifetime / 2
)

type session struct {
	id string
	// csrf has to come back in a header with every request that changes something. A page on
	// another origin can make the browser send the cookie but cannot read or set this.
	csrf    string
	created time.Time

	// mu is held only for moments, never across a call to Proxmox VE: lookup and prune wait on it.
	mu        sync.Mutex
	ticket    *pve.Ticket
	challenge *pve.Challenge
	// renewing is set while one request renews the ticket; the session's other requests go on with
	// the ticket they have, which still holds for as long again.
	renewing bool
}

// sessions holds every session in memory only: tickets never touch the disk, and a restart signs
// everyone out.
type sessions struct {
	accounts Accounts
	// maxAge ends a session however often its ticket was renewed: Proxmox VE renews tickets without
	// asking the realm or the second factor again.
	maxAge time.Duration
	now    func() time.Time

	mu   sync.Mutex
	byID map[string]*session
}

func newSessions(accounts Accounts, maxAge time.Duration) *sessions {
	return &sessions{accounts: accounts, maxAge: maxAge, now: time.Now, byID: map[string]*session{}}
}

// tokenBytes makes session IDs and CSRF tokens 256 bits long.
const tokenBytes = 32

func randomToken() string {
	b := make([]byte, tokenBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

// start opens a session with a ticket or a pending challenge, and sets its cookie. Every sign-in
// step gets a new ID, so an ID seen before sign-in is worthless after it.
func (s *sessions) start(w http.ResponseWriter, ticket *pve.Ticket, challenge *pve.Challenge) *session {
	sess := &session{id: randomToken(), csrf: randomToken(), created: s.now(), ticket: ticket, challenge: challenge}
	s.mu.Lock()
	s.byID[sess.id] = sess
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sess.id,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	return sess
}

// end forgets the request's session, if any, and clears its cookie.
func (s *sessions) end(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.byID, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

// lookup returns the request's session, or nil when it has none or it has ended.
func (s *sessions) lookup(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	sess := s.byID[c.Value]
	s.mu.Unlock()
	if sess == nil {
		return nil
	}
	if s.ended(sess, s.now()) {
		s.forget(sess)
		return nil
	}
	return sess
}

// prune forgets ended sessions; lookup does it for the ones still asked for. Every user's requests
// look their session up under s.mu, so no session's lock is waited for while holding it.
func (s *sessions) prune() {
	now := s.now()
	s.mu.Lock()
	all := slices.Collect(maps.Values(s.byID))
	s.mu.Unlock()
	for _, sess := range all {
		if s.ended(sess, now) {
			s.forget(sess)
		}
	}
}

func (s *sessions) forget(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID[sess.id] == sess {
		delete(s.byID, sess.id)
	}
}

func (s *sessions) ended(sess *session, now time.Time) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	switch {
	case now.Sub(sess.created) >= s.maxAge:
		return true
	case sess.challenge != nil:
		return now.Sub(sess.created) >= challengeLifetime
	case sess.ticket != nil:
		return now.Sub(sess.ticket.Issued) >= pve.TicketLifetime
	default:
		return true
	}
}

// pruneEvery prunes until ctx ends.
func (s *sessions) pruneEvery(ctx context.Context, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.prune()
		}
	}
}

// signedIn returns the session's ticket, renewed when it is old. nil means the session holds only
// a challenge, or Proxmox VE refused to renew (the user was disabled or expired). A renewal that
// fails for any other reason keeps the ticket, which is still valid, and is tried again next time.
// One request renews at a time, without the session's lock: a slow Proxmox VE holds up that request
// alone.
func (s *sessions) signedIn(ctx context.Context, sess *session) *pve.Ticket {
	sess.mu.Lock()
	ticket := sess.ticket
	if ticket == nil || sess.renewing || s.now().Sub(ticket.Issued) < renewAfter {
		sess.mu.Unlock()
		return ticket
	}
	sess.renewing = true
	sess.mu.Unlock()
	renewed, err := s.accounts.Renew(ctx, ticket)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.renewing = false
	switch {
	case errors.Is(err, pve.ErrAuthentication):
		sess.ticket = nil
		return nil
	case err != nil:
		return ticket
	}
	sess.ticket = renewed
	return renewed
}

// csrfHeader carries a session's CSRF token, in the canonical form Go reads header names in.
const csrfHeader = "X-Kubehenge-Csrf"

func (sess *session) checkCSRF(r *http.Request) bool {
	got := r.Header.Get(csrfHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) == 1
}
