package installer

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/kubehenge/kubehenge/pkg/acme"
	"github.com/kubehenge/kubehenge/pkg/certfiles"
)

// What the interface questions are about: the address people open the web interface at, and the
// certificate it serves there.

// NameState is what the DNS check found for the name people open the interface at.
type NameState int

// The outcomes of the check of the DNS name.
const (
	NameUnchecked NameState = iota
	// NameMatches says the name resolves to an address the container has.
	NameMatches
	// NameElsewhere says the name resolves, but not to the container's address.
	NameElsewhere
	// NameMissing says the name has no record.
	NameMissing
	// NameUnknown says the check could not compare: the lookup failed, or the container's address
	// comes from DHCP or router advertisements and is not known yet.
	NameUnknown
)

// NameCheck is one lookup of the DNS name, valid for the name and addresses in Key.
type NameCheck struct {
	Key   string
	State NameState
	// Found is what the name resolved to, for the outcomes that compare it.
	Found []string
}

// certMonth is how long before its end a certificate gets a caution.
const certMonth = 30 * 24 * time.Hour

// maxCertFile is the most the installer reads of one certificate file. certfiles refuses a file over
// its own limit, which this exceeds by one byte so that the refusal names it.
const maxCertFile = 1<<20 + 1

// maxSuggestions caps the completions of a path.
const maxSuggestions = 50

func (s *Session) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// normaliseName turns what was typed or pasted into the DNS name it means, or says which rule it
// breaks. It holds the one rule of the form, the restore and the options.
func normaliseName(in string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(in))
	name = strings.TrimPrefix(strings.TrimPrefix(name, "https://"), "http://")
	name = strings.TrimSuffix(name, "/")
	const example = "such as kubehenge.example.org"
	switch {
	case name == "":
		return "", nil
	case strings.IndexFunc(name, unicode.IsSpace) >= 0:
		return "", errors.New("enter the DNS name without spaces, " + example)
	}
	if _, err := netip.ParseAddr(strings.Trim(name, "[]")); err == nil {
		return "", errors.New("enter a DNS name " + example + ", not an IP address")
	}
	switch {
	case strings.Contains(name, "/"):
		return "", errors.New("enter the DNS name without a path, " + example)
	case strings.Contains(name, ":"):
		return "", errors.New("enter the DNS name without a port, " + example)
	case !dnsNameRE.MatchString(name):
		return "", errors.New("enter a DNS name " + example)
	}
	return name, nil
}

// fixedAddresses are the addresses the container is given by the options, IPv4 first. DHCP and
// router advertisements give none the installer knows before the container runs.
func (s *Session) fixedAddresses() []string {
	var out []string
	for _, ip := range []string{s.Cfg.IP, s.Cfg.IP6} {
		if modeOf(ip) != optStatic {
			continue
		}
		if p, err := netip.ParsePrefix(ip); err == nil {
			out = append(out, p.Addr().String())
		} else if a, aerr := netip.ParseAddr(ip); aerr == nil {
			out = append(out, a.String())
		}
	}
	return out
}

// publicHost is the host people open the interface at, empty while it is an address DHCP will give.
func (s *Session) publicHost() string {
	if s.Cfg.Name != "" {
		return s.Cfg.Name
	}
	if fixed := s.fixedAddresses(); len(fixed) > 0 {
		return fixed[0]
	}
	return ""
}

func (s *Session) nameKey() string {
	return s.Cfg.Name + "|" + strings.Join(s.fixedAddresses(), ",")
}

// CheckName looks the DNS name up and keeps what it found with the name and addresses it was made
// for, so that a result for another pair is never shown. No outcome stops the install.
func (s *Session) CheckName(ctx context.Context) {
	res := NameCheck{Key: s.nameKey()}
	name := s.Cfg.Name
	if name == "" || s.Node.LookupHost == nil {
		s.NameCheck = NameCheck{}
		return
	}
	fixed := s.fixedAddresses()
	addrs, err := s.Node.LookupHost(ctx, name)
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound, err == nil && len(addrs) == 0:
		res.State = NameMissing
	case err != nil:
		res.State = NameUnknown
	case len(fixed) == 0:
		res.State, res.Found = NameUnknown, addrs
	case anyOf(addrs, fixed):
		res.State = NameMatches
	default:
		res.State, res.Found = NameElsewhere, addrs
	}
	s.NameCheck = res
}

