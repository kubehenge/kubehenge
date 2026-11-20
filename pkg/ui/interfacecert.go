package ui

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kubehenge/kubehenge/pkg/acme"
	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/certfiles"
)

// Where the certificate the interface serves comes from.
const (
	certificateInstaller  = "installer"
	certificateUploaded   = "uploaded"
	certificateSelfSigned = "self-signed"
	certificateACME       = acme.SourceACME
)

const (
	// maxCertificateFiles and maxCertificateFile bound an upload. A certificate, its chain and its
	// key fit in a few kilobytes, so a larger file is a wrong one.
	maxCertificateFiles = 8
	maxCertificateFile  = 1 << 20
	maxPassphrase       = 4 << 10
	// uploadOverhead is what a multipart body adds to the files themselves.
	uploadOverhead = 64 << 10
	// certificateMinValidity is how much validity a certificate needs left to be put in place.
	certificateMinValidity = 24 * time.Hour
	// selfSignedDays is 825 days, the longest validity browsers accept from a private CA, less the
	// hour the certificate starts back. That hour keeps a browser whose clock is a few minutes behind
	// from refusing it as not yet valid.
	selfSignedDays = 824
	serialBits     = 128
	lookupTimeout  = 2 * time.Second
	// sourceLine opens the stored file, before its first PEM block, which readers skip.
	sourceLine = "# source: "
	// orderLine follows it for a certificate from ACME, naming the order that issued it, so that a
	// restart knows the order was delivered.
	orderLine = "# order: "
)

// InterfaceCertificate is the certificate the interface and Headlamp serve. It is the installer's,
// in files, until an administrator gives another. That one is kept in the appliance's directory,
// which backups hold, and served from then on, also after a restart. A change takes effect for the
// next connection.
type InterfaceCertificate struct {
	// Roots complete the chain of an uploaded certificate. Nil takes the system's.
	Roots *x509.CertPool
	// Lookup gives the addresses a name resolves to. Nil asks the system's resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)

	dir, certFile, keyFile string
	// memory keeps changes in memory, for the demo, which has no appliance directory.
	memory bool
	// readInstaller reads the installer's certificate. The demo has its own.
	readInstaller func(now time.Time) (*servedCertificate, error)
	files         certificateFiles
	rootsOnce     sync.Once
	// demoIssue makes the certificates the demo's ACME orders give.
	demoIssue func(at time.Time) (certPEM, keyPEM []byte, err error)
	// acme orders and renews the certificate when an administrator set it up. Nil when the interface
	// has nowhere to keep ACME's setting.
	acme *acme.Manager

	// mu is held by whoever changes the certificate, so that the stored file and the certificate
	// served are always the same one.
	mu   sync.Mutex
	live atomic.Pointer[servedCertificate]
}

// servedCertificate is a certificate being served and what is known of its chain.
type servedCertificate struct {
	pair       tls.Certificate
	source     string
	chain      []*x509.Certificate
	incomplete bool
	missing    string
	// notServed is why the stored file is left alone and not served.
	notServed string
	// order is the ACME order that issued the certificate.
	order string
}

// NewInterfaceCertificate serves the certificate files the installer made, or the one in dir when
// an administrator gave another. An empty dir leaves the certificate to the installer.
func NewInterfaceCertificate(dir, certFile, keyFile string) *InterfaceCertificate {
	c := &InterfaceCertificate{dir: dir, certFile: certFile, keyFile: keyFile}
	c.readInstaller = c.readInstallerFiles
	return c
}

// Load picks the certificate to serve. A stored file that does not read is left in place, logged,
// and shown to administrators, and the installer's is served.
func (c *InterfaceCertificate) Load(now time.Time, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	problem := ""
	if c.dir != "" {
		c.removeLeftovers()
		stored, err := c.readStored(now)
		switch {
		case err == nil:
			c.live.Store(stored)
			return nil
		case !errors.Is(err, fs.ErrNotExist):
			problem = err.Error()
			log.Error("the stored interface certificate is not served", "file", c.storedPath(), "reason", problem)
		}
	}
	installer, err := c.readInstaller(now)
	if err != nil {
		return err
	}
	installer.notServed = problem
	c.live.Store(installer)
	return nil
}

