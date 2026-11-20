package acme

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/challenge"
)

// fakeProvider is a DNS provider that sets nothing.
type fakeProvider struct{}

func (fakeProvider) Present(context.Context, string, string, string) error { return nil }
func (fakeProvider) CleanUp(context.Context, string, string, string) error { return nil }

var fakeProviders ProviderFunc = func(name string, credentials map[string]string) (challenge.Provider, error) {
	if name != "fake" {
		return nil, ErrUnknownProvider
	}
	if credentials["FAKE_TOKEN"] == "" {
		return nil, errors.New("fake: some credentials information is missing: FAKE_TOKEN")
	}
	return fakeProvider{}, nil
}

func newManager(t *testing.T, dir string) *Manager {
	t.Helper()
	m := New(dir)
	m.Name = func() string { return testHost }
	m.HTTPListening = true
	m.NewProvider = fakeProviders
	return m
}

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	refusal, ok := errors.AsType[*RefusalError](err)
	if !ok {
		t.Fatalf("the error is %v, not a refusal", err)
	}
	return refusal.Code
}

// The setting names a directory, a proof and an account, and the view shows it back.
func TestSettingIsKeptAndShownBack(t *testing.T) {
	t.Parallel()
	m := newManager(t, t.TempDir())
	v, err := m.Save(&Request{Directory: DirectoryLetsEncryptStaging, Proof: ProofHTTP, Email: "admin@example.org", Profile: "shortlived"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Directory == "" || v.Proof != ProofHTTP || v.Email != "admin@example.org" || v.Profile != "shortlived" || v.State != "ordering" || v.PublicName != testHost {
		t.Errorf("the view is %+v", v)
	}
	if got := m.View(); got == nil || got.Directory != v.Directory {
		t.Errorf("a later view is %+v", got)
	}
	// The interface reads both lists as arrays, never null.
	if s := stringify(t, v); !strings.Contains(s, `"resolvers":[]`) || !strings.Contains(s, `"dnsCredentialsSet":[]`) {
		t.Errorf("the view's lists are %s", s)
	}
	for req, code := range map[*Request]string{
		{Directory: "http://ca.example.org/dir", Proof: ProofHTTP}:                                          CodeDirectory,
		{Directory: DirectoryLetsEncrypt, Proof: "dns-02"}:                                                  CodeProof,
		{Directory: DirectoryLetsEncrypt, Proof: ProofHTTP, Email: "not an address"}:                        CodeEmail,
		{Directory: DirectoryLetsEncrypt, Proof: ProofHTTP, DirectoryCA: "nonsense"}:                        CodeDirectoryCA,
		{Directory: DirectoryLetsEncrypt, Proof: ProofDNS, Resolvers: []string{"nonsense"}}:                 CodeResolvers,
		{Directory: DirectoryLetsEncrypt, Proof: ProofHTTP, DNSProvider: "fake"}:                            CodeDNSProvider,
		{Directory: DirectoryLetsEncrypt, Proof: ProofHTTP, ExternalAccountBinding: &Binding{KeyID: "kid"}}: CodeBinding,
	} {
		if _, err = m.Save(req); refusalCode(t, err) != code {
			t.Errorf("%+v is refused with %v, want %s", req, err, code)
		}
	}
	m.Name = func() string { return "192.0.2.1" }
	if _, err = m.Save(&Request{Directory: DirectoryLetsEncrypt, Proof: ProofHTTP}); refusalCode(t, err) != CodeNoPublicName {
		t.Errorf("an address as the public name gives %v", err)
	}
	m.Name, m.HTTPListening = func() string { return testHost }, false
	if _, err = m.Save(&Request{Directory: DirectoryLetsEncrypt, Proof: ProofHTTP}); refusalCode(t, err) != CodeNoHTTP {
		t.Errorf("http-01 with no port 80 gives %v", err)
	}
}

const tokenSecret = "tok-very-secret-value-1234"

var hmacSecret = base64.RawURLEncoding.EncodeToString([]byte("secret-hmac-key-value"))

func secretRequest() *Request {
	return &Request{
		Directory: DirectoryLetsEncrypt, Proof: ProofDNS, DNSProvider: "fake", DNSCredentials: map[string]string{"FAKE_TOKEN": tokenSecret},
		ExternalAccountBinding: &Binding{KeyID: "kid", HMACKey: hmacSecret},
	}
}

// The files that hold secrets are readable by their owner only, and no answer, view or log line
// repeats a secret.
func TestSecretsAreKeptPrivateAndNeverShown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var logs bytes.Buffer
	m := newManager(t, dir)
	m.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	v, err := m.Save(secretRequest())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("the directory holds %v (%v)", entries, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "acme-") {
			t.Errorf("%s does not start with acme-, so a reset would leave it", e.Name())
		}
		if runtime.GOOS == "windows" {
			continue
		}
		info, statErr := os.Stat(filepath.Join(dir, e.Name()))
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v (%v)", e.Name(), info.Mode(), statErr)
		}
	}
	m.fail(&Setting{HMACKey: hmacSecret, DNSCredentials: map[string]string{"FAKE_TOKEN": tokenSecret}}, &State{}, errors.New("rejected "+tokenSecret+" and "+hmacSecret))
	shown := m.View()
	if !strings.HasPrefix(shown.Error, "Rejected ") {
		t.Errorf("the error shown is %q, not a sentence", shown.Error)
	}
	if v.ExternalAccountBinding == nil || !v.ExternalAccountBinding.HMACKeySet || len(v.DNSCredentialsSet) != 1 || v.DNSCredentialsSet[0] != "FAKE_TOKEN" {
		t.Errorf("the view does not say which secrets are set: %+v", v)
	}
	for _, text := range []string{logs.String(), stringify(t, v), stringify(t, shown)} {
		if strings.Contains(text, hmacSecret) || strings.Contains(text, tokenSecret) {
			t.Errorf("a secret appears in %q", text)
		}
	}
}

