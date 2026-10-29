package ui

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// openIDCookie binds an OpenID sign-in to the browser that started it. It is Lax, not Strict: the
// identity provider sends the browser back with a navigation from its own site.
const openIDCookie = "__Host-kubehenge-openid"

// openIDStateLifetime is how long the browser has to come back from the identity provider.
const openIDStateLifetime = 10 * time.Minute

// noPublicURL is why an OpenID sign-in cannot start or finish without the public address.
const noPublicURL = "OpenID sign-in needs the interface's public address, which an administrator sets with --public-url"

// redirectURL is where the identity provider sends users back, under the configured public address;
// ok is false while none is configured. The address the browser used is never taken instead: the
// Host header is the client's to choose, and a sign-in code must go nowhere it names.
func (s *Server) redirectURL() (string, bool) {
	base := strings.TrimSuffix(s.PublicURL, "/")
	return base + "/api/openid/callback", base != ""
}

// openIDStart sends the browser to the identity provider of an OpenID realm.
func (s *Server) openIDStart(w http.ResponseWriter, r *http.Request) {
	redirect, ok := s.redirectURL()
	if !ok {
		openIDFailed(w, r, noPublicURL)
		return
	}
	realm := r.URL.Query().Get("realm")
	realms, err := s.Accounts.Realms(r.Context())
	if err != nil {
		openIDFailed(w, r, "Proxmox VE did not answer")
		return
	}
	known := false
	for _, rm := range realms {
		known = known || (rm.Realm == realm && rm.Type == "openid")
	}
	if !known {
		openIDFailed(w, r, "no OpenID realm "+realm)
		return
	}
	authURL, err := s.Accounts.OpenIDAuthURL(r.Context(), realm, redirect)
	if err != nil {
		s.Log.Error("openid auth-url", "realm", realm, "error", err)
		openIDFailed(w, r, "Proxmox VE could not start the sign-in")
		return
	}
	u, err := url.Parse(authURL)
	state := ""
	if err == nil {
		state = u.Query().Get("state")
	}
	if state == "" {
		openIDFailed(w, r, "the identity provider's address carries no state")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: openIDCookie, Value: stateDigest(state), Path: "/", MaxAge: int(openIDStateLifetime.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authURL, http.StatusSeeOther) //nolint:gosec // Proxmox VE's answer for a configured realm, not the request's
}

// openIDCallback completes the sign-in the identity provider returns with. The state has to be the
// one this browser started with, so nobody can sign a victim in to an account of their own.
func (s *Server) openIDCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code, state := q.Get("code"), q.Get("state")
	cookie, err := r.Cookie(openIDCookie)
	http.SetCookie(w, &http.Cookie{Name: openIDCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if err != nil || state == "" || code == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(stateDigest(state))) != 1 {
		openIDFailed(w, r, "this sign-in was not started here; start again")
		return
	}
	if q.Get("error") != "" {
		openIDFailed(w, r, "the identity provider refused: "+q.Get("error"))
		return
	}
	redirect, ok := s.redirectURL()
	if !ok {
		openIDFailed(w, r, noPublicURL)
		return
	}
	done, ok := s.limiter.reserve(count{key: "address:" + clientAddress(r), allowed: addressFailuresAllowed})
	if !ok {
		openIDFailed(w, r, "too many failed sign-ins; try again later")
		return
	}
	ticket, err := s.Accounts.OpenIDLogin(r.Context(), code, state, redirect)
	done(err != nil)
	if err != nil {
		s.Log.Info("openid sign-in refused", "address", clientAddress(r), "error", err)
		openIDFailed(w, r, "the sign-in through the identity provider failed")
		return
	}
	s.sessions.end(w, r)
	s.sessions.start(w, ticket, nil)
	s.Log.Info("signed in", "user", ticket.Username, "address", clientAddress(r), "via", "openid")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// stateDigest stands for the state in the cookie: Proxmox VE's state holds characters a cookie
// value cannot.
func stateDigest(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

func openIDFailed(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/?"+url.Values{"signin-error": {message}}.Encode(), http.StatusSeeOther)
}