// GetCertificate is the one place both listeners take their certificate from. Reading the current
// one on each handshake is what lets a change reach the next connection with no restart.
func (c *InterfaceCertificate) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c.acme != nil {
		if challenge := c.acme.ChallengeCertificate(hello); challenge != nil {
			return challenge, nil
		}
	}
	served := c.live.Load()
	if served == nil {
		return nil, errors.New("no certificate is loaded")
	}
	return &served.pair, nil
}

func (c *InterfaceCertificate) storedPath() string {
	return filepath.Join(c.dir, appliance.InterfaceCertificateFile)
}

func (c *InterfaceCertificate) roots() *x509.CertPool {
	c.rootsOnce.Do(func() {
		if c.Roots == nil {
			c.Roots, _ = x509.SystemCertPool()
		}
	})
	return c.Roots
}

// canChange tells whether a change has somewhere to be kept.
func (c *InterfaceCertificate) canChange() bool { return c.dir != "" || c.memory }

// served is the current certificate, nil before Load.
func (c *InterfaceCertificate) served() *servedCertificate { return c.live.Load() }

func (c *InterfaceCertificate) readStored(now time.Time) (*servedCertificate, error) {
	raw, err := os.ReadFile(c.storedPath())
	if err != nil {
		return nil, fmt.Errorf("reading the stored certificate: %w", err)
	}
	b, err := certfiles.Read([]certfiles.File{{Name: appliance.InterfaceCertificateFile, Data: raw}}, "", c.roots(), now)
	if err != nil {
		return nil, fmt.Errorf("reading the stored certificate: %w", err)
	}
	source, order := storedHeader(string(raw))
	return newServed(b, source, order)
}

// storedHeader reads the source, and for ACME the order, from the comment lines that open the
// stored file. A file without them was uploaded.
func storedHeader(raw string) (string, string) {
	source, order := certificateUploaded, ""
	for line := range strings.Lines(raw) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, sourceLine):
			if s := strings.TrimPrefix(line, sourceLine); s == certificateSelfSigned || s == certificateACME {
				source = s
			}
		case strings.HasPrefix(line, orderLine):
			order = strings.TrimPrefix(line, orderLine)
		default:
			return source, order
		}
	}
	return source, order
}

// readInstallerFiles reads --tls-cert and --tls-key. An expired certificate is served all the same,
// since the interface warns about it and a locked-out administrator needs the page.
func (c *InterfaceCertificate) readInstallerFiles(now time.Time) (*servedCertificate, error) {
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return nil, fmt.Errorf("--tls-cert and --tls-key: %w", err)
	}
	served := &servedCertificate{pair: pair, source: certificateInstaller, chain: []*x509.Certificate{pair.Leaf}}
	if b := c.installerChain(pair.Leaf, now); b != nil {
		served.chain, served.incomplete, served.missing = b.Chain, b.Incomplete, b.MissingIssuer
	}
	return served, nil
}

// installerChain reads the installer's files for what they say of the chain, which administrators
// see. Nil when they do not read, which leaves the certificate served as it is.
func (c *InterfaceCertificate) installerChain(leaf *x509.Certificate, now time.Time) *certfiles.Bundle {
	certPEM, certErr := os.ReadFile(c.certFile)
	keyPEM, keyErr := os.ReadFile(c.keyFile)
	if certErr != nil || keyErr != nil {
		return nil
	}
	// Read judges the certificate's validity at the time it is given, so it is given one within it.
	at := now
	if at.After(leaf.NotAfter) {
		at = leaf.NotAfter
	}
	if at.Before(leaf.NotBefore) {
		at = leaf.NotBefore
	}
	b, err := certfiles.Read([]certfiles.File{{Name: filepath.Base(c.certFile), Data: certPEM}, {Name: filepath.Base(c.keyFile), Data: keyPEM}}, "", c.roots(), at)
	if err != nil {
		return nil
	}
	return b
}

func newServed(b *certfiles.Bundle, source, order string) (*servedCertificate, error) {
	pair, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("the certificate and key do not make a pair: %w", err)
	}
	pair.Leaf = b.Leaf
	return &servedCertificate{pair: pair, source: source, order: order, chain: b.Chain, incomplete: b.Incomplete, missing: b.MissingIssuer}, nil
}