// anyOf reports whether a list of addresses holds one of the wanted, as addresses and not as text.
func anyOf(list, want []string) bool {
	for _, a := range list {
		pa, err := netip.ParseAddr(a)
		if err != nil {
			continue
		}
		for _, w := range want {
			if pw, werr := netip.ParseAddr(w); werr == nil && pa.Unmap() == pw.Unmap() {
				return true
			}
		}
	}
	return false
}

// nameState is the DNS check as far as it holds for the options now.
func (s *Session) nameState() NameCheck {
	if s.Cfg.Name == "" || s.NameCheck.Key != s.nameKey() {
		return NameCheck{}
	}
	return s.NameCheck
}

// addressSource says where a container's address comes from when no option fixes it.
func (s *Session) addressSource() string {
	c := &s.Cfg
	switch {
	case c.IP == optNone && c.IP6 == optDHCP:
		return "DHCPv6"
	case c.IP == optNone && c.IP6 == optAuto:
		return "router advertisements"
	}
	return "DHCP"
}

// interfaceText is the Interface row: the address people open, and for a name what DNS says.
func (s *Session) interfaceText() string {
	fixed := s.fixedAddresses()
	if s.Cfg.Name == "" {
		if len(fixed) > 0 {
			return "https://" + urlHost(fixed[0])
		}
		return "the address comes from " + s.addressSource()
	}
	phrase := "DNS not checked"
	switch st := s.nameState(); st.State {
	case NameMatches:
		phrase = "resolves to the container"
	case NameMissing:
		phrase = "no DNS record"
	case NameElsewhere:
		phrase = "resolves to " + strings.Join(st.Found, ", ")
	case NameUnknown:
		if len(st.Found) > 0 {
			phrase = "resolves to " + strings.Join(st.Found, ", ") + ", the container's address is not fixed"
		}
	case NameUnchecked:
	}
	return "https://" + s.Cfg.Name + " (" + phrase + ")"
}

// certText is the Certificate row.
func (s *Session) certText() string {
	if a := s.Cfg.ACME; a.Directory != "" {
		return "from " + acmeServer(a.Directory) + " by ACME, " + cmp.Or(a.Proof, acme.ProofTLSALPN)
	}
	if s.Cfg.CertFile == "" {
		return "made here, signed by the Proxmox VE CA"
	}
	if s.Own == nil || s.Own.Leaf == nil {
		return "files of your own"
	}
	leaf := s.Own.Leaf
	return fmt.Sprintf("%s, issued by %s, until %s", certLabel(leaf), issuerName(leaf), leaf.NotAfter.UTC().Format(time.DateOnly))
}

// certLabel names a certificate by its common name, else its first DNS name.
func certLabel(c *x509.Certificate) string {
	switch {
	case c.Subject.CommonName != "":
		return c.Subject.CommonName
	case len(c.DNSNames) > 0:
		return c.DNSNames[0]
	}
	return c.Subject.String()
}

func issuerName(c *x509.Certificate) string {
	if c.Issuer.CommonName != "" {
		return c.Issuer.CommonName
	}
	return c.Issuer.String()
}

// companyCA says whether this node's API certificate comes from a CA a company runs, which the
// browsers of its people trust and a certificate made here is not signed by.
func (s *Session) companyCA() bool {
	if s.CA == nil {
		return false
	}
	switch s.CA.Kind {
	case CASystem, CAServed, CAGiven:
		return true
	case CACluster, CAInsecure:
	}
	return false
}

// madeNote says when browsers accept a certificate made here, and on a company node what avoids
// their warning.
func (s *Session) madeNote() string {
	if s.companyCA() {
		return "Browsers warn until they trust the Proxmox VE CA. Files from " + s.CA.Subject + ", which signed this node's API certificate, avoid the warning"
	}
	return "Browsers warn until they trust the Proxmox VE CA, as for this node's own interface"
}

