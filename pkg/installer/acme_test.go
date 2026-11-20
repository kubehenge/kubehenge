package installer

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/acme"
)

const (
	acmeSettingPath = "/var/lib/kubehenge/acme-setting.json"
	acmeStateCat    = "pct exec 100 -- cat /var/lib/kubehenge/acme-state.json"
	storedCertSed   = "pct exec 100 -- sed -n /-BEGIN CERTIFICATE-/,/-END CERTIFICATE-/p /var/lib/kubehenge/interface-certificate.pem"
)

// acmeIssued has the fake appliance report a certificate from ACME, served from its stored file.
func acmeIssued(t *testing.T, h *harness) ca {
	t.Helper()
	issuer := newCA(t, "Test ACME CA")
	h.exec.on(acmeStateCat, `{"delivered":true,"lastSuccess":"2026-11-12T12:00:30Z"}`)
	h.exec.on(storedCertSed, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.cert.Raw})))
	return issuer
}

func indexOf(calls []string, prefix string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// An install with --acme gives the running appliance the setting, restarts it, and ends on the
// certificate the first order issued.
func TestACMEInstallPutsTheSettingAndEndsOnTheIssuedCertificate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	issuer := acmeIssued(t, h)
	cfg := Config{Name: "kubehenge.example.org", PVEEntry: "no", ACME: acme.Files{Directory: "letsencrypt", Email: "admin@example.org", Proof: acme.ProofHTTP}}
	out, err := plainOn(t, h, cfg)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var set acme.Setting
	if err = json.Unmarshal(h.exec.pushed[acmeSettingPath], &set); err != nil {
		t.Fatalf("the setting pushed: %v", err)
	}
	if set.Directory != "https://acme-v02.api.letsencrypt.org/directory" || set.Proof != acme.ProofHTTP || set.Email != "admin@example.org" || set.Generation == "" {
		t.Errorf("the setting is %+v", set)
	}
	var st acme.State
	if err = json.Unmarshal(h.exec.pushed["/var/lib/kubehenge/acme-state.json"], &st); err != nil || st.Want != set.Generation {
		t.Errorf("the state pushed asks for no order: %+v %v", st, err)
	}
	calls := h.exec.calls
	push := indexOf(calls, "pct push 100 "+filepath.Join(h.work, "acme", "acme-setting.json")+" "+acmeSettingPath+" --perms 0600")
	chown := indexOf(calls, "pct exec 100 -- chown kubehenge:kubehenge")
	restart := indexOf(calls, "pct exec 100 -- systemctl restart kubehenge.service")
	start := indexOf(calls, "pct exec 100 -- sh -c")
	if push < 0 || chown < push || restart < chown || !strings.Contains(calls[chown], acmeSettingPath) {
		t.Errorf("push %d, chown %d, restart %d:\n%s", push, chown, restart, strings.Join(calls, "\n"))
	}
	if start > push {
		t.Error("the setting went in before the service first started")
	}
	if _, err = os.Stat(filepath.Join(h.work, "acme")); !os.IsNotExist(err) {
		t.Errorf("a copy of the setting stays on the node: %v", err)
	}
	for _, want := range []string{"ok   ACME certificate: issued by Test ACME CA", "Certificate SHA-256 fingerprint: " + fingerprint(issuer.cert)} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	steps := h.runner(h.session(cfg), nil).Steps()
	ids := make([]string, 0, len(steps))
	for _, s := range steps {
		ids = append(ids, s.ID)
	}
	if i := slices.Index(ids, stepACMEName); i < 1 || ids[i-1] != "service" || ids[i+1] != stepUpdatesName {
		t.Errorf("the steps are %v", ids)
	}
}

// A first order that fails or is still running leaves the install done, with a warning that says so.
func TestACMEOrderThatFailsOrRunsOnWarns(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]string{
		`{"want":"a","error":"Connection refused by 192.0.2.77:80","attempts":1}`: "kubehenge: the ACME order failed: connection refused by 192.0.2.77:80. The interface serves the certificate made here meanwhile and tries again",
		`{"want":"a"}`: "kubehenge: the ACME order is still running. Follow it under Administration, Certificates",
	} {
		h := newHarness(t, opts{dhcp: true})
		h.exec.on(acmeStateCat, state)
		out, err := plainOn(t, h, Config{Name: "kubehenge.example.org", PVEEntry: "no", ACME: acme.Files{Directory: "letsencrypt"}})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(out, want) || !strings.Contains(out, "skip ACME certificate") || !strings.Contains(out, "Kubehenge is running in container 100") {
			t.Errorf("state %s:\n%s", state, out)
		}
	}
}