// certificateFiles are the file operations a change makes, which a test fails one at a time.
type certificateFiles interface {
	CreateTemp(dir, pattern string) (certificateFile, error)
	Rename(oldPath, newPath string) error
	Remove(name string) error
}

type certificateFile interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

type osFiles struct{}

func (osFiles) CreateTemp(dir, pattern string) (certificateFile, error) { //nolint:ireturn // the file seam a test fails
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, fmt.Errorf("creating a file: %w", err)
	}
	return f, nil
}

func (osFiles) Rename(oldPath, newPath string) error {
	if err := os.Rename(oldPath, newPath); err != nil { //nolint:gosec // the temporary file made beside the stored one
		return fmt.Errorf("renaming a file: %w", err)
	}
	return nil
}

func (osFiles) Remove(name string) error {
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("removing a file: %w", err)
	}
	return nil
}

func (c *InterfaceCertificate) fileOps() certificateFiles { //nolint:ireturn // the file seam a test fails
	if c.files == nil {
		return osFiles{}
	}
	return c.files
}

// removeLeftovers deletes the temporary files a change cut off by a crash left, which hold a key.
func (c *InterfaceCertificate) removeLeftovers() {
	left, _ := filepath.Glob(c.storedPath() + ".*")
	for _, name := range left {
		_ = c.fileOps().Remove(name)
	}
}

// store writes the certificate to a temporary file beside the stored one, syncs it and renames it
// over the stored one, so that a crash leaves the old file or the new one.
func (c *InterfaceCertificate) store(b *certfiles.Bundle, source, order string) (err error) {
	f, err := c.fileOps().CreateTemp(c.dir, appliance.InterfaceCertificateFile+".*")
	if err != nil {
		return err //nolint:wrapcheck // the file operations name what failed
	}
	defer func() {
		if err != nil {
			_ = c.fileOps().Remove(f.Name())
		}
	}()
	header := sourceLine + source + "\n"
	if order != "" {
		header += orderLine + order + "\n"
	}
	data := slices.Concat([]byte(header), b.KeyPEM, b.CertPEM)
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the certificate: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing the certificate: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("closing the certificate: %w", err)
	}
	return c.fileOps().Rename(f.Name(), c.storedPath()) //nolint:wrapcheck // the file operations name what failed
}

// replace keeps b, which an administrator chose, and serves it. ACME stops with it, so that it never
// replaces a certificate chosen after it. The caller holds mu.
func (c *InterfaceCertificate) replace(b *certfiles.Bundle, source string) (*servedCertificate, error) {
	next, err := newServed(b, source, "")
	if err != nil {
		return nil, err
	}
	undo := c.stopACME()
	served, err := c.install(next, b)
	if err != nil {
		undo.Restore()
		return nil, err
	}
	return served, nil
}

// install keeps b, which next was made from, and serves next. The caller holds mu.
func (c *InterfaceCertificate) install(next *servedCertificate, b *certfiles.Bundle) (*servedCertificate, error) {
	if !c.memory {
		if err := c.store(b, next.source, next.order); err != nil {
			return nil, err
		}
	}
	c.live.Store(next)
	return next, nil
}

// reset serves the installer's certificate again and forgets the stored one, and ACME with it. The
// caller holds mu.
func (c *InterfaceCertificate) reset(now time.Time) (*servedCertificate, error) {
	next, err := c.readInstaller(now)
	if err != nil {
		return nil, err
	}
	undo := c.stopACME()
	if !c.memory {
		if err = c.fileOps().Remove(c.storedPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			undo.Restore()
			return nil, err //nolint:wrapcheck // the file operations name what failed
		}
	}
	c.live.Store(next)
	return next, nil
}

// certificateNames are the DNS names and addresses a certificate is valid for.
func certificateNames(cert *x509.Certificate) ([]string, []netip.Addr) {
	var ips []netip.Addr
	for _, ip := range cert.IPAddresses {
		if addr, ok := netip.AddrFromSlice(ip); ok {
			ips = append(ips, addr.Unmap())
		}
	}
	return slices.Clone(cert.DNSNames), ips
}

