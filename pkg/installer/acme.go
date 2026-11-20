package installer

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/kubehenge/kubehenge/pkg/acme"
	"github.com/kubehenge/kubehenge/pkg/appliance"
)

const (
	stepACMEName = "acme"
	// acmeWait bounds the wait for the first order. The order goes on after it in the interface,
	// which retries a failure.
	acmeWait = 3 * time.Minute
)

// acmeStatePath is the order's progress in the appliance, which holds no secret.
var acmeStatePath = path.Join(path.Dir(appliance.InterfaceCertificatePath), acme.StateFile)

// CheckACME checks the ACME options as the interface will take them, so that a refusal shows before
// anything is installed.
func (s *Session) CheckACME() error {
	if s.Cfg.ACME.Directory == "" {
		return nil
	}
	return saveACME(&s.Cfg.ACME, s.Cfg.Name, "")
}

// saveACME keeps the ACME options in dir as the interface's setting, for the DNS name the
// certificate is for. With dir empty it only checks them.
func saveACME(f *acme.Files, name, dir string) error {
	if name == "" || net.ParseIP(name) != nil {
		return settingErr(settingCert, "ACME needs the DNS name people open the interface at", "Give it under DNS name or with --name, or choose another certificate")
	}
	req, err := f.Request()
	if err != nil {
		return settingErr(settingCert, err.Error(), "")
	}
	if req.Proof == "" {
		req.Proof = acme.ProofTLSALPN
	}
	m := acme.New(dir)
	m.Name = func() string { return name }
	// The appliance listens on port 80 for the http-01 proof.
	m.HTTPListening = true
	if _, err = m.Save(req); err != nil {
		if refusal, ok := errors.AsType[*acme.RefusalError](err); ok {
			return settingErr(settingCert, lowerFirst(refusal.Message), "")
		}
		return fmt.Errorf("keeping the ACME setting: %w", err)
	}
	return nil
}

// acmeServer names the ACME server of a directory as people know it.
func acmeServer(directory string) string {
	switch directory {
	case acme.DirectoryLetsEncrypt:
		return "Let's Encrypt"
	case acme.DirectoryLetsEncryptStaging:
		return "Let's Encrypt's staging server"
	}
	if u, err := url.Parse(directory); err == nil && u.Host != "" {
		return u.Host
	}
	return directory
}

// acmeCautions say where Let's Encrypt checks the name, which is the internet, and that a name
// pointing at an address DHCP gives cannot be checked before the install.
func (s *Session) acmeCautions() []string {
	a := s.Cfg.ACME
	if a.Directory == "" || a.Proof == acme.ProofDNS {
		return nil
	}
	var out []string
	if !ownServer(a.Directory) {
		port := "443"
		if a.Proof == acme.ProofHTTP {
			port = "80"
		}
		out = append(out, "Let's Encrypt checks "+s.Cfg.Name+" from the internet on port "+port+". Let it reach the appliance there, or choose the DNS proof")
	}
	if len(s.fixedAddresses()) == 0 {
		out = append(out, "ACME orders fail until "+s.Cfg.Name+" points at the address "+s.addressSource()+" gives, shown at the end. The appliance tries again for a day")
	}
	return out
}

// acmeOther is the ACME server choice that asks for a directory's address.
const acmeOther = "other"

// acmeAnswers are the answers of the ACME steps until they are taken.
type acmeAnswers struct {
	server, directory, proof, email, provider, credentials, ca, keyID, hmac string
}

// ownServer reports whether a directory is a server other than Let's Encrypt's, which may need its
// CA trusted and an account binding.
func ownServer(directory string) bool {
	return directory != acme.DirectoryLetsEncrypt && directory != acme.DirectoryLetsEncryptStaging
}

