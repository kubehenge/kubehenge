package ui

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"software.sslmate.com/src/go-pkcs12"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/certfiles"
)

const certificateHost = "kubehenge.example.org"

var certificateAddress = netip.MustParseAddr("192.0.2.40")

// issueCertificate makes a certificate that is its own CA for names and ips, valid from notBefore
// to notAfter, with its key.
func issueCertificate(t *testing.T, notBefore, notAfter time.Time, names []string, ips ...string) *certfiles.Bundle {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "test " + serial.String()}, NotBefore: notBefore, NotAfter: notAfter,
		DNSNames: names, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	b, err := bundleOf(tmpl, tmpl, key, key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serveCertificate makes the installer's certificate for names and ips, valid until notAfter, and
// has h's interface serve it from files, with an appliance directory of its own.
func (h *harness) serveCertificate(notAfter time.Time, names []string, ips ...string) *InterfaceCertificate {
	h.t.Helper()
	b := issueCertificate(h.t, h.clock.Add(-24*time.Hour), notAfter, names, ips...)
	dir := h.t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, b.CertPEM, 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, b.KeyPEM, 0o600); err != nil {
		h.t.Fatal(err)
	}
	c := NewInterfaceCertificate(h.t.TempDir(), certFile, keyFile)
	c.Roots = x509.NewCertPool()
	c.Lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{certificateAddress}, nil }
	if err := c.Load(h.clock, nil); err != nil {
		h.t.Fatal(err)
	}
	h.server.Certificate = c
	return c
}

// administered is a harness whose interface serves a certificate for certificateHost, with an
// administrator signed in.
func administered(t *testing.T) (*harness, *browser, *InterfaceCertificate) {
	t.Helper()
	h := newHarness(t)
	c := h.serveCertificate(h.clock.AddDate(2, 0, 0), []string{certificateHost, "kubehenge"}, certificateAddress.String())
	h.server.PublicURL = "https://" + certificateHost
	h.server.OwnAddresses = func() []netip.Addr { return []netip.Addr{certificateAddress} }
	a := h.browser()
	a.signIn("admin@pve")
	return h, a, c
}

func (c *InterfaceCertificate) fingerprint() string {
	return appliance.Fingerprint(c.served().pair.Leaf)
}

// goodCertificate is a certificate for the public name, valid for a year, as files.
func goodCertificate(t *testing.T, h *harness) *certfiles.Bundle {
	t.Helper()
	return issueCertificate(t, h.clock.Add(-time.Hour), h.clock.AddDate(1, 0, 0), []string{certificateHost}, certificateAddress.String())
}

type formPart struct {
	field, name string
	data        []byte
}

func filePart(name string, data []byte) formPart { return formPart{"file", name, data} }

func multipartBody(t *testing.T, preamble int, parts ...formPart) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	// Text before the first boundary is skipped line by line, so it is made of short lines.
	buf.WriteString(strings.Repeat("xxxxxxx\r\n", preamble/9+1))
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		var err error
		var dst io.Writer
		if p.name == "" {
			err = w.WriteField(p.field, string(p.data))
		} else if dst, err = w.CreateFormFile(p.field, p.name); err == nil {
			_, err = dst.Write(p.data)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

// upload sends the files as a browser's form does.
func (b *browser) upload(parts ...formPart) *httptest.ResponseRecorder {
	b.h.t.Helper()
	return b.uploadWith(0, parts...)
}

func (b *browser) uploadWith(preamble int, parts ...formPart) *httptest.ResponseRecorder {
	b.h.t.Helper()
	body, contentType := multipartBody(b.h.t, preamble, parts...)
	req := httptest.NewRequestWithContext(b.h.t.Context(), http.MethodPut, "https://kubehenge.example/api/appliance/certificate", body)
	req.RemoteAddr = "192.0.2.10:50000"
	req.Header.Set("Content-Type", contentType)
	if b.cookie != "" {
		req.Header.Set("Cookie", sessionCookie+"="+b.cookie)
	}
	if b.csrf != "" {
		req.Header.Set(csrfHeader, b.csrf)
	}
	rec := httptest.NewRecorder()
	b.h.handler.ServeHTTP(rec, req)
	return rec
}

// refusal is the code a change was refused with.
func refusal(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var v certificateRefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || rec.Code != status || v.Code != code {
		t.Fatalf("answered %d %s, want %d %s", rec.Code, rec.Body, status, code)
	}
}

func certificateOf(t *testing.T, rec *httptest.ResponseRecorder) uploadView {
	t.Helper()
	expect(t, rec, http.StatusOK)
	var v uploadView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // a path the test made
}

func storedFiles(t *testing.T, c *InterfaceCertificate) []string {
	t.Helper()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// handshake connects to a listener that takes its certificate from get, and returns the
// fingerprint of the one it presents.
func handshake(t *testing.T, get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) string {
	t.Helper()
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		defer func() { _ = server.Close() }()
		_ = tls.Server(server, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: get}).HandshakeContext(t.Context())
	}()
	conn := tls.Client(client, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}) //nolint:gosec // only the presented certificate is read
	if err := conn.HandshakeContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	return appliance.Fingerprint(conn.ConnectionState().PeerCertificates[0])
}

