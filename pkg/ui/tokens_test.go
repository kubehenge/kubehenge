package ui

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAnAPITokenActsAsItsUser(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	script := &browser{h: h, token: "dev@pve!ci=secret"} //nolint:gosec // the fake accounts' own

	rec := script.do("GET", "/api/session", "")
	expect(t, rec, http.StatusOK)
	var v sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.User != "dev@pve!ci" || v.CSRF != "" {
		t.Fatalf("session = %+v", v)
	}
	// A change needs no CSRF token, and is the token's, by its name.
	expect(t, script.do("PATCH", "/api/clusters/default/one", `{"controlPlaneReplicas":3}`), http.StatusOK)
	got := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, got); err != nil {
		t.Fatal(err)
	}
	if n := got.Spec.Topology.ControlPlane.Replicas; n == nil || *n != 3 {
		t.Fatalf("control planes = %v", n)
	}
	// Proxmox VE is asked once for the token, not for every request.
	expect(t, script.do("GET", "/api/clusters", ""), http.StatusOK)
	h.accounts.mu.Lock()
	checks := h.accounts.tokenChecks
	h.accounts.mu.Unlock()
	if checks != 1 {
		t.Errorf("the token was checked %d times", checks)
	}

	// The token holds only its user's privileges.
	viewer := &browser{h: h, token: "viewer@pve!ci=secret"} //nolint:gosec // the fake accounts' own
	expect(t, viewer.do("GET", "/api/clusters/default/one", ""), http.StatusOK)
	expect(t, viewer.do("PATCH", "/api/clusters/default/one", `{"controlPlaneReplicas":1}`), http.StatusForbidden)

	// A token Proxmox VE does not take, or a user's name with a made-up secret, gets nothing.
	for _, token := range []string{"dev@pve!ci=guess", "dev@pve", "nobody@pve!ci=secret"} {
		expect(t, (&browser{h: h, token: token}).do("GET", "/api/clusters", ""), http.StatusUnauthorized)
	}
}

const tokenSignIn = `{"id":"dev@pve!ci","secret":"secret"}`

// A token signs in to the browser as a password does, for users whose second factor is a security
// key that answers only on Proxmox VE's own page.
func TestSigningInWithAnAPIToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	h.accounts.revoked = map[string]bool{}
	b := h.browser()
	rec := b.do("POST", "/api/session/token", tokenSignIn)
	expect(t, rec, http.StatusOK)
	var v sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.User != "dev@pve!ci" || v.CSRF == "" || b.cookie == "" {
		t.Fatalf("session = %+v, cookie %q", v, b.cookie)
	}

	// The session carries the CSRF token as any other, and holds the token's user's privileges.
	expect(t, b.do("GET", "/api/clusters/default/one", ""), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"controlPlaneReplicas":3}`), http.StatusOK)
	b.csrf = "guess"
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"controlPlaneReplicas":1}`), http.StatusForbidden)
	b.csrf = v.CSRF

	// It ends when Proxmox VE stops taking the token, at the next renewal.
	h.clock = h.clock.Add(renewAfter + time.Minute)
	expect(t, b.do("GET", "/api/session", ""), http.StatusOK)
	h.accounts.mu.Lock()
	h.accounts.revoked["dev@pve!ci"] = true
	h.accounts.mu.Unlock()
	h.clock = h.clock.Add(renewAfter)
	expect(t, b.do("GET", "/api/session", ""), http.StatusUnauthorized)

	again := h.browser()
	expect(t, again.do("POST", "/api/session/token", `{"id":"viewer@pve!ci","secret":"secret"}`), http.StatusOK)
	expect(t, again.do("DELETE", "/api/session", ""), http.StatusNoContent)
	expect(t, again.do("GET", "/api/session", ""), http.StatusUnauthorized)
}

// A token with privilege separation holds the privileges both it and its user hold.
func TestASeparatedTokenSessionHoldsTheIntersection(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	h.accounts.tokenGrants = map[string]map[string][]string{"dev@pve!narrow": {"/pool/kubehenge": {"VM.Audit"}}}
	b := h.browser()
	expect(t, b.do("POST", "/api/session/token", `{"id":"dev@pve!narrow","secret":"secret"}`), http.StatusOK)
	expect(t, b.do("GET", "/api/clusters/default/one", ""), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"controlPlaneReplicas":3}`), http.StatusForbidden)
	rec := b.do("GET", "/api/session", "")
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"canCreate":false`) {
		t.Errorf("a token that cannot create is offered it: %s", rec.Body)
	}
}

func TestAWrongAPITokenIsRefusedAndLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	for _, body := range []string{`{"id":"nobody@pve!ci","secret":"secret"}`, `{"id":"dev@pve","secret":"secret"}`} {
		expect(t, b.do("POST", "/api/session/token", body), http.StatusUnauthorized)
	}
	expect(t, b.do("POST", "/api/session/token", `{"id":"dev@pve!ci","secret":""}`), http.StatusBadRequest)
	expect(t, b.do("POST", "/api/session/token", `not json`), http.StatusBadRequest)
	for range failuresAllowed {
		expect(t, b.do("POST", "/api/session/token", `{"id":"dev@pve!ci","secret":"guess"}`), http.StatusUnauthorized)
	}
	if b.cookie != "" {
		t.Errorf("a refused token got the session %q", b.cookie)
	}
	// The count is the token's, whether it is given in the form or the header.
	expect(t, b.do("POST", "/api/session/token", tokenSignIn), http.StatusTooManyRequests)
	expect(t, (&browser{h: h, token: "dev@pve!ci=secret"}).do("GET", "/api/clusters", ""), http.StatusTooManyRequests)
	h.clock = h.clock.Add(16 * time.Minute)
	expect(t, b.do("POST", "/api/session/token", tokenSignIn), http.StatusOK)
}

// A refused token is logged by its ID, never with its secret, also when the header has no
// separator between the two.
func TestARefusedTokenLogsNoSecret(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var logged bytes.Buffer
	h.server.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	for _, token := range []string{"dev@pve!ci=guess", "dev@pve!ci:guess"} {
		expect(t, (&browser{h: h, token: token}).do("GET", "/api/clusters", ""), http.StatusUnauthorized)
	}
	if strings.Contains(logged.String(), "guess") || strings.Count(logged.String(), `"token":"dev@pve!ci"`) != 1 {
		t.Errorf("the refusals were logged as %s", &logged)
	}
}
