package acme

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The port 80 listener sends every request that is no challenge to the same path on https, with the
// host and no port, and refuses a challenge nobody is waiting for.
func TestPort80RedirectsToHTTPSAndAnswersOnlyPendingChallenges(t *testing.T) {
	t.Parallel()
	m := newManager(t, "")
	h := m.HTTPHandler()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://kubehenge.example.org:80/admin?x=1", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://kubehenge.example.org/admin?x=1" {
		t.Errorf("a page gives %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://kubehenge.example.org/.well-known/acme-challenge/unknown", http.NoBody))
	if rec.Code == http.StatusOK {
		t.Error("a challenge nobody waits for was answered")
	}
	m.challenges.mu.Lock()
	m.challenges.paths = map[string]string{"/.well-known/acme-challenge/tok": "tok.keyauth"}
	m.challenges.mu.Unlock()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://kubehenge.example.org/.well-known/acme-challenge/tok", http.NoBody))
	if rec.Code != http.StatusOK || rec.Body.String() != "tok.keyauth" {
		t.Errorf("a pending challenge gives %d %q", rec.Code, rec.Body.String())
	}
}

// With no challenge pending the manager adds nothing to a handshake.
func TestNoChallengeCertificateWhileNothingIsPending(t *testing.T) {
	t.Parallel()
	m := newManager(t, "")
	hello := &tls.ClientHelloInfo{ServerName: testHost, SupportedProtos: []string{ALPNProtocol}}
	if m.ALPNPending(hello) || m.ChallengeCertificate(hello) != nil {
		t.Error("a challenge certificate is offered with none pending")
	}
}

// negotiated is the protocol a client that offers protos and names host agrees with the listener.
func negotiated(t *testing.T, m *Manager, host string, protos ...string) (string, error) {
	t.Helper()
	other, _, _ := selfSignedPair(t)
	base := &tls.Config{
		MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if c := m.ChallengeCertificate(h); c != nil {
				return c, nil
			}
			return &other, nil
		},
	}
	base.GetConfigForClient = m.ConfigForClient(base)
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		defer func() { _ = server.Close() }()
		_ = tls.Server(server, base).HandshakeContext(t.Context())
	}()
	conn := tls.Client(client, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, NextProtos: protos, InsecureSkipVerify: true}) //nolint:gosec // only the protocol is read
	if err := conn.HandshakeContext(t.Context()); err != nil {
		return "", err
	}
	return conn.ConnectionState().NegotiatedProtocol, nil
}

func selfSignedPair(t *testing.T) (tls.Certificate, []byte, []byte) {
	t.Helper()
	certPEM, keyPEM := selfSigned(t, time.Now().Add(-time.Hour), time.Hour)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certPEM, keyPEM
}

// acme-tls/1 is negotiated only while a tls-alpn-01 challenge is pending for the name asked for, so
// the interface's own clients negotiate what they would without ACME.
func TestACMEProtocolIsOfferedOnlyWhileAChallengeIsPending(t *testing.T) {
	t.Parallel()
	m := newManager(t, "")
	if proto, err := negotiated(t, m, testHost, "h2"); err != nil || proto != "h2" {
		t.Errorf("a browser negotiates %q (%v) with nothing pending", proto, err)
	}
	if _, err := negotiated(t, m, testHost, ALPNProtocol); err == nil {
		t.Error("acme-tls/1 was negotiated with nothing pending")
	}
	if err := (&alpnProvider{&m.challenges}).Present(t.Context(), testHost, "", "tok.keyauth"); err != nil {
		t.Fatal(err)
	}
	if proto, err := negotiated(t, m, testHost, ALPNProtocol); err != nil || proto != ALPNProtocol {
		t.Errorf("the validator negotiates %q (%v) with a challenge pending", proto, err)
	}
	if proto, err := negotiated(t, m, testHost, "h2", "http/1.1"); err != nil || proto != "h2" {
		t.Errorf("a browser negotiates %q (%v) while a challenge is pending", proto, err)
	}
	if _, err := negotiated(t, m, "other.example.org", ALPNProtocol); err == nil {
		t.Error("acme-tls/1 was negotiated for a name with no challenge")
	}
}