// Both listeners ask one function for their certificate on each handshake, so a change reaches the
// next connection of either with no restart.
func TestBothListenersTakeTheirCertificateFromOneFunction(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	before := c.fingerprint()
	if got := handshake(t, c.GetCertificate); got != before {
		t.Fatalf("the interface listener presents %s, want %s", got, before)
	}
	if got := handshake(t, c.GetCertificate); got != before {
		t.Fatalf("Headlamp's listener presents %s, want %s", got, before)
	}
	uploaded := goodCertificate(t, h)
	expect(t, a.upload(filePart("host.pem", uploaded.CertPEM), filePart("host.key", uploaded.KeyPEM)), http.StatusOK)
	want := appliance.Fingerprint(uploaded.Leaf)
	for _, listener := range []string{"interface", "Headlamp"} {
		if got := handshake(t, c.GetCertificate); got != want || got == before {
			t.Errorf("after the change the %s listener presents %s, want %s", listener, got, want)
		}
	}
	empty := &InterfaceCertificate{}
	if _, err := empty.GetCertificate(nil); err == nil {
		t.Error("a certificate that was never loaded is served")
	}
}

// failingFiles fails one step of a change, or holds the rename until released.
type failingFiles struct {
	step    string
	entered chan struct{}
	release chan struct{}
}

func (f *failingFiles) CreateTemp(dir, pattern string) (certificateFile, error) { //nolint:ireturn // the interface's own seam
	if f.step == "create" {
		return nil, errors.New("no space left")
	}
	inner, err := osFiles{}.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &failingFile{certificateFile: inner, step: f.step}, nil
}

func (f *failingFiles) Rename(oldPath, newPath string) error {
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.release
	}
	if f.step == "rename" {
		return errors.New("read-only file system")
	}
	return osFiles{}.Rename(oldPath, newPath)
}

func (*failingFiles) Remove(name string) error { return osFiles{}.Remove(name) }

type failingFile struct {
	certificateFile

	step string
}

func (f *failingFile) Write(p []byte) (int, error) {
	if f.step == "write" {
		return 0, errors.New("no space left")
	}
	return f.certificateFile.Write(p)
}

func (f *failingFile) Sync() error {
	if f.step == "sync" {
		return errors.New("input/output error")
	}
	return f.certificateFile.Sync()
}

func (f *failingFile) Close() error {
	err := f.certificateFile.Close()
	if f.step == "close" {
		return errors.New("input/output error")
	}
	return err
}

// A certificate is written whole to a temporary file, synced and renamed over the stored one, in a
// file only the service reads. A failure at any step leaves the old file and the old certificate.
func TestACertificateIsWrittenAtomicallyAndAFailureChangesNothing(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	first := goodCertificate(t, h)
	certificateOf(t, a.upload(filePart("a.pem", first.CertPEM), filePart("a.key", first.KeyPEM)))
	if got := storedFiles(t, c); len(got) != 1 || got[0] != appliance.InterfaceCertificateFile {
		t.Fatalf("the appliance directory holds %v after a change, want the one file", got)
	}
	path := filepath.Join(c.dir, appliance.InterfaceCertificateFile)
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("the stored file: %v %v, want mode 0600", info, err)
	}
	stored, err := readFile(path)
	if err != nil || !strings.HasPrefix(string(stored), "# source: uploaded\n") {
		t.Fatalf("the stored file starts %.40q: %v", stored, err)
	}
	serving := c.fingerprint()
	for _, step := range []string{"create", "write", "sync", "close", "rename"} {
		c.files = &failingFiles{step: step}
		next := goodCertificate(t, h)
		refusal(t, a.upload(filePart("b.pem", next.CertPEM), filePart("b.key", next.KeyPEM)), http.StatusInternalServerError, "store")
		if got := c.fingerprint(); got != serving {
			t.Errorf("failing at %s, the interface serves %s, want %s", step, got, serving)
		}
		if got, err := readFile(path); err != nil || !bytes.Equal(got, stored) {
			t.Errorf("failing at %s, the stored file changed: %v", step, err)
		}
		if got := storedFiles(t, c); len(got) != 1 {
			t.Errorf("failing at %s, the appliance directory holds %v", step, got)
		}
	}
}