// certificateExpires is when the certificate being served runs out. Zero when not known.
func (s *Server) certificateExpires() time.Time {
	if served := s.servedCertificate(); served != nil {
		return served.pair.Leaf.NotAfter
	}
	return time.Time{}
}

// certificateCovers are the DNS names and addresses the certificate being served is for.
func (s *Server) certificateCovers() ([]string, []netip.Addr) {
	if served := s.servedCertificate(); served != nil {
		return certificateNames(served.pair.Leaf)
	}
	return nil, nil
}

func (s *Server) servedCertificate() *servedCertificate {
	if s.Certificate == nil {
		return nil
	}
	return s.Certificate.served()
}

// certificateView is the certificate being served, as administrators see it.
type certificateView struct {
	// Source is installer, uploaded or self-signed.
	Source      string    `json:"source"`
	Subject     string    `json:"subject"`
	DNSNames    []string  `json:"dnsNames"`
	IPAddresses []string  `json:"ipAddresses"`
	Issuer      string    `json:"issuer"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"`
	// Chain holds the subjects of the leaf and its intermediates, in order.
	Chain         []string `json:"chain"`
	Incomplete    bool     `json:"incomplete"`
	MissingIssuer string   `json:"missingIssuer,omitempty"`
	SelfSigned    bool     `json:"selfSigned"`
	PublicURL     string   `json:"publicURL"`
	// PublicNameResolves tells whether the public name leads to one of the appliance's own
	// addresses. Absent when there is no public name or the appliance does not know its addresses.
	PublicNameResolves *bool `json:"publicNameResolves,omitempty"`
	// NotServed is why the file an administrator stored is not served.
	NotServed string `json:"notServed,omitempty"`
	// Changeable tells whether the interface has somewhere to keep a changed certificate.
	Changeable bool `json:"changeable"`
	// ACME is the ACME setting and how its order stands. Absent when ACME is not set.
	ACME *acme.View `json:"acme,omitempty"`
}

// uploadView is the answer to an upload: the certificate now served, and what the files held that
// the chain does not use.
type uploadView struct {
	certificateView

	LeftOut []string `json:"leftOut"`
}

func (s *Server) certificateViewOf(ctx context.Context, c *InterfaceCertificate) certificateView {
	served := c.served()
	leaf := served.pair.Leaf
	names, ips := certificateNames(leaf)
	v := certificateView{
		Source: served.source, Subject: leaf.Subject.String(), DNSNames: names, IPAddresses: []string{}, Issuer: leaf.Issuer.String(),
		NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(), Fingerprint: appliance.Fingerprint(leaf), Chain: []string{},
		Incomplete: served.incomplete, MissingIssuer: served.missing, SelfSigned: isSelfSigned(leaf),
		PublicURL: s.PublicURL, NotServed: served.notServed, Changeable: c.canChange(),
	}
	if v.DNSNames == nil {
		v.DNSNames = []string{}
	}
	for _, ip := range ips {
		v.IPAddresses = append(v.IPAddresses, ip.String())
	}
	for _, cert := range served.chain {
		v.Chain = append(v.Chain, cert.Subject.String())
	}
	v.PublicNameResolves = s.publicNameResolves(ctx, c)
	if c.acme != nil {
		v.ACME = c.acme.View()
	}
	return v
}

// publicHost is the host of the public address, without a port or brackets. Empty when there is
// none.
func (s *Server) publicHost() string {
	u, err := url.Parse(s.PublicURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (s *Server) publicNameResolves(ctx context.Context, c *InterfaceCertificate) *bool {
	host := s.publicHost()
	if host == "" || s.OwnAddresses == nil {
		return nil
	}
	own := s.OwnAddresses()
	var found []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		found = []netip.Addr{addr.Unmap()}
	} else {
		lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		lookup := c.Lookup
		if lookup == nil {
			lookup = lookupHost
		}
		found, _ = lookup(lookupCtx, host)
	}
	resolves := slices.ContainsFunc(found, func(a netip.Addr) bool { return slices.Contains(own, a.Unmap()) })
	return &resolves
}

func lookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", host, err)
	}
	return addrs, nil
}

