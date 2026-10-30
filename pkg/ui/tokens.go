package ui

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// tokenPrefix starts an Authorization header that carries a Proxmox VE API token, as Proxmox VE's
// own API takes one: PVEAPIToken=user@realm!name=secret. Scripts and the kubehenge CLI act with it
// as the token's user, with the token's privileges, and need no session or CSRF token: a browser
// sends no such header to another site without the interface allowing it, which it never does.
const tokenPrefix = "PVEAPIToken=" //nolint:gosec // the header's scheme, not a credential

// tokenRecheck is how long a token Proxmox VE took is trusted before it is asked again, so that a
// script's requests do not each make one more; its privileges are asked for on every request.
const tokenRecheck = time.Minute

// tokenUsers are the API tokens Proxmox VE took a moment ago, by their hash.
type tokenUsers struct {
	mu   sync.Mutex
	seen map[[sha256.Size]byte]tokenSeen
}

type tokenSeen struct {
	ticket *pve.Ticket
	until  time.Time
}

// maxTokensSeen bounds what many tokens can make the interface keep.
const maxTokensSeen = 1024

// tokenUser returns the ticket for the API token the request carries, once Proxmox VE has taken
// it, or answers the request and returns nil.
func (s *Server) tokenUser(w http.ResponseWriter, r *http.Request, token string) *pve.Ticket {
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	s.tokens.mu.Lock()
	seen, ok := s.tokens.seen[key]
	s.tokens.mu.Unlock()
	if ok && now.Before(seen.until) {
		return seen.ticket
	}
	id, _, _ := strings.Cut(token, "=")
	done, allowed := s.limiter.reserve(signInCounts(id, r)...)
	if !allowed {
		writeError(w, http.StatusTooManyRequests, "too many failed sign-ins. Try again later")
		return nil
	}
	ticket, err := s.Accounts.TokenUser(r.Context(), token)
	done(errors.Is(err, pve.ErrAuthentication))
	if err != nil {
		if errors.Is(err, pve.ErrAuthentication) {
			s.Log.Info("API token refused", "token", id, "address", clientAddress(r))
			writeError(w, http.StatusUnauthorized, "Proxmox VE refused the API token")
			return nil
		}
		s.proxmoxError(w, err)
		return nil
	}
	s.tokens.mu.Lock()
	defer s.tokens.mu.Unlock()
	if s.tokens.seen == nil || len(s.tokens.seen) >= maxTokensSeen {
		s.tokens.seen = map[[sha256.Size]byte]tokenSeen{}
	}
	s.tokens.seen[key] = tokenSeen{ticket: ticket, until: now.Add(tokenRecheck)}
	return ticket
}