// acmeFields asks the ACME server, the proof and the contact address.
func acmeFields(m *Model) []huh.Field {
	a, f := &m.a.acme, m.S.Cfg.ACME
	a.server, a.proof, a.email = cmp.Or(f.Directory, acme.DirectoryLetsEncrypt), cmp.Or(f.Proof, acme.ProofTLSALPN), f.Email
	if ownServer(a.server) {
		a.server, a.directory = acmeOther, f.Directory
	}
	return []huh.Field{
		choose("ACME server", &a.server,
			huh.NewOption("Let's Encrypt", acme.DirectoryLetsEncrypt),
			huh.NewOption("Let's Encrypt staging", acme.DirectoryLetsEncryptStaging),
			huh.NewOption("Other", acmeOther)),
		choose("Proof", &a.proof,
			huh.NewOption("TLS on port 443", acme.ProofTLSALPN),
			huh.NewOption("HTTP on port 80", acme.ProofHTTP),
			huh.NewOption("A DNS record, through your DNS provider", acme.ProofDNS)),
		input("Contact e-mail", &a.email, "optional", nil),
	}
}

// acmeMoreFields asks what the DNS proof and a server of one's own need.
func acmeMoreFields(m *Model) []huh.Field {
	a, f := &m.a.acme, m.S.Cfg.ACME
	var fields []huh.Field
	if f.Proof == acme.ProofDNS {
		a.provider, a.credentials = f.DNSProvider, f.DNSCredentialsFile
		fields = append(fields,
			input("DNS provider", &a.provider, "its name, such as cloudflare or rfc2136", func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("enter the DNS provider's name")
				}
				return nil
			}),
			input("DNS credentials file", &a.credentials, "NAME=value lines of the provider's variables", nil))
	}
	if ownServer(f.Directory) {
		a.directory, a.ca, a.keyID, a.hmac = f.Directory, f.DirectoryCAFile, f.KeyID, f.HMACKeyFile
		fields = append([]huh.Field{
			input("Directory URL", &a.directory, "the https address of the server's directory", func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("enter the directory's https address")
				}
				return nil
			}),
		}, fields...)
		fields = append(fields,
			input("Server CA file", &a.ca, "PEM, where the server's certificate is not publicly trusted", nil),
			input("Account key ID", &a.keyID, "where the server asks for account binding", nil),
			input("HMAC key file", &a.hmac, "the key the server gave with the key ID", nil))
	}
	return fields
}

// acmeApply takes the answers of an ACME step. It asks next what the DNS proof or a server of one's
// own needs, then checks the setting as the interface will.
func acmeApply(m *Model) tea.Cmd {
	a, f := &m.a.acme, &m.S.Cfg.ACME
	if m.a.certStep == certStepACME {
		if a.takeFirst(f) {
			m.a.certStep = certStepACMEMore
			return m.askCertAgain()
		}
	} else {
		a.takeMore(f)
	}
	if err := m.S.CheckACME(); err != nil {
		m.note = upperFirst(neutral(err.Error()))
		return m.askCertAgain()
	}
	m.a.certStep = certStepChoice
	return nil
}

// takeFirst copies the first form's answers and clears what no longer applies. It reports whether
// the second form has questions left.
func (a *acmeAnswers) takeFirst(f *acme.Files) bool {
	f.Directory, f.Proof, f.Email = a.server, a.proof, strings.TrimSpace(a.email)
	if a.server == acmeOther {
		// The second form asks the directory's address.
		f.Directory = strings.TrimSpace(a.directory)
	}
	if f.Proof != acme.ProofDNS {
		f.DNSProvider, f.DNSCredentialsFile, f.Resolvers = "", "", nil
	}
	if !ownServer(f.Directory) {
		f.DirectoryCAFile, f.KeyID, f.HMACKeyFile = "", "", ""
	}
	return f.Proof == acme.ProofDNS || ownServer(f.Directory)
}

