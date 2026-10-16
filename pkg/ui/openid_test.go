package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func get(t *testing.T, h *harness, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), "GET", "https://kubehenge.example"+path, nil)
	req.RemoteAddr = "192.0.2.10:50000"
	for _, c := range cookies {
		req.Header.Add("Cookie", c.Name+"="+c.Value)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func cookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

func TestOpenIDSignIn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := get(t, h, "/api/openid/start?realm=sso")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || loc.Host != "idp.example" || loc.Query().Get("state") != `"st-sso"` {
		t.Fatalf("start: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if loc.Query().Get("redirect_uri") != "https://kubehenge.example/api/openid/callback" {
		t.Fatalf("redirect URL %q", loc.Query().Get("redirect_uri"))
	}
	state := cookie(rec, openIDCookie)
	if state == nil || state.Value != stateDigest(`"st-sso"`) || !state.HttpOnly || !state.Secure || state.SameSite != http.SameSiteLaxMode {
		t.Fatalf("state cookie %+v", state)
	}
	st := url.QueryEscape(`"st-sso"`)

	// The state must be the one this browser started with.
	rec = get(t, h, "/api/openid/callback?code=good&state="+st)
	if !strings.Contains(rec.Header().Get("Location"), "signin-error=") || cookie(rec, sessionCookie) != nil {
		t.Fatalf("callback without the cookie: %s", rec.Header().Get("Location"))
	}
	rec = get(t, h, "/api/openid/callback?code=good&state="+url.QueryEscape(`"other"`), state)
	if !strings.Contains(rec.Header().Get("Location"), "signin-error=this") {
		t.Fatalf("another state: %s", rec.Header().Get("Location"))
	}
	rec = get(t, h, "/api/openid/callback?code=bad&state="+st, state)
	if !strings.Contains(rec.Header().Get("Location"), "signin-error=the+sign-in+through+the+identity+provider+failed") {
		t.Fatalf("bad code: %s", rec.Header().Get("Location"))
	}
	rec = get(t, h, "/api/openid/callback?code=good&state="+st, state)
	session := cookie(rec, sessionCookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" || session == nil {
		t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	b := h.browser()
	b.cookie = session.Value
	rec = b.do("GET", "/api/session", "")
	if !strings.Contains(rec.Body.String(), `"user":"sso-user@sso"`) {
		t.Fatalf("session = %s", rec.Body)
	}
}

func TestOpenIDOnlyForOpenIDRealms(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, realm := range []string{"pve", "nope"} {
		rec := get(t, h, "/api/openid/start?realm="+realm)
		if !strings.Contains(rec.Header().Get("Location"), "signin-error=") {
			t.Errorf("%s: %s", realm, rec.Header().Get("Location"))
		}
	}
}

func TestOpenIDUsesThePublicURL(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.PublicURL = "https://k8s.example.org/"
	rec := get(t, h, "/api/openid/start?realm=sso")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("redirect_uri") != "https://k8s.example.org/api/openid/callback" {
		t.Fatalf("redirect URL %q", loc.Query().Get("redirect_uri"))
	}
}