// interfaceCautions are the cautions the interface questions add to the summary.
func (s *Session) interfaceCautions() []string {
	var out []string
	st := s.nameState()
	fixed := strings.Join(s.fixedAddresses(), " or ")
	target := "the appliance's address, which " + s.addressSource() + " gives,"
	if fixed != "" {
		target = fixed
	}
	switch st.State {
	case NameMissing:
		out = append(out, fmt.Sprintf("%s has no DNS record. Point it at %s in your DNS", s.Cfg.Name, target))
	case NameElsewhere:
		out = append(out, fmt.Sprintf("%s resolves to %s, not %s. Point it at %s in your DNS", s.Cfg.Name, strings.Join(st.Found, ", "), fixed, fixed))
	case NameUnchecked, NameMatches, NameUnknown:
	}
	if s.Cfg.CertFile == "" && s.Cfg.ACME.Directory == "" && s.companyCA() {
		out = append(out, s.madeNote())
	}
	out = append(out, s.acmeCautions()...)
	if b := s.Own; s.Cfg.CertFile != "" && b != nil && b.Leaf != nil {
		if end := b.Leaf.NotAfter; end.Before(s.clock().Add(certMonth)) {
			out = append(out, "The certificate ends on "+end.UTC().Format(time.DateOnly)+". Renew it before then")
		}
		if b.Incomplete {
			out = append(out, "The certificate chain lacks "+b.MissingIssuer+". Add the file that holds it to Certificate")
		}
	}
	return out
}

// AllCautions are the cautions of the API check and of the interface questions.
func (s *Session) AllCautions() []string {
	return append(append([]string(nil), s.Cautions...), s.interfaceCautions()...)
}

// PassphraseError says encrypted material needs a passphrase, or the one given is wrong.
type PassphraseError struct {
	// File is the file that is encrypted, as it was named.
	File string
	// Given says a passphrase was tried.
	Given bool
}

func (e *PassphraseError) Error() string { return e.User().Error() }

// User is the error for a run that cannot ask: it names the option that gives the passphrase.
func (e *PassphraseError) User() error {
	if e.Given {
		return settingErr(settingCert, "the passphrase does not open "+e.File, "Put the right one on the first line of the file --cert-passphrase-file names")
	}
	return settingErr(settingCert, e.File+" is encrypted", "Give its passphrase in a file with --cert-passphrase-file <file>")
}

// plainCertErr is err for a run without a terminal, which cannot ask for a passphrase.
func plainCertErr(err error) error {
	if pe, ok := errors.AsType[*PassphraseError](err); ok {
		return pe.User()
	}
	return err
}

// ownFiles are the files --cert and --key name.
func (c *Config) ownFiles() []string {
	files := splitList(c.CertFile)
	if c.KeyFile != "" {
		files = append(files, c.KeyFile)
	}
	return files
}

// passphraseText is the passphrase of the options: one the user typed, else the first line of
// --cert-passphrase-file.
func (c *Config) passphraseText() (string, error) {
	if c.passphrase != "" || c.CertPassphraseFile == "" {
		return c.passphrase, nil
	}
	data, err := readCertFile(c.CertPassphraseFile)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSuffix(line, "\r"), nil
}

// readCertFile reads a file the options or the form name, and says in a sentence what is wrong with
// a path that is none.
func readCertFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, settingErr(settingCert, path+" does not exist. Give the path of an existing file", "")
	case err != nil:
		return nil, settingErr(settingCert, path+" cannot be read", "")
	case info.IsDir():
		return nil, settingErr(settingCert, path+" is a directory, not a file", "")
	}
	f, err := os.Open(path) //nolint:gosec // the path is the one the user named.
	if err != nil {
		return nil, settingErr(settingCert, path+" cannot be read", "")
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxCertFile))
	if err != nil {
		return nil, settingErr(settingCert, path+" cannot be read", "")
	}
	return data, nil
}

// readOwn reads the certificate files of the options, in any format, against this node's trust
// store. A failure names the file or the rule it breaks.
func (n *Node) readOwn(c *Config, now time.Time) (*certfiles.Bundle, error) {
	paths := c.ownFiles()
	files := make([]certfiles.File, 0, len(paths))
	for _, p := range paths {
		data, err := readCertFile(p)
		if err != nil {
			return nil, err
		}
		files = append(files, certfiles.File{Name: p, Data: data})
	}
	pass, err := c.passphraseText()
	if err != nil {
		return nil, err
	}
	var roots *x509.CertPool
	if data, rerr := n.readFile(systemCA); rerr == nil {
		roots = poolOf(data)
	}
	b, err := certfiles.Read(files, pass, roots, now)
	switch {
	case errors.Is(err, certfiles.ErrPassphrase):
		file, _ := strings.CutSuffix(err.Error(), ": "+certfiles.ErrPassphrase.Error())
		return nil, &PassphraseError{File: file, Given: pass != ""}
	case err != nil:
		return nil, settingErr(settingCert, err.Error(), "")
	}
	return b, nil
}