// writeCertificateError answers a refusal with a code the page can act on and a sentence it can
// show as it is.
func writeCertificateError(w http.ResponseWriter, status int, code, message string, names []string) {
	writeJSON(w, status, certificateRefusal{Error: message, Code: code, Names: names, RequestID: w.Header().Get(requestIDHeader)})
}

// certificateRefusal is the answer to a change that was refused.
type certificateRefusal struct {
	Error string `json:"error"`
	// Code is what a page acts on.
	Code string `json:"code"`
	// Names are those a certificate covers, for a refusal over a name it does not.
	Names     []string `json:"names,omitempty"`
	RequestID string   `json:"requestId,omitempty"`
}

// certificateTarget is the certificate a request changes or reads, once the user is known to be an
// administrator.
func (s *Server) certificateTarget(w http.ResponseWriter, r *http.Request, u user) *InterfaceCertificate {
	c := s.Certificate
	if c == nil || c.served() == nil {
		writeError(w, http.StatusNotFound, "the certificate is managed in the appliance")
		return nil
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return nil
	}
	return c
}

// certificateToChange is certificateTarget for a request that changes it, which needs somewhere to
// keep the change.
func (s *Server) certificateToChange(w http.ResponseWriter, r *http.Request, u user) *InterfaceCertificate {
	c := s.certificateTarget(w, r, u)
	if c != nil && !c.canChange() {
		writeCertificateError(w, http.StatusConflict, "no-appliance-directory", "The interface has no appliance directory to keep a certificate in. Start it with --appliance-dir", nil)
		return nil
	}
	return c
}

func (s *Server) showCertificate(w http.ResponseWriter, r *http.Request, u user) {
	c := s.certificateTarget(w, r, u)
	if c == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.certificateViewOf(r.Context(), c))
}

// lockChange takes the change lock, or refuses the request when another change holds it.
func lockChange(w http.ResponseWriter, c *InterfaceCertificate) bool {
	if !c.mu.TryLock() {
		writeCertificateError(w, http.StatusConflict, "busy", "Another change of the certificate is under way. Try again in a moment", nil)
		return false
	}
	return true
}

// changeFailed answers a change that could not be kept. Nothing was changed.
func (s *Server) changeFailed(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.ErrorContext(r.Context(), "interface certificate change", "error", err)
	writeCertificateError(w, http.StatusInternalServerError, "store", "The certificate could not be kept. Check the appliance directory and try again", nil)
}

// changed records a change, with the user and the new fingerprint and nothing of the key.
func (s *Server) changed(r *http.Request, u user, what string, served *servedCertificate) {
	s.Log.InfoContext(r.Context(), what, "user", u.ticket.Username, "source", served.source, "fingerprint", appliance.Fingerprint(served.pair.Leaf))
}

// uploadCertificate replaces the certificate with the one in the files sent.
func (s *Server) uploadCertificate(w http.ResponseWriter, r *http.Request, u user) {
	c := s.certificateToChange(w, r, u)
	if c == nil {
		return
	}
	files, passphrase, ok := readCertificateUpload(w, r)
	if !ok {
		return
	}
	now := s.now()
	b, err := certfiles.Read(files, passphrase, c.roots(), now)
	if err != nil {
		refuseCertificateFiles(w, err, passphrase)
		return
	}
	if !s.certificateFits(w, b.Leaf, now) {
		return
	}
	if !lockChange(w, c) {
		return
	}
	defer c.mu.Unlock()
	served, err := c.replace(b, certificateUploaded)
	if err != nil {
		s.changeFailed(w, r, err)
		return
	}
	s.changed(r, u, "interface certificate replaced", served)
	left := b.Dropped
	if left == nil {
		left = []string{}
	}
	writeJSON(w, http.StatusOK, uploadView{certificateView: s.certificateViewOf(r.Context(), c), LeftOut: left})
}

