package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

// runTrusting runs a command with the login in loginFile and trust in place of the system's.
func runTrusting(t *testing.T, loginFile string, trust interfaceTrust, args ...string) (string, error) {
	t.Helper()
	ctx := context.WithValue(context.WithValue(t.Context(), loginFileKey{}, loginFile), trustKey{}, trust)
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

// storedFileOf is cert as the interface keeps one an administrator gave it.
func storedFileOf(t *testing.T, cert *tls.Certificate) []byte {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Concat([]byte("# source: uploaded\n"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), pemOf(cert))
}

func writeFileIn(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The interface keeps the certificate an administrator gave it in the appliance's directory, which
// container backups hold, so a restore serves it again. The key directory, which backups leave out,
// holds none.
func TestTheStoredCertificateIsReadFromTheApplianceDirectory(t *testing.T) {
	t.Parallel()
	o, _ := serveOptionsIn(t, t.TempDir())
	o.a.KeyDir = t.TempDir()
	given := signedBy(t, nil, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	served := func() []byte {
		t.Helper()
		if err := o.loadCertificate(time.Now()); err != nil {
			t.Fatal(err)
		}
		pair, err := o.s.Certificate.GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		return pair.Certificate[0]
	}
	writeFileIn(t, filepath.Join(o.a.KeyDir, appliance.InterfaceCertificateFile), storedFileOf(t, &given))
	if bytes.Equal(served(), given.Certificate[0]) {
		t.Error("the certificate in the key directory, which backups leave out, is served")
	}
	writeFileIn(t, filepath.Join(o.a.Dir, appliance.InterfaceCertificateFile), storedFileOf(t, &given))
	if !bytes.Equal(served(), given.Certificate[0]) {
		t.Error("the certificate in the appliance's directory is not served")
	}
}

// A login trusts a certificate its CA or the system's roots vouch for, so that a change of the
// interface's certificate to one from a public or company CA keeps it working. A certificate
// neither vouches for is refused with its fingerprint and the login that fixes it.
func TestTheCLITrustsACertificateTheSavedCAOrTheSystemVouchesFor(t *testing.T) {
	t.Parallel()
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	own, public := signedBy(t, nil, start, end), signedBy(t, nil, start, end)
	cert := signedBy(t, &own, start, end)
	f := newFakeInterface(t)
	srv := serving(t, f, &cert)
	system := x509.NewCertPool()
	system.AppendCertsFromPEM(pemOf(&public))
	trust := interfaceTrust{systemRoots: func() (*x509.CertPool, error) { return system.Clone(), nil }, installerCert: filepath.Join(t.TempDir(), "none")}
	caFile := writeTemp(t, string(pemOf(&own)))
	if _, err := runTrusting(t, f.loginFile, trust, "login", srv.URL, "--ca", caFile, "--user", "admin@pve", "--password-file", writeTemp(t, fakePassword)); err != nil {
		t.Fatalf("login: %v", err)
	}
	for name, next := range map[string]tls.Certificate{"one the login's CA signed": signedBy(t, &own, start, end), "one the system's roots vouch for": signedBy(t, &public, start, end)} {
		cert = next
		if out, err := runTrusting(t, f.loginFile, trust, "appliance", "upgrades"); err != nil || !strings.Contains(out, "Installed     0.1.0") {
			t.Errorf("after a change to %s: %v\n%s", name, err, out)
		}
	}
	cert = signedBy(t, nil, start, end)
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	want := "it presents the certificate " + appliance.Fingerprint(leaf) + ", which this login does not trust. Run kubehenge login " + srv.URL + " --ca " + caFile
	if _, err := runTrusting(t, f.loginFile, trust, "appliance", "upgrades"); err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("after a change to one nothing vouches for: %v", err)
	}
}

// In the appliance a login made without --ca trusts the certificate the interface serves: the one an
// administrator gave it while that reads cleanly, else the installer's. A change of it, and a reset
// back to the installer's, keep the login working.
func TestTheApplianceLoginFollowsTheServedCertificate(t *testing.T) {
	t.Parallel()
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	installer, given := signedBy(t, nil, start, end), signedBy(t, nil, start, end)
	cert := installer
	f := newFakeInterface(t)
	srv := serving(t, f, &cert)
	dir := t.TempDir()
	trust := interfaceTrust{
		systemRoots:   func() (*x509.CertPool, error) { return x509.NewCertPool(), nil },
		installerCert: filepath.Join(dir, "tls.crt"), storedCert: filepath.Join(dir, appliance.InterfaceCertificateFile),
	}
	writeFileIn(t, trust.installerCert, pemOf(&installer))
	if _, err := runTrusting(t, f.loginFile, trust, "login", srv.URL, "--user", "admin@pve", "--password-file", writeTemp(t, fakePassword)); err != nil {
		t.Fatalf("login in the appliance without --ca: %v", err)
	}
	upgrades := func(when string) {
		t.Helper()
		if out, err := runTrusting(t, f.loginFile, trust, "appliance", "upgrades"); err != nil || !strings.Contains(out, "Installed     0.1.0") {
			t.Errorf("%s: %v\n%s", when, err, out)
		}
	}
	cert = given
	writeFileIn(t, trust.storedCert, storedFileOf(t, &given))
	upgrades("serving the certificate an administrator gave")
	cert = installer
	if err := os.Remove(trust.storedCert); err != nil {
		t.Fatal(err)
	}
	upgrades("serving the installer's again")
}

// The upgrade's health check, in the updater and in kubehenge appliance health, expects the stored
// certificate where the interface keeps it, and --stored-cert names another file.
func TestHealthAndTheUpdaterExpectTheStoredCertificate(t *testing.T) {
	t.Parallel()
	if c := updateCheck(); c.StoredFile != appliance.InterfaceCertificatePath || c.CertFile != appliance.InstallerCertificatePath {
		t.Errorf("the updater expects %q, else %q", c.StoredFile, c.CertFile)
	}
	for args, want := range map[string]string{"": appliance.InterfaceCertificatePath, "--stored-cert=/elsewhere.pem": "/elsewhere.pem"} {
		o := &applianceOptions{}
		if err := o.healthCommand().ParseFlags(strings.Fields(args)); err != nil || o.check.StoredFile != want {
			t.Errorf("health %s expects %q, want %q: %v", args, o.check.StoredFile, want, err)
		}
	}
}

// A request with a body says its type: JSON, which the interface takes only as application/json,
// or the type of a form already encoded.
func TestARequestSaysTheTypeOfItsBody(t *testing.T) {
	t.Parallel()
	types := make(chan string, 2)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		types <- r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	ctx := context.WithValue(t.Context(), trustKey{}, interfaceTrust{systemRoots: func() (*x509.CertPool, error) { return x509.NewCertPool(), nil }})
	c, err := newInterfaceClient(ctx, &login{URL: srv.URL, CA: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body any
		want string
	}{
		{map[string]string{"a": "JSON body"}, "application/json"},
		{formBody{contentType: "multipart/form-data; boundary=b", data: []byte("--b--")}, "multipart/form-data; boundary=b"},
	} {
		if _, err := c.call(ctx, http.MethodPost, "/api/anything", tc.body, nil); err != nil {
			t.Fatal(err)
		}
		if got := <-types; got != tc.want {
			t.Errorf("%T is sent as %q, want %q", tc.body, got, tc.want)
		}
	}
}

// The demo given --tls-cert serves that certificate, not the demo's own.
func TestTheDemoServesTheCertificateOfTLSCert(t *testing.T) {
	t.Parallel()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	o := &serveOptions{demo: true, listen: address}
	o.logs.level = "info"
	o.certFile, o.keyFile = writeCertificate(t, t.TempDir())
	want, err := tls.LoadX509KeyPair(o.certFile, o.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.runDemo(ctx) }()
	var got []byte
	for range 100 {
		conn, dialErr := (&tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", address) //nolint:gosec // only the presented certificate is read
		if dialErr == nil {
			if secured, ok := conn.(*tls.Conn); ok {
				got = secured.ConnectionState().PeerCertificates[0].Raw
			}
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("the demo gave %v once its context ended", err)
	}
	if !bytes.Equal(got, want.Certificate[0]) {
		t.Error("the demo does not serve the certificate of --tls-cert")
	}
}

// The certificate's table says when it is self-signed, when its chain lacks an issuer and whether
// the public name resolves to the appliance. A passphrase file that cannot be read stops a replace.
func TestTheCertificateTableSaysWhatIsKnownOfIt(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	for want, v := range map[string]certificateAnswer{
		"Self-signed":                          {SelfSigned: true},
		"the chain lacks CN=Example CA":        {Incomplete: true, MissingIssuer: "CN=Example CA"},
		", resolves to this appliance":         {PublicURL: "https://kubehenge.example.org", PublicNameResolves: &yes},
		", does not resolve to this appliance": {PublicURL: "https://kubehenge.example.org", PublicNameResolves: &no},
	} {
		cmd := newApplianceCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := printCertificate(cmd, &v); err != nil || !strings.Contains(out.String(), want) {
			t.Errorf("%+v printed %v\n%s", v, err, out.String())
		}
	}
	f := newFakeInterface(t)
	missing := filepath.Join(t.TempDir(), "none")
	if _, err := f.run(f.args("admin@pve", "certificate", "replace", writeTemp(t, "x"), "--passphrase-file", missing)...); err == nil {
		t.Error("a replace went ahead with a passphrase file that cannot be read")
	}
}