// A stored certificate is served when it reads cleanly. When it does not, the installer's is served,
// the file stays, and the reason is logged and shown to administrators.
func TestAStoredCertificateIsServedOnlyWhenItReadsCleanly(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	installer := c.fingerprint()
	uploaded := goodCertificate(t, h)
	certificateOf(t, a.upload(filePart("a.pem", uploaded.CertPEM), filePart("a.key", uploaded.KeyPEM)))
	path := filepath.Join(c.dir, appliance.InterfaceCertificateFile)
	good, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expired := issueCertificate(t, h.clock.AddDate(-2, 0, 0), h.clock.AddDate(-1, 0, 0), []string{certificateHost})
	other := goodCertificate(t, h)
	for _, tc := range []struct {
		name string
		file []byte
		want string
	}{
		{"garbage", []byte("not a certificate"), "reading the stored certificate"},
		{"a key without its certificate", uploaded.KeyPEM, "reading the stored certificate"},
		{"a certificate that ran out", append(expired.KeyPEM, expired.CertPEM...), "reading the stored certificate"},
		{"a key of another certificate", append(other.KeyPEM, uploaded.CertPEM...), "reading the stored certificate"},
		{"an empty file", nil, "reading the stored certificate"},
	} {
		if err := os.WriteFile(path, tc.file, 0o600); err != nil {
			t.Fatal(err)
		}
		var logged bytes.Buffer
		next := NewInterfaceCertificate(c.dir, c.certFile, c.keyFile)
		next.Roots = c.Roots
		if err := next.Load(h.clock, slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if next.fingerprint() != installer || next.served().source != certificateInstaller {
			t.Errorf("%s: serves %s from %s, want the installer's", tc.name, next.fingerprint(), next.served().source)
		}
		if !strings.Contains(next.served().notServed, tc.want) || !strings.Contains(logged.String(), tc.want) || !strings.Contains(logged.String(), appliance.InterfaceCertificateFile) {
			t.Errorf("%s: not said why: %q, log %q", tc.name, next.served().notServed, logged.String())
		}
		if got, err := readFile(path); err != nil || !bytes.Equal(got, tc.file) {
			t.Errorf("%s: the file was changed: %v", tc.name, err)
		}
		h.server.Certificate = next
		v := certificateOf(t, a.do("GET", "/api/appliance/certificate", ""))
		if v.NotServed != next.served().notServed || v.Source != certificateInstaller {
			t.Errorf("%s: administrators see %q from %s", tc.name, v.NotServed, v.Source)
		}
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	next := NewInterfaceCertificate(c.dir, c.certFile, c.keyFile)
	next.Roots = c.Roots
	if err := next.Load(h.clock, nil); err != nil || next.fingerprint() != appliance.Fingerprint(uploaded.Leaf) || next.served().notServed != "" {
		t.Errorf("the good stored file is not served: %v %s", err, next.served().notServed)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := next.Load(h.clock, nil); err != nil || next.fingerprint() != installer || next.served().notServed != "" {
		t.Errorf("with no stored file the installer's is served without a reason: %v %q", err, next.served().notServed)
	}
}

// The installer's certificate is served even after it ran out, since an administrator locked out
// by it needs the page to replace it.
func TestAnExpiredInstallerCertificateIsStillServed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.serveCertificate(h.clock.Add(-time.Hour), []string{certificateHost})
	if c.served().source != certificateInstaller || !c.served().pair.Leaf.NotAfter.Before(h.clock) {
		t.Errorf("serves %s ending %s", c.served().source, c.served().pair.Leaf.NotAfter)
	}
}

// Leftovers of a change a crash cut off hold a key, so a start removes them.
func TestAStartRemovesWhatACutOffChangeLeft(t *testing.T) {
	t.Parallel()
	h, _, c := administered(t)
	for _, name := range []string{appliance.InterfaceCertificateFile, appliance.InterfaceCAFile, appliance.PublicURLFile, appliance.HeadlampEnvFile} {
		if err := os.WriteFile(filepath.Join(c.dir, name+".123456"), []byte("key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Load(h.clock, nil); err != nil {
		t.Fatal(err)
	}
	if got := storedFiles(t, c); len(got) != 0 {
		t.Errorf("the appliance directory holds %v", got)
	}
}

// What Setup tells about the certificate follows the one served: its expiry and the names and
// addresses it is for.
func TestTheExpiryAndNamesFollowTheServedCertificate(t *testing.T) {
	t.Parallel()
	h, a, _ := administered(t)
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
	moved := netip.MustParseAddr("192.0.2.41")
	h.server.OwnAddresses = func() []netip.Addr { return []netip.Addr{moved} }
	a.host = moved.String()
	if got := a.do("GET", "/api/setup", "").Body.String(); !strings.Contains(got, `"addressMoved":{"from":["192.0.2.40"],"to":["192.0.2.41"]}`) {
		t.Fatalf("the installer's certificate is for 192.0.2.40, setup = %s", got)
	}
	uploaded := issueCertificate(t, h.clock.Add(-time.Hour), time.Date(2027, 5, 6, 7, 8, 9, 0, time.UTC), []string{certificateHost}, moved.String())
	certificateOf(t, a.upload(filePart("a.pem", uploaded.CertPEM), filePart("a.key", uploaded.KeyPEM)))
	got := a.do("GET", "/api/setup", "").Body.String()
	if !strings.Contains(got, `"certificateExpires":"2027-05-06T07:08:09Z"`) || strings.Contains(got, "addressMoved") {
		t.Errorf("setup after the upload = %s", got)
	}
	names, ips := h.server.certificateCovers()
	if len(names) != 1 || names[0] != certificateHost || len(ips) != 1 || ips[0] != moved {
		t.Errorf("the certificate is for %v %v", names, ips)
	}
}

// A stored certificate comes back after a restart, with the source it had.
func TestAStoredCertificateSurvivesARestart(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	installer := c.fingerprint()
	uploaded := goodCertificate(t, h)
	certificateOf(t, a.upload(filePart("a.pem", uploaded.CertPEM), filePart("a.key", uploaded.KeyPEM)))
	restarted := func() *InterfaceCertificate {
		next := NewInterfaceCertificate(c.dir, c.certFile, c.keyFile)
		next.Roots = x509.NewCertPool()
		if err := next.Load(h.clock.AddDate(0, 0, 1), nil); err != nil {
			t.Fatal(err)
		}
		return next
	}
	if next := restarted(); next.fingerprint() != appliance.Fingerprint(uploaded.Leaf) || next.served().source != certificateUploaded {
		t.Errorf("after a restart: %s from %s", next.fingerprint(), next.served().source)
	}
	if got := certificateOf(t, a.do("POST", "/api/appliance/certificate/self-signed", "")); got.Source != certificateSelfSigned {
		t.Fatalf("source %s", got.Source)
	}
	if next := restarted(); next.served().source != certificateSelfSigned || next.fingerprint() != c.fingerprint() || next.fingerprint() == installer {
		t.Errorf("after a restart the self-signed one is %s from %s", next.fingerprint(), next.served().source)
	}
}

// Administrators see what is served, who it is for and whether the public name leads here.
func TestAdministratorsSeeTheServedCertificate(t *testing.T) {
	t.Parallel()
	_, a, c := administered(t)
	v := certificateOf(t, a.do("GET", "/api/appliance/certificate", ""))
	leaf := c.served().pair.Leaf
	if v.Source != "installer" || v.Fingerprint != appliance.Fingerprint(leaf) || !v.SelfSigned || !v.Changeable || v.PublicURL != "https://"+certificateHost ||
		strings.Join(v.DNSNames, ",") != certificateHost+",kubehenge" || strings.Join(v.IPAddresses, ",") != "192.0.2.40" ||
		v.PublicNameResolves == nil || !*v.PublicNameResolves || len(v.Chain) != 1 || !v.NotAfter.Equal(leaf.NotAfter) || v.Issuer == "" || v.Subject == "" {
		t.Errorf("view = %+v", v)
	}
	if !strings.Contains(v.Fingerprint, ":") || v.Fingerprint != strings.ToUpper(v.Fingerprint) {
		t.Errorf("the fingerprint %s is not uppercase SHA-256 pairs", v.Fingerprint)
	}
	c.Lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.99")}, nil
	}
	if v = certificateOf(t, a.do("GET", "/api/appliance/certificate", "")); v.PublicNameResolves == nil || *v.PublicNameResolves {
		t.Errorf("a public name leading elsewhere says it resolves: %v", v.PublicNameResolves)
	}
}

// An upload is refused, with a code and a sentence, before it changes anything.
func TestAnUploadIsLimitedAndRefusedBeforeItChangesAnything(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	good := goodCertificate(t, h)
	serving := c.fingerprint()
	tooBig := bytes.Repeat([]byte("a"), maxCertificateFile+1)
	many := make([]formPart, maxCertificateFiles+1)
	for i := range many {
		many[i] = filePart("f.pem", good.CertPEM)
	}
	soon := issueCertificate(t, h.clock.Add(-time.Hour), h.clock.Add(12*time.Hour), []string{certificateHost})
	elsewhere := issueCertificate(t, h.clock.Add(-time.Hour), h.clock.AddDate(1, 0, 0), []string{"other.example.org"}, "192.0.2.77")
	ran := issueCertificate(t, h.clock.AddDate(-2, 0, 0), h.clock.AddDate(-1, 0, 0), []string{certificateHost})
	for _, tc := range []struct {
		name   string
		send   func() *httptest.ResponseRecorder
		status int
		code   string
		inText string
	}{
		{"JSON", func() *httptest.ResponseRecorder { return a.do("PUT", "/api/appliance/certificate", `{"a":1}`) }, 415, "not-multipart", "multipart"},
		{"no body", func() *httptest.ResponseRecorder { return a.do("PUT", "/api/appliance/certificate", "") }, 415, "not-multipart", ""},
		{"no files", func() *httptest.ResponseRecorder { return a.upload(formPart{"passphrase", "", []byte("x")}) }, 422, "no-files", "no file"},
		{"an unknown field", func() *httptest.ResponseRecorder { return a.upload(formPart{"other", "", []byte("x")}) }, 400, "malformed", "file and passphrase"},
		{"a long passphrase", func() *httptest.ResponseRecorder {
			return a.upload(filePart("a", good.CertPEM), formPart{"passphrase", "", bytes.Repeat([]byte("p"), maxPassphrase+1)})
		}, 400, "malformed", "4 KiB"},
		{"too many files", func() *httptest.ResponseRecorder { return a.upload(many...) }, 413, "too-many-files", "at most 8"},
		{"a file that is too large", func() *httptest.ResponseRecorder { return a.upload(filePart("huge.pem", tooBig)) }, 413, "file-too-large", "huge.pem is larger than 1 MiB"},
		{"a body that is too large", func() *httptest.ResponseRecorder {
			return a.uploadWith(maxCertificateFiles*maxCertificateFile+uploadOverhead+1, filePart("a", good.CertPEM))
		}, 413, "too-large", "8 files of 1 MiB"},
		{"text", func() *httptest.ResponseRecorder { return a.upload(filePart("notes.txt", []byte("hello"))) }, 422, "unreadable", ""},
		{"a certificate without its key", func() *httptest.ResponseRecorder { return a.upload(filePart("a.pem", good.CertPEM)) }, 422, "unreadable", ""},
		{"a certificate that ran out", func() *httptest.ResponseRecorder {
			return a.upload(filePart("a.pem", ran.CertPEM), filePart("a.key", ran.KeyPEM))
		}, 422, "unreadable", ""},
		{"a certificate with a day left or less", func() *httptest.ResponseRecorder {
			return a.upload(filePart("a.pem", soon.CertPEM), filePart("a.key", soon.KeyPEM))
		}, 422, "expires-soon", "ends on 2026-10-13"},
		{"another name", func() *httptest.ResponseRecorder {
			return a.upload(filePart("a.pem", elsewhere.CertPEM), filePart("a.key", elsewhere.KeyPEM))
		}, 422, "wrong-name", "not valid for kubehenge.example.org. It covers other.example.org, 192.0.2.77"},
	} {
		rec := tc.send()
		refusal(t, rec, tc.status, tc.code)
		if !strings.Contains(rec.Body.String(), tc.inText) {
			t.Errorf("%s: the answer %s lacks %q", tc.name, rec.Body, tc.inText)
		}
		if c.fingerprint() != serving || len(storedFiles(t, c)) != 0 {
			t.Fatalf("%s: changed the certificate or wrote a file: %v", tc.name, storedFiles(t, c))
		}
	}
	rec := a.upload(filePart("a.pem", elsewhere.CertPEM), filePart("a.key", elsewhere.KeyPEM))
	var names certificateRefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &names); err != nil || strings.Join(names.Names, ",") != "other.example.org,192.0.2.77" {
		t.Errorf("a wrong name lists %v: %v", names.Names, err)
	}
}

// An encrypted key or PKCS#12 file needs its passphrase, and a wrong one is refused with its own
// code. Files come in any order, with text before the PEM, and what the chain does not use is
// reported.
func TestAnUploadTakesEncryptedFilesInAnyFormat(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	good := goodCertificate(t, h)
	key, err := x509.ParsePKCS8PrivateKey(pemBlock(t, good.KeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	pfx, err := pkcs12.Modern.Encode(key, good.Leaf, nil, "open sesame")
	if err != nil {
		t.Fatal(err)
	}
	serving := c.fingerprint()
	refusal(t, a.upload(filePart("host.pfx", pfx)), http.StatusUnprocessableEntity, "passphrase")
	rec := a.upload(filePart("host.pfx", pfx), formPart{"passphrase", "", []byte("wrong")})
	refusal(t, rec, http.StatusUnprocessableEntity, "passphrase")
	if strings.Contains(rec.Body.String(), "wrong") && !strings.Contains(rec.Body.String(), "does not open host.pfx") {
		t.Errorf("answer = %s", rec.Body)
	}
	if c.fingerprint() != serving {
		t.Fatal("a refused passphrase changed the certificate")
	}
	v := certificateOf(t, a.upload(filePart("host.pfx", pfx), formPart{"passphrase", "", []byte("open sesame")}))
	if v.Source != certificateUploaded || v.Fingerprint != appliance.Fingerprint(good.Leaf) {
		t.Errorf("a PKCS#12 upload gave %+v", v)
	}
	unused := issueCertificate(t, h.clock.Add(-time.Hour), h.clock.AddDate(1, 0, 0), []string{"unused.example.org"})
	order := []formPart{
		filePart("notes.txt", append([]byte("Bag Attributes\n  friendlyName: host\n"), unused.CertPEM...)),
		filePart("host.key", good.KeyPEM),
		filePart("host.pem", append([]byte("subject=CN=host\n"), good.CertPEM...)),
	}
	v = certificateOf(t, a.upload(order...))
	if v.Fingerprint != appliance.Fingerprint(good.Leaf) || len(v.LeftOut) == 0 || v.LeftOut == nil {
		t.Errorf("files in any order gave %+v", v)
	}
}

func pemBlock(t *testing.T, data []byte) []byte {
	t.Helper()
	start := bytes.Index(data, []byte("-----BEGIN"))
	end := bytes.Index(data, []byte("-----END"))
	if start < 0 || end < 0 {
		t.Fatal("no PEM block")
	}
	body := data[bytes.IndexByte(data[start:], '\n')+start+1 : end]
	der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(body)), ""))
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// A self-signed certificate covers the names and addresses of the one served, lasts the installer's
// 824 days, and is not valid before the hour before it was made.
func TestASelfSignedCertificateCoversTheNamesOfTheServedOne(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	before := c.fingerprint()
	v := certificateOf(t, a.do("POST", "/api/appliance/certificate/self-signed", ""))
	leaf := c.served().pair.Leaf
	if v.Source != certificateSelfSigned || !v.SelfSigned || v.Fingerprint == before || v.Fingerprint != appliance.Fingerprint(leaf) {
		t.Errorf("view = %+v", v)
	}
	if strings.Join(leaf.DNSNames, ",") != certificateHost+",kubehenge" || len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.ParseIP("192.0.2.40")) {
		t.Errorf("the certificate is for %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if want := h.clock.Add(-time.Hour); !leaf.NotBefore.Equal(want.Truncate(time.Second)) || leaf.NotAfter.Sub(leaf.NotBefore) != 824*24*time.Hour {
		t.Errorf("valid %s to %s", leaf.NotBefore, leaf.NotAfter)
	}
	if err := leaf.VerifyHostname(certificateHost); err != nil {
		t.Error(err)
	}
	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || leaf.KeyUsage != x509.KeyUsageDigitalSignature || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("key %T usage %v %v", leaf.PublicKey, leaf.KeyUsage, leaf.ExtKeyUsage)
	}
	// A certificate that names nothing leaves nothing to cover.
	nameless := issueCertificate(t, h.clock.Add(-time.Hour), h.clock.AddDate(1, 0, 0), nil)
	c.live.Store(&servedCertificate{pair: tlsPair(t, nameless), source: certificateInstaller})
	refusal(t, a.do("POST", "/api/appliance/certificate/self-signed", ""), http.StatusUnprocessableEntity, "no-names")
}

func tlsPair(t *testing.T, b *certfiles.Bundle) tls.Certificate {
	t.Helper()
	served, err := newServed(b, certificateInstaller, "")
	if err != nil {
		t.Fatal(err)
	}
	return served.pair
}

// A reset serves the installer's certificate and forgets the stored one, also after a restart.
func TestAResetServesTheInstallersCertificateAndForgetsTheStoredOne(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	installer := c.fingerprint()
	refusal2 := func() {
		if got := storedFiles(t, c); len(got) != 0 {
			t.Errorf("the appliance directory holds %v", got)
		}
	}
	v := certificateOf(t, a.do("DELETE", "/api/appliance/certificate", ""))
	if v.Source != certificateInstaller || v.Fingerprint != installer {
		t.Errorf("a reset with nothing stored gave %+v", v)
	}
	uploaded := goodCertificate(t, h)
	certificateOf(t, a.upload(filePart("a.pem", uploaded.CertPEM), filePart("a.key", uploaded.KeyPEM)))
	v = certificateOf(t, a.do("DELETE", "/api/appliance/certificate", ""))
	if v.Source != certificateInstaller || v.Fingerprint != installer || c.fingerprint() != installer {
		t.Errorf("a reset gave %+v", v)
	}
	refusal2()
	next := NewInterfaceCertificate(c.dir, c.certFile, c.keyFile)
	next.Roots = x509.NewCertPool()
	if err := next.Load(h.clock, nil); err != nil || next.fingerprint() != installer {
		t.Errorf("after a restart: %v %s", err, next.fingerprint())
	}
}

// A reset whose file cannot be removed changes nothing, and serving does not outrun the file.
func TestAResetThatCannotForgetTheFileChangesNothing(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	uploaded := goodCertificate(t, h)
	certificateOf(t, a.upload(filePart("a.pem", uploaded.CertPEM), filePart("a.key", uploaded.KeyPEM)))
	c.files = removeFails{}
	refusal(t, a.do("DELETE", "/api/appliance/certificate", ""), http.StatusInternalServerError, "store")
	if c.fingerprint() != appliance.Fingerprint(uploaded.Leaf) {
		t.Error("the interface serves the installer's while the stored file stays")
	}
}

type removeFails struct{ osFiles }

func (removeFails) Remove(string) error { return errors.New("read-only file system") }

// Changes run one at a time: a second asks to try again while the first is under way, and many at
// once leave the stored file and the certificate served the same.
func TestCertificateChangesAreSerialised(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	hold := &failingFiles{entered: make(chan struct{}), release: make(chan struct{})}
	c.files = hold
	first := goodCertificate(t, h)
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- a.upload(filePart("a.pem", first.CertPEM), filePart("a.key", first.KeyPEM)) }()
	<-hold.entered
	refusal(t, a.do("POST", "/api/appliance/certificate/self-signed", ""), http.StatusConflict, "busy")
	refusal(t, a.do("DELETE", "/api/appliance/certificate", ""), http.StatusConflict, "busy")
	second := goodCertificate(t, h)
	refusal(t, a.upload(filePart("b.pem", second.CertPEM), filePart("b.key", second.KeyPEM)), http.StatusConflict, "busy")
	if got := certificateOf(t, a.do("GET", "/api/appliance/certificate", "")); got.Source != certificateInstaller {
		t.Errorf("a reader sees %s while the change is under way", got.Source)
	}
	close(hold.release)
	certificateOf(t, <-done)
	if c.fingerprint() != appliance.Fingerprint(first.Leaf) {
		t.Error("the first change was lost")
	}
	hold.entered = nil
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			var rec *httptest.ResponseRecorder
			switch i % 4 {
			case 0:
				next := goodCertificate(t, h)
				rec = a.upload(filePart("n.pem", next.CertPEM), filePart("n.key", next.KeyPEM))
			case 1:
				rec = a.do("POST", "/api/appliance/certificate/self-signed", "")
			case 2:
				rec = a.do("DELETE", "/api/appliance/certificate", "")
			default:
				rec = a.do("GET", "/api/appliance/certificate", "")
			}
			if rec.Code != http.StatusOK && rec.Code != http.StatusConflict {
				t.Errorf("change %d answered %d %s", i, rec.Code, rec.Body)
			}
		})
	}
	wg.Wait()
	if len(storedFiles(t, c)) > 1 {
		t.Fatalf("the appliance directory holds %v", storedFiles(t, c))
	}
	stored, err := c.readStored(h.clock)
	switch {
	case err == nil && appliance.Fingerprint(stored.pair.Leaf) != c.fingerprint():
		t.Error("the stored file and the certificate served differ")
	case err != nil && c.served().source != certificateInstaller:
		t.Errorf("serves %s from nothing stored: %v", c.served().source, err)
	}
}