// The ACME options are checked as the interface takes them before anything changes.
func TestACMEOptionsAreCheckedBeforeAnythingChanges(t *testing.T) {
	t.Parallel()
	creds := filepath.Join(t.TempDir(), "dns.env")
	writeTestFile(t, creds, []byte("RFC2136_NAMESERVER=192.0.2.53\n"))
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{ACME: acme.Files{Directory: "letsencrypt"}}, "ACME needs the DNS name people open the interface at"},
		{Config{Name: "kubehenge.example.org", ACME: acme.Files{Directory: "http://ca.example.org/acme"}}, "the ACME directory is letsencrypt, letsencrypt-staging or an https address"},
		{Config{Name: "kubehenge.example.org", ACME: acme.Files{Directory: "letsencrypt", Proof: acme.ProofDNS}}, "the dns-01 proof needs a DNS provider"},
		{Config{Name: "kubehenge.example.org", ACME: acme.Files{Directory: "letsencrypt", Proof: acme.ProofDNS, DNSProvider: "no-such-provider"}}, "no-such-provider is not a DNS provider"},
		{Config{Name: "kubehenge.example.org", ACME: acme.Files{Directory: "letsencrypt", Proof: acme.ProofDNS, DNSProvider: "rfc2136", DNSCredentialsFile: creds + ".gone"}}, "reading " + creds + ".gone"},
		{Config{Name: "kubehenge.example.org", ACME: acme.Files{Directory: "letsencrypt", Email: "nobody"}}, "e-mail address is not an address"},
		{Config{Name: "kubehenge.example.org", CertFile: "/root/a.pem", ACME: acme.Files{Directory: "letsencrypt"}}, "--acme and --cert exclude each other"},
		{Config{Name: "kubehenge.example.org", ACME: acme.Files{Email: "admin@example.org"}}, "the --acme- options go with --acme"},
		{Config{Certificate: true, NoName: true, ACME: acme.Files{Directory: "letsencrypt"}}, "--acme needs the DNS name, which --no-name drops"},
		{Config{Upgrade: true, ACME: acme.Files{Directory: "letsencrypt"}}, "--acme goes with an install or --certificate"},
	} {
		h := newHarness(t, opts{dhcp: true})
		c.cfg.PVEEntry = "no"
		err := c.cfg.Validate()
		if err == nil {
			_, err = plainOn(t, h, c.cfg)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) || h.exec.changed() != 0 {
			t.Errorf("%+v: %v, %d changes", c.cfg.ACME, err, h.exec.changed())
		}
	}
	h := newHarness(t, opts{dhcp: true})
	cfg := Config{Name: "kubehenge.example.org", PVEEntry: "no", ACME: acme.Files{Directory: "letsencrypt", Proof: acme.ProofDNS, DNSProvider: "rfc2136", DNSCredentialsFile: creds}}
	acmeIssued(t, h)
	if out, err := plainOn(t, h, cfg); err != nil {
		t.Fatalf("a DNS proof with its credentials: %v\n%s", err, out)
	}
	var set acme.Setting
	if err := json.Unmarshal(h.exec.pushed[acmeSettingPath], &set); err != nil || set.DNSCredentials["RFC2136_NAMESERVER"] != "192.0.2.53" {
		t.Errorf("the credentials pushed: %+v %v", set, err)
	}
}

// --certificate with --acme removes the old setting, puts the new one in before the restart, and
// reports the certificate issued.
func TestACMEReplacesTheCertificateOfAnInstalledAppliance(t *testing.T) {
	t.Parallel()
	h, r, out := upgradeHarness(t)
	h.exec.on("pct config 100", "hostname: kubehenge\ntags: kubehenge\nnet0: name=eth0,bridge=vmbr0,ip=192.0.2.40/24\n")
	issuer := acmeIssued(t, h)
	r.S.Cfg.Certificate, r.S.Cfg.Upgrade, r.S.Cfg.Name = true, false, "kubehenge.example.org"
	r.S.Cfg.ACME = acme.Files{Directory: "letsencrypt-staging"}
	res, err := r.ReplaceCertificate(context.Background(), out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	calls := h.exec.calls
	rm := indexOf(calls, "pct exec 100 -- sh -c rm -f /var/lib/kubehenge/acme-*")
	push := indexOf(calls, "pct push 100 "+filepath.Join(h.work, "acme", "acme-setting.json"))
	restart := indexOf(calls, "pct exec 100 -- flock -n -E 75 /var/lib/kubehenge sh -c systemctl restart")
	if rm < 0 || push < rm || restart < push {
		t.Errorf("rm %d, push %d, restart %d:\n%s", rm, push, restart, strings.Join(calls, "\n"))
	}
	if res.Fingerprint != fingerprint(issuer.cert) || !strings.Contains(out.String(), "ok   Certificate from Let's Encrypt's staging server") {
		t.Errorf("the result is %+v:\n%s", res, out)
	}
}

// Saved answers keep the ACME options, and drop them once they fail the checks.
func TestSavedACMERestoresOrIsNamedAsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	answers := filepath.Join(t.TempDir(), "a.json")
	restore := func(cfg Config) *Session {
		if err := saveAnswers(answers, &cfg); err != nil {
			t.Fatal(err)
		}
		s := prepared(t, h, Config{})
		s.Dropped = nil
		s.Restore(context.Background(), loadAnswers(answers))
		return s
	}
	f := acme.Files{Directory: "letsencrypt", Proof: acme.ProofHTTP, Email: "admin@example.org"}
	if s := restore(Config{Name: "kubehenge.example.org", ACME: f}); s.Cfg.ACME.Directory != "letsencrypt" || s.Cfg.ACME.Email != f.Email {
		t.Errorf("restored %+v", s.Cfg.ACME)
	}
	f.Proof = acme.ProofDNS
	if s := restore(Config{Name: "kubehenge.example.org", ACME: f}); s.Cfg.ACME.Directory != "" || !contains(s.Dropped, "certificate") {
		t.Errorf("a setting the interface refuses: %+v %v", s.Cfg.ACME, s.Dropped)
	}
}

