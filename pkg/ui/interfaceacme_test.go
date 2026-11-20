package ui

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/challenge"

	"github.com/kubehenge/kubehenge/pkg/acme"
)

const acmeSecretToken = "dns-token-0123456789abcdef"

var acmeSecretHMAC = base64.RawURLEncoding.EncodeToString([]byte("hello-hmac-key-value"))

type quietProvider struct{}

func (quietProvider) Present(context.Context, string, string, string) error { return nil }
func (quietProvider) CleanUp(context.Context, string, string, string) error { return nil }

// acmeRig is an administered interface, the browser signed in to it, its certificate and the ACME
// manager that orders it.
type acmeRig struct {
	h *harness
	a *browser
	c *InterfaceCertificate
	m *acme.Manager
}

// administeredWithACME is an administered interface whose certificate an ACME manager can order. The
// server it asks is a function that issues for the public name.
func administeredWithACME(t *testing.T) acmeRig {
	t.Helper()
	h, a, c := administered(t)
	m := acme.New(c.dir)
	now := h.clock
	m.Name = func() string {
		u, err := url.Parse(h.server.PublicAddress())
		if err != nil {
			return ""
		}
		return u.Hostname()
	}
	m.HTTPListening = true
	m.Now = func() time.Time { return now }
	m.NewProvider = func(name string, credentials map[string]string) (challenge.Provider, error) {
		if name != "fake" {
			return nil, acme.ErrUnknownProvider
		}
		if credentials["FAKE_TOKEN"] == "" {
			return nil, errors.New("fake: some credentials information is missing: FAKE_TOKEN")
		}
		return quietProvider{}, nil
	}
	m.Obtain = func(_ context.Context, _ *acme.Setting, name string, _ *x509.Certificate) ([]byte, []byte, error) {
		b := issueCertificate(t, now.Add(-time.Hour), now.AddDate(0, 0, 90), []string{name}, certificateAddress.String())
		return b.CertPEM, b.KeyPEM, nil
	}
	m.ARI = func(context.Context, *acme.Setting, *x509.Certificate) (time.Time, error) {
		return time.Time{}, errors.New("no renewal information")
	}
	c.UseACME(m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return acmeRig{h: h, a: a, c: c, m: m}
}

func servedSource(c *InterfaceCertificate) string { return c.served().source }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func acmeView(t *testing.T, rec *httptest.ResponseRecorder, status int) *acme.View {
	t.Helper()
	expect(t, rec, status)
	var v struct {
		ACME *acme.View `json:"acme"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.ACME
}

const stagingRequest = `{"directory":"letsencrypt-staging","proof":"http-01","email":"admin@example.org"}`

// The certificate answer carries the ACME state, saving starts the first order, and an order on
// demand makes another.
func TestACMEIsSetOrderedAndShownThroughTheAPI(t *testing.T) {
	t.Parallel()
	r := administeredWithACME(t)
	a, c := r.a, r.c
	if v := acmeView(t, a.do("GET", "/api/appliance/certificate", ""), http.StatusOK); v != nil {
		t.Fatalf("an interface with no setting shows %+v", v)
	}
	v := acmeView(t, a.do("PUT", "/api/appliance/certificate/acme", stagingRequest), http.StatusAccepted)
	if v == nil || v.Directory == "" || v.Proof != "http-01" || v.Email != "admin@example.org" || v.PublicName != certificateHost {
		t.Fatalf("the answer to a save shows %+v", v)
	}
	waitFor(t, "the first certificate", func() bool { return servedSource(c) == acme.SourceACME })
	first := c.fingerprint()
	waitFor(t, "the issued state", func() bool {
		v = acmeView(t, a.do("GET", "/api/appliance/certificate", ""), http.StatusOK)
		return v != nil && v.State == "issued"
	})
	if v.LastSuccess == nil || v.NextRenewal == nil {
		t.Errorf("an issued setting shows %+v", v)
	}
	if rec := a.do("GET", "/api/appliance/certificate", ""); !strings.Contains(rec.Body.String(), `"source":"acme"`) {
		t.Errorf("the certificate answer says %s", rec.Body)
	}
	acmeView(t, a.do("POST", "/api/appliance/certificate/acme/order", ""), http.StatusAccepted)
	waitFor(t, "the second certificate", func() bool { return c.fingerprint() != first })
}

// Every refusal has the shape {"error","code"} and a status the page can act on.
func TestACMERefusalsHaveACodeAndAStatus(t *testing.T) {
	t.Parallel()
	r := administeredWithACME(t)
	h, a := r.h, r.a
	put := func(body string) *httptest.ResponseRecorder {
		return a.do("PUT", "/api/appliance/certificate/acme", body)
	}
	refusal(t, a.do("POST", "/api/appliance/certificate/acme/order", ""), http.StatusConflict, acme.CodeNotSet)
	refusal(t, put(`{"directory":"letsencrypt","proof":"dns-02"}`), http.StatusUnprocessableEntity, acme.CodeProof)
	refusal(t, put(`{"directory":"http://insecure.example.org","proof":"http-01"}`), http.StatusUnprocessableEntity, acme.CodeDirectory)
	refusal(t, put(`{"directory":"letsencrypt","proof":"dns-01"}`), http.StatusUnprocessableEntity, acme.CodeDNSProvider)
	refusal(t, put(`{"directory":"letsencrypt","proof":"dns-01","dnsProvider":"nosuch"}`), http.StatusUnprocessableEntity, acme.CodeDNSProvider)
	refusal(t, put(`{"directory":"letsencrypt","proof":"dns-01","dnsProvider":"fake"}`), http.StatusUnprocessableEntity, acme.CodeDNSCredential)
	refusal(t, put(`{"directory":"letsencrypt","proof":"http-01","externalAccountBinding":{"keyID":"k"}}`), http.StatusUnprocessableEntity, acme.CodeBinding)
	rec := put(`{"directory":"letsencrypt","proof":"http-01","directoryCA":"nonsense"}`)
	refusal(t, rec, http.StatusUnprocessableEntity, acme.CodeDirectoryCA)
	var v certificateRefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Error == "" {
		t.Errorf("a refusal answers %s", rec.Body)
	}
	if rec = put(`{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON gives %d", rec.Code)
	}
	h.server.PublicURL = "https://192.0.2.40"
	refusal(t, put(stagingRequest), http.StatusConflict, acme.CodeNoPublicName)
	if v := acmeView(t, a.do("GET", "/api/appliance/certificate", ""), http.StatusOK); v != nil {
		t.Errorf("refused requests left a setting: %+v", v)
	}
}

// The providers answer lists the names and says that it lists no variables.
func TestACMEProvidersAnswerListsNamesAndNoVariables(t *testing.T) {
	t.Parallel()
	a := administeredWithACME(t).a
	rec := a.do("GET", "/api/appliance/certificate/acme/providers", "")
	expect(t, rec, http.StatusOK)
	var v struct {
		Providers       []string `json:"providers"`
		VariablesListed bool     `json:"variablesListed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.VariablesListed || len(v.Providers) < 100 || !strings.Contains(strings.Join(v.Providers, " "), "rfc2136") {
		t.Errorf("the answer lists %d providers, variablesListed %v", len(v.Providers), v.VariablesListed)
	}
}

// No answer, log line or stored certificate repeats a secret, and the answer says which are set.
func TestACMESecretsAppearInNoAnswerOrLog(t *testing.T) {
	t.Parallel()
	r := administeredWithACME(t)
	h, a, c := r.h, r.a, r.c
	var logged lockedBuffer
	h.server.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	body := `{"directory":"letsencrypt-staging","proof":"dns-01","dnsProvider":"fake","dnsCredentials":{"FAKE_TOKEN":"` + acmeSecretToken + `"},` +
		`"externalAccountBinding":{"keyID":"kid","hmacKey":"` + acmeSecretHMAC + `"}}`
	seen := make([]string, 0, 3)
	seen = append(seen, a.do("PUT", "/api/appliance/certificate/acme", body).Body.String())
	waitFor(t, "the certificate", func() bool { return servedSource(c) == acme.SourceACME })
	seen = append(seen, a.do("GET", "/api/appliance/certificate", "").Body.String())
	v := acmeView(t, a.do("GET", "/api/appliance/certificate", ""), http.StatusOK)
	if v.ExternalAccountBinding == nil || !v.ExternalAccountBinding.HMACKeySet || len(v.DNSCredentialsSet) != 1 || v.DNSCredentialsSet[0] != "FAKE_TOKEN" {
		t.Errorf("the answer does not say which secrets are set: %+v", v)
	}
	seen = append(seen, a.do("PUT", "/api/appliance/certificate/acme", `{"directory":"letsencrypt","proof":"dns-01","dnsProvider":"fake","dnsCredentials":{"FAKE_TOKEN":""}}`).Body.String())
	for _, text := range append(seen, logged.String()) {
		for _, secret := range []string{acmeSecretToken, acmeSecretHMAC} {
			if strings.Contains(text, secret) {
				t.Errorf("a secret leaks into %.200s", text)
			}
		}
	}
	if !strings.Contains(logged.String(), "interface certificate ordering set") {
		t.Error("saving the setting left no audit line")
	}
}

// Choosing another certificate removes the ACME setting and its secrets, and a change that fails
// leaves them in place.
func TestChoosingAnotherCertificateRemovesTheACMESetting(t *testing.T) {
	t.Parallel()
	r := administeredWithACME(t)
	h, a, c, m := r.h, r.a, r.c, r.m
	save := func() {
		t.Helper()
		acmeView(t, a.do("PUT", "/api/appliance/certificate/acme", stagingRequest), http.StatusAccepted)
		waitFor(t, "the ACME certificate", func() bool {
			return servedSource(c) == acme.SourceACME && m.View() != nil && m.View().State == "issued"
		})
	}
	files := func() int {
		n := 0
		for _, name := range storedFiles(t, c) {
			if strings.HasPrefix(name, "acme-") {
				n++
			}
		}
		return n
	}
	save()
	if files() == 0 {
		t.Fatal("saving kept no file")
	}
	// A change that fails keeps the setting.
	c.files = &failingFiles{step: "write"}
	a.do("POST", "/api/appliance/certificate/self-signed", "")
	c.files = nil
	if m.View() == nil || files() == 0 {
		t.Error("a failed change removed the setting")
	}
	refusal(t, a.upload(filePart("bad.pem", []byte("nonsense"))), http.StatusUnprocessableEntity, "unreadable")
	if m.View() == nil {
		t.Error("a refused upload removed the setting")
	}
	for name, change := range map[string]func() *httptest.ResponseRecorder{
		"an upload": func() *httptest.ResponseRecorder {
			g := goodCertificate(t, h)
			return a.upload(filePart("fullchain.pem", g.CertPEM), filePart("host.key", g.KeyPEM))
		},
		"a self-signed certificate": func() *httptest.ResponseRecorder { return a.do("POST", "/api/appliance/certificate/self-signed", "") },
		"a reset":                   func() *httptest.ResponseRecorder { return a.do("DELETE", "/api/appliance/certificate", "") },
	} {
		save()
		rec := change()
		expect(t, rec, http.StatusOK)
		if m.View() != nil || files() != 0 {
			t.Errorf("%s left %d ACME files", name, files())
		}
		if v := acmeView(t, a.do("GET", "/api/appliance/certificate", ""), http.StatusOK); v != nil {
			t.Errorf("%s left %+v in the answer", name, v)
		}
		if servedSource(c) == acme.SourceACME {
			t.Errorf("%s left the ACME certificate served", name)
		}
	}
}

// A restart serves the ACME certificate again with the order that issued it, so that the loop
// knows the setting's order was served and orders nothing more.
func TestARestartKeepsTheACMECertificateAndItsOrder(t *testing.T) {
	t.Parallel()
	r := administeredWithACME(t)
	h, a, c, m := r.h, r.a, r.c, r.m
	acmeView(t, a.do("PUT", "/api/appliance/certificate/acme", stagingRequest), http.StatusAccepted)
	waitFor(t, "the ACME certificate", func() bool { return servedSource(c) == acme.SourceACME && m.View().State == "issued" })
	restarted := NewInterfaceCertificate(c.dir, c.certFile, c.keyFile)
	restarted.Roots, restarted.Lookup = c.Roots, c.Lookup
	if err := restarted.Load(h.clock, nil); err != nil {
		t.Fatal(err)
	}
	if got := restarted.served(); got.source != acme.SourceACME || got.order == "" || got.order != c.served().order || restarted.fingerprint() != c.fingerprint() {
		t.Errorf("after a restart: source %q, order %q, want %q, %q", got.source, got.order, acme.SourceACME, c.served().order)
	}
}

// An interface that gave up shows a notification, once, until the setting is saved again.
func TestGivingUpOnACMEShowsOneNotification(t *testing.T) {
	t.Parallel()
	r := administeredWithACME(t)
	c, m := r.c, r.m
	var found notifyFound
	c.acme = m
	s := &Server{Certificate: c}
	s.findCertificate(&found)
	if len(found.problems) != 0 {
		t.Fatalf("an interface that never failed shows %v", found.problems)
	}
	// The manager reads its files again at each decision, so the progress a day of failures leaves
	// is written as it would be.
	m.Now = time.Now
	if _, err := m.Save(&acme.Request{Directory: acme.DirectoryLetsEncrypt, Proof: acme.ProofHTTP}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the order to end", func() bool { return m.View().State == "issued" })
	st := `{"error":"the server refused","givenUp":true}`
	if err := os.WriteFile(filepath.Join(c.dir, "acme-state.json"), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	s.findCertificate(&found)
	if len(found.problems) != 1 || found.problems[0].Key != notifyCertificateKey || !strings.Contains(found.problems[0].Text, "the server refused") {
		t.Errorf("a given-up setting shows %+v", found.problems)
	}
}

// The demo answers the ACME endpoints in memory with an issued state for a proof that works and a
// failing state for DNS, and keeps its certificate in its own CA.
func TestDemoAnswersACMEWithAnIssuedAndAFailingState(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	show := func() *acme.View {
		t.Helper()
		rec := b.do("GET", "/api/appliance/certificate", "")
		var v struct {
			Source string     `json:"source"`
			ACME   *acme.View `json:"acme"`
		}
		decodeDemo(t, rec, &v)
		if v.ACME != nil && v.ACME.State == "issued" && v.Source != acme.SourceACME {
			t.Errorf("an issued setting serves a certificate from %q", v.Source)
		}
		return v.ACME
	}
	if show() != nil {
		t.Fatal("the demo starts with ACME set")
	}
	if rec := b.do("PUT", "/api/appliance/certificate/acme", `{"directory":"letsencrypt","proof":"http-01"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("saving: %d %s", rec.Code, rec.Body)
	}
	waitFor(t, "the demo's issued state", func() bool { v := show(); return v != nil && v.State == "issued" })
	rec := b.do("PUT", "/api/appliance/certificate/acme", `{"directory":"letsencrypt","proof":"dns-01","dnsProvider":"cloudflare","dnsCredentials":{"CLOUDFLARE_DNS_API_TOKEN":"`+acmeSecretToken+`"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("saving dns-01: %d %s", rec.Code, rec.Body)
	}
	waitFor(t, "the demo's failing state", func() bool { v := show(); return v != nil && v.State == "failing" })
	if v := show(); v.Error != "T"+demoACMEFailure[1:] || v.NextAttempt == nil || v.DNSProvider != "cloudflare" {
		t.Errorf("the failing state is %+v", v)
	}
	if rec = b.do("GET", "/api/appliance/certificate/acme/providers", ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), acmeSecretToken) {
		t.Errorf("providers: %d %s", rec.Code, rec.Body)
	}
}