// Only administrators read or change the certificate, and a change needs the CSRF token.
func TestOnlyAdministratorsChangeTheCertificate(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	serving := c.fingerprint()
	good := goodCertificate(t, h)
	form := []formPart{filePart("a.pem", good.CertPEM), filePart("a.key", good.KeyPEM)}
	d := h.browser()
	d.signIn("dev@pve")
	for _, step := range []struct {
		name   string
		send   func(*browser) *httptest.ResponseRecorder
		status int
	}{
		{"show", func(b *browser) *httptest.ResponseRecorder { return b.do("GET", "/api/appliance/certificate", "") }, 0},
		{"upload", func(b *browser) *httptest.ResponseRecorder { return b.upload(form...) }, 0},
		{"self-signed", func(b *browser) *httptest.ResponseRecorder {
			return b.do("POST", "/api/appliance/certificate/self-signed", "")
		}, 0},
		{"reset", func(b *browser) *httptest.ResponseRecorder { return b.do("DELETE", "/api/appliance/certificate", "") }, 0},
	} {
		if rec := step.send(d); rec.Code != http.StatusForbidden {
			t.Errorf("%s by a user who may not set up: %d %s", step.name, rec.Code, rec.Body)
		}
		anon := h.browser()
		if rec := step.send(anon); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s signed out: %d %s", step.name, rec.Code, rec.Body)
		}
		if step.name == "show" {
			continue
		}
		noCSRF := *a
		noCSRF.csrf = ""
		if rec := step.send(&noCSRF); rec.Code != http.StatusForbidden {
			t.Errorf("%s without the CSRF token: %d %s", step.name, rec.Code, rec.Body)
		}
	}
	if c.fingerprint() != serving || len(storedFiles(t, c)) != 0 {
		t.Error("a refused request changed the certificate")
	}
	h.server.Certificate = nil
	if rec := a.do("GET", "/api/appliance/certificate", ""); rec.Code != http.StatusNotFound {
		t.Errorf("without a certificate to manage: %d %s", rec.Code, rec.Body)
	}
	h.server.Certificate = &InterfaceCertificate{}
	if rec := a.do("GET", "/api/appliance/certificate", ""); rec.Code != http.StatusNotFound {
		t.Errorf("with one never loaded: %d %s", rec.Code, rec.Body)
	}
}

