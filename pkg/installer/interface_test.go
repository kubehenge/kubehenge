package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/smallstep/pkcs7"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/kubehenge/kubehenge/pkg/certfiles"
)

// testNow is the clock of the certificate tests, which the runner's clock matches.
var testNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func atTestNow() time.Time { return testNow }

// ownPKI makes the certificates and files an administrator brings.
type ownPKI struct {
	dir string
	ca  ca
}

func newOwnPKI(t *testing.T) *ownPKI {
	t.Helper()
	return &ownPKI{dir: t.TempDir(), ca: newCA(t, "Example Issuing CA")}
}

// leafSpec is a leaf certificate: its names and how many days it lasts from testNow.
type leafSpec struct {
	cn       string
	dns, ips []string
	days     int
}

func (p *ownPKI) leaf(t *testing.T, s leafSpec) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: newSerial(t), Subject: pkix.Name{CommonName: s.cn},
		NotBefore: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: testNow.Add(time.Duration(s.days) * 24 * time.Hour),
		DNSNames: s.dns, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, ip := range s.ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca.cert, &key.PublicKey, p.ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func keyPEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func (p *ownPKI) write(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(p.dir, name)
	writeTestFile(t, path, data)
	return path
}

// pair writes a leaf's certificate and key as two PEM files and returns their paths.
func (p *ownPKI) pair(t *testing.T, name string, s leafSpec) (string, string) {
	t.Helper()
	cert, key := p.leaf(t, s)
	return p.write(t, name+".pem", encodeCerts(cert)), p.write(t, name+".key", keyPEM(t, key))
}

// pfx writes a leaf with its key and the issuing CA as a PKCS#12 file under the passphrase.
func (p *ownPKI) pfx(t *testing.T, name, pass string, s leafSpec) string {
	t.Helper()
	cert, key := p.leaf(t, s)
	data, err := pkcs12.Modern.Encode(key, cert, []*x509.Certificate{p.ca.cert}, pass)
	if err != nil {
		t.Fatal(err)
	}
	return p.write(t, name, data)
}

var corpName = leafSpec{cn: "kubehenge.example.org", dns: []string{"kubehenge.example.org"}, days: 400}

// ownSession is a prepared session for the harness, on the test clock, with the options given.
func ownSession(t *testing.T, h *harness, cfg Config) *Session { //nolint:gocritic // tests pass a Config literal.
	t.Helper()
	s := prepared(t, h, cfg)
	s.Now = atTestNow
	return s
}

