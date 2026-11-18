package cli

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	fakeCookie   = "__Host-kubehenge-session"
	fakeCSRF     = "csrf-token"
	fakePassword = "the password"
)

// fakeInterface stands in for the appliance's interface: it signs in admin@pve and user@pve, of
// whom only the first may look after the appliance, and refuses as the interface does.
type fakeInterface struct {
	t   *testing.T
	srv *httptest.Server
	// loginFile is this fake's own, so tests using one run side by side.
	loginFile string

	mu     sync.Mutex
	user   string
	token  string
	bodies map[string]map[string]any
	// sessions are the signed-in sessions by cookie, each sign-in's its own. ended are the ones
	// signed out, in order.
	sessions map[string]bool
	ended    []string
	signIns  int
	signOuts int
	setups   int
	// client is the client a password sign-in said it came from.
	client string
}

func newFakeInterface(t *testing.T) *fakeInterface {
	t.Helper()
	f := &fakeInterface{
		t:         t,
		loginFile: filepath.Join(t.TempDir(), "login.json"),
		bodies:    map[string]map[string]any{},
		sessions:  map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", f.signIn)
	mux.HandleFunc("DELETE /api/session", f.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(fakeCookie); err == nil {
			delete(f.sessions, c.Value)
			f.ended = append(f.ended, c.Value)
		}
		f.signOuts++
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/session", f.signedIn(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `{}`)
	}))
	mux.HandleFunc("GET /api/clusters", f.signedIn(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `{"clusters":[]}`)
	}))
	mux.HandleFunc("GET /api/appliance/upgrades", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `{"version":"0.1.0","available":["0.2.0"],"status":{"state":"failed","from":"0.1.0","to":"0.2.0",
			"time":"2027-03-01T12:00:00Z","rolledBack":true,
			"error":"release 0.2.0 did not start: the interface did not answer within 2m0s; rolled back to 0.1.0"}}`)
	}))
	mux.HandleFunc("GET /api/appliance/reboot-ready", f.admin(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("node") {
		case "pve2":
			answer(w, http.StatusOK, `{"ready":true,"answer":{"node":"pve2","ready":false,"blocker":"team-a/web: stopping pve2 would leave 1 of 3 etcd members, and its quorum is 2 (web-cp-0, web-cp-1)"}}`)
		case "":
			answer(w, http.StatusOK, `{"ready":false,"blocker":"management: etcd member kubehenge-cp-1 on pve2 is not healthy: its node is not ready",
				"answer":{"ready":false,"blocker":"management: etcd member kubehenge-cp-1 on pve2 is not healthy: its node is not ready"}}`)
		default:
			answer(w, http.StatusOK, `{"ready":true,"answer":{"node":"pve1","ready":true}}`)
		}
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
	mux.HandleFunc("POST /api/appliance/etcd-backups/recover", f.admin(func(w http.ResponseWriter, r *http.Request) {
		if f.bodies[r.Method+" "+r.URL.Path]["passphrase"] != "right" {
			answer(w, http.StatusForbidden, `{"error":"wrong passphrase"}`)
			return
		}
		answer(w, http.StatusAccepted, `{}`)
	}))
	mux.HandleFunc("POST /api/setup/take-over", f.admin(func(w http.ResponseWriter, r *http.Request) {
		if f.bodies[r.Method+" "+r.URL.Path]["passphrase"] != "right" {
			answer(w, http.StatusForbidden, `{"error":"wrong passphrase"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/notifications", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `[{"kind":"notify-webhook","at":"2027-03-01T12:00:00Z","title":"web: VM paused"},{"kind":"notify-ntfy"},
			{"kind":"notify-email","at":"2027-03-01T12:00:00Z","title":"web: VM paused","error":"signing in: 535 bad credentials"}]`)
	}))
	mux.HandleFunc("POST /api/notifications/test", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `[{"kind":"notify-webhook","at":"2027-03-01T12:05:00Z","title":"Kubehenge test"},
			{"kind":"notify-email","at":"2027-03-01T12:05:00Z","title":"Kubehenge test","error":"signing in: 535 bad credentials"}]`)
	}))
	mux.HandleFunc("GET /api/activity", f.admin(func(w http.ResponseWriter, r *http.Request) {
		days := "7"
		if r.URL.Query().Get("days") == "3" {
			days = "3"
		}
		answer(w, http.StatusOK, `{"entries":[{"at":"2027-03-01T12:00:00Z","user":"admin@pve","what":"changed cluster",
			"cluster":"default/web","details":{"requestId":"ab12"}},{"at":"2027-03-01T11:00:00Z","user":"mallory@pve",
			"what":"sign-in refused","details":{"address":"10.0.0.9"}},{"at":"2027-03-01T10:00:00Z","user":"bob@pve",
			"what":"headlamp sign-in","failed":true}],"days":"`+days+`"}`)
	}))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/activity", f.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != "web" || r.URL.Query().Get("days") != "30" {
			answer(w, http.StatusNotFound, `{"error":"no such cluster"}`)
			return
		}
		answer(w, http.StatusOK, `{"entries":[]}`)
	}))
	const saved = `"appliance":true,"allowed":true,"settings":{"bridge":"vmbr0","timeZone":"Europe/Zurich"}`
	mux.HandleFunc("GET /api/setup", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		f.setups++
		if f.setups == 1 {
			answer(w, http.StatusOK, `{`+saved+`,"job":{"kind":"etcd-restore","state":"running","log":["Restoring"],"lines":1}}`)
			return
		}
		answer(w, http.StatusOK, `{`+saved+`,"job":{"kind":"etcd-restore","state":"succeeded","log":["The snapshot matches its checksum","etcd runs"],"lines":3}}`)
	}))
	mux.HandleFunc("POST /api/setup", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		if field(savedBody(f), "timeZone") == "Mars/Base" {
			answer(w, http.StatusUnprocessableEntity, `{"error":"Time zone: Mars/Base is not an IANA time zone"}`)
			return
		}
		answer(w, http.StatusAccepted, `{}`)
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
	f.client = r.Header.Get("X-Kubehenge-Client")
	f.signIns++
	id := fmt.Sprintf("session-%d", f.signIns)
	f.sessions[id] = true
	f.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: fakeCookie, Value: id, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	answer(w, http.StatusOK, `{"user":"`+req["username"]+`","csrf":"`+fakeCSRF+`"}`)
}

// state is the sessions signed in, sorted, and those signed out, in order.
func (f *fakeInterface) state() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("signed in %v, ended %v", slices.Sorted(maps.Keys(f.sessions)), f.ended)
}

// signedInAs tells whether session id is signed in.
func (f *fakeInterface) signedInAs(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id]
}

// signedIn holds a request to the session and, unless it only reads, its CSRF token, and records
// its body.
func (f *fakeInterface) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		byToken := f.token != "" && r.Header.Get("Authorization") == "PVEAPIToken="+f.token
		if c, err := r.Cookie(fakeCookie); !byToken && (err != nil || !f.signedInAs(c.Value)) {
			answer(w, http.StatusUnauthorized, `{"error":"not signed in"}`)
			return
		}
		if !byToken && r.Method != http.MethodGet && r.Header.Get("X-Kubehenge-Csrf") != fakeCSRF {
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

// run runs a command with the fake's login file.
func (f *fakeInterface) run(args ...string) (string, error) {
	f.t.Helper()
	return runIn(f.t, f.loginFile, args...)
}

// login runs kubehenge login against the fake as user, into the login file that the commands after
// it read.
func (f *fakeInterface) login(user, password string) error {
	f.t.Helper()
	_, err := f.run("login", f.srv.URL, "--ca", f.certFile(), "--user", user, "--password-file", writeTemp(f.t, password))
	return err
}

// certFile is a file holding the certificate the fake presents.
func (f *fakeInterface) certFile() string {
	f.t.Helper()
	cert := filepath.Join(f.t.TempDir(), "tls.crt")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return cert
}

// args are an appliance command's arguments, after a login as user.
func (f *fakeInterface) args(user string, rest ...string) []string {
	f.t.Helper()
	if err := f.login(user, fakePassword); err != nil {
		f.t.Fatal(err)
	}
	return append([]string{"appliance"}, rest...)
}

func TestApplianceUpgradesSayARollBackAndWhy(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := f.run(f.args("admin@pve", "upgrades")...)
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
	if f.signOuts != 0 {
		t.Errorf("a command signed out, so the login ended with it: %d", f.signOuts)
	}
	out, err = f.run(f.args("admin@pve", "upgrades", "--json")...)
	if err != nil || !json.Valid([]byte(out)) || !strings.Contains(out, `"rolledBack":true`) {
		t.Errorf("--json: %v\n%s", err, out)
	}
	if _, err := f.run(f.args("admin@pve", "upgrade", "0.2.0")...); err != nil {
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
		user string
		args []string
		want string
	}{
		{"user@pve", []string{"etcd-backups"}, "user@pve needs Sys.Modify on /"},
		{"user@pve", []string{"upgrades"}, "user@pve needs Sys.Modify on /"},
		{"admin@pve", []string{"etcd-backups", "restore", "20270301T120000Z-v1.37.1.db.age", "--passphrase-file", writeTemp(t, "wrong")}, "wrong passphrase"},
	} {
		if _, err := f.run(f.args(c.user, c.args...)...); err == nil || err.Error() != c.want {
			t.Errorf("%v: %v, want %q", c.args[0], err, c.want)
		}
	}
	if err := f.login("admin@pve", "not it"); err == nil || err.Error() != "sign-in failed: check the user name, password and realm" {
		t.Errorf("a wrong password: %v", err)
	}
	if _, err := newFakeInterface(t).run("appliance", "etcd-backups"); err == nil || err.Error() != notLoggedIn {
		t.Errorf("without a login: %v", err)
	}
}

// ready-for-reboot ends with an error, which is exit status 1. The error names the first blocker.
func TestApplianceReadyForReboot(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := f.run(f.args("admin@pve", "ready-for-reboot", "--node", "pve1")...)
	if err != nil || out != "Ready to reboot pve1\n" {
		t.Errorf("a node that may go: %v\n%s", err, out)
	}
	_, err = f.run(f.args("admin@pve", "ready-for-reboot", "--node", "pve2")...)
	if !errors.Is(err, errNotReadyForReboot) || !strings.Contains(err.Error(), "team-a/web: stopping pve2 would leave 1 of 3 etcd members") {
		t.Errorf("a node that holds a quorum: %v", err)
	}
	_, err = f.run(f.args("admin@pve", "ready-for-reboot")...)
	if !errors.Is(err, errNotReadyForReboot) || !strings.Contains(err.Error(), "management: etcd member kubehenge-cp-1 on pve2 is not healthy") {
		t.Errorf("a member down: %v", err)
	}
	if _, err = f.run(f.args("user@pve", "ready-for-reboot")...); err == nil || err.Error() != "user@pve needs Sys.Modify on /" {
		t.Errorf("a user without privileges: %v", err)
	}
}

func TestApplianceEtcdBackups(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := f.run(f.args("admin@pve", "etcd-backups")...)
	if err != nil || !strings.Contains(out, "Schedule       every 6 h, newest 2 kept") || !strings.Contains(out, "Last snapshot  20270301T120000Z-v1.37.1.db.age") {
		t.Errorf("status: %v\n%s", err, out)
	}
	out, err = f.run(f.args("admin@pve", "etcd-backups", "snapshots")...)
	if err != nil || !strings.Contains(out, "20270301T120000Z-v1.37.1.db.age  2027-03-01T12:00:00Z  v1.37.1     37482528") {
		t.Errorf("snapshots: %v\n%s", err, out)
	}
	out, err = f.run(f.args("admin@pve", "etcd-backups", "take")...)
	if err != nil || out != "Taken: 20270301T130000Z-v1.37.1.db.age\n" {
		t.Errorf("take: %v\n%s", err, out)
	}
	out, err = f.run(f.args("admin@pve", "etcd-backups", "set-up", "--target", "s3", "--every-hours", "4", "--keep", "3",
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
	out, err = f.run(f.args("admin@pve", "etcd-backups", "restore", "20270301T120000Z-v1.37.1.db.age", "--passphrase-file", writeTemp(t, "right"))...)
	wantOut := "Restoring\nThe snapshot matches its checksum\netcd runs\nRestored from 20270301T120000Z-v1.37.1.db.age\n"
	if err != nil || out != wantOut {
		t.Errorf("restore: %v\n%q\nwant %q", err, out, wantOut)
	}
}

// recover goes through the route the interface's button takes, with no snapshot to name.
func TestApplianceEtcdBackupsRecover(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if _, err := f.run(f.args("admin@pve", "etcd-backups", "recover", "20270301T120000Z-v1.37.1.db.age", "--passphrase-file", writeTemp(t, "right"))...); err == nil {
		t.Error("recover took a snapshot's name")
	}
	if _, err := f.run(f.args("admin@pve", "etcd-backups", "recover", "--passphrase-file", writeTemp(t, "wrong"))...); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Errorf("a wrong passphrase: %v", err)
	}
	out, err := f.run(f.args("admin@pve", "etcd-backups", "recover", "--passphrase-file", writeTemp(t, "right"))...)
	if err != nil || !strings.HasSuffix(out, "Recovered the management cluster\n") {
		t.Errorf("recover: %v\n%s", err, out)
	}
	if got := f.bodies["POST /api/appliance/etcd-backups/recover"]; got["passphrase"] != "right" {
		t.Errorf("recover sent %v", got)
	}
}

func TestApplianceEtcdBackupsShowTheVolume(t *testing.T) {
	t.Parallel()
	last := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	v := &appliance.EtcdBackupStatus{
		Volume: &appliance.VolumeBackupStatus{
			Claim: "kubehenge-system/kubehenge-backups",
			Last:  &provisioner.EtcdSnapshot{Name: "20270301T120000Z-v1.37.2.db.age", Time: last},
			Copy:  &appliance.BackupCopy{Snapshot: "20270301T120000Z-v1.37.2.db.age", Bytes: 3 << 20, Cap: 1 << 30},
		},
		Where: &appliance.BackupSummary{Where: []string{"backup volume kubehenge-system/kubehenge-backups", "appliance copy"}, Last: &last, Error: "backup volume: unreachable"},
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := printBackups(cmd, v); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"backup volume kubehenge-system/kubehenge-backups, appliance copy", "2027-03-01T12:00:00Z",
		"backup volume: unreachable", "20270301T120000Z-v1.37.2.db.age, 3 of 1024 MiB",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the status lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Off") {
		t.Errorf("backups on the volume show as off:\n%s", out.String())
	}
}

// take-over needs --overwrite and the passphrase, and says what the next apply does.
func TestApplianceTakeOver(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if _, err := f.run(f.args("admin@pve", "take-over", "--passphrase-file", writeTemp(t, "right"))...); err == nil || !strings.Contains(err.Error(), "Give --overwrite") {
		t.Errorf("without --overwrite: %v", err)
	}
	if _, err := f.run(f.args("admin@pve", "take-over", "--overwrite", "--passphrase-file", writeTemp(t, "wrong"))...); err == nil || err.Error() != "wrong passphrase" {
		t.Errorf("wrong passphrase: %v", err)
	}
	out, err := f.run(f.args("admin@pve", "take-over", "--overwrite", "--passphrase-file", writeTemp(t, "right"))...)
	if err != nil || out != "Taken over. The next apply brings the management cluster to this state\n" {
		t.Errorf("take-over: %v\n%q", err, out)
	}
}

// The notification commands show each target's last sending, and a test that a target refused fails.
func TestApplianceNotifications(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := f.run(f.args("admin@pve", "notifications")...)
	for _, want := range []string{
		"Webhook  sent 2027-03-01T12:00:00Z: web: VM paused",
		"ntfy     nothing sent",
		"Email    failed 2027-03-01T12:00:00Z: web: VM paused (signing in: 535 bad credentials)",
	} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("notifications: %v, output lacks %q:\n%s", err, want, out)
		}
	}
	out, err = f.run(f.args("admin@pve", "notifications", "test")...)
	if err == nil || err.Error() != "Email did not take the test" || !strings.Contains(out, "Webhook  sent 2027-03-01T12:05:00Z: Kubehenge test") {
		t.Errorf("test: %v\n%s", err, out)
	}
	if _, err := f.run(f.args("user@pve", "notifications", "test")...); err == nil || err.Error() != "user@pve needs Sys.Modify on /" {
		t.Errorf("test as user@pve: %v", err)
	}
}

func TestApplianceActivity(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := f.run(f.args("admin@pve", "activity")...)
	want := "2027-03-01T12:00:00Z  admin@pve    changed cluster            default/web  \n" +
		"2027-03-01T11:00:00Z  mallory@pve  sign-in refused                         address 10.0.0.9\n" +
		"2027-03-01T10:00:00Z  bob@pve      headlamp sign-in (failed)               \n"
	if err != nil || out != want {
		t.Errorf("activity: %v\n%q\nwant %q", err, out, want)
	}
	out, err = f.run(f.args("admin@pve", "activity", "--json", "--days", "3")...)
	if err != nil || !strings.Contains(out, `"days":"3"`) {
		t.Errorf("activity --json --days 3: %v\n%s", err, out)
	}
	out, err = f.run(f.args("user@pve", "activity", "--cluster", "default/web", "--days", "30")...)
	if err != nil || out != "Nothing done\n" {
		t.Errorf("a cluster's activity: %v\n%q", err, out)
	}
	if _, err := f.run(f.args("user@pve", "activity", "--cluster", "web")...); err == nil || !strings.Contains(err.Error(), "namespace/name") {
		t.Errorf("--cluster without a namespace: %v", err)
	}
	if _, err := f.run(f.args("user@pve", "activity")...); err == nil {
		t.Error("the appliance's activity was shown to a user who may not look after it")
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
