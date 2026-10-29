package pve

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// openIDRealm is Proxmox VE with an OpenID realm, corp, whose identity provider is at
// idp.example.org and returns users to the interface's callback.
func openIDRealm(t *testing.T) *Accounts {
	t.Helper()
	const callback = "https://kubehenge.internal/oidc/callback"
	form := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
			http.Error(w, "not a form", http.StatusBadRequest)
			return false
		}
		if _, err := r.Cookie("PVEAuthCookie"); err == nil {
			t.Error("an OpenID request carried a ticket")
		}
		if r.PostForm.Get("redirect-url") != callback {
			refuseInStatusLine(t, w, "500 redirect-url does not match")
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api2/json/access/openid/auth-url", func(w http.ResponseWriter, r *http.Request) {
		if !form(w, r) {
			return
		}
		switch r.PostForm.Get("realm") {
		case "corp":
			replyData(t, w, "https://idp.example.org/authorize?client_id=pve&state=s1&redirect_uri="+callback)
		case "misconfigured":
			replyData(t, w, "idp.example.org/authorize")
		default:
			refuseInStatusLine(t, w, "500 domain '"+r.PostForm.Get("realm")+"' does not exist")
		}
	})
	mux.HandleFunc("POST /api2/json/access/openid/login", func(w http.ResponseWriter, r *http.Request) {
		if !form(w, r) {
			return
		}
		switch code, state := r.PostForm.Get("code"), r.PostForm.Get("state"); {
		case code == "c1" && state == "s1":
			replyData(t, w, map[string]any{"username": "carol@corp", "ticket": "PVE:carol@corp:1", "CSRFPreventionToken": "csrf-carol"})
		case code == "unknown-user":
			http.Error(w, "authentication failure", http.StatusUnauthorized)
		case code == "no-ticket":
			replyData(t, w, map[string]any{"username": "carol@corp"})
		default:
			refuseInStatusLine(t, w, "500 unable to authenticate - invalid state")
		}
	})
	return serveAPI(t, mux)
}

func TestOpenIDAuthURLIsWhereProxmoxVESendsTheUser(t *testing.T) {
	t.Parallel()
	a := openIDRealm(t)
	u, err := a.OpenIDAuthURL(context.Background(), "corp", "https://kubehenge.internal/oidc/callback")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://idp.example.org/authorize?") || !strings.Contains(u, "state=s1") {
		t.Errorf("sign-in URL %q", u)
	}
}

func TestOpenIDAuthURLFailsWithoutAnAddress(t *testing.T) {
	t.Parallel()
	a := openIDRealm(t)
	ctx := context.Background()
	if _, err := a.OpenIDAuthURL(ctx, "misconfigured", "https://kubehenge.internal/oidc/callback"); err == nil || !strings.Contains(err.Error(), "no sign-in URL for realm misconfigured") {
		t.Errorf("an answer that is no URL: %v", err)
	}
	var apiErr *APIError
	if _, err := a.OpenIDAuthURL(ctx, "nowhere", "https://kubehenge.internal/oidc/callback"); !errors.As(err, &apiErr) || !strings.Contains(apiErr.Status, "does not exist") {
		t.Errorf("an unknown realm: %v", err)
	}
	if _, err := a.OpenIDAuthURL(ctx, "corp", "https://elsewhere.example.org/"); err == nil {
		t.Error("a redirect Proxmox VE refused gave a sign-in URL")
	}
}

func TestOpenIDLoginGivesATicket(t *testing.T) {
	t.Parallel()
	before := time.Now()
	ticket, err := openIDRealm(t).OpenIDLogin(context.Background(), "c1", "s1", "https://kubehenge.internal/oidc/callback")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Username != "carol@corp" || ticket.Ticket != "PVE:carol@corp:1" || ticket.CSRF != "csrf-carol" || ticket.Issued.Before(before) || time.Since(ticket.Issued) > time.Minute {
		t.Errorf("ticket %+v", ticket)
	}
}

// Every refusal is an authentication failure, which tells the user no more than a wrong password.
func TestOpenIDLoginRefusalsAreAuthenticationFailures(t *testing.T) {
	t.Parallel()
	a := openIDRealm(t)
	for code, reason := range map[string]string{"stale": "invalid state", "unknown-user": "", "no-ticket": ""} {
		ticket, err := a.OpenIDLogin(context.Background(), code, "s1", "https://kubehenge.internal/oidc/callback")
		if !errors.Is(err, ErrAuthentication) || ticket != nil {
			t.Errorf("%s: %v, %+v", code, err, ticket)
			continue
		}
		// Proxmox VE's reason goes to the log with the error.
		if reason != "" && !strings.Contains(err.Error(), reason) {
			t.Errorf("%s: %v lacks Proxmox VE's reason", code, err)
		}
	}
}

// A failure to reach Proxmox VE is no verdict on the user, so it is not an authentication failure.
func TestOpenIDLoginFailsWhenProxmoxVEDropsTheConnection(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api2/json/access/openid/login", func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	_, err := serveAPI(t, mux).OpenIDLogin(context.Background(), "c1", "s1", "https://kubehenge.internal/oidc/callback")
	if err == nil || errors.Is(err, ErrAuthentication) {
		t.Errorf("a dropped connection: %v", err)
	}
}

func TestSecondFactorChallengeExpiresWithTheTicket(t *testing.T) {
	t.Parallel()
	before := time.Now()
	_, c, err := fakeTicketAPI(t).SignIn(context.Background(), "bob@pve", "secret")
	if err != nil || c == nil {
		t.Fatalf("challenge %v, %v", c, err)
	}
	after := time.Now()
	if c.Expired(after) {
		t.Error("a challenge just issued has expired")
	}
	if c.Expired(before.Add(TicketLifetime - time.Second)) {
		t.Error("a challenge expired before its ticket's lifetime")
	}
	if !c.Expired(after.Add(TicketLifetime)) {
		t.Error("a challenge outlived its ticket")
	}
}