// until presses Enter on each question until the screen holds the text.
func (u *ui) until(want string) {
	u.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for presses := 0; presses < 20 && time.Now().Before(deadline); {
		switch screen := u.rec.screen(); {
		case strings.Contains(screen, want):
			u.settle()
			return
		case strings.Contains(screen, "enter "):
			u.send(keyUnless{tea.KeyPressMsg{Code: tea.KeyEnter}, want}, "enter")
			presses++
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	u.waitFor(want)
	u.settle()
}

// paste types a whole text as one key, as a terminal's paste does.
func (u *ui) paste(s string) {
	u.t.Helper()
	u.press(tea.KeyPressMsg{Code: rune(s[0]), Text: s})
}

func (u *ui) esc() { u.press(tea.KeyPressMsg{Code: tea.KeyEscape}) }

// ownUI starts the terminal installer on the DHCP road with the test clock.
func ownUI(t *testing.T, h *harness, cfg Config, w, l int) *ui { //nolint:gocritic // tests pass a Config literal.
	t.Helper()
	cfg.PVEEntry = "no"
	return startUIWith(t, h, cfg, w, l, func(s *Session) { s.Now = atTestNow })
}

// toFiles opens the files form of the certificate question.
func (u *ui) toFiles() {
	u.t.Helper()
	u.until("Made here, signed by")
	u.down(1)
	u.enter(1)
	u.waitFor("Certificate files")
}

// 1. The Interface step sits between Network and Storage in the header, which fits 80 columns.
func TestInterfaceStepSitsBetweenNetworkAndStorage(t *testing.T) {
	t.Parallel()
	at := -1
	for i, n := range crumbNames {
		if n == "Interface" {
			at = i
		}
	}
	if at != crumbInterface || crumbNames[at-1] != "Network" || crumbNames[at+1] != "Storage" {
		t.Fatalf("the crumbs are %v, Interface at %d", crumbNames, at)
	}
	m := NewModel(&Session{}, nil)
	m.width = 80
	for _, id := range []string{settingName, settingCert} {
		m.stage, m.asking = stAsk, id
		if m.crumb() != crumbInterface {
			t.Errorf("%s is not under Interface", id)
		}
	}
	for l := range strings.SplitSeq(m.header(), "\n") {
		if w := ansi.StringWidth(l); w > 80 {
			t.Errorf("the header is %d columns: %q", w, l)
		}
	}
}

// 2. The name question follows DNS, is asked unless an option or a saved answer gave it, and its
// placeholder says what empty opens.
func TestNameQuestionRoadAndPlaceholder(t *testing.T) {
	t.Parallel()
	order := func(id string) int {
		for i, s := range settingOrder {
			if s == id {
				return i
			}
		}
		return -1
	}
	if order(settingName) != order(settingDNS)+1 {
		t.Fatalf("the name does not follow DNS: %v", settingOrder)
	}
	h := newHarness(t, opts{dhcp: true})
	for _, c := range []struct {
		cfg   Config
		asked bool
	}{{Config{}, true}, {Config{Name: "kubehenge.example.org"}, false}, {Config{NoName: true, Certificate: true}, false}, {Config{Yes: true}, false}, {Config{DryRun: true}, false}} {
		m := NewModel(prepared(t, h, c.cfg), nil)
		m.initFromOptions()
		if got := m.needed(settingName); got != c.asked {
			t.Errorf("%+v: asked %v, want %v", c.cfg, got, c.asked)
		}
	}
	u := ownUI(t, h, Config{}, 100, 40)
	u.until("DNS name of the interface")
	if !strings.Contains(u.rec.screen(), "empty opens the address DHCP gives") {
		t.Errorf("DHCP placeholder:\n%s", u.rec.screen())
	}
	static := ownUI(t, newHarness(t, opts{}), Config{IP: "192.0.2.10/24", Gateway: "192.0.2.1"}, 100, 40)
	static.until("DNS name of the interface")
	if !strings.Contains(static.rec.screen(), "empty opens https://192.0.2.10") {
		t.Errorf("static placeholder:\n%s", static.rec.screen())
	}
}

// 3. Pasted input becomes the name, and the rules are refused in the form with an example.
func TestNameInputIsNormalisedAndRefused(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"https://kubehenge.example.org/": "kubehenge.example.org", "\tKubeHenge.Example.ORG": "kubehenge.example.org", "": ""} {
		if got, err := normaliseName(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for in, rule := range map[string]string{
		"kubehenge.example.org:8443": "without a port", "kubehenge.example.org/ui": "without a path", "kube henge.example.org": "without spaces",
		"192.0.2.10": "not an IP address", "[2001:db8::10]": "not an IP address", "kubehenge": "enter a DNS name",
	} {
		_, err := normaliseName(in)
		if err == nil || !strings.Contains(err.Error(), rule) || !strings.Contains(err.Error(), "kubehenge.example.org") {
			t.Errorf("%q: %v, want the rule %q with an example", in, err, rule)
		}
	}
	u := ownUI(t, newHarness(t, opts{dhcp: true}), Config{}, 100, 40)
	u.until("DNS name of the interface")
	u.paste("https://Kubehenge.Example.org:8443/")
	u.enter(1)
	u.waitFor("without a port")
	if !strings.Contains(u.rec.screen(), "DNS name of the interface") {
		t.Errorf("a name with a port left the form:\n%s", u.rec.screen())
	}
}

// 4. The lookup runs beside the interface, and keeps one of four outcomes on the session.
func TestNameLookupHasFourOutcomesAndDoesNotBlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true, hosts: map[string][]string{
		"here.example.org": {"192.0.2.40"}, "there.example.org": {"198.51.100.9"}, "six.example.org": {"2001:db8::40"},
	}})
	check := func(name string, cfg Config, want NameState, found string) {
		t.Helper()
		cfg.Name = name
		s := h.session(cfg)
		s.CheckName(context.Background())
		if s.NameCheck.State != want || strings.Join(s.NameCheck.Found, ",") != found {
			t.Errorf("%s with %q %q: %v %v, want %v %q", name, cfg.IP, cfg.IP6, s.NameCheck.State, s.NameCheck.Found, want, found)
		}
	}
	v4 := Config{IP: "192.0.2.40/24"}
	check("here.example.org", v4, NameMatches, "")
	check("six.example.org", Config{IP: "none", IP6: "2001:db8::40/64"}, NameMatches, "")
	check("there.example.org", v4, NameElsewhere, "198.51.100.9")
	check("none.example.org", v4, NameMissing, "")
	check("here.example.org", Config{IP: "dhcp"}, NameUnknown, "192.0.2.40")
	check("none.example.org", Config{IP: "dhcp"}, NameMissing, "")
	failing := newHarness(t, opts{lookupErr: errors.New("server failure")})
	s := failing.session(Config{IP: "192.0.2.40/24", Name: "x.example.org"})
	s.CheckName(context.Background())
	if s.NameCheck.State != NameUnknown {
		t.Errorf("a failed lookup is %v", s.NameCheck.State)
	}

	release := make(chan struct{})
	slow := newHarness(t, opts{})
	slow.node.LookupHost = func(_ context.Context, name string) ([]string, error) {
		if name == "slow.example.org" {
			<-release
		}
		return []string{"192.0.2.10"}, nil
	}
	u := ownUI(t, slow, Config{Name: "slow.example.org", IP: "192.0.2.10/24", Gateway: "192.0.2.1"}, 100, 40)
	time.AfterFunc(500*time.Millisecond, func() { close(release) })
	u.until("If it fails")
	if !strings.Contains(u.rec.screen(), "resolves to the container") {
		t.Errorf("the summary after the slow lookup:\n%s", u.rec.screen())
	}
}