// readCertificateUpload reads the files and the passphrase of an upload, refusing one that holds
// too many or too large files before it reads more.
func readCertificateUpload(w http.ResponseWriter, r *http.Request) ([]certfiles.File, string, bool) {
	body := http.MaxBytesReader(w, r.Body, maxCertificateFiles*maxCertificateFile+uploadOverhead)
	r.Body = body
	reader, err := r.MultipartReader()
	if err != nil {
		writeCertificateError(w, http.StatusUnsupportedMediaType, "not-multipart", "Send the files as multipart/form-data", nil)
		return nil, "", false
	}
	up := &certificateUpload{w: w, body: body}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			refuseUploadRead(w, body, err)
			return nil, "", false
		}
		if !up.take(part) {
			return nil, "", false
		}
	}
	if len(up.files) == 0 {
		writeCertificateError(w, http.StatusUnprocessableEntity, "no-files", "The upload holds no file. Send the certificate and its key", nil)
		return nil, "", false
	}
	return up.files, up.passphrase, true
}

// certificateUpload collects the parts of an upload, answering the first one it refuses.
type certificateUpload struct {
	w          http.ResponseWriter
	body       io.Reader
	files      []certfiles.File
	passphrase string
}

// take keeps a part, or answers a refusal and tells it did not.
func (u *certificateUpload) take(part *multipart.Part) bool {
	switch part.FormName() {
	case "file":
		if len(u.files) == maxCertificateFiles {
			writeCertificateError(u.w, http.StatusRequestEntityTooLarge, "too-many-files", fmt.Sprintf("An upload holds at most %d files. Send fewer", maxCertificateFiles), nil)
			return false
		}
		name := part.FileName()
		if name == "" {
			name = fmt.Sprintf("file %d", len(u.files)+1)
		}
		data, err := io.ReadAll(io.LimitReader(part, maxCertificateFile+1))
		if err != nil {
			refuseUploadRead(u.w, u.body, err)
			return false
		}
		if len(data) > maxCertificateFile {
			writeCertificateError(u.w, http.StatusRequestEntityTooLarge, "file-too-large", name+" is larger than 1 MiB, more than a certificate file holds. Send the certificate file", nil)
			return false
		}
		u.files = append(u.files, certfiles.File{Name: name, Data: data})
	case "passphrase":
		data, err := io.ReadAll(io.LimitReader(part, maxPassphrase+1))
		if err != nil || len(data) > maxPassphrase {
			writeCertificateError(u.w, http.StatusBadRequest, "malformed", "The passphrase is longer than 4 KiB. Send the right one", nil)
			return false
		}
		u.passphrase = string(data)
	default:
		writeCertificateError(u.w, http.StatusBadRequest, "malformed", "An upload holds only file and passphrase fields. Send those", nil)
		return false
	}
	return true
}

// refuseUploadRead answers an upload whose body could not be read to the end. The multipart reader
// does not always pass the limit's error on, so the body is asked as well.
func refuseUploadRead(w http.ResponseWriter, body io.Reader, err error) {
	_, again := body.Read(make([]byte, 1))
	_, tooLarge := errors.AsType[*http.MaxBytesError](err)
	if _, limit := errors.AsType[*http.MaxBytesError](again); tooLarge || limit {
		writeCertificateError(w, http.StatusRequestEntityTooLarge, "too-large", "The upload is larger than 8 files of 1 MiB. Send the certificate files only", nil)
		return
	}
	writeCertificateError(w, http.StatusBadRequest, "malformed", "The upload is not valid multipart/form-data", nil)
}

// refuseCertificateFiles answers files certfiles could not read.
func refuseCertificateFiles(w http.ResponseWriter, err error, passphrase string) {
	if errors.Is(err, certfiles.ErrPassphrase) {
		file, _ := strings.CutSuffix(err.Error(), ": "+certfiles.ErrPassphrase.Error())
		message := file + " is encrypted. Send its passphrase"
		if passphrase != "" {
			message = "The passphrase does not open " + file + ". Send the right one"
		}
		writeCertificateError(w, http.StatusUnprocessableEntity, "passphrase", message, nil)
		return
	}
	writeCertificateError(w, http.StatusUnprocessableEntity, "unreadable", err.Error(), nil)
}

