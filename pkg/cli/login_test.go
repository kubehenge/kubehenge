package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The commands that reach the interface have no sign-in of their own: kubehenge login is the one
// way, and their help says so.
func TestNoCommandHasASignInOfItsOwn(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"appliance", "upgrades", "--user", "root@pam"},
		{"appliance", "upgrades", "--password-file", "f"},
		{"appliance", "upgrades", "--totp", "123456"},
		{"appliance", "--cert", "c", "upgrades"},
		{"appliance", "--address", "a", "upgrades"},
		{"cluster", "list", "--url", "https://example.org"},
		{"cluster", "list", "--ca", "f"},
	} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%v: %v, want an unknown flag", args, err)
		}
	}
	root := NewRoot()
	for _, path := range [][]string{{"appliance"}, {"cluster"}} {
		if sub, _, err := root.Find(path); err != nil || !strings.Contains(sub.Long, "kubehenge login") {
			t.Errorf("%v does not say it acts as the user of kubehenge login: %v", path, err)
		}
	}
	health, _, err := root.Find([]string{"appliance", "health"})
	if err != nil || health.Flag("cert") == nil || health.Flag("address") == nil {
		t.Errorf("health lacks the certificate and address it checks: %v", err)
	}
}

// One login serves the appliance commands and the cluster commands alike, and is kept readable by
// its owner alone.
func TestOneLoginServesEveryCommand(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run("appliance", "upgrades"); err != nil || !strings.Contains(out, "Installed     0.1.0") {
		t.Errorf("appliance upgrades: %v\n%s", err, out)
	}
	if out, err := f.run("cluster", "list", "-o", "json"); err != nil || !strings.Contains(out, `"clusters"`) {
		t.Errorf("cluster list: %v\n%s", err, out)
	}
	info, err := os.Stat(f.loginFile)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the login file is readable by others: %v", info.Mode())
	}
}

// A login ends with kubehenge logout, which also ends the session at the interface.
func TestLogoutEndsTheSessionAndForgetsTheLogin(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run("logout"); err != nil {
		t.Fatal(err)
	}
	if f.signOuts != 1 {
		t.Errorf("signed out %d times", f.signOuts)
	}
	if _, err := f.run("appliance", "upgrades"); err == nil || err.Error() != notLoggedIn {
		t.Errorf("after logout: %v", err)
	}
}