// 5. The result shown is the one for the name and address the options hold now.
func TestNameResultNeverGoesStale(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true, hosts: map[string][]string{"here.example.org": {"192.0.2.40"}}})
	s := ownSession(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", Name: "here.example.org"})
	s.CheckName(context.Background())
	if !strings.Contains(s.interfaceText(), "resolves to the container") {
		t.Fatalf("a fresh check: %s", s.interfaceText())
	}
	s.Cfg.IP = "192.0.2.41/24"
	if !strings.Contains(s.interfaceText(), "DNS not checked") {
		t.Errorf("a check for another address: %s", s.interfaceText())
	}
	s.CheckName(context.Background())
	if !strings.Contains(s.interfaceText(), "resolves to 192.0.2.40") {
		t.Errorf("the check after the address changed: %s", s.interfaceText())
	}
	s.Cfg.Name = "gone.example.org"
	if !strings.Contains(s.interfaceText(), "DNS not checked") {
		t.Errorf("a check for another name: %s", s.interfaceText())
	}

	u := ownUI(t, h, Config{Name: "here.example.org", IP: "192.0.2.40/24", Gateway: "192.0.2.1"}, 100, 40)
	u.until("If it fails")
	if !strings.Contains(u.rec.screen(), "resolves to the container") {
		t.Fatalf("the summary lacks the result:\n%s", u.rec.screen())
	}
	u.press(tea.KeyPressMsg{Code: tea.KeyRight})
	u.enter(1)
	u.waitFor("enter change")
	m := u.rec.inner
	at := 0
	for _, id := range settingList {
		if id == settingName {
			break
		}
		if m.listed(id) {
			at++
		}
	}
	u.down(at)
	u.enter(1)
	u.waitFor("DNS name of the interface")
	for range len("here.example.org") {
		u.press(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	u.typed("gone.example.org")
	u.enter(1)
	u.waitFor("If it fails")
	if screen := u.rec.screen(); !strings.Contains(screen, "no DNS record") || strings.Contains(screen, "resolves to the container") {
		t.Errorf("the summary after the name changed:\n%s", screen)
	}
}

// 6. The summary always has an Interface row, and no DNS name row.
func TestSummaryInterfaceRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true, hosts: map[string][]string{"here.example.org": {"192.0.2.40"}}})
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", Name: "here.example.org"}, "https://here.example.org (resolves to the container)"},
		{Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1"}, "https://192.0.2.40"},
		{Config{IP: "none", IP6: "2001:db8::40/64", Gateway6: "2001:db8::1"}, "https://[2001:db8::40]"},
		{Config{IP: "dhcp"}, "the address comes from DHCP"},
	} {
		s := ownSession(t, h, c.cfg)
		s.CheckName(context.Background())
		var got string
		for _, r := range s.Rows(true) {
			if r.Label == "DNS name" {
				t.Errorf("%+v: the old DNS name row is back", c.cfg)
			}
			if r.Label == "Interface" {
				got = r.Value
			}
		}
		if got != c.want {
			t.Errorf("%+v: Interface %q, want %q", c.cfg, got, c.want)
		}
	}
}

// 7. No record and another address each add a caution, in the summary, the plain output and the
// dry run.
func TestDNSCautionsAreInEveryOutput(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{hosts: map[string][]string{"there.example.org": {"198.51.100.9"}}})
	for name, want := range map[string]string{
		"gone.example.org":  "gone.example.org has no DNS record. Point it at 192.0.2.40 in your DNS",
		"there.example.org": "there.example.org resolves to 198.51.100.9, not 192.0.2.40. Point it at 192.0.2.40 in your DNS",
	} {
		cfg := Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", Name: name, PVEEntry: "no", DryRun: true}
		s := h.session(cfg)
		var out bytes.Buffer
		if err := RunPlain(context.Background(), s, func(emit func(Event)) *Runner { return h.runner(s, emit) }, &out); err != nil {
			t.Fatalf("%v\n%s", err, &out)
		}
		if !strings.Contains(out.String(), "kubehenge: "+want) || !strings.Contains(out.String(), "Dry run") {
			t.Errorf("the dry run lacks %q:\n%s", want, &out)
		}
		m := NewModel(s, nil)
		m.width, m.height = 100, 40
		m.stage = stSummary
		if !strings.Contains(ansi.Strip(m.summaryScreen()), strings.SplitN(want, ". ", 2)[0]) {
			t.Errorf("the summary lacks the caution for %s:\n%s", name, m.summaryScreen())
		}
	}
}