// certificateFits refuses a certificate with less than a day left or one that is not valid for the
// public host, and tells whether it passed.
func (s *Server) certificateFits(w http.ResponseWriter, leaf *x509.Certificate, now time.Time) bool {
	if leaf.NotAfter.Before(now.Add(certificateMinValidity)) {
		writeCertificateError(w, http.StatusUnprocessableEntity, "expires-soon",
			"The certificate ends on "+leaf.NotAfter.UTC().Format(time.DateOnly)+". Send one valid for at least another day", nil)
		return false
	}
	host := s.publicHost()
	if host == "" || leaf.VerifyHostname(host) == nil {
		return true
	}
	dns, ips := certificateNames(leaf)
	covers := dns
	for _, ip := range ips {
		covers = append(covers, ip.String())
	}
	list := strings.Join(covers, ", ")
	if list == "" {
		list = "no name"
	}
	writeCertificateError(w, http.StatusUnprocessableEntity, "wrong-name",
		fmt.Sprintf("The certificate is not valid for %s. It covers %s. Send one that covers it", host, list), append([]string{}, covers...))
	return false
}

// makeSelfSignedCertificate replaces the certificate with a new self-signed one for the names and
// addresses the served certificate covers.
func (s *Server) makeSelfSignedCertificate(w http.ResponseWriter, r *http.Request, u user) {
	c := s.certificateToChange(w, r, u)
	if c == nil {
		return
	}
	if !lockChange(w, c) {
		return
	}
	defer c.mu.Unlock()
	b, err := selfSigned(c.served().pair.Leaf, s.now())
	if errors.Is(err, errNoNames) {
		writeCertificateError(w, http.StatusUnprocessableEntity, "no-names", "The certificate being served names no DNS name or address, so a self-signed one has nothing to cover. Upload one", nil)
		return
	}
	if err != nil {
		s.changeFailed(w, r, err)
		return
	}
	served, err := c.replace(b, certificateSelfSigned)
	if err != nil {
		s.changeFailed(w, r, err)
		return
	}
	s.changed(r, u, "interface certificate made self-signed", served)
	writeJSON(w, http.StatusOK, s.certificateViewOf(r.Context(), c))
}

var errNoNames = errors.New("the certificate names nothing")

