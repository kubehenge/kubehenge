package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// certificateForm is the files and passphrase of an upload, as the fake interface read them.
type certificateForm struct {
	files      map[string]string
	passphrase string
}

const fakeCertificate = `{"source":"%s","subject":"CN=kubehenge.example.org","dnsNames":["kubehenge.example.org","kubehenge"],
	"ipAddresses":["192.0.2.40"],"issuer":"CN=Example CA","notBefore":"2026-10-12T09:00:00Z","notAfter":"2028-10-13T09:00:00Z",
	"fingerprint":"AB:CD","chain":["CN=kubehenge.example.org","CN=Example CA"],"incomplete":false,"selfSigned":false,
	"publicURL":"https://kubehenge.example.org","publicNameResolves":false,"notServed":"%s","leftOut":["CN=Unused"]}`

// certificateRoutes adds the routes of the interface's certificate: one that shows an installer's
// certificate, and changes that answer with the source they would make.
func (f *fakeInterface) certificateRoutes(mux *http.ServeMux) {
	// signedIn holds mu while a handler runs.
	view := func(source, notServed string) string {
		v := fmt.Sprintf(fakeCertificate, source, notServed)
		if f.ca {
			v = strings.TrimSuffix(v, "}") + `,"applianceCA":{"fingerprint":"EF:01","permits":["kubehenge.example.org","192.0.2.40"],"pem":"CA PEM\n"}}`
		}
		return v
	}
	mux.HandleFunc("GET /api/appliance/certificate", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, view("installer", "it is expired"))
	}))
	mux.HandleFunc("POST /api/appliance/certificate/appliance-ca", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		f.ca = true
		answer(w, http.StatusOK, view("appliance-ca", ""))
	}))
	mux.HandleFunc("PUT /api/appliance/certificate", f.admin(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			answer(w, http.StatusUnsupportedMediaType, `{"error":"Send the files as multipart/form-data","code":"not-multipart"}`)
			return
		}
		f.form = certificateForm{files: map[string]string{}, passphrase: r.FormValue("passphrase")}
		for _, h := range r.MultipartForm.File["file"] {
			part, err := h.Open()
			if err != nil {
				answer(w, http.StatusBadRequest, `{"error":"unreadable"}`)
				return
			}
			content, err := io.ReadAll(part)
			if err != nil {
				answer(w, http.StatusBadRequest, `{"error":"unreadable"}`)
				return
			}
			f.form.files[h.Filename] = string(content)
		}
		if _, ok := f.form.files["host.pfx"]; ok && f.form.passphrase != "right" {
			answer(w, http.StatusUnprocessableEntity, `{"error":"host.pfx is encrypted. Send its passphrase","code":"passphrase"}`)
			return
		}
		answer(w, http.StatusOK, view("uploaded", ""))
	}))
	mux.HandleFunc("POST /api/appliance/certificate/self-signed", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, view("self-signed", ""))
	}))
	mux.HandleFunc("DELETE /api/appliance/certificate", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, view("installer", ""))
	}))
	mux.HandleFunc("PUT /api/appliance/certificate/address", f.admin(func(w http.ResponseWriter, r *http.Request) {
		name, ok := f.bodies[r.Method+" "+r.URL.Path]["name"].(string)
		if !ok || name != "moved.example.org" {
			answer(w, http.StatusUnprocessableEntity, `{"error":"\"`+name+`\" is not a DNS name or an IP address","code":"bad-address"}`)
			return
		}
		answer(w, http.StatusOK, strings.Replace(view("self-signed", ""), "https://kubehenge.example.org", "https://moved.example.org", 1))
	}))
	withACME := func(w http.ResponseWriter, r *http.Request) {
		f.acme = append(f.acme, r.Method+" "+r.URL.Path)
		if f.bodies[r.Method+" "+r.URL.Path]["proof"] == "dns-02" {
			answer(w, http.StatusUnprocessableEntity, `{"error":"The proof is tls-alpn-01, http-01 or dns-01. Give one of these","code":"acme-proof"}`)
			return
		}
		answer(w, http.StatusAccepted, strings.TrimSuffix(view("acme", ""), "}")+`,"acme":`+fakeACME+`}`)
	}
	mux.HandleFunc("PUT /api/appliance/certificate/acme", f.admin(withACME))
	mux.HandleFunc("POST /api/appliance/certificate/acme/order", f.admin(withACME))
	mux.HandleFunc("GET /api/appliance/certificate/acme/providers", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `{"providers":["cloudflare","rfc2136"],"variablesListed":false}`)
	}))
}

const fakeACME = `{"directory":"https://acme.example.org/dir","proof":"dns-01","dnsProvider":"rfc2136","dnsCredentialsSet":["RFC2136_TSIG_SECRET"],
	"resolvers":[],"state":"failing","givenUp":false,"error":"the DNS provider could not add the record","errorAt":"2026-11-20T09:00:00Z",
	"nextAttempt":"2026-11-20T09:02:00Z","lastSuccess":"2026-10-20T09:00:00Z","nextRenewal":"2026-12-20T09:00:00Z"}`

