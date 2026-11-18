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
	"io"
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

// A password login says it is the CLI's, which the interface keeps for longer than a browser's.
func TestAPasswordLoginSaysItIsTheCLIs(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	if f.client != "cli" {
		t.Errorf("the sign-in came from %q", f.client)
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

// A new login ends the session of the one it replaces, which nothing could use any more. A login
// that failed ends nothing, and an interface that is gone holds no new login up.
func TestANewLoginEndsTheOneItReplaces(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	if err := f.login("admin@pve", "wrong"); err == nil || f.signOuts != 0 {
		t.Fatalf("a refused login: %v, signed out %d times", err, f.signOuts)
	}
	if err := f.login("user@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	if f.signOuts != 1 {
		t.Errorf("signed out %d times, want 1", f.signOuts)
	}

	gone := newFakeInterface(t)
	gone.loginFile = f.loginFile
	if err := gone.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	gone.srv.Close()
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatalf("a login replacing one at an interface that is gone: %v", err)
	}
	if _, err := f.run("appliance", "upgrades"); err != nil {
		t.Errorf("after the new login: %v", err)
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
	if _, err := f.run("cluster", "list"); err == nil || !strings.Contains(err.Error(), "run kubehenge login "+f.srv.URL+" again") {
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
	if out, err := f.run("login", f.srv.URL, "--ca", f.certFile(), "--token-file", writeTemp(t, "dev@pve!ci=secret")); err != nil || !strings.Contains(out, "Signed in to") {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if out, err := f.run("cluster", "list", "-o", "json"); err != nil || !strings.Contains(out, `"clusters"`) {
		t.Errorf("cluster list: %v\n%s", err, out)
	}
	if _, err := f.run("login", f.srv.URL, "--ca", f.certFile(), "--token-file", writeTemp(t, "dev@pve!ci=guess")); err == nil {
		t.Error("a refused token was kept")
	}
}

// An address typed without its scheme is taken as https, so that a bare host name works.
func TestLoginTakesABareHostAsHTTPS(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	f.token = "dev@pve!ci=secret"
	bare := strings.TrimPrefix(f.srv.URL, "https://")
	out, err := f.run("login", bare, "--ca", f.certFile(), "--token-file", writeTemp(t, "dev@pve!ci=secret"))
	if err != nil || !strings.Contains(out, "Signed in to "+f.srv.URL) {
		t.Fatalf("login with %q: %v\n%s", bare, err, out)
	}
	if out, err := f.run("cluster", "list", "-o", "json"); err != nil || !strings.Contains(out, `"clusters"`) {
		t.Errorf("cluster list: %v\n%s", err, out)
	}
}

// A login given both a user and a token names the rule, and a refused certificate names the flag
// that fixes it.
func TestLoginRefusalsSayWhatToGive(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	_, err := f.run("login", f.srv.URL, "--ca", f.certFile(), "--user", "admin@pve", "--token-file", writeTemp(t, "dev@pve!ci=secret"))
	if err == nil || err.Error() != "give --user or --token-file, not both" {
		t.Errorf("a user and a token: %v", err)
	}
	_, err = f.run("login", f.srv.URL, "--token-file", writeTemp(t, "dev@pve!ci=secret"))
	if err == nil || !strings.HasSuffix(err.Error(), ": give --ca the CA of the interface's certificate") {
		t.Errorf("an unknown certificate: %v", err)
	}
}

// Logging out twice says the second time that there was no login to end.
func TestLogoutWithoutALoginSaysSo(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Signed out", "Not signed in"} {
		if out, err := f.run("logout"); err != nil || strings.TrimSpace(out) != want {
			t.Errorf("logout: %v, %q, want %q", err, out, want)
		}
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
		clocks: {"--user", "admin@pve", "--password-file", writeTemp(t, "pw")},
		"the login has ended: run kubehenge login " + srv.URL + " again": {"--token-file", writeTemp(t, "dev@pve!ci=revoked")},
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

// signedBy is a certificate for 127.0.0.1 valid from notBefore to notAfter, signed by parent and
// its key, or self-signed as a CA where parent is nil.
func signedBy(t *testing.T, parent *tls.Certificate, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	issuer, signer := tmpl, any(key)
	if parent == nil {
		tmpl.IsCA, tmpl.BasicConstraintsValid = true, true
	} else {
		if issuer, err = x509.ParseCertificate(parent.Certificate[0]); err != nil {
			t.Fatal(err)
		}
		signer = parent.PrivateKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// serving is the fake's interface served with whatever certificate *cert holds at each handshake.
func serving(t *testing.T, f *fakeInterface, cert *tls.Certificate) *httptest.Server {
	t.Helper()
	f.srv.Close()
	srv := httptest.NewUnstartedServer(f.srv.Config.Handler)
	// httptest serves its own certificate to a client that names no server, as one dialling an
	// address does, unless each handshake gets a config of its own.
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert}}, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func pemOf(cert *tls.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
}

// An expired CA given as --ca vouches for no certificate it signed, even one valid when it was:
// only the very certificate in the file is trusted past its expiry.
func TestAnExpiredCAVouchesForNoOtherCertificate(t *testing.T) {
	t.Parallel()
	start := time.Now().Add(-30 * 24 * time.Hour)
	ca := signedBy(t, nil, start, time.Now().Add(-24*time.Hour))
	other := signedBy(t, &ca, start.Add(-time.Hour), start.Add(time.Hour))
	srv := serving(t, newFakeInterface(t), &other)
	caFile := writeTemp(t, string(pemOf(&ca)))
	_, err := runIn(t, filepath.Join(t.TempDir(), "login.json"), "login", srv.URL, "--ca", caFile, "--user", "admin@pve", "--password-file", writeTemp(t, fakePassword))
	if err == nil || !strings.Contains(err.Error(), errOtherCertificate.Error()+": give --ca the CA of the interface's certificate") {
		t.Errorf("a login through another certificate the expired CA signed: %v", err)
	}
}

// The appliance renews its certificate in place, and the login made with --ca on that file keeps
// working. With the file gone, the refusal names the login that fixes it.
func TestARenewedCertificateKeepsTheLogin(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	cert := signedBy(t, nil, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := serving(t, f, &cert)
	tlsCrt := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(tlsCrt, pemOf(&cert), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run("login", srv.URL, "--ca", tlsCrt, "--user", "admin@pve", "--password-file", writeTemp(t, fakePassword)); err != nil {
		t.Fatalf("login: %v", err)
	}
	cert = signedBy(t, nil, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err := os.WriteFile(tlsCrt, pemOf(&cert), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run("appliance", "upgrades"); err != nil || !strings.Contains(out, "Installed     0.1.0") {
		t.Errorf("appliance upgrades after the renewal: %v\n%s", err, out)
	}
	if err := os.Remove(tlsCrt); err != nil {
		t.Fatal(err)
	}
	cert = signedBy(t, nil, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if _, err := f.run("appliance", "upgrades"); err == nil || !strings.HasSuffix(err.Error(), "run kubehenge login "+srv.URL+" --ca "+tlsCrt) {
		t.Errorf("appliance upgrades through a certificate no longer trusted: %v", err)
	}
}

// A --ca file without a certificate is named, and logging in again with it would not help.
func TestACAFileWithoutACertificateIsNamed(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	empty := writeTemp(t, "not a certificate")
	_, err := f.run("login", f.srv.URL, "--ca", empty, "--user", "admin@pve", "--password-file", writeTemp(t, fakePassword))
	if err == nil || err.Error() != empty+" holds no PEM certificate" {
		t.Errorf("login with a --ca holding no certificate: %v", err)
	}
}

// An answer is written whole however large, up to what the interface takes back as an export, and
// one past that is refused rather than cut.
func TestAnAnswerIsWrittenWholeOrRefused(t *testing.T) {
	t.Parallel()
	kubeconfig := "apiVersion: v1\nkind: Config\n# " + strings.Repeat("x", 5<<20) + "\n"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/clusters/team-a/web/kubeconfig", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, kubeconfig)
	})
	mux.HandleFunc("GET /api/clusters/team-a/huge/kubeconfig", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.CopyN(w, zeros{}, maxInterfaceAnswer+1)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if out, err := runCluster(t, srv.URL, "dev@pve!ci=secret", "kubeconfig", "web", "-n", "team-a"); err != nil || out != kubeconfig {
		t.Errorf("a kubeconfig of %d bytes: %v, wrote %d", len(kubeconfig), err, len(out))
	}
	if _, err := runCluster(t, srv.URL, "dev@pve!ci=secret", "kubeconfig", "huge", "-n", "team-a"); err == nil || !strings.Contains(err.Error(), "over 64 MiB") {
		t.Errorf("an answer past the cap: %v", err)
	}
}

// zeros reads as an endless run of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
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
		for range 1000 {
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