// selfSigned makes a self-signed certificate for the names of old, with the key type and lifetime
// the installer's own certificates have.
func selfSigned(old *x509.Certificate, now time.Time) (*certfiles.Bundle, error) {
	if len(old.DNSNames) == 0 && len(old.IPAddresses) == 0 {
		return nil, errNoNames
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("making a key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, fmt.Errorf("making a serial number: %w", err)
	}
	cn := old.Subject.CommonName
	if cn == "" && len(old.DNSNames) > 0 {
		cn = old.DNSNames[0]
	}
	start := now.Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: start, NotAfter: start.AddDate(0, 0, selfSignedDays),
		DNSNames: slices.Clone(old.DNSNames), IPAddresses: slices.Clone(old.IPAddresses),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	return bundleOf(tmpl, tmpl, key, key)
}

// demoAddress is the address the demo's appliance has, and its certificate is for.
var demoAddress = netip.MustParseAddr("192.0.2.10")

const (
	demoHost      = "kubehenge.example.org"
	demoPublicURL = "https://" + demoHost

	// The demo's certificates, relative to the day it shows.
	demoACMEDays      = 90
	demoDay           = 24 * time.Hour
	demoCAYears       = 9
	demoLeafSerial    = 2
	demoLeafDaysSince = 30
	demoLeafDaysLeft  = 335
)

// demoCertificate is the demo's: a certificate for example.org names and a documentation address,
// signed by a CA of its own, held in memory. Its keys come from fixed seeds, so that a demo of a
// fixed time shows the same fingerprint every run.
func demoCertificate(now time.Time) (*InterfaceCertificate, error) {
	day := now.UTC().Truncate(demoDay)
	seed := func(name string) crypto.Signer {
		sum := sha256.Sum256([]byte("kubehenge demo " + name))
		return ed25519.NewKeyFromSeed(sum[:])
	}
	caKey, leafKey := seed("ca"), seed("interface")
	ca, err := bundleOf(&x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Example Org Internal CA", Organization: []string{"Example Org"}},
		NotBefore: day.AddDate(-1, 0, 0), NotAfter: day.AddDate(demoCAYears, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, nil, caKey, caKey)
	if err != nil {
		return nil, err
	}
	leaf, err := bundleOf(&x509.Certificate{
		SerialNumber: big.NewInt(demoLeafSerial), Subject: pkix.Name{CommonName: demoHost},
		NotBefore: day.AddDate(0, 0, -demoLeafDaysSince), NotAfter: day.AddDate(0, 0, demoLeafDaysLeft),
		DNSNames: []string{demoHost, "kubehenge"}, IPAddresses: []net.IP{net.IP(demoAddress.AsSlice())},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca.Leaf, caKey, leafKey, ca.Leaf)
	if err != nil {
		return nil, err
	}
	installer, err := newServed(leaf, certificateInstaller, "")
	if err != nil {
		return nil, err
	}
	c := &InterfaceCertificate{
		Roots: x509.NewCertPool(), memory: true,
		Lookup:        func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{demoAddress}, nil },
		readInstaller: func(time.Time) (*servedCertificate, error) { copied := *installer; return &copied, nil },
	}
	// A certificate ordered through ACME in the demo is the same kind, from the same CA, with a key
	// of its own each time.
	c.demoIssue = func(at time.Time) ([]byte, []byte, error) {
		key, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			return nil, nil, fmt.Errorf("making a key: %w", keyErr)
		}
		b, issueErr := bundleOf(&x509.Certificate{
			SerialNumber: big.NewInt(at.Unix()), Subject: pkix.Name{CommonName: demoHost},
			NotBefore: at.Add(-time.Hour), NotAfter: at.AddDate(0, 0, demoACMEDays),
			DNSNames:    []string{demoHost},
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}, ca.Leaf, caKey, key, ca.Leaf)
		if issueErr != nil {
			return nil, nil, issueErr
		}
		return b.CertPEM, b.KeyPEM, nil
	}
	err = c.Load(now, nil)
	return c, err
}

// fillDemoCertificate gives the demo the certificate it serves, whose changes stay in memory, and
// the public name and address it is for. A certificate given beforehand is the demo's own.
func (s *Server) fillDemoCertificate(now time.Time) error {
	if s.Certificate == nil {
		var err error
		if s.Certificate, err = demoCertificate(now); err != nil {
			return fmt.Errorf("the demo's certificate: %w", err)
		}
	}
	s.Certificate.memory = true
	if s.PublicURL == "" {
		s.PublicURL = demoPublicURL
	}
	s.Certificate.useDemoACME(s.now)
	s.OwnAddresses = func() []netip.Addr { return []netip.Addr{demoAddress} }
	return nil
}

// bundleOf makes the certificate tmpl for key, signed by parent's signer, as the files of a
// certificate with its key and the certificates above it.
func bundleOf(tmpl, parent *x509.Certificate, signer, key crypto.Signer, above ...*x509.Certificate) (*certfiles.Bundle, error) {
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, key.Public(), signer)
	if err != nil {
		return nil, fmt.Errorf("making a certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("reading the certificate made: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("writing the key: %w", err)
	}
	b := &certfiles.Bundle{Leaf: leaf, Chain: append([]*x509.Certificate{leaf}, above...), KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})}
	for _, cert := range b.Chain {
		b.CertPEM = append(b.CertPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	return b, nil
}

// isSelfSigned tells whether a certificate is signed by its own key. A certificate that is no CA
// does not pass CheckSignatureFrom, so the signature is checked directly.
func isSelfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil
}

// resetCertificate serves the installer's certificate again.
func (s *Server) resetCertificate(w http.ResponseWriter, r *http.Request, u user) {
	c := s.certificateToChange(w, r, u)
	if c == nil {
		return
	}
	if !lockChange(w, c) {
		return
	}
	defer c.mu.Unlock()
	served, err := c.reset(s.now())
	if err != nil {
		s.changeFailed(w, r, err)
		return
	}
	s.changed(r, u, "interface certificate set back to the installer's", served)
	writeJSON(w, http.StatusOK, s.certificateViewOf(r.Context(), c))
}