// The acme command sends the setting with its secrets read from files, never from arguments, shows
// the ACME state, orders on demand and lists the DNS providers.
func TestApplianceCertificateACMESetsOrdersAndShowsTheState(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	hmac := namedFile(t, "hmac.txt", "aG1hYy1rZXk\n")
	ca := namedFile(t, "ca.pem", "CA PEM")
	creds := namedFile(t, "dns.env", "# the DNS server\nRFC2136_NAMESERVER=192.0.2.53:53\nRFC2136_TSIG_SECRET=c2VjcmV0=\n\n")
	out, err := f.run(f.args("admin@pve", "certificate", "acme", "--directory", "https://acme.example.org/dir", "--directory-ca-file", ca,
		"--eab-key-id", "kid", "--eab-hmac-key-file", hmac, "--proof", "dns-01", "--dns-provider", "rfc2136", "--dns-credentials-file", creds,
		"--resolver", "192.0.2.53:53", "--email", "admin@example.org")...)
	if err != nil {
		t.Fatal(err)
	}
	const put = "PUT /api/appliance/certificate/acme"
	sent := f.bodies[put]
	if len(f.acme) != 1 || f.acme[0] != put || sent == nil {
		t.Fatalf("the interface got %v %v", f.acme, sent)
	}
	binding, credentials := object(t, sent, "externalAccountBinding"), object(t, sent, "dnsCredentials")
	if sent["directory"] != "https://acme.example.org/dir" || sent["proof"] != "dns-01" || sent["dnsProvider"] != "rfc2136" || sent["directoryCA"] != "CA PEM" ||
		binding["keyID"] != "kid" || binding["hmacKey"] != "aG1hYy1rZXk" || credentials["RFC2136_TSIG_SECRET"] != "c2VjcmV0=" || credentials["RFC2136_NAMESERVER"] != "192.0.2.53:53" ||
		sent["email"] != "admin@example.org" {
		t.Errorf("the request was %v", sent)
	}
	for _, want := range []string{
		"ACME                  failing", "ACME proof            dns-01 through rfc2136", "ACME credentials set  RFC2136_TSIG_SECRET",
		"ACME error            the DNS provider could not add the record (2026-11-20T09:00:00Z)", "ACME retry            2026-11-20T09:02:00Z",
		"ACME next renewal     2026-12-20T09:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
}

// object is the JSON object a request sent under key.
func object(t *testing.T, sent map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := sent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is %v, not an object", key, sent[key])
	}
	return o
}

// With no setting given the acme command sends the defaults. A refusal, an HMAC key as an argument and
// a bad credentials file fail, and order and providers reach the interface.
func TestApplianceCertificateACMEDefaultsRefusalsOrderAndProviders(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	const put = "PUT /api/appliance/certificate/acme"
	out, err := f.run(f.args("admin@pve", "certificate", "acme", "--json")...)
	if err != nil || !json.Valid([]byte(out)) {
		t.Errorf("--json: %v\n%s", err, out)
	}
	if sent := f.bodies[put]; sent["directory"] != "letsencrypt" || sent["proof"] != "tls-alpn-01" || sent["externalAccountBinding"] != nil {
		t.Errorf("the defaults sent %v", sent)
	}
	if _, err = f.run(f.args("admin@pve", "certificate", "acme", "--proof", "dns-02")...); err == nil || !strings.Contains(err.Error(), "Give one of these") {
		t.Errorf("a refusal gives %v", err)
	}
	if _, err = f.run(f.args("admin@pve", "certificate", "acme", "--eab-hmac-key", "secret")...); err == nil {
		t.Error("an HMAC key was taken as an argument")
	}
	if _, err = f.run(f.args("admin@pve", "certificate", "acme", "--dns-credentials-file", namedFile(t, "bad.env", "NOEQUALS\n"))...); err == nil || !strings.Contains(err.Error(), "line 1 is not NAME=value") {
		t.Errorf("a bad credentials file gives %v", err)
	}
	if out, err = f.run(f.args("admin@pve", "certificate", "acme", "order")...); err != nil || f.acme[len(f.acme)-1] != "POST /api/appliance/certificate/acme/order" || !strings.Contains(out, "ACME proof") {
		t.Errorf("order: %v %v\n%s", err, f.acme, out)
	}
	if out, err = f.run(f.args("admin@pve", "certificate", "acme", "providers")...); err != nil || out != "cloudflare\nrfc2136\n" {
		t.Errorf("providers: %v %q", err, out)
	}
}