// With no appliance directory a certificate has nowhere to be kept, so a change is refused and a
// show says so.
func TestWithoutAnApplianceDirectoryTheCertificateCannotChange(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	c.dir = ""
	if v := certificateOf(t, a.do("GET", "/api/appliance/certificate", "")); v.Changeable {
		t.Error("a certificate with nowhere to be kept says it is changeable")
	}
	good := goodCertificate(t, h)
	refusal(t, a.upload(filePart("a.pem", good.CertPEM), filePart("a.key", good.KeyPEM)), http.StatusConflict, "no-appliance-directory")
	refusal(t, a.do("POST", "/api/appliance/certificate/self-signed", ""), http.StatusConflict, "no-appliance-directory")
	refusal(t, a.do("DELETE", "/api/appliance/certificate", ""), http.StatusConflict, "no-appliance-directory")
}

// No answer, log line or activity entry holds a key or a passphrase, and each change is recorded
// with who made it and the new fingerprint.
func TestNoKeyOrPassphraseIsAnsweredOrLogged(t *testing.T) {
	t.Parallel()
	h, a, c := administered(t)
	good := goodCertificate(t, h)
	key, err := x509.ParsePKCS8PrivateKey(pemBlock(t, good.KeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	const passphrase = "correct horse battery staple"
	pfx, err := pkcs12.Modern.Encode(key, good.Leaf, nil, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	var logged lockedBuffer
	var seen []string
	collect := func(rec *httptest.ResponseRecorder) { seen = append(seen, rec.Body.String()) }
	h.server.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	collect(a.upload(filePart("host.pfx", pfx), formPart{"passphrase", "", []byte("not " + passphrase)}))
	collect(a.upload(filePart("host.pfx", pfx), formPart{"passphrase", "", []byte(passphrase)}))
	collect(a.do("GET", "/api/appliance/certificate", ""))
	collect(a.do("POST", "/api/appliance/certificate/self-signed", ""))
	c.files = &failingFiles{step: "write"}
	collect(a.do("POST", "/api/appliance/certificate/self-signed", ""))
	c.files = nil
	collect(a.do("DELETE", "/api/appliance/certificate", ""))
	keyDER := base64.StdEncoding.EncodeToString(pemBlock(t, good.KeyPEM))
	for _, text := range append(seen, logged.String()) {
		for _, secret := range []string{passphrase, "PRIVATE KEY", keyDER[:40]} {
			if strings.Contains(text, secret) {
				t.Errorf("%q leaks into %.200s", secret, text)
			}
		}
	}
	if got := logged.String(); !strings.Contains(got, "interface certificate replaced") || !strings.Contains(got, `"user":"admin@pve"`) ||
		!strings.Contains(got, appliance.Fingerprint(good.Leaf)) || !strings.Contains(got, "interface certificate made self-signed") || !strings.Contains(got, "set back to the installer") {
		t.Errorf("the changes are not all recorded: %s", got)
	}
	if !strings.Contains(logged.String(), `"level":"ERROR"`) {
		t.Error("a change that could not be kept is not logged as an error")
	}
}

// The activity log shows who changed the certificate, from what and to what, and no secret.
func TestTheActivityLogRecordsCertificateChanges(t *testing.T) {
	t.Parallel()
	h, a, _ := administered(t)
	good := goodCertificate(t, h)
	certificateOf(t, a.upload(filePart("a.pem", good.CertPEM), filePart("a.key", good.KeyPEM)))
	rec := a.do("GET", "/api/activity", "")
	expect(t, rec, http.StatusOK)
	got := rec.Body.String()
	if !strings.Contains(got, "interface certificate replaced") || !strings.Contains(got, "admin@pve") || !strings.Contains(got, appliance.Fingerprint(good.Leaf)) || strings.Contains(got, "PRIVATE KEY") {
		t.Errorf("activity = %s", got)
	}
}

// A swap signs no one out and leaves Headlamp as it is: its sessions and its upstream are not
// touched, and a session made before the swap works after it.
func TestASwapSignsNoOneOutAndLeavesHeadlampAlone(t *testing.T) {
	t.Parallel()
	h, a, _ := administered(t)
	upstream := withHeadlamp(t, h)
	other := h.browser()
	other.signIn("dev@pve")
	headlamp := h.server.Headlamp
	asked := len(*upstream)
	cookies := [2]string{a.cookie, other.cookie}
	good := goodCertificate(t, h)
	certificateOf(t, a.upload(filePart("a.pem", good.CertPEM), filePart("a.key", good.KeyPEM)))
	certificateOf(t, a.do("POST", "/api/appliance/certificate/self-signed", ""))
	certificateOf(t, a.do("DELETE", "/api/appliance/certificate", ""))
	if cookies != [2]string{a.cookie, other.cookie} {
		t.Error("a swap changed a session")
	}
	for _, b := range []*browser{a, other} {
		if rec := b.do("GET", "/api/session", ""); rec.Code != http.StatusOK {
			t.Errorf("after the swaps a session answers %d %s", rec.Code, rec.Body)
		}
	}
	if h.server.Headlamp != headlamp || len(*upstream) != asked {
		t.Errorf("a swap reached Headlamp: %d requests after %d", len(*upstream), asked)
	}
}

// The demo answers the certificate's show and every change in memory, and writes nothing.
func TestTheDemoChangesTheCertificateInMemory(t *testing.T) {
	t.Parallel()
	s, b := newDemo(t)
	b.signIn()
	rec := b.do("GET", "/api/appliance/certificate", "")
	expect(t, rec, http.StatusOK)
	var v uploadView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Source != certificateInstaller || !v.Changeable || v.PublicURL != demoPublicURL || v.PublicNameResolves == nil || !*v.PublicNameResolves || len(v.Chain) != 2 {
		t.Fatalf("demo certificate = %s", rec.Body)
	}
	installer := v.Fingerprint
	if installer != demoFingerprint(t) {
		t.Error("the demo's fingerprint is not the same on every run")
	}
	good := issueCertificate(t, demoAt.Add(-time.Hour), demoAt.AddDate(1, 0, 0), []string{demoHost})
	body, contentType := multipartBody(t, 0, filePart("a.pem", good.CertPEM), filePart("a.key", good.KeyPEM))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "http://localhost/api/appliance/certificate", body)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(b.cookie)
	req.Header.Set(csrfHeader, b.csrf)
	up := httptest.NewRecorder()
	b.handler.ServeHTTP(up, req)
	if got := certificateOf(t, up); got.Source != certificateUploaded || got.Fingerprint != appliance.Fingerprint(good.Leaf) {
		t.Errorf("the demo's upload gave %+v", got)
	}
	if got := certificateOf(t, b.do("POST", "/api/appliance/certificate/self-signed", "")); got.Source != certificateSelfSigned {
		t.Errorf("the demo's self-signed gave %+v", got)
	}
	if got := certificateOf(t, b.do("DELETE", "/api/appliance/certificate", "")); got.Source != certificateInstaller || got.Fingerprint != installer {
		t.Errorf("the demo's reset gave %+v", got)
	}
	if s.Certificate.dir != "" || !s.Certificate.memory {
		t.Errorf("the demo keeps its certificate in %q", s.Certificate.dir)
	}
	if rec := b.do("POST", "/api/appliance/upgrades", `{}`); rec.Code == http.StatusOK {
		t.Error("the demo changes more than its certificate")
	}
}

func demoFingerprint(t *testing.T) string {
	t.Helper()
	c, err := demoCertificate(demoAt)
	if err != nil {
		t.Fatal(err)
	}
	return c.fingerprint()
}