// 8. A name that does not lead to the appliance gets its address beside it on the end screens.
func TestEndGivesTheAddressWhereTheNameDoesNotResolve(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	out, err := plainOn(t, h, Config{Name: "gone.example.org", PVEEntry: "no"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Also at https://192.0.2.77/") {
		t.Errorf("the plain end lacks the address:\n%s", out)
	}
	good := newHarness(t, opts{dhcp: true, hosts: map[string][]string{"here.example.org": {"192.0.2.77"}}})
	if out, err = plainOn(t, good, Config{Name: "here.example.org", PVEEntry: "no"}); err != nil || strings.Contains(out, "Also at") {
		t.Errorf("a name that resolves: %v\n%s", err, out)
	}
	u := ownUI(t, h, Config{Name: "gone.example.org"}, 100, 40)
	u.until("If it fails")
	u.enter(1)
	u.waitFor("Kubehenge is running")
	if screen := u.rec.screen(); !strings.Contains(screen, "Also at") || !strings.Contains(screen, "https://192.0.2.77/") {
		t.Errorf("the end screen lacks the address:\n%s", screen)
	}
}

// 9. The certificate question follows the name, offers two choices, and is asked unless --cert or a
// saved answer gave it.
func TestCertificateQuestionRoad(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	certPath, keyPath := pki.pair(t, "own", corpName)
	h := newHarness(t, opts{dhcp: true})
	idx := func(id string) int {
		for i, s := range settingOrder {
			if s == id {
				return i
			}
		}
		return -1
	}
	if idx(settingCert) != idx(settingName)+1 {
		t.Fatalf("the certificate does not follow the name: %v", settingOrder)
	}
	for _, c := range []struct {
		cfg   Config
		asked bool
	}{{Config{}, true}, {Config{CertFile: certPath, KeyFile: keyPath}, false}, {Config{Yes: true}, false}} {
		m := NewModel(prepared(t, h, c.cfg), nil)
		m.initFromOptions()
		if got := m.needed(settingCert); got != c.asked {
			t.Errorf("%+v: asked %v, want %v", c.cfg, got, c.asked)
		}
	}
	u := ownUI(t, h, Config{}, 100, 40)
	u.until("Made here, signed by")
	for _, want := range []string{"Made here, signed by this cluster's CA", "Files of your own"} {
		if !strings.Contains(u.rec.screen(), want) {
			t.Errorf("the choice lacks %q:\n%s", want, u.rec.screen())
		}
	}
}

// 10. A homelab node preselects the made certificate and shows no note.
func TestHomelabNodePreselectsTheMadeCertificate(t *testing.T) {
	t.Parallel()
	u := ownUI(t, newHarness(t, opts{dhcp: true}), Config{}, 100, 40)
	u.until("Made here, signed by")
	if screen := u.rec.screen(); strings.Contains(screen, "API certificate comes from") {
		t.Errorf("a note on a homelab node:\n%s", screen)
	}
	u.enter(1)
	u.until("If it fails")
	if !strings.Contains(u.rec.screen(), "made here, signed by this cluster's CA") || strings.Contains(u.rec.screen(), "Note ") {
		t.Errorf("the made certificate is not the default:\n%s", u.rec.screen())
	}
	for _, kind := range []CAKind{CACluster, CAInsecure} {
		if (&Session{CA: &CAChoice{Kind: kind}}).companyCA() {
			t.Errorf("kind %v counts as a company CA", kind)
		}
	}
	if (&Session{}).companyCA() {
		t.Error("no CA counts as a company CA")
	}
}

// 11. A company node says where its API certificate comes from, preselects files, and cautions on
// the made certificate.
func TestCompanyNodeNoteAndCaution(t *testing.T) {
	t.Parallel()
	u := ownUI(t, newHarness(t, opts{dhcp: true, untrusted: true}), Config{}, 100, 40)
	u.until("Trust this CA")
	u.key('y')
	u.until("API certificate comes from")
	screen := u.rec.screen()
	if !strings.Contains(screen, "Corporate Root") || !strings.Contains(screen, "warn about one made here") || !strings.Contains(screen, "> Files of your own") {
		t.Errorf("the note:\n%s", screen)
	}
	u.enter(1)
	u.waitFor("Certificate files")
	u.esc()
	u.waitFor("API certificate comes from")
	u.down(1)
	u.enter(1)
	u.until("If it fails")
	screen = u.rec.screen()
	if !strings.Contains(screen, "Give your own under Certificate") || !strings.Contains(screen, "Corporate Root") {
		t.Errorf("the caution after choosing the made certificate:\n%s", screen)
	}
	for _, kind := range []CAKind{CASystem, CAServed, CAGiven} {
		if !(&Session{CA: &CAChoice{Kind: kind}}).companyCA() {
			t.Errorf("kind %v is no company CA", kind)
		}
	}
}

// 12. Files of any format and order are read, with spaces around the commas.
func TestOwnFilesInAnyFormatAndOrder(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	h := newHarness(t, opts{dhcp: true})
	cert, key := pki.leaf(t, corpName)
	certFile := pki.write(t, "a.pem", encodeCerts(cert))
	keyFile := pki.write(t, "b.key", keyPEM(t, key))
	both := pki.write(t, "both.pem", append(keyPEM(t, key), encodeCerts(cert, pki.ca.cert)...))
	pfxFile := pki.write(t, "c.pfx", func() []byte {
		data, err := pkcs12.Modern.Encode(key, cert, []*x509.Certificate{pki.ca.cert}, "pw")
		if err != nil {
			t.Fatal(err)
		}
		return data
	}())
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	derCert, derKey := pki.write(t, "d.cer", cert.Raw), pki.write(t, "d.der", keyDER)
	p7, err := pkcs7.DegenerateCertificate(append(append([]byte(nil), cert.Raw...), pki.ca.cert.Raw...))
	if err != nil {
		t.Fatal(err)
	}
	p7b := pki.write(t, "e.p7b", p7)
	passFile := pki.write(t, "pass.txt", []byte("pw\n"))
	for name, c := range map[string]Config{
		"cert then key":    {CertFile: certFile + " , " + keyFile},
		"key then cert":    {CertFile: keyFile + "," + certFile},
		"one bundle":       {CertFile: both},
		"pfx":              {CertFile: pfxFile, CertPassphraseFile: passFile},
		"der":              {CertFile: derCert + "," + derKey},
		"p7b and key":      {CertFile: p7b, KeyFile: keyFile},
		"key option joins": {CertFile: certFile, KeyFile: keyFile},
	} {
		c.Name = "kubehenge.example.org"
		s := ownSession(t, h, c)
		if err := s.CheckCertificate(); err != nil || s.Own == nil || s.Own.Leaf.Subject.CommonName != "kubehenge.example.org" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// 12. The files input completes file and directory names under the path typed so far.
func TestFilesFormCompletesPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "cert.pem"), []byte("x"))
	writeTestFile(t, filepath.Join(dir, "certs", "inner.pem"), []byte("x"))
	writeTestFile(t, filepath.Join(dir, "other.key"), []byte("x"))
	prefix := dir + string(filepath.Separator)
	got := completePaths("first.pem, " + prefix + "ce")
	want := []string{"first.pem, " + prefix + "cert.pem", "first.pem, " + prefix + "certs" + string(filepath.Separator)}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
	if got = completePaths(""); got != nil {
		t.Errorf("an empty value completes to %q", got)
	}
}