// The terminal asks the server, the proof and the address, then what the DNS proof needs. Esc goes
// back a form at a time, and a provider the interface does not know shows its refusal.
func TestACMEQuestionRoad(t *testing.T) {
	t.Parallel()
	u := ownUI(t, newHarness(t, opts{dhcp: true}), Config{Name: "kubehenge.example.org"}, 100, 40)
	u.until("Made here, signed by")
	u.down(2)
	u.enter(1)
	u.waitFor("ACME server")
	for _, want := range []string{"letsencrypt", "TLS on port 443", "HTTP on port 80", "A DNS record", "Contact e-mail"} {
		if !strings.Contains(u.rec.screen(), want) {
			t.Errorf("the ACME form lacks %q:\n%s", want, u.rec.screen())
		}
	}
	u.enter(1)
	u.down(2)
	u.enter(2)
	u.waitFor("DNS provider")
	u.esc()
	u.waitFor("ACME server")
	u.esc()
	u.waitFor("> From an ACME server")
	u.enter(1)
	u.waitFor("> A DNS record")
	u.enter(3)
	u.waitFor("DNS provider")
	u.paste("no-such-provider")
	u.enter(2)
	u.waitFor("No-such-provider is not a DNS provider")
	u.esc()
	u.waitFor("ACME server")
	u.enter(1)
	u.up(2)
	u.enter(2)
	u.until("from Let's Encrypt by ACME, tls-alpn-01")
}

// The repeat command carries every ACME option, and the summary names the server and the proof.
func TestACMERepeatCommandAndRow(t *testing.T) {
	t.Parallel()
	f := acme.Files{ //nolint:gosec // file names and a key ID, no secret.
		Directory: "https://ca.example.org/acme/directory", Email: "admin@example.org", Proof: acme.ProofDNS, DirectoryCAFile: "/root/ca.pem",
		KeyID: "kid", HMACKeyFile: "/root/hmac", Profile: "shortlived", DNSProvider: "rfc2136", DNSCredentialsFile: "/root/dns.env", Resolvers: []string{"192.0.2.53:53"},
	}
	cfg := Config{Name: "kubehenge.example.org", ACME: f}
	got := cfg.RepeatCommand()
	for _, want := range []string{
		"--acme https://ca.example.org/acme/directory", "--acme-email admin@example.org", "--acme-proof dns-01", "--acme-directory-ca-file /root/ca.pem",
		"--acme-eab-key-id kid", "--acme-eab-hmac-key-file /root/hmac", "--acme-profile shortlived", "--acme-dns-provider rfc2136",
		"--acme-dns-credentials-file /root/dns.env", "--acme-resolver 192.0.2.53:53",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	s := &Session{Cfg: cfg}
	if row := s.certText(); row != "from ca.example.org by ACME, dns-01" {
		t.Errorf("the row is %q", row)
	}
	s.Cfg.ACME = acme.Files{Directory: "letsencrypt"}
	if row := s.certText(); row != "from Let's Encrypt by ACME, tls-alpn-01" {
		t.Errorf("the row is %q", row)
	}
	if c := s.acmeCaution(); c != "Let's Encrypt checks kubehenge.example.org from the internet on port 443. Let it reach the appliance there, or choose the DNS proof" {
		t.Errorf("the caution is %q", c)
	}
	s.Cfg.ACME.Proof = acme.ProofDNS
	if c := s.acmeCaution(); c != "" {
		t.Errorf("the DNS proof needs no port, yet: %q", c)
	}
}
