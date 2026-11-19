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
	view := func(source, notServed string) string { return fmt.Sprintf(fakeCertificate, source, notServed) }
	mux.HandleFunc("GET /api/appliance/certificate", f.admin(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, view("installer", "it is expired"))
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
	if _, err = f.run(f.args("admin@pve", "certificate", "replace", "host.pfx")...); err == nil {
		t.Error("a file that is not there was sent")
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