// namedFile writes content as it is into a file called name.
func namedFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The certificate command shows what the interface serves and changes it with the four actions of
// the page, a passphrase coming from a file and never from an argument.
func TestApplianceCertificateShowsAndChangesWhatTheInterfaceServes(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := f.run(f.args("admin@pve", "certificate")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Source       installer", "Names        kubehenge.example.org, kubehenge, 192.0.2.40", "Valid        2026-10-12 to 2028-10-13", "Fingerprint  AB:CD",
		"Chain        CN=kubehenge.example.org > CN=Example CA", "Public URL   https://kubehenge.example.org, does not resolve to this appliance",
		"Not served   the stored certificate, since it is expired",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if out, err = f.run(f.args("admin@pve", "certificate", "--json")...); err != nil || !json.Valid([]byte(out)) || !strings.Contains(out, `"fingerprint":"AB:CD"`) {
		t.Errorf("--json: %v\n%s", err, out)
	}
	pem, key, secret := namedFile(t, "host.pem", "CERT"), namedFile(t, "host.key", "KEY"), writeTemp(t, "right")
	out, err = f.run(f.args("admin@pve", "certificate", "replace", pem, key)...)
	if err != nil || !strings.Contains(out, "Source       uploaded") || !strings.Contains(out, "Left out") || !strings.Contains(out, "CN=Unused") {
		t.Fatalf("replace: %v\n%s", err, out)
	}
	if got := f.form.files[filepath.Base(pem)] + f.form.files[filepath.Base(key)]; got != "CERTKEY" || len(f.form.files) != 2 || f.form.passphrase != "" {
		t.Errorf("the interface got %v with passphrase %q", f.form.files, f.form.passphrase)
	}
	if _, err = f.run(f.args("admin@pve", "certificate", "replace", "host.pfx")...); err == nil || err.Error() != "host.pfx does not exist. Give the path of an existing file" {
		t.Errorf("a file that is not there: %v", err)
	}
	pfx := namedFile(t, "host.pfx", "PFX")
	if _, err = f.run(f.args("admin@pve", "certificate", "replace", pfx)...); err == nil || !strings.Contains(err.Error(), "host.pfx is encrypted. Send its passphrase") {
		t.Errorf("an encrypted file without its passphrase: %v", err)
	}
	if out, err = f.run(f.args("admin@pve", "certificate", "replace", pfx, "--passphrase-file", secret, "--json")...); err != nil || f.form.passphrase != "right" || !strings.Contains(out, `"source":"uploaded"`) {
		t.Errorf("with its passphrase: %v %q\n%s", err, f.form.passphrase, out)
	}
	if _, err = f.run(f.args("admin@pve", "certificate", "replace", pfx, "--passphrase", "right")...); err == nil {
		t.Error("a passphrase is taken as an argument")
	}
	if out, err = f.run(f.args("admin@pve", "certificate", "self-sign")...); err != nil || !strings.Contains(out, "Source       self-signed") {
		t.Errorf("self-sign: %v\n%s", err, out)
	}
	if out, err = f.run(f.args("admin@pve", "certificate", "reset")...); err != nil || !strings.Contains(out, "Source       installer") {
		t.Errorf("reset: %v\n%s", err, out)
	}
	if _, err = f.run(f.args("admin@pve", "certificate", "replace")...); err == nil || !strings.Contains(err.Error(), "give the certificate files") {
		t.Errorf("replace without files: %v", err)
	}
}

// address sends the name as it is given and shows the address the interface answers with, or its
// refusal.
func TestApplianceCertificateAddressChangesThePublicAddress(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := f.run(f.args("admin@pve", "certificate", "address", "moved.example.org")...)
	if err != nil || !strings.Contains(out, "Public URL   https://moved.example.org") {
		t.Errorf("address: %v\n%s", err, out)
	}
	if _, err = f.run(f.args("admin@pve", "certificate", "address", "bank example")...); err == nil || !strings.Contains(err.Error(), `"bank example" is not a DNS name`) {
		t.Errorf("a refused name: %v", err)
	}
	if _, err = f.run(f.args("admin@pve", "certificate", "address")...); err == nil || !strings.Contains(err.Error(), "give the DNS name or IP address") {
		t.Errorf("without a name: %v", err)
	}
}

// appliance-ca has the interface sign with the appliance's own CA, and ca prints that CA to trust,
// or says there is none yet.
func TestApplianceCertificateSignsWithTheAppliancesCAAndPrintsIt(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if _, err := f.run(f.args("admin@pve", "certificate", "ca")...); err == nil || !strings.Contains(err.Error(), "the appliance has no CA of its own") {
		t.Errorf("ca without one: %v", err)
	}
	out, err := f.run(f.args("admin@pve", "certificate", "appliance-ca")...)
	if err != nil || !strings.Contains(out, "Source        appliance-ca") || !strings.Contains(out, "Appliance CA  EF:01, for kubehenge.example.org, 192.0.2.40") {
		t.Errorf("appliance-ca: %v\n%s", err, out)
	}
	if out, err = f.run(f.args("admin@pve", "certificate", "ca")...); err != nil || out != "CA PEM\n" {
		t.Errorf("ca: %v %q", err, out)
	}
}

func TestApplianceCertificateIsRefusedToWhoMayNotSetUp(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	for _, args := range [][]string{{"certificate"}, {"certificate", "self-sign"}, {"certificate", "reset"}, {"certificate", "replace", namedFile(t, "host.pem", "CERT")}} {
		if _, err := f.run(f.args("user@pve", args...)...); err == nil || !strings.Contains(err.Error(), "needs Sys.Modify on /") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if len(f.form.files) != 0 {
		t.Errorf("an upload by a user who may not set up was read: %v", f.form.files)
	}
}