// takeMore copies the second form's answers.
func (a *acmeAnswers) takeMore(f *acme.Files) {
	if f.Proof == acme.ProofDNS {
		f.DNSProvider, f.DNSCredentialsFile = strings.TrimSpace(a.provider), strings.TrimSpace(a.credentials)
	}
	if ownServer(f.Directory) {
		f.Directory = strings.TrimSpace(a.directory)
		f.DirectoryCAFile, f.KeyID, f.HMACKeyFile = strings.TrimSpace(a.ca), strings.TrimSpace(a.keyID), strings.TrimSpace(a.hmac)
	}
}

// putACME writes the ACME setting into the appliance's directory, as the interface keeps it.
func (r *Runner) putACME(ctx context.Context, name string) error {
	dir := filepath.Join(r.Work, "acme")
	if err := os.MkdirAll(dir, permDir); err != nil {
		return fmt.Errorf("making %s: %w", dir, err)
	}
	// The setting holds secrets, so no copy stays behind.
	defer func() { _ = os.RemoveAll(dir) }()
	if err := saveACME(&r.S.Cfg.ACME, name, dir); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, appliance.ACMEFilePrefix+"*"))
	if err != nil {
		return fmt.Errorf("listing the ACME setting: %w", err)
	}
	failed := userErr("copying the ACME setting into container "+r.S.Cfg.CTID+" failed", "See "+r.LogPath)
	chown := []string{"chown", "kubehenge:kubehenge"}
	for _, f := range files {
		to := path.Join(path.Dir(appliance.InterfaceCertificatePath), filepath.Base(f))
		if err = r.push(ctx, f, to, "0600"); err != nil {
			return failed
		}
		chown = append(chown, to)
	}
	if _, err = r.inCT(ctx, chown...); err != nil {
		return failed
	}
	return nil
}

// acmeOutcome is how the first order went within acmeWait: the certificate issued, the order's
// error, or neither while it still runs.
type acmeOutcome struct {
	leaf    *x509.Certificate
	failure string
}

// awaitACME follows the first order until the interface serves its certificate, the order fails, or
// acmeWait passes.
func (r *Runner) awaitACME(ctx context.Context) (acmeOutcome, error) {
	for range int(acmeWait / stepPause) {
		if out, err := r.inCT(ctx, "cat", acmeStatePath); err == nil {
			var st acme.State
			if json.Unmarshal([]byte(out), &st) == nil {
				switch {
				case !st.LastSuccess.IsZero():
					return acmeOutcome{leaf: r.storedCertificate(ctx)}, nil
				case st.Error != "":
					return acmeOutcome{failure: st.Error}, nil
				}
			}
		}
		if err := r.pause(ctx); err != nil {
			return acmeOutcome{}, err
		}
	}
	return acmeOutcome{}, nil
}

// acmeNote says how the first order went, for a run's last lines. It is empty once it issued.
func acmeNote(got acmeOutcome) string {
	switch {
	case got.leaf != nil:
		return ""
	case got.failure != "":
		return "the ACME order failed: " + lowerFirst(got.failure) + ". The interface serves the certificate made here meanwhile and tries again. Follow it under Administration, Certificates"
	}
	return "the ACME order is still running. Follow it under Administration, Certificates"
}

// stepACME gives the running appliance its ACME setting and restarts it, so that it orders its
// certificate. The certificate made here stays until the order succeeds.
func (r *Runner) stepACME(ctx context.Context) error {
	if err := r.putACME(ctx, r.S.Cfg.Name); err != nil {
		return err
	}
	if _, err := r.inCT(ctx, "systemctl", "restart", "kubehenge.service"); err != nil {
		return r.serviceFailure(ctx, "the service did not start again with the ACME setting")
	}
	got, err := r.awaitACME(ctx)
	if err != nil {
		return err
	}
	if got.leaf != nil {
		r.acmeLeaf = got.leaf
		r.finish(stepACMEName, "issued by "+issuerName(got.leaf))
		return nil
	}
	r.warnings = append(r.warnings, acmeNote(got))
	detail := "still ordering"
	if got.failure != "" {
		detail = "order failed"
	}
	r.emit(Event{Step: stepACMEName, State: StepSkipped, Detail: detail})
	return nil
}