// OwnCertificate checks the administrator's certificate for the host people open, which is empty
// while it is an address DHCP will give.
func OwnCertificate(b *certfiles.Bundle, host string, now time.Time) (*Certificate, error) {
	leaf := b.Leaf
	label := certLabel(leaf)
	if leaf.NotAfter.Before(now.Add(certMinValidity)) {
		return nil, settingErr(settingCert, "the certificate for "+label+" ends on "+leaf.NotAfter.UTC().Format(time.DateOnly), "Give one valid for at least another day")
	}
	if host != "" {
		if err := leaf.VerifyHostname(host); err != nil {
			dns, ips := certNames(leaf)
			names := strings.Join(append(dns, ips...), ", ")
			if names == "" {
				names = "no name"
			}
			return nil, settingErr(settingCert, fmt.Sprintf("the certificate for %s is not valid for %s. It covers %s", label, host, names),
				"Give one that covers it, or change the DNS name")
		}
	}
	return &Certificate{CertPEM: b.CertPEM, KeyPEM: b.KeyPEM, Fingerprint: fingerprint(leaf), NotAfter: leaf.NotAfter}, nil
}

// CheckCertificate reads the certificate files of the options and checks them for the host people
// open. It keeps what it read for the summary, and clears it when the options name no files.
func (s *Session) CheckCertificate() error {
	s.Own = nil
	if s.Cfg.CertFile == "" {
		return nil
	}
	b, err := s.Node.readOwn(&s.Cfg, s.clock())
	if err != nil {
		return err
	}
	if _, err = OwnCertificate(b, s.publicHost(), s.clock()); err != nil {
		return err
	}
	s.Own = b
	return nil
}

// completeCertPaths offers completions for the files typed so far. It runs on the update loop,
// since huh runs a SuggestionsFunc on a goroutine of its own while the input writes the same string.
func (m *Model) completeCertPaths() {
	if m.a.certInput == nil || m.a.certPaths == m.a.completed {
		return
	}
	m.a.completed = m.a.certPaths
	m.a.certInput.Suggestions(completePaths(m.a.certPaths))
}

// completePaths completes the last path of a comma-separated list, as whole values for an input.
func completePaths(value string) []string {
	last := value
	if _, after, found := strings.CutLast(value, ","); found {
		last = strings.TrimLeft(after, " ")
	}
	if last == "" {
		return nil
	}
	head := value[:len(value)-len(last)]
	dir, base := filepath.Split(last)
	read := dir
	if read == "" {
		read = "."
	}
	entries, err := os.ReadDir(read)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) || strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		if e.IsDir() {
			name += string(filepath.Separator)
		}
		out = append(out, head+dir+name)
		if len(out) == maxSuggestions {
			break
		}
	}
	return out
}

// The steps of the certificate question, and the choices of its first.
const (
	certStepChoice = iota
	certStepFiles
	certStepPassphrase
	// certStepACME asks the ACME server, the proof and the email address, and certStepACMEMore what
	// the DNS proof or a server of one's own needs.
	certStepACME
	certStepACMEMore
)

// certStepBack is the step Esc goes back to from each step after the choice.
var certStepBack = map[int]int{
	certStepFiles: certStepChoice, certStepPassphrase: certStepFiles, certStepACME: certStepChoice, certStepACMEMore: certStepACME,
}

const (
	certMade  = "made"
	certFiles = "files"
	certACME  = "acme"
)

func nameSetting() *setting {
	return &setting{
		label:   "DNS name",
		applies: always,
		open:    func(m *Model) bool { return !m.S.Cfg.Yes && !m.S.Cfg.DryRun },
		value:   func(m *Model) string { return orNone(m.S.Cfg.Name) },
		fields: func(m *Model) []huh.Field {
			m.a.name = m.S.Cfg.Name
			hint := "the address DHCP gives"
			if fixed := m.S.fixedAddresses(); len(fixed) > 0 {
				hint = "https://" + urlHost(fixed[0])
			}
			return []huh.Field{input("DNS name of the interface", &m.a.name, "empty: "+hint, func(s string) error {
				_, err := normaliseName(s)
				return err
			})}
		},
		apply: func(m *Model) tea.Cmd {
			m.S.Cfg.Name, _ = normaliseName(m.a.name)
			return nil
		},
	}
}