func stringify(t *testing.T, v *View) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A provider the registry does not know is refused with its name, and a known provider missing a
// credential is refused with the provider's own words.
func TestUnknownDNSProviderIsRefused(t *testing.T) {
	t.Parallel()
	m := newManager(t, t.TempDir())
	_, err := m.Save(&Request{Directory: DirectoryLetsEncrypt, Proof: ProofDNS, DNSProvider: "nosuchprovider"})
	if refusalCode(t, err) != CodeDNSProvider || !strings.Contains(err.Error(), "nosuchprovider") || !strings.Contains(err.Error(), "names no variables") {
		t.Errorf("an unknown provider gives %v", err)
	}
	_, err = m.Save(&Request{Directory: DirectoryLetsEncrypt, Proof: ProofDNS, DNSProvider: "fake"})
	if refusalCode(t, err) != CodeDNSCredential || !strings.Contains(err.Error(), "FAKE_TOKEN") {
		t.Errorf("a missing credential gives %v", err)
	}
	_, err = m.Save(&Request{Directory: DirectoryLetsEncrypt, Proof: ProofDNS, DNSProvider: "fake", DNSCredentials: map[string]string{"bad name": "x"}})
	if refusalCode(t, err) != CodeDNSCredential {
		t.Errorf("a variable name with a space gives %v", err)
	}
	if m.View() != nil {
		t.Error("a refused setting was kept")
	}
	// The registry is lego's: it knows rfc2136 and refuses exec, which would run a program.
	names := Providers()
	if !slices.Contains(names, "rfc2136") || !slices.Contains(names, "cloudflare") || slices.Contains(names, "exec") {
		t.Errorf("the registry lists %d providers", len(names))
	}
	for _, name := range names {
		if _, err = legoProvider(name, nil); errors.Is(err, ErrUnknownProvider) {
			t.Errorf("the list names %s, which the registry does not know", name)
		}
	}
	if _, err = legoProvider("exec", nil); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("exec gives %v", err)
	}
}

// A save that leaves a secret empty keeps the one saved for the same provider or key ID, and a
// change of provider drops the old credentials.
func TestSavingAgainKeepsSecretsLeftEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := newManager(t, dir)
	if _, err := m.Save(secretRequest()); err != nil {
		t.Fatal(err)
	}
	again := secretRequest()
	again.DNSCredentials["FAKE_TOKEN"], again.ExternalAccountBinding.HMACKey = "", ""
	if _, err := m.Save(again); err != nil {
		t.Fatal(err)
	}
	set, _, err := m.load()
	if err != nil || set.DNSCredentials["FAKE_TOKEN"] != tokenSecret || set.HMACKey != hmacSecret {
		t.Errorf("the saved setting is %+v (%v)", set, err)
	}
	// The view names no directory CA, so saving the same directory again keeps it, and another
	// directory drops it.
	ca := string(first(selfSigned(t, time.Now(), time.Hour)))
	withCA := &Request{Directory: "https://ca.example.org/dir", Proof: ProofHTTP, DirectoryCA: ca}
	for _, step := range []struct {
		req  *Request
		want string
	}{
		{withCA, ca},
		{&Request{Directory: withCA.Directory, Proof: ProofTLSALPN}, ca},
		{&Request{Directory: DirectoryLetsEncrypt, Proof: ProofHTTP}, ""},
	} {
		if _, err = m.Save(step.req); err != nil {
			t.Fatal(err)
		}
		if set, _, err = m.load(); err != nil || set.DirectoryCA != step.want {
			t.Errorf("after saving %+v the CA is %q (%v)", step.req, set.DirectoryCA, err)
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "acme-setting.json")); errors.Is(err, fs.ErrNotExist) {
		t.Error("the setting is not in the appliance directory")
	}
}