// A session the interface has ended says to log in again, not that the user is refused.
func TestAnEndedLoginSaysToLogInAgain(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	path := f.loginFile
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own login file
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"value":"session"`, `"value":"expired"`, 1)), 0o600); err != nil { //nolint:gosec // the test's own login file
		t.Fatal(err)
	}
	if _, err := f.run("cluster", "list"); err == nil || !strings.Contains(err.Error(), "run kubehenge login <url> again") {
		t.Errorf("an ended session: %v", err)
	}
}

// The interface's own certificate given as --ca is trusted for the name it carries, so that the
// appliance's shell can log in by its loopback address.
func TestLoginTrustsTheCertificateItWasGivenByItsName(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	address := strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1)
	if out, err := f.run("login", address, "--ca", f.certFile(), "--user", "admin@pve", "--password-file", writeTemp(t, fakePassword)); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
}

// A Proxmox VE API token is kept as it is, and every command sends it.
func TestLoginKeepsAToken(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	f.token = "dev@pve!ci=secret"
	if _, err := f.run("login", f.srv.URL, "--ca", f.certFile()); err == nil {
		t.Error("a login with neither a user nor a token")
	}
	if out, err := f.run("login", f.srv.URL, "--ca", f.certFile(), "--token-file", writeTemp(t, "dev@pve!ci=secret")); err != nil || !strings.Contains(out, "Logged in to") {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if out, err := f.run("cluster", "list", "-o", "json"); err != nil || !strings.Contains(out, `"clusters"`) {
		t.Errorf("cluster list: %v\n%s", err, out)
	}
	if _, err := f.run("login", f.srv.URL, "--ca", f.certFile(), "--token-file", writeTemp(t, "dev@pve!ci=guess")); err == nil {
		t.Error("a refused token was kept")
	}
}

// secondFactorInterface signs in as the interface does for a user with a TOTP code: the password
// starts a challenge session, and the right code ends it and starts the session commands use.
func secondFactorInterface(t *testing.T) *httptest.Server {
	t.Helper()
	const sessionCookie = "__Host-kubehenge-session"
	start := func(w http.ResponseWriter, id, body string) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		answer(w, http.StatusOK, body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, _ *http.Request) {
		start(w, "challenge", `{"csrf":"c1","secondFactor":{"totp":true}}`)
	})
	mux.HandleFunc("POST /api/session/second-factor", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if c, err := r.Cookie(sessionCookie); err != nil || c.Value != "challenge" || json.NewDecoder(r.Body).Decode(&req) != nil {
			answer(w, http.StatusUnauthorized, `{"error":"sign in with your password first"}`)
			return
		}
		if req["code"] != "123456" {
			answer(w, http.StatusUnauthorized, `{"error":"Wrong code, a locked factor or a cluster without quorum."}`)
			return
		}
		start(w, "signed-in", `{"csrf":"c2"}`)
	})
	mux.HandleFunc("GET /api/clusters", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err != nil || c.Value != "signed-in" {
			answer(w, http.StatusUnauthorized, `{"error":"not signed in"}`)
			return
		}
		w.Header().Set("X-Kubehenge-Version", "0.1.0")
		answer(w, http.StatusOK, `{"clusters":[]}`)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A login with a second factor keeps the session the code starts, not the challenge's.
func TestASecondFactorLoginKeepsTheSessionTheCodeStarts(t *testing.T) {
	t.Parallel()
	srv := secondFactorInterface(t)
	file := filepath.Join(t.TempDir(), "login.json")
	ca := writeTemp(t, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	if out, err := runIn(t, file, "login", srv.URL, "--ca", ca, "--user", "otp@pve", "--password-file", writeTemp(t, "pw"), "--totp", "123456"); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if out, err := runIn(t, file, "cluster", "list", "-o", "json"); err != nil || !strings.Contains(out, `"clusters"`) {
		t.Errorf("cluster list after the login: %v\n%s", err, out)
	}
}

// Only a refused session or token says to log in again. A wrong code, or a refusal the interface
// gives a signed-in user, shows the interface's reason, which logging in again would not change.
func TestOnlyARefusedLoginSaysToLogInAgain(t *testing.T) {
	t.Parallel()
	const clocks = "Proxmox VE nodes disagree about the time. Compare timedatectl on each node and read pvecm status."
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `{}`)
	})
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Host-kubehenge-session", Value: "s", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		answer(w, http.StatusOK, `{"csrf":"c"}`)
	})
	mux.HandleFunc("GET /api/clusters", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("__Host-kubehenge-session"); err == nil {
			w.Header().Set("X-Kubehenge-Version", "0.1.0")
			answer(w, http.StatusUnauthorized, `{"error":"`+clocks+`"}`)
			return
		}
		answer(w, http.StatusUnauthorized, `{"error":"Proxmox VE refused the API token"}`)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := writeTemp(t, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	for want, by := range map[string][]string{
		clocks + " (401)": {"--user", "admin@pve", "--password-file", writeTemp(t, "pw")},
		"the login has ended: run kubehenge login <url> again": {"--token-file", writeTemp(t, "dev@pve!ci=revoked")},
	} {
		file := filepath.Join(t.TempDir(), "login.json")
		if out, err := runIn(t, file, append([]string{"login", srv.URL, "--ca", ca}, by...)...); err != nil {
			t.Fatalf("login: %v\n%s", err, out)
		}
		if _, err := runIn(t, file, "cluster", "list"); err == nil || err.Error() != want {
			t.Errorf("%v: %v, want %q", by[0], err, want)
		}
	}
	totp := secondFactorInterface(t)
	ca = writeTemp(t, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: totp.Certificate().Raw})))
	_, err := runIn(t, filepath.Join(t.TempDir(), "login.json"), "login", totp.URL, "--ca", ca, "--user", "otp@pve", "--password-file", writeTemp(t, "pw"), "--totp", "000000")
	if err == nil || !strings.HasPrefix(err.Error(), "Wrong code") {
		t.Errorf("a wrong code: %v", err)
	}
}

// expiredCertificate is a self-signed certificate for 127.0.0.1 that expired an hour ago.
func expiredCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The appliance's shell logs in with the appliance's own certificate once it has expired, and the
// appliance commands reach it, as the way to recover. The cluster commands still check the clock,
// and another expired certificate is not trusted.
func TestAnExpiredApplianceCertificateKeepsTheApplianceCommands(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	f.srv.Close()
	srv := httptest.NewUnstartedServer(f.srv.Config.Handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{expiredCertificate(t)}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	certFile := func(cert tls.Certificate) string {
		return writeTemp(t, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})))
	}
	login := func(cert tls.Certificate) error {
		_, err := f.run("login", srv.URL, "--ca", certFile(cert), "--user", "admin@pve", "--password-file", writeTemp(t, fakePassword))
		return err
	}
	if err := login(srv.TLS.Certificates[0]); err != nil {
		t.Fatalf("login: %v", err)
	}
	if out, err := f.run("appliance", "upgrades"); err != nil || !strings.Contains(out, "Installed     0.1.0") {
		t.Errorf("appliance upgrades: %v\n%s", err, out)
	}
	if _, err := f.run("cluster", "list"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("cluster list through an expired certificate: %v", err)
	}
	if err := login(expiredCertificate(t)); err == nil || !strings.Contains(err.Error(), "unknown authority") {
		t.Errorf("login with another expired certificate: %v", err)
	}
}

// --user signs in by password although the token variable is set, as a flag beats the environment.
func TestUserBeatsTheTokenVariable(t *testing.T) {
	f := newFakeInterface(t)
	t.Setenv("KUBEHENGE_PROXMOX_TOKEN", "dev@pve!ci=secret")
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatalf("a password login with the token variable set: %v", err)
	}
	if _, err := f.run("login", f.srv.URL, "--ca", f.certFile(), "--user", "admin@pve", "--token-file", writeTemp(t, "dev@pve!ci=secret")); err == nil {
		t.Error("a login with both a user and a token file")
	}
}

// A command that reads the login while another command saves one reads a whole login.
func TestALoginIsReadWholeWhileAnotherIsSaved(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(t.Context(), loginFileKey{}, filepath.Join(t.TempDir(), "login.json"))
	logins := []*login{
		{URL: "https://kubehenge.example.org", Token: "dev@pve!ci=" + strings.Repeat("0123456789abcdef", 64)},
		{URL: "https://k.example", Cookie: "c", Value: "v", CSRF: "x"},
	}
	if err := logins[0].save(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var failed error
	go func() {
		defer close(done)
		for range 100 {
			if _, err := readLogin(ctx); err != nil {
				failed = err
				return
			}
		}
	}()
	for i := 0; ; i++ {
		select {
		case <-done:
			if failed != nil {
				t.Errorf("a read beside the saves: %v", failed)
			}
			return
		default:
		}
		if err := logins[i%2].save(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
