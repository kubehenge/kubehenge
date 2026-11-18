package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
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
	// A CLI session's ticket is renewed unused once it is this old, as often as Proxmox VE's own
	// interface does. The session then outlasts a break of an hour and a half, in which the
	// interface is stopped or cannot reach Proxmox VE.
	renewUnusedAfter = 15 * time.Minute
	// renewalsAtOnce and renewalTimeout bound a round of unused renewals, so that a Proxmox VE node
	// which hangs holds up few of them and none for long.
	renewalsAtOnce = 8
	renewalTimeout = 10 * time.Second
)

type session struct {
	id string
	// csrf has to come back in a header with every request that changes something. A page on
	// another origin can make the browser send the cookie but cannot read or set this.
	csrf    string
	created time.Time
	// cli marks a password sign-in by the kubehenge CLI. A script cannot type the password again
	// every few hours, so it lasts longer and is kept across restarts.
	cli bool

	// mu is held only for moments, never across a call to Proxmox VE: lookup and tend wait on it.
	mu        sync.Mutex
	ticket    *pve.Ticket
	challenge *pve.Challenge
	// renewing is set while one request, or tend, renews the ticket. The session's requests go on
	// with the ticket they have, which still holds for as long again.
	renewing bool
}

// sessions holds every session in memory. The CLI's alone are also kept sealed in store, so that a
// restart signs browsers out but not scripts.
type sessions struct {
	accounts Accounts
	// maxAge ends a session however often its ticket was renewed: Proxmox VE renews tickets without
	// asking the realm or the second factor again. cliMaxAge does so for the CLI's.
	maxAge    time.Duration
	cliMaxAge time.Duration
	now       func() time.Time
	// store keeps the CLI's sessions across restarts. Nil keeps them in memory only.
	store sessionStore
	log   *slog.Logger

	// saving lets one save at a time write, each with what holds when it starts, so the last one
	// written is the newest.
	saving sync.Mutex
	mu     sync.Mutex
	byID   map[string]*session
}

// sessionStore keeps the CLI's sessions encrypted, so that a ticket never touches the disk in the
// clear.
type sessionStore interface {
	KeepSessions(plain []byte) error
	KeptSessions() ([]byte, error)
}

// keptSession is what is kept of a CLI session and its Proxmox VE ticket. A password sign-in's
// ticket has no API token.
type keptSession struct {
	ID         string    `json:"id"`
	CSRF       string    `json:"csrf"`
	Created    time.Time `json:"created"`
	User       string    `json:"user"`
	Ticket     string    `json:"ticket"`
	TicketCSRF string    `json:"ticketCsrf"`
	Issued     time.Time `json:"issued"`
}

// newSessions reads back the sessions store keeps. A zero cliMaxAge gives the CLI's the browser's.
func newSessions(accounts Accounts, maxAge, cliMaxAge time.Duration, store sessionStore, log *slog.Logger) *sessions {
	if cliMaxAge == 0 {
		cliMaxAge = maxAge
	}
	s := &sessions{accounts: accounts, maxAge: maxAge, cliMaxAge: cliMaxAge, now: time.Now, store: store, log: log, byID: map[string]*session{}}
	s.load()
	return s
}

// load reads back the kept sessions. What does not open or parse is discarded, and the interface
// starts without them.
func (s *sessions) load() {
	if s.store == nil {
		return
	}
	raw, err := s.store.KeptSessions()
	var kept []keptSession
	if err == nil && raw != nil {
		err = json.Unmarshal(raw, &kept)
	}
	if err != nil {
		s.log.Warn("the kept sessions are discarded", "error", err)
		s.save()
		return
	}
	for i := range kept {
		k := &kept[i]
		ticket := &pve.Ticket{Username: k.User, Ticket: k.Ticket, CSRF: k.TicketCSRF, Issued: k.Issued}
		s.byID[k.ID] = &session{id: k.ID, csrf: k.CSRF, created: k.Created, cli: true, ticket: ticket}
	}
}

// save replaces what store keeps with the CLI's signed-in sessions. Call it without holding s.mu
// or a session's lock.
func (s *sessions) save() {
	if s.store == nil {
		return
	}
	s.saving.Lock()
	defer s.saving.Unlock()
	s.mu.Lock()
	all := slices.Collect(maps.Values(s.byID))
	s.mu.Unlock()
	kept := []keptSession{}
	for _, sess := range all {
		if !sess.cli {
			continue
		}
		sess.mu.Lock()
		if sess.ticket != nil {
			t := sess.ticket
			kept = append(kept, keptSession{
				ID: sess.id, CSRF: sess.csrf, Created: sess.created, User: t.Username, Ticket: t.Ticket, TicketCSRF: t.CSRF, Issued: t.Issued,
			})
		}
		sess.mu.Unlock()
	}
	raw, err := json.Marshal(kept)
	if err == nil {
		err = s.store.KeepSessions(raw)
	}
	if err != nil {
		s.log.Error("keeping the CLI's sessions", "error", err)
	}
}

// tokenBytes makes session IDs and CSRF tokens 256 bits long.
const tokenBytes = 32

