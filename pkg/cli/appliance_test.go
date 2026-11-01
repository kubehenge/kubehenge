package cli

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	fakeCSRF     = "csrf-token"
	fakePassword = "the password"
)

// fakeInterface stands in for the appliance's interface: it signs in admin@pve and user@pve, of
// whom only the first may look after the appliance, and refuses as the interface does.
type fakeInterface struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	user     string
	bodies   map[string]map[string]any
	signOuts int
	setups   int
}

func newFakeInterface(t *testing.T) *fakeInterface {
	t.Helper()
	f := &fakeInterface{t: t, bodies: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", f.signIn)
	mux.HandleFunc("DELETE /api/session", f.signedIn(func(w http.ResponseWriter, _ *http.Request) {
		f.signOuts++
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/appliance/upgrades", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `{"version":"0.1.0","available":["0.2.0"],"status":{"state":"failed","from":"0.1.0","to":"0.2.0",
			"time":"2027-03-01T12:00:00Z","rolledBack":true,
			"error":"release 0.2.0 did not start: the interface did not answer within 2m0s; rolled back to 0.1.0"}}`)
	}))
	mux.HandleFunc("POST /api/appliance/upgrades", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	mux.HandleFunc("GET /api/appliance/etcd-backups", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		last := "20270301T120000Z-v1.37.1.db.age"
		if f.bodies["POST /api/appliance/etcd-backups/snapshots"] != nil {
			last = "20270301T130000Z-v1.37.1.db.age"
		}
		answer(w, http.StatusOK, `{"settings":{"target":"s3","everyHours":6,"keep":2,"since":"2027-03-01T11:00:00Z"},
			"last":{"name":"`+last+`","time":"2027-03-01T12:00:00Z"},"next":"2027-03-01T18:00:00Z"}`)
	}))
	mux.HandleFunc("PUT /api/appliance/etcd-backups", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `{"settings":{"target":"s3","everyHours":4,"keep":3,"since":"2027-03-01T11:00:00Z"}}`)
	}))
	mux.HandleFunc("POST /api/appliance/etcd-backups/snapshots", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusAccepted, `{"taking":true}`)
	}))
	mux.HandleFunc("GET /api/appliance/etcd-backups/snapshots", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `{"snapshots":[{"name":"20270301T120000Z-v1.37.1.db.age","time":"2027-03-01T12:00:00Z",
			"kubernetesVersion":"v1.37.1","size":37482528}]}`)
	}))
	mux.HandleFunc("POST /api/appliance/etcd-backups/snapshots/{name}/restore", f.admin(func(w http.ResponseWriter, r *http.Request) {
		if f.bodies[r.Method+" "+r.URL.Path]["passphrase"] != "right" {
			answer(w, http.StatusForbidden, `{"error":"wrong passphrase"}`)
			return
		}
		answer(w, http.StatusAccepted, `{}`)
	}))
	mux.HandleFunc("GET /api/notifications", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `[{"kind":"notify-webhook","at":"2027-03-01T12:00:00Z","title":"web: VM paused"},{"kind":"notify-ntfy"},
			{"kind":"notify-email","at":"2027-03-01T12:00:00Z","title":"web: VM paused","error":"signing in: 535 bad credentials"}]`)
	}))
	mux.HandleFunc("POST /api/notifications/test", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `[{"kind":"notify-webhook","at":"2027-03-01T12:05:00Z","title":"Kubehenge test"},
			{"kind":"notify-email","at":"2027-03-01T12:05:00Z","title":"Kubehenge test","error":"signing in: 535 bad credentials"}]`)
	}))
	mux.HandleFunc("GET /api/setup", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		f.setups++
		if f.setups == 1 {
			answer(w, http.StatusOK, `{"job":{"kind":"etcd-restore","state":"running","log":["Restoring"],"lines":1}}`)
			return
		}
		answer(w, http.StatusOK, `{"job":{"kind":"etcd-restore","state":"succeeded","log":["The snapshot matches its checksum","etcd runs"],"lines":3}}`)
	}))
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeInterface) signIn(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		answer(w, http.StatusBadRequest, `{"error":"not JSON"}`)
		return
	}
	if req["password"] != fakePassword {
		answer(w, http.StatusUnauthorized, `{"error":"sign-in failed: check the user name, password and realm"}`)
		return
	}
	f.mu.Lock()
	f.user = req["username"]
	f.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "__Host-kubehenge-session", Value: "session", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	answer(w, http.StatusOK, `{"user":"`+req["username"]+`","csrf":"`+fakeCSRF+`"}`)
}

// signedIn holds a request to the session and, unless it only reads, its CSRF token, and records
// its body.
func (f *fakeInterface) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("__Host-kubehenge-session"); err != nil || c.Value != "session" {
			answer(w, http.StatusUnauthorized, `{"error":"not signed in"}`)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Kubehenge-Csrf") != fakeCSRF {
			answer(w, http.StatusForbidden, `{"error":"missing or wrong CSRF token"}`)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			f.bodies[r.Method+" "+r.URL.Path] = body
		} else {
			f.bodies[r.Method+" "+r.URL.Path] = map[string]any{}
		}
		next(w, r)
	}
}

// admin is signedIn for what only administrators may do.
func (f *fakeInterface) admin(next http.HandlerFunc) http.HandlerFunc {
	return f.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if f.user != "admin@pve" {
			answer(w, http.StatusForbidden, `{"error":"`+f.user+` needs Sys.Modify on /"}`)
			return
		}
		next(w, r)
	})
}

func answer(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// args are the flags that reach the fake as user, the certificate and password in files.
func (f *fakeInterface) args(user string, rest ...string) []string {
	f.t.Helper()
	dir := f.t.TempDir()
	cert := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o600); err != nil {
		f.t.Fatal(err)
	}
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte(fakePassword+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return append([]string{"appliance", "--cert", cert, "--address", f.srv.Listener.Addr().String(), "--user", user, "--password-file", password}, rest...)
}

func TestApplianceUpgradesSayARollBackAndWhy(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := run(t, f.args("admin@pve", "upgrades")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Installed     0.1.0", "Available     0.2.0",
		"Last upgrade  to 0.2.0 rolled back to 0.1.0, 2027-03-01T12:00:00Z",
		"Reason        release 0.2.0 did not start: the interface did not answer within 2m0s; rolled back to 0.1.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if f.signOuts != 1 {
		t.Errorf("signed out %d times", f.signOuts)
	}
	out, err = run(t, f.args("admin@pve", "upgrades", "--json")...)
	if err != nil || !json.Valid([]byte(out)) || !strings.Contains(out, `"rolledBack":true`) {
		t.Errorf("--json: %v\n%s", err, out)
	}
	if _, err := run(t, f.args("admin@pve", "upgrade", "0.2.0")...); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies["POST /api/appliance/upgrades"]["version"]; got != "0.2.0" {
		t.Errorf("upgrade asked for %v", got)
	}
}

// The command is refused with the interface's own words, by the interface's own checks.
func TestApplianceRefusalsAreTheInterfaces(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{f.args("user@pve", "etcd-backups"), "user@pve needs Sys.Modify on /"},
		{f.args("user@pve", "upgrades"), "user@pve needs Sys.Modify on /"},
		{f.args("admin@pve", "etcd-backups", "restore", "20270301T120000Z-v1.37.1.db.age", "--passphrase-file", writeTemp(t, "wrong")), "wrong passphrase"},
		{f.args("admin@pve", "--password-file", writeTemp(t, "not it"), "etcd-backups"), "sign-in failed: check the user name, password and realm"},
		{f.args("", "etcd-backups"), "--user is required (env KUBEHENGE_USER)"},
	} {
		if _, err := run(t, c.args...); err == nil || err.Error() != c.want {
			t.Errorf("%v: %v, want %q", c.args[len(c.args)-1], err, c.want)
		}
	}
}

func TestApplianceEtcdBackups(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := run(t, f.args("admin@pve", "etcd-backups")...)
	if err != nil || !strings.Contains(out, "Schedule       every 6 h, newest 2 kept") || !strings.Contains(out, "Last snapshot  20270301T120000Z-v1.37.1.db.age") {
		t.Errorf("status: %v\n%s", err, out)
	}
	out, err = run(t, f.args("admin@pve", "etcd-backups", "snapshots")...)
	if err != nil || !strings.Contains(out, "20270301T120000Z-v1.37.1.db.age  2027-03-01T12:00:00Z  v1.37.1     37482528") {
		t.Errorf("snapshots: %v\n%s", err, out)
	}
	out, err = run(t, f.args("admin@pve", "etcd-backups", "take")...)
	if err != nil || out != "Taken: 20270301T130000Z-v1.37.1.db.age\n" {
		t.Errorf("take: %v\n%s", err, out)
	}
	out, err = run(t, f.args("admin@pve", "etcd-backups", "set-up", "--target", "s3", "--every-hours", "4", "--keep", "3",
		"--access-key-id", "key", "--secret-access-key-file", writeTemp(t, "secret"), "--passphrase-file", writeTemp(t, "right"))...)
	if err != nil || !strings.Contains(out, "every 4 h, newest 3 kept") {
		t.Errorf("set-up: %v\n%s", err, out)
	}
	want := map[string]any{"target": "s3", "everyHours": 4.0, "keep": 3.0, "accessKeyID": "key", "secretAccessKey": "secret", "passphrase": "right"}
	got := f.bodies["PUT /api/appliance/etcd-backups"]
	for k, v := range want {
		if got[k] != v {
			t.Errorf("set-up sent %s = %v, want %v", k, got[k], v)
		}
	}
	out, err = run(t, f.args("admin@pve", "etcd-backups", "restore", "20270301T120000Z-v1.37.1.db.age", "--passphrase-file", writeTemp(t, "right"))...)
	wantOut := "Restoring\nThe snapshot matches its checksum\netcd runs\nRestored from 20270301T120000Z-v1.37.1.db.age\n"
	if err != nil || out != wantOut {
		t.Errorf("restore: %v\n%q\nwant %q", err, out, wantOut)
	}
}

// The notification commands show each target's last sending, and a test that a target refused fails.
func TestApplianceNotifications(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := run(t, f.args("admin@pve", "notifications")...)
	for _, want := range []string{
		"Webhook  sent 2027-03-01T12:00:00Z: web: VM paused",
		"ntfy     nothing sent",
		"Email    failed 2027-03-01T12:00:00Z: web: VM paused (signing in: 535 bad credentials)",
	} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("notifications: %v, output lacks %q:\n%s", err, want, out)
		}
	}
	out, err = run(t, f.args("admin@pve", "notifications", "test")...)
	if err == nil || err.Error() != "Email did not take the test" || !strings.Contains(out, "Webhook  sent 2027-03-01T12:05:00Z: Kubehenge test") {
		t.Errorf("test: %v\n%s", err, out)
	}
	if _, err := run(t, f.args("user@pve", "notifications", "test")...); err == nil || err.Error() != "user@pve needs Sys.Modify on /" {
		t.Errorf("test as user@pve: %v", err)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