// 13. A path that is missing, a directory or unreadable returns to the form naming it.
func TestUnusablePathReturnsToTheFilesForm(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	missing, dir := filepath.Join(pki.dir, "missing.pem"), pki.dir
	u := ownUI(t, newHarness(t, opts{dhcp: true}), Config{Name: "kubehenge.example.org"}, 100, 40)
	u.toFiles()
	u.paste(missing)
	u.enter(1)
	u.waitFor("does not exist")
	if screen := u.rec.screen(); !strings.Contains(screen, filepath.Base(missing)) || !strings.Contains(screen, "Certificate files") {
		t.Errorf("a missing file:\n%s", screen)
	}
	for range len(missing) {
		u.press(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	u.paste(dir)
	u.enter(1)
	u.waitFor("is a directory")
	_, err := readCertFile(dir)
	if err == nil || !strings.Contains(err.Error(), dir+" is a directory, not a file") {
		t.Errorf("a directory: %v", err)
	}
	locked := pki.write(t, "locked.pem", []byte("x"))
	if err = os.Chmod(locked, 0); err != nil || runtime.GOOS == "windows" {
		t.Skip("a file cannot be made unreadable here")
	}
	if f, oerr := os.Open(locked); oerr == nil { //nolint:gosec // the test's own file.
		_ = f.Close()
		t.Skip("this user reads any file")
	}
	if _, err = readCertFile(locked); err == nil || !strings.Contains(err.Error(), locked+" cannot be read") {
		t.Errorf("an unreadable file: %v", err)
	}
}

// 14. A passphrase is asked in a masked form, retried on a wrong one, left with Esc, and never kept.
func TestPassphraseFormMasksAndKeepsNothing(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	pfx := pki.pfx(t, "host.pfx", "s3cret-word", corpName)
	answers := filepath.Join(t.TempDir(), "answers.json")
	h := newHarness(t, opts{dhcp: true})
	u := startUIWith(t, h, Config{PVEEntry: "no", Name: "kubehenge.example.org"}, 100, 40, func(s *Session) { s.Now, s.AnswersPath = atTestNow, answers })
	u.toFiles()
	u.paste(pfx)
	u.enter(1)
	u.waitFor("Passphrase for host.pfx")
	u.paste("wrong-guess")
	if screen := u.rec.screen(); strings.Contains(screen, "wrong-guess") {
		t.Fatalf("the passphrase shows on screen:\n%s", screen)
	}
	u.enter(1)
	u.waitFor("does not open host.pfx")
	u.esc()
	u.waitFor("Certificate files")
	u.enter(1)
	u.waitFor("Passphrase for host.pfx")
	u.paste("s3cret-word")
	u.enter(1)
	u.until("If it fails")
	screen := u.rec.screen()
	if !strings.Contains(screen, "kubehenge.example.org, issued by Example Issuing CA") {
		t.Errorf("the summary lacks the certificate:\n%s", screen)
	}
	u.enter(1)
	u.waitFor("Kubehenge is running")
	for name, text := range map[string]string{
		"screen": screen, "repeat command": u.s.Cfg.RepeatCommand(), "error": "", "log": strings.Join(h.exec.calls, "\n"),
	} {
		if strings.Contains(text, "s3cret-word") || strings.Contains(text, "wrong-guess") {
			t.Errorf("the %s holds a passphrase", name)
		}
	}
	if err := saveAnswers(answers, &u.s.Cfg); err != nil {
		t.Fatal(err)
	}
	if data := mustRead(t, answers); strings.Contains(string(data), "s3cret-word") {
		t.Errorf("the answers hold the passphrase: %s", data)
	}
	_, err := certfiles.Read([]certfiles.File{{Name: "host.pfx", Data: mustRead(t, pfx)}}, "nope", nil, testNow)
	if !errors.Is(err, certfiles.ErrPassphrase) || strings.Contains(err.Error(), "nope") {
		t.Errorf("the reader's error: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 15. Any other reader error returns to the files form with the reader's sentence.
func TestReaderErrorReturnsToTheFilesForm(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	notes := pki.write(t, "notes.txt", []byte("not a certificate"))
	u := ownUI(t, newHarness(t, opts{dhcp: true}), Config{Name: "kubehenge.example.org"}, 100, 40)
	u.toFiles()
	u.paste(notes)
	u.enter(1)
	u.waitFor("PEM, DER, PKCS#7 or PKCS#12")
	if !strings.Contains(u.rec.screen(), "Certificate files") {
		t.Errorf("the form is gone:\n%s", u.rec.screen())
	}
}

// 16. The certificate must be valid for the name, else the fixed address, and the form lists what it
// does carry. A DHCP address with no name is not known to check.
func TestCertificateMustCoverTheNameOrAddress(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	other, otherKey := pki.pair(t, "other", leafSpec{cn: "wiki.example.org", dns: []string{"wiki.example.org", "docs.example.org"}, ips: []string{"192.0.2.99"}, days: 400})
	h := newHarness(t, opts{dhcp: true})
	s := ownSession(t, h, Config{CertFile: other, KeyFile: otherKey, Name: "kubehenge.example.org"})
	err := s.CheckCertificate()
	if err == nil || !strings.Contains(err.Error(), "not valid for kubehenge.example.org") || !strings.Contains(err.Error(), "wiki.example.org, docs.example.org, 192.0.2.99") {
		t.Errorf("a name the certificate lacks: %v", err)
	}
	s = ownSession(t, h, Config{CertFile: other, KeyFile: otherKey, IP: "192.0.2.40/24", Gateway: "192.0.2.1"})
	if err = s.CheckCertificate(); err == nil || !strings.Contains(err.Error(), "not valid for 192.0.2.40") {
		t.Errorf("an address the certificate lacks: %v", err)
	}
	s = ownSession(t, h, Config{CertFile: other, KeyFile: otherKey, IP: "192.0.2.99/24", Gateway: "192.0.2.1"})
	if err = s.CheckCertificate(); err != nil {
		t.Errorf("a certificate for the address: %v", err)
	}
	s = ownSession(t, h, Config{CertFile: other, KeyFile: otherKey, IP: "dhcp"})
	if err = s.CheckCertificate(); err != nil {
		t.Errorf("a DHCP address cannot be compared yet: %v", err)
	}
	u := ownUI(t, h, Config{Name: "kubehenge.example.org"}, 100, 40)
	u.toFiles()
	u.paste(other + "," + otherKey)
	u.enter(1)
	u.waitFor("docs.example.org, 192.0.2.99")
	if !strings.Contains(u.rec.screen(), "Certificate files") {
		t.Errorf("the mismatch left the form:\n%s", u.rec.screen())
	}
}

// 17. Under a day left is refused, under 30 days is taken with a caution giving the date.
func TestCertificateEndMargins(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	h := newHarness(t, opts{dhcp: true})
	check := func(days int) (*Session, error) {
		spec := corpName
		spec.days = days
		c, k := pki.pair(t, "m"+strings.Repeat("x", days), spec)
		s := ownSession(t, h, Config{CertFile: c, KeyFile: k, Name: "kubehenge.example.org"})
		err := s.CheckCertificate()
		return s, err
	}
	if _, err := check(0); err == nil || !strings.Contains(err.Error(), "ends on 2026-11-12") || !strings.Contains(err.Error(), "at least another day") {
		t.Errorf("a certificate that ends today: %v", err)
	}
	s, err := check(20)
	if err != nil {
		t.Fatal(err)
	}
	want := "The certificate ends on 2026-12-02. Renew it before then"
	if got := s.AllCautions(); !contains(got, want) {
		t.Errorf("cautions %q lack %q", got, want)
	}
	s, err = check(90)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.AllCautions() {
		if strings.Contains(c, "ends on") {
			t.Errorf("a certificate with 90 days left: %q", c)
		}
	}
}

// 18. A chain that lacks its issuer is taken with a caution, the node's trust store counts as
// roots, and left-out certificates are a note.
func TestIncompleteChainDroppedCertificatesAndRoots(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	h := newHarness(t, opts{dhcp: true})
	cert, key := pki.leaf(t, corpName)
	stray, _ := newOwnPKI(t).leaf(t, leafSpec{cn: "stray.example.org", dns: []string{"stray.example.org"}, days: 400})
	certFile := pki.write(t, "c.pem", encodeCerts(cert, stray))
	keyFile := pki.write(t, "c.key", keyPEM(t, key))
	s := ownSession(t, h, Config{CertFile: certFile, KeyFile: keyFile, Name: "kubehenge.example.org"})
	if err := s.CheckCertificate(); err != nil {
		t.Fatal(err)
	}
	var found string
	for _, c := range s.AllCautions() {
		if strings.Contains(c, "chain lacks") {
			found = c
		}
	}
	if !strings.Contains(found, "CN=Example Issuing CA") || !strings.Contains(found, "Add the file that holds it") {
		t.Errorf("the incomplete chain caution is %q in %q", found, s.AllCautions())
	}
	if len(s.Own.Dropped) != 1 || strings.Contains(strings.Join(s.AllCautions(), "|"), "stray") {
		t.Errorf("the stray certificate is %v, and is no caution", s.Own.Dropped)
	}
	writeTestFile(t, filepath.Join(h.root, systemCA), encodeCerts(pki.ca.cert))
	if err := s.CheckCertificate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.AllCautions() {
		if strings.Contains(c, "chain lacks") {
			t.Errorf("an issuer in the trust store is still missing: %q", c)
		}
	}
	var out bytes.Buffer
	plain := h.session(Config{CertFile: certFile, KeyFile: keyFile, Name: "kubehenge.example.org", PVEEntry: "no", DryRun: true})
	plain.Now = atTestNow
	if err := RunPlain(context.Background(), plain, func(emit func(Event)) *Runner { return h.runner(plain, emit) }, &out); err != nil || !strings.Contains(out.String(), "Left out, since the chain does not use them: CN=stray.example.org") {
		t.Errorf("the plain output lacks the left-out note: %v\n%s", err, &out)
	}
	u := startUIWith(t, h, Config{Name: "kubehenge.example.org"}, 100, 40, func(s *Session) { s.Now = atTestNow })
	u.toFiles()
	u.paste(certFile + "," + keyFile)
	u.enter(1)
	u.waitFor("Left out, since the chain does not use them")
}

// 19. The summary always has a Certificate row.
func TestCertificateRow(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	h := newHarness(t, opts{dhcp: true})
	row := func(s *Session) string {
		for _, r := range s.Rows(true) {
			if r.Label == "Certificate" {
				return r.Value
			}
		}
		return ""
	}
	if got := row(ownSession(t, h, Config{})); got != "made here, signed by this cluster's CA" {
		t.Errorf("the made certificate: %q", got)
	}
	c, k := pki.pair(t, "r", corpName)
	s := ownSession(t, h, Config{CertFile: c, KeyFile: k, Name: "kubehenge.example.org"})
	if err := s.CheckCertificate(); err != nil {
		t.Fatal(err)
	}
	want := "kubehenge.example.org, issued by Example Issuing CA, until " + testNow.Add(400*24*time.Hour).Format(time.DateOnly)
	if got := row(s); got != want {
		t.Errorf("the own certificate: %q, want %q", got, want)
	}
}

// 20. A name or address that changes after an own certificate was accepted checks it again.
func TestChangingTheNameChecksTheCertificateAgain(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	c, k := pki.pair(t, "n", leafSpec{cn: "one.example.org", dns: []string{"one.example.org"}, days: 400})
	h := newHarness(t, opts{dhcp: true})
	u := ownUI(t, h, Config{CertFile: c, KeyFile: k, Name: "one.example.org"}, 100, 40)
	u.until("If it fails")
	u.press(tea.KeyPressMsg{Code: tea.KeyRight})
	u.enter(1)
	u.waitFor("enter change")
	m := u.rec.inner
	at := 0
	for _, id := range settingList {
		if id == settingName {
			break
		}
		if m.listed(id) {
			at++
		}
	}
	u.down(at)
	u.enter(1)
	u.waitFor("DNS name of the interface")
	for range len("one.example.org") {
		u.press(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	u.typed("two.example.org")
	u.enter(1)
	u.waitFor("not valid for two.example.org")
	if !strings.Contains(u.rec.screen(), "Files of your own") {
		t.Errorf("the certificate question did not open again:\n%s", u.rec.screen())
	}
}

// 21. Both settings stay in the list of changes.
func TestNameAndCertificateStayInTheChangeList(t *testing.T) {
	t.Parallel()
	u := ownUI(t, newHarness(t, opts{dhcp: true}), Config{}, 100, 40)
	u.until("If it fails")
	u.press(tea.KeyPressMsg{Code: tea.KeyRight})
	u.enter(1)
	u.waitFor("enter change")
	m := u.rec.inner
	if !m.listed(settingName) || !m.listed(settingCert) {
		t.Fatal("the settings are not listed")
	}
	screen := u.rec.screen()
	if !strings.Contains(screen, "DNS name") || !strings.Contains(screen, "Certificate") {
		t.Errorf("the list lacks them:\n%s", screen)
	}
}

// 22. --cert takes a list of files in any format, --key joins it, --cert-passphrase-file gives the
// passphrase, and a run that cannot ask stops before anything is created.
func TestCertOptionsListKeyAndPassphraseFile(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	pfx := pki.pfx(t, "host.pfx", "pw", corpName)
	pass := pki.write(t, "pass.txt", []byte("pw\r\nsecond line\n"))
	h := newHarness(t, opts{dhcp: true})
	out, err := plainOn(t, h, Config{CertFile: pfx, Name: "kubehenge.example.org", PVEEntry: "no"})
	if err == nil || !strings.Contains(err.Error(), "--cert-passphrase-file") || !strings.Contains(err.Error(), "host.pfx") || h.exec.changed() != 0 {
		t.Errorf("an encrypted file without the option: %v, changed %d\n%s", err, h.exec.changed(), out)
	}
	h = newHarness(t, opts{dhcp: true})
	if out, err = plainOn(t, h, Config{CertFile: pfx, CertPassphraseFile: pass, Name: "kubehenge.example.org", PVEEntry: "no"}); err != nil {
		t.Errorf("with the option: %v\n%s", err, out)
	}
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{KeyFile: "k.pem"}, "--key goes with --cert"},
		{Config{CertPassphraseFile: "p"}, "--cert-passphrase-file goes with --cert"},
		{Config{CertFile: pki.dir + "/missing.pem"}, "missing.pem does not exist"},
		{Config{CertFile: pfx, CertPassphraseFile: pki.dir + "/nope"}, "nope does not exist"},
	} {
		err = c.cfg.Validate()
		if err == nil {
			err = c.cfg.CheckOptionFiles()
		}
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "PEM") {
			t.Errorf("%+v: %v, want %q", c.cfg, err, c.want)
		}
	}
	cfg := Config{CertFile: pfx + ", " + pass}
	if err = cfg.CheckOptionFiles(); err != nil {
		t.Errorf("a list with spaces: %v", err)
	}
}

// 23. --yes, --dry-run and the plain run take the defaults without asking, and print the same rows.
func TestYesDryRunAndPlainTakeTheDefaults(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true, untrusted: true})
	for _, cfg := range []Config{{Yes: true}, {DryRun: true}} {
		m := NewModel(prepared(t, h, cfg), nil)
		m.initFromOptions()
		if m.needed(settingName) || m.needed(settingCert) {
			t.Errorf("%+v asks the interface questions", cfg)
		}
	}
	s := h.session(Config{PVEEntry: "no", DryRun: true, TrustServedCA: true, Name: "gone.example.org"})
	var out bytes.Buffer
	if err := RunPlain(context.Background(), s, func(emit func(Event)) *Runner { return h.runner(s, emit) }, &out); err != nil {
		t.Fatalf("%v\n%s", err, &out)
	}
	for _, want := range []string{
		"Interface", "https://gone.example.org (no DNS record)", "Certificate", "made here, signed by this cluster's CA",
		"kubehenge: gone.example.org has no DNS record.", "kubehenge: This node's API certificate comes from CN=Corporate Root",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the dry run lacks %q:\n%s", want, &out)
		}
	}
}

// 24. An install and a certificate replacement read an own certificate the same way, in any format.
func TestInstallAndReplacementReadEveryFormat(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	pfx := pki.pfx(t, "host.pfx", "pw", corpName)
	pass := pki.write(t, "pass.txt", []byte("pw\n"))
	h := newHarness(t, opts{dhcp: true})
	s := h.session(Config{CertFile: pfx, CertPassphraseFile: pass, Name: "kubehenge.example.org", PVEEntry: "no"})
	s.Now = atTestNow
	var out bytes.Buffer
	if err := RunPlain(context.Background(), s, func(emit func(Event)) *Runner { return h.runner(s, emit) }, &out); err != nil {
		t.Fatalf("install: %v\n%s", err, &out)
	}
	rh, r, rout := upgradeHarness(t)
	rh.exec.on("pct config 100", "hostname: kubehenge\ntags: kubehenge\nnet0: name=eth0,bridge=vmbr0,ip=192.0.2.40/24\n")
	r.S.Cfg.Certificate, r.S.Cfg.Upgrade = true, false
	r.S.Cfg.CertFile, r.S.Cfg.CertPassphraseFile, r.S.Cfg.Name = pfx, pass, "kubehenge.example.org"
	if res, err := r.ReplaceCertificate(context.Background(), rout); err != nil || res.Fingerprint == "" {
		t.Fatalf("replacement: %v\n%s", err, rout)
	}
	r.S.Cfg.CertPassphraseFile = ""
	if _, err := r.ReplaceCertificate(context.Background(), rout); err == nil || !strings.Contains(err.Error(), "--cert-passphrase-file") {
		t.Errorf("an encrypted file without the option: %v", err)
	}
}

// 25. The container gets the leaf with its intermediates and an unencrypted PEM key, as before.
func TestPushedCertificateKeepsItsShape(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	pfx := pki.pfx(t, "host.pfx", "pw", corpName)
	pass := pki.write(t, "pass.txt", []byte("pw\n"))
	h := newHarness(t, opts{dhcp: true})
	s := h.session(Config{CertFile: pfx, CertPassphraseFile: pass, Name: "kubehenge.example.org", PVEEntry: "no"})
	s.Now = atTestNow
	var out bytes.Buffer
	if err := RunPlain(context.Background(), s, func(emit func(Event)) *Runner { return h.runner(s, emit) }, &out); err != nil {
		t.Fatalf("%v\n%s", err, &out)
	}
	crt, key := mustRead(t, filepath.Join(h.work, "tls.crt")), mustRead(t, filepath.Join(h.work, "tls.key"))
	if n := bytes.Count(crt, []byte("BEGIN CERTIFICATE")); n != 1 || bytes.Contains(crt, []byte("PRIVATE")) {
		t.Errorf("tls.crt holds %d certificates:\n%s", n, crt)
	}
	if !bytes.HasPrefix(key, []byte("-----BEGIN PRIVATE KEY-----")) || bytes.Contains(key, []byte("ENCRYPTED")) {
		t.Errorf("tls.key is not an unencrypted PEM key:\n%s", key)
	}
	calls := strings.Join(h.exec.calls, "\n")
	if !strings.Contains(calls, "/etc/kubehenge/tls.crt --perms 0644") || !strings.Contains(calls, "/etc/kubehenge/tls.key --perms 0600") {
		t.Errorf("the modes changed:\n%s", calls)
	}
}

// 26. Saved answers keep the files, read them again on restore, and drop what no longer fits.
func TestSavedCertificateRestoresOrIsNamedAsDropped(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	c, k := pki.pair(t, "s", corpName)
	pfx := pki.pfx(t, "s.pfx", "pw", corpName)
	h := newHarness(t, opts{dhcp: true})
	answers := filepath.Join(t.TempDir(), "a.json")
	save := func(cfg Config) *savedAnswers {
		if err := saveAnswers(answers, &cfg); err != nil {
			t.Fatal(err)
		}
		return loadAnswers(answers)
	}
	restore := func(a *savedAnswers) *Session {
		s := prepared(t, h, Config{})
		s.Now = atTestNow
		s.Dropped = nil
		s.Restore(context.Background(), a)
		return s
	}
	if s := restore(save(Config{CertFile: c, KeyFile: k, Name: "kubehenge.example.org"})); s.Cfg.CertFile != c+","+k || s.Own == nil {
		t.Errorf("restored %q, read %v", s.Cfg.CertFile, s.Own)
	}
	if s := restore(save(Config{CertFile: c, KeyFile: k, Name: "other.example.org"})); s.Cfg.CertFile != "" || !contains(s.Dropped, "certificate") {
		t.Errorf("a certificate that no longer fits: %q %v", s.Cfg.CertFile, s.Dropped)
	}
	if s := restore(save(Config{CertFile: pfx, Name: "kubehenge.example.org"})); s.Cfg.CertFile != "" || !contains(s.Dropped, "certificate") {
		t.Errorf("a file that needs a passphrase: %q %v", s.Cfg.CertFile, s.Dropped)
	}
	a := save(Config{CertFile: c, KeyFile: k, Name: "kubehenge.example.org"})
	if err := os.Remove(c); err != nil {
		t.Fatal(err)
	}
	if s := restore(a); s.Cfg.CertFile != "" || !contains(s.Dropped, "certificate") {
		t.Errorf("files that are gone: %q %v", s.Cfg.CertFile, s.Dropped)
	}
	u := startUIWith(t, h, Config{PVEEntry: "no"}, 100, 40, func(s *Session) { s.Now, s.AnswersPath, s.Saved = atTestNow, answers, a })
	u.waitFor("the saved certificate, which fail the checks")
}

// 27. The repeat command carries the name, every certificate file and the passphrase file.
func TestRepeatCommandCarriesTheInterfaceOptions(t *testing.T) {
	t.Parallel()
	cfg := Config{Name: "kubehenge.example.org", CertFile: "/root/a.pem,/root/b.key", CertPassphraseFile: "/root/pass.txt", passphrase: "typed-secret"} //nolint:gosec // test data.
	got := cfg.RepeatCommand()
	for _, want := range []string{"--name kubehenge.example.org", "--cert /root/a.pem,/root/b.key", "--cert-passphrase-file /root/pass.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "typed-secret") {
		t.Errorf("the repeat command holds the passphrase: %q", got)
	}
}

// 29. The screens of the Interface step at both sizes, each fitting its terminal.
func TestInterfaceScreens(t *testing.T) {
	t.Parallel()
	pki := newOwnPKI(t)
	pfx := pki.pfx(t, "host.pfx", "pw", corpName)
	for _, sz := range screens {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()
			home := ownUI(t, newHarness(t, opts{dhcp: true}), Config{}, sz.w, sz.l)
			home.until("DNS name of the interface")
			home.golden("name")
			home.paste("kubehenge.example.org")
			home.enter(1)
			home.waitFor("Made here, signed by")
			home.golden("choice-homelab")
			home.down(1)
			home.enter(1)
			home.waitFor("Certificate files")
			home.paste("wiki-cert.pem")
			home.enter(1)
			home.waitFor("does not exist")
			home.golden("files-error")
			for range len("wiki-cert.pem") {
				home.press(tea.KeyPressMsg{Code: tea.KeyBackspace})
			}
			home.paste(pfx)
			home.enter(1)
			home.waitFor("Passphrase for host.pfx")
			home.golden("passphrase")

			corp := ownUI(t, newHarness(t, opts{dhcp: true, untrusted: true}), Config{Name: "gone.example.org"}, sz.w, sz.l)
			corp.until("Trust this CA")
			corp.key('y')
			corp.until("API certificate comes from")
			corp.golden("choice-company")
			corp.down(1)
			corp.enter(1)
			corp.until("If it fails")
			corp.golden("summary-cautions")
		})
	}
}

// --name is lowercased, as DNS names are compared.
func TestNormaliseLowercasesTheNameOption(t *testing.T) {
	t.Parallel()
	c := Config{Name: " KubeHenge.Example.org "}
	if err := c.Normalise(); err != nil || c.Name != "kubehenge.example.org" {
		t.Errorf("%q %v", c.Name, err)
	}
}
