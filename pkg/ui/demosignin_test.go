package ui

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Signing out of the demo leaves the sign-in page, which takes any password and any token.
func TestDemoSignsInAgainAfterSignOut(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	for _, in := range []struct{ path, body string }{
		{"/api/session", `{"username":"admin","realm":"pve","password":"anything"}`},
		{"/api/session/token", `{"id":"admin@pve!demo","secret":"anything"}`},
	} {
		if rec := b.do(http.MethodDelete, "/api/session", ""); rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
			t.Fatalf("DELETE /api/session: %d %s", rec.Code, rec.Body)
		}
		rec := b.do(http.MethodPost, in.path, in.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s after signing out: %d %s", in.path, rec.Code, rec.Body)
		}
		var v sessionView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookie {
				b.cookie = c
			}
		}
		b.csrf = v.CSRF
	}
}

// The setup's final check only reads, so the demo answers it where it shows the settings form.
func TestDemoAnswersTheSetupCheck(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	rec := b.do(http.MethodGet, "/api/setup", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/setup: %d %s", rec.Code, rec.Body)
	}
	var status struct {
		Settings json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	rec = b.do(http.MethodPost, "/api/setup/check", `{"settings":`+string(status.Settings)+`}`)
	if rec.Code != http.StatusOK {
		t.Errorf("POST /api/setup/check: %d %s, want the check's answer", rec.Code, rec.Body)
	}
}