func randomToken() string {
	b := make([]byte, tokenBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

// start opens a session with a ticket or a pending challenge, and sets its cookie. Every sign-in
// step gets a new ID, so an ID seen before sign-in is worthless after it. A CLI session is kept once
// it holds a ticket, never while it waits for a second factor.
func (s *sessions) start(w http.ResponseWriter, ticket *pve.Ticket, challenge *pve.Challenge, cli bool) *session { //nolint:revive // flag-parameter: cli is what the session is, which it keeps, not a mode of start
	sess := &session{id: randomToken(), csrf: randomToken(), created: s.now(), cli: cli, ticket: ticket, challenge: challenge}
	s.mu.Lock()
	s.byID[sess.id] = sess
	s.mu.Unlock()
	if cli && ticket != nil {
		s.save()
	}
	setSessionCookie(w, sess.id)
	return sess
}

func setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// end forgets the request's session, if any, and clears its cookie.
func (s *sessions) end(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		sess := s.byID[c.Value]
		delete(s.byID, c.Value)
		s.mu.Unlock()
		if sess != nil && sess.cli {
			s.save()
		}
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

func (s *sessions) forget(sess *session) {
	if s.drop(sess) {
		s.save()
	}
}

// drop takes sess out of memory and tells whether store still keeps it.
func (s *sessions) drop(sess *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID[sess.id] != sess {
		return false
	}
	delete(s.byID, sess.id)
	return sess.cli
}

func (s *sessions) ended(sess *session, now time.Time) bool {
	maxAge := s.maxAge
	if sess.cli {
		maxAge = s.cliMaxAge
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	switch {
	case now.Sub(sess.created) >= maxAge:
		return true
	case sess.challenge != nil:
		return now.Sub(sess.created) >= challengeLifetime
	case sess.ticket != nil:
		return now.Sub(sess.ticket.Issued) >= pve.TicketLifetime
	default:
		return true
	}
}

// tend forgets ended sessions, which lookup does for the ones still asked for, and renews the
// tickets of the CLI's sessions that are left: a script may leave its sign-in unused for longer than
// a ticket lasts. One that Proxmox VE refuses ends. A browser's ticket is renewed by its own
// requests alone, so a closed tab's session runs out. Every user's requests look their session up
// under s.mu, so no session's lock is waited for while holding it. The kept sessions are written
// once, however many changed.
func (s *sessions) tend(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	all := slices.Collect(maps.Values(s.byID))
	s.mu.Unlock()
	var changed atomic.Bool
	var renewals sync.WaitGroup
	slots := make(chan struct{}, renewalsAtOnce)
	for _, sess := range all {
		switch {
		case s.ended(sess, now):
			if s.drop(sess) {
				changed.Store(true)
			}
		case sess.cli:
			slots <- struct{}{}
			renewals.Go(func() {
				defer func() { <-slots }()
				bounded, cancel := context.WithTimeout(ctx, renewalTimeout)
				defer cancel()
				_, renewed := s.renew(bounded, sess, renewUnusedAfter)
				if s.ended(sess, s.now()) {
					renewed = s.drop(sess)
				}
				if renewed {
					changed.Store(true)
				}
			})
		}
	}
	renewals.Wait()
	if changed.Load() {
		s.save()
	}
}

// pruneEvery tends the sessions until ctx ends.
func (s *sessions) pruneEvery(ctx context.Context, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tend(ctx)
		}
	}
}

// signedIn returns the session's ticket, renewed when it is old. nil means the session holds only
// a challenge, or Proxmox VE refused to renew (the user was disabled or expired). A renewal that
// fails for any other reason keeps the ticket, which is still valid, and is tried again next time.
// One request renews at a time, without the session's lock: a slow Proxmox VE holds up that request
// alone.
func (s *sessions) signedIn(ctx context.Context, sess *session) *pve.Ticket {
	ticket, changed := s.renew(ctx, sess, renewAfter)
	// A kept session needs its renewed ticket after a restart, since the old one runs out sooner.
	if changed && sess.cli {
		s.save()
	}
	return ticket
}

// renew is signedIn for a ticket at least after old, and tells whether the ticket changed. It does
// not save.
func (s *sessions) renew(ctx context.Context, sess *session, after time.Duration) (*pve.Ticket, bool) {
	sess.mu.Lock()
	ticket := sess.ticket
	if ticket == nil || sess.renewing || s.now().Sub(ticket.Issued) < after {
		sess.mu.Unlock()
		return ticket, false
	}
	sess.renewing = true
	sess.mu.Unlock()
	renewed, err := s.accounts.Renew(ctx, ticket)
	sess.mu.Lock()
	sess.renewing = false
	switch {
	case errors.Is(err, pve.ErrAuthentication):
		sess.ticket = nil
	case err == nil:
		sess.ticket = renewed
	}
	current := sess.ticket
	sess.mu.Unlock()
	return current, current != ticket
}

// csrfHeader carries a session's CSRF token, in the canonical form Go reads header names in.
const csrfHeader = "X-Kubehenge-Csrf"

// clientHeader set to cliClient on a password sign-in makes it a CLI session.
const (
	clientHeader = "X-Kubehenge-Client"
	cliClient    = "cli"
)

func (sess *session) checkCSRF(r *http.Request) bool {
	got := r.Header.Get(csrfHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) == 1
}
