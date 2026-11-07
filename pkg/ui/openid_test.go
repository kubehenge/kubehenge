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
	return getAt(t, h, "kubehenge.example", path, cookies...)
}

// getAt is get by the name the browser reached the interface at.
func getAt(t *testing.T, h *harness, host, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), "GET", "https://"+host+path, nil)
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
	h.server.PublicURL = "https://kubehenge.example"
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
	if !strings.Contains(rec.Header().Get("Location"), "signin-error=sign-in+through+the+identity+provider+failed") {
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

// The Host header is the client's to choose, so without a public address there is nowhere the
// identity provider's code may safely go.
func TestOpenIDNeedsThePublicURL(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := get(t, h, "/api/openid/start?realm=sso")
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "signin-error=OpenID+sign-in+needs+the+public+address") || cookie(rec, openIDCookie) != nil {
		t.Fatalf("start without a public address: %s", loc)
	}
	h.server.PublicURL = "https://kubehenge.example"
	state := cookie(get(t, h, "/api/openid/start?realm=sso"), openIDCookie)
	h.server.PublicURL = ""
	rec = get(t, h, "/api/openid/callback?code=good&state="+url.QueryEscape(`"st-sso"`), state)
	if !strings.Contains(rec.Header().Get("Location"), "signin-error=OpenID") || cookie(rec, sessionCookie) != nil {
		t.Fatalf("callback without a public address: %s", rec.Header().Get("Location"))
	}
}

func TestOpenIDUsesThePublicURL(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.PublicURL = "https://k8s.example.org/"
	rec := getAt(t, h, "k8s.example.org", "/api/openid/start?realm=sso")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("redirect_uri") != "https://k8s.example.org/api/openid/callback" {
		t.Fatalf("redirect URL %q", loc.Query().Get("redirect_uri"))
	}
}

// The identity provider returns to the public address, and the state cookie is host-only, so a
// sign-in started under another name would come back to a host without its cookie. The start
// moves the browser to the public address first, whatever Host it names.
func TestOpenIDFromAnotherNameStartsAtThePublicURL(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.PublicURL = "https://kubehenge.example"
	for _, host := range []string{"192.0.2.40", "kubehenge", "kubehenge.example:8443", "evil.example"} {
		rec := getAt(t, h, host, "/api/openid/start?realm=sso")
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://kubehenge.example/api/openid/start?moved=1&realm=sso" || cookie(rec, openIDCookie) != nil {
			t.Errorf("from %s: %d %s, cookie %v", host, rec.Code, rec.Header().Get("Location"), cookie(rec, openIDCookie))
		}
	}

	// The public name itself, in any case and with its default port, starts the sign-in.
	for _, host := range []string{"kubehenge.example", "KubeHenge.Example", "kubehenge.example:443"} {
		rec := getAt(t, h, host, "/api/openid/start?realm=sso")
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://idp.example") || cookie(rec, openIDCookie) == nil {
			t.Errorf("from %s: %d %s", host, rec.Code, loc)
		}
	}

	// A public address with a port is compared with it.
	h.server.PublicURL = "https://kubehenge.example:8443"
	rec := getAt(t, h, "kubehenge.example", "/api/openid/start?realm=sso")
	if rec.Header().Get("Location") != "https://kubehenge.example:8443/api/openid/start?moved=1&realm=sso" {
		t.Errorf("without the port: %s", rec.Header().Get("Location"))
	}

	// A proxy that rewrites the Host header cannot make the browser go round: a start that was
	// moved once goes on whatever Host it arrives with.
	rec = getAt(t, h, "192.0.2.40", "/api/openid/start?moved=1&realm=sso")
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://idp.example") {
		t.Errorf("a start already moved: %s", loc)
	}
}