func certSetting() *setting {
	return &setting{
		label:   "Certificate",
		applies: always,
		open:    func(m *Model) bool { return !m.S.Cfg.Yes && !m.S.Cfg.DryRun },
		value:   func(m *Model) string { return m.S.certText() },
		fields:  certFields,
		apply:   certApply,
	}
}

// certFields is the form of the step the certificate question is at.
func certFields(m *Model) []huh.Field {
	switch m.a.certStep {
	case certStepFiles:
		if files := m.S.Cfg.ownFiles(); len(files) > 0 {
			m.a.certPaths = strings.Join(files, ",")
		}
		m.a.certInput = input("Certificate files", &m.a.certPaths, "files, comma-separated, such as cert.pem,key.pem", func(s string) error {
			if len(splitList(s)) == 0 {
				return errors.New("enter one or more files, comma-separated")
			}
			return nil
		})
		m.a.completed = "\x00"
		m.completeCertPaths()
		return []huh.Field{m.a.certInput}
	case certStepPassphrase:
		return []huh.Field{input("Passphrase for "+filepath.Base(m.a.certFile), &m.a.passphrase, "", func(s string) error {
			if s == "" {
				return errors.New("enter the passphrase")
			}
			return nil
		}).EchoMode(huh.EchoModePassword)}
	case certStepACME:
		return acmeFields(m)
	case certStepACMEMore:
		return acmeMoreFields(m)
	}
	switch {
	case m.S.Cfg.CertFile != "":
		m.a.certChoice = certFiles
	case m.S.Cfg.ACME.Directory != "":
		m.a.certChoice = certACME
	case m.a.certChoice != certMade:
		m.a.certChoice = certMade
		if m.S.companyCA() {
			m.a.certChoice = certFiles
		}
	}
	sel := choose("Certificate", &m.a.certChoice,
		huh.NewOption("Made here, signed by the Proxmox VE CA", certMade), huh.NewOption("Files of your own", certFiles),
		huh.NewOption("ACME, such as Let's Encrypt", certACME))
	// The note wraps over two lines, which the select's own height does not count.
	return []huh.Field{sel.Description(m.S.madeNote()).Height(certNoteRows)}
}

// certApply takes the answer of the step the certificate question is at. It goes to the next step,
// or back to the form that failed, until the files read.
func certApply(m *Model) tea.Cmd {
	c := &m.S.Cfg
	switch m.a.certStep {
	case certStepFiles:
		c.CertFile, c.KeyFile = strings.Join(splitList(m.a.certPaths), ","), ""
		c.passphrase = ""
	case certStepPassphrase:
		c.passphrase, m.a.passphrase = m.a.passphrase, ""
	case certStepACME, certStepACMEMore:
		return acmeApply(m)
	default:
		if m.a.certChoice != certACME {
			c.ACME = acme.Files{}
		}
		if m.a.certChoice != certFiles {
			c.CertFile, c.KeyFile, c.passphrase = "", "", ""
			m.S.Own = nil
		}
		switch m.a.certChoice {
		case certFiles:
			m.a.certStep = certStepFiles
		case certACME:
			m.a.certStep = certStepACME
		default:
			return nil
		}
		return m.askCertAgain()
	}
	err := m.S.CheckCertificate()
	if pe, ok := errors.AsType[*PassphraseError](err); ok {
		c.passphrase = ""
		m.a.certStep, m.a.certFile = certStepPassphrase, pe.File
		if pe.Given {
			m.note = "The passphrase does not open " + filepath.Base(pe.File)
		}
		return m.askCertAgain()
	}
	if err != nil {
		c.CertFile, c.KeyFile = "", ""
		m.a.certStep = certStepFiles
		m.note = upperFirst(neutral(err.Error()))
		return m.askCertAgain()
	}
	m.a.certStep = certStepChoice
	if d := m.S.Own.Dropped; len(d) > 0 {
		m.note = droppedNote(d)
	}
	return nil
}

// droppedNote names the certificates the files held that the chain does not use.
func droppedNote(dropped []string) string {
	return "Left out, since the chain does not use them: " + strings.Join(dropped, " and ")
}

// askCertAgain opens the certificate question once more after an answer that did not settle it.
func (m *Model) askCertAgain() tea.Cmd {
	m.answered[settingCert] = false
	return m.open(settingCert)
}

func upperFirst(s string) string {
	r := []rune(s)
	first, _, _ := strings.Cut(s, " ")
	if len(r) > 0 && !strings.ContainsAny(first, `./\`) {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}
