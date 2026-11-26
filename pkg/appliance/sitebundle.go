package appliance

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// siteCALabel and proxyCALabel open the messages about the pasted authorities, as the interface
// labels their fields.
const (
	siteCALabel  = "Site CA:"
	proxyCALabel = "Proxy certificate:"
)

// pemCertificate is the type of a PEM block that holds a certificate.
const pemCertificate = "CERTIFICATE"

// siteDialTimeout bounds the read of one host's chain, so that a host that does not answer holds a
// save up for no longer.
const siteDialTimeout = 6 * time.Second

// siteRegistries are the registries the management nodes pull from, each read in its mirrors' place
// where it has mirrors.
var siteRegistries = []string{"ghcr.io", "registry.k8s.io"}

// siteCheckTimeout bounds the whole check of the Site CA at a save, the hosts being read in parallel.
const siteCheckTimeout = 2 * siteDialTimeout

// noteSite tells the job's log which faults of the hosts' chains the run goes on past with the
// certificates of the last run, and which hosts did not answer.
func noteSite(j *Job, unchecked []string, kept SiteChainError) {
	if len(kept.Lines) > 0 {
		j.Add("Kept the certificates of the last run, since the nodes would refuse these chains:")
		for _, fault := range kept.Lines {
			j.Add(fault)
		}
	}
	if len(unchecked) > 0 {
		j.Add("The Site CA was not checked against " + strings.Join(unchecked, ", ") + ". They did not answer.")
	}
}

// SiteChains reads the chain a host presents over TLS, leaf first, as the appliance reaches it with
// the settings' proxy.
type SiteChains func(ctx context.Context, s *Settings, host string) ([]*x509.Certificate, error)

// siteTrust is the authority the nodes and the downloads trust for the site: the bundle derived from
// the Site CA when there is one, else the Site CA as pasted.
func (s *Settings) siteTrust() string {
	if s.SiteCA == "" {
		return ""
	}
	if s.SiteBundle != "" {
		return s.SiteBundle
	}
	return s.SiteCA
}

// siteHosts are the hosts whose chains the Site CA has to cover for the management nodes to come up:
// the Talos image factory, the registries or their https mirrors, Sigstore's TUF repository, and an
// https release mirror that has no authority of its own.
func (s *Settings) siteHosts() []string {
	hosts := []string{"factory.talos.dev"}
	add := func(address string) {
		if u, err := url.Parse(address); err == nil && u.Scheme == schemeHTTPS && u.Host != "" && !slices.Contains(hosts, u.Host) {
			hosts = append(hosts, u.Host)
		}
	}
	for _, registry := range siteRegistries {
		mirrors := s.RegistryMirrors[registry]
		if len(mirrors) == 0 {
			mirrors = []string{"https://" + registry}
		}
		for _, m := range mirrors {
			add(m)
		}
	}
	add("https://tuf-repo-cdn.sigstore.dev")
	if m := s.ReleaseMirror; m != nil && m.CABundle == "" {
		add(m.URL)
	}
	return hosts
}

// SiteTrustFor is the authority the nodes get for the Site CA ca: the saved bundle while ca is the
// saved Site CA, else ca as pasted.
func (a *Appliance) SiteTrustFor(ca string) string {
	if saved := a.SavedSettings(); saved != nil && saved.SiteCA == ca {
		return saved.siteTrust()
	}
	return ca
}

// SiteChainError are what keeps the nodes from trusting the chains the hosts present, one line each.
// Roots are the roots such hosts presented that the Site CA lacks, for the person to add.
type SiteChainError struct {
	Lines []string
	Roots []MissingRoot
}

// MissingRoot is a root a host presented that the Site CA lacks: its short name, for a button or a
// prompt to name it by, and the certificate as PEM.
type MissingRoot struct {
	Name string `json:"name"`
	PEM  string `json:"pem"`
}

// WithRoots is siteCA with each root appended that it does not hold yet.
func WithRoots(siteCA string, roots []MissingRoot) string {
	have := strings.TrimSpace(siteCA)
	parts := []string{have}
	for _, r := range roots {
		if p := strings.TrimSpace(r.PEM); !strings.Contains(have, p) && !slices.Contains(parts, p) {
			parts = append(parts, p)
		}
	}
	return strings.TrimPrefix(strings.Join(parts, "\n"), "\n") + "\n"
}

func (f SiteChainError) Error() string { return strings.Join(f.Lines, "\n") }

// ResolveSite derives the bundles the nodes and downloads trust from the pasted Site CA and proxy
// CA and the chains the hosts present, and sets them on s. The pasted authorities stay as they are.
// A host that does not answer is returned in unchecked, and fails nothing. A chain the nodes would
// refuse with the bundles fails with SiteChainError.
func (a *Appliance) ResolveSite(ctx context.Context, s *Settings) ([]string, error) {
	authorities, err := a.siteAuthorities(s)
	clearBundles(s)
	if err != nil {
		return nil, err
	}
	if len(authorities) == 0 || a.SiteChains == nil {
		return nil, nil
	}
	// An https proxy is verified with the copies the last run found, as the downloads verify it.
	for _, auth := range authorities {
		*auth.bundle = auth.derived()
	}
	answered, unchecked := a.readChains(ctx, s)
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if err := judgeChains(authorities, a.publicRoots(), answered, now); err != nil {
		clearBundles(s)
		return unchecked, err
	}
	for _, auth := range authorities {
		*auth.bundle = auth.derived()
	}
	return unchecked, nil
}

func clearBundles(s *Settings) {
	s.SiteBundle = ""
	if s.Proxy != nil {
		s.Proxy.Bundle = ""
	}
}

// resolveSiteKeeping is ResolveSite for a run that leaves the Site CA and the proxy's CA as they
// were saved. A chain the nodes would refuse keeps the bundles of the last run and comes back in
// kept, since the nodes go on as they are and the fault should not block other settings. A first or
// changed authority still fails.
func (a *Appliance) resolveSiteKeeping(ctx context.Context, s *Settings) ([]string, SiteChainError, error) {
	unchecked, err := a.ResolveSite(ctx, s)
	faults, ok := errors.AsType[SiteChainError](err)
	saved := a.SavedSettings()
	if !ok || saved == nil || saved.SiteCA != s.SiteCA || proxyCAOf(saved) != proxyCAOf(s) {
		return unchecked, SiteChainError{}, err
	}
	s.SiteBundle = saved.SiteBundle
	if s.Proxy != nil && saved.Proxy != nil {
		s.Proxy.Bundle = saved.Proxy.Bundle
	}
	return unchecked, faults, nil
}

// proxyCAOf is the CA of the settings' proxy, empty without a proxy.
func proxyCAOf(s *Settings) string {
	if !s.Proxy.Enabled() {
		return ""
	}
	return s.Proxy.CABundle
}

// SiteCheck is what CheckSite found: the errors a save would refuse with, the warnings of faults it
// goes on past with the last run's certificates, and the roots that the hosts presented and the Site
// CA lacks. All are empty where the nodes would trust every host that answered.
type SiteCheck struct {
	Errors       []string
	Warnings     []string
	MissingRoots []MissingRoot
}

// CheckSite runs the check a save runs, on a copy of s, for the setup to show its faults before a
// save.
func (a *Appliance) CheckSite(ctx context.Context, s *Settings) SiteCheck {
	c := *s
	if s.Proxy != nil {
		proxy := *s.Proxy
		c.Proxy = &proxy
	}
	_, kept, err := a.resolveSiteKeeping(ctx, &c)
	if faults, ok := errors.AsType[SiteChainError](err); ok {
		return SiteCheck{Errors: faults.Lines, MissingRoots: faults.Roots}
	}
	if err != nil {
		return SiteCheck{Errors: []string{err.Error()}}
	}
	return SiteCheck{Warnings: kept.Lines, MissingRoots: kept.Roots}
}

// readChains reads what each host presents, the hosts in parallel, each within siteDialTimeout.
func (a *Appliance) readChains(ctx context.Context, s *Settings) ([]hostChain, []string) {
	hosts := s.siteHosts()
	chains := make([][]*x509.Certificate, len(hosts))
	failed := make([]error, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Go(func() {
			dial, cancel := context.WithTimeout(ctx, siteDialTimeout)
			defer cancel()
			chains[i], failed[i] = a.SiteChains(dial, s, host)
		})
	}
	wg.Wait()
	var answered []hostChain
	var unchecked []string
	for i, host := range hosts {
		if failed[i] != nil || len(chains[i]) == 0 {
			unchecked = append(unchecked, host)
			continue
		}
		answered = append(answered, hostChain{host: host, chain: chains[i]})
	}
	return answered, unchecked
}

// siteAuthority is an authority the owner pasted: its roots, the renamed copies of them the nodes
// need beside them, and the field of the settings its bundle goes in.
type siteAuthority struct {
	label  string
	pasted string
	roots  []*x509.Certificate
	copies []*x509.Certificate
	bundle *string
}

// siteAuthorities are the pasted Site CA and proxy CA. Each keeps the copies the saved bundle holds
// while the saved authority is the same, so that a run whose hosts do not answer, or answer
// otherwise, takes none away from the nodes and rolls no cluster.
func (a *Appliance) siteAuthorities(s *Settings) ([]*siteAuthority, error) {
	saved := a.SavedSettings()
	if saved == nil {
		saved = &Settings{}
	}
	var out []*siteAuthority
	if s.SiteCA != "" {
		kept := ""
		if saved.SiteCA == s.SiteCA {
			kept = saved.SiteBundle
		}
		auth, err := newSiteAuthority(siteCALabel, s.SiteCA, kept, &s.SiteBundle)
		if err != nil {
			return nil, err
		}
		out = append(out, auth)
	}
	if s.Proxy.Enabled() && s.Proxy.CABundle != "" {
		kept := ""
		if saved.Proxy.Enabled() && saved.Proxy.CABundle == s.Proxy.CABundle {
			kept = saved.Proxy.Bundle
		}
		auth, err := newSiteAuthority(proxyCALabel, s.Proxy.CABundle, kept, &s.Proxy.Bundle)
		if err != nil {
			return nil, err
		}
		out = append(out, auth)
	}
	return out, nil
}

func newSiteAuthority(label, pasted, kept string, bundle *string) (*siteAuthority, error) {
	roots, err := parseCertificates(pasted)
	if err != nil {
		return nil, fmt.Errorf("%s the pasted text %w", label, err)
	}
	auth := &siteAuthority{label: label, pasted: pasted, roots: roots, bundle: bundle}
	held, _ := parseCertificates(kept)
	for _, c := range held {
		// Only a renamed root carries a pasted root's key.
		if !slices.ContainsFunc(roots, c.Equal) && slices.ContainsFunc(roots, sameKey(c)) {
			auth.add(c)
		}
	}
	return auth, nil
}

func (s *siteAuthority) add(c *x509.Certificate) {
	if !slices.ContainsFunc(s.copies, c.Equal) {
		s.copies = append(s.copies, c)
	}
}

// derived is the pasted text followed by the copies in a fixed order, so that the same copies give
// the same bundle. Empty when the pasted roots need none.
func (s *siteAuthority) derived() string {
	if len(s.copies) == 0 {
		return ""
	}
	slices.SortFunc(s.copies, func(x, y *x509.Certificate) int { return bytes.Compare(x.Raw, y.Raw) })
	var out strings.Builder
	out.WriteString(s.pasted)
	if !strings.HasSuffix(s.pasted, "\n") {
		out.WriteByte('\n')
	}
	for _, c := range s.copies {
		out.Write(pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: c.Raw}))
	}
	return out.String()
}

// publicRoots are the authorities the nodes ship with, whose chains are no concern of the pasted
// ones.
func (a *Appliance) publicRoots() *x509.CertPool {
	if a.sitePublic != nil {
		return a.sitePublic
	}
	// The appliance container's own store is no stand-in: it also holds what the installer adds
	// for Proxmox VE, which the nodes do not trust.
	return nodeRoots()
}

// DialChain is the SiteChains of the network: it opens a TLS connection to the host, through the
// settings' proxy when there is one, and returns what the host presented without judging it. An
// https proxy is verified as every download verifies it, so that its credentials go nowhere else,
// and a proxy that fails that or refuses the tunnel leaves the host unread.
func (a *Appliance) DialChain(ctx context.Context, s *Settings, host string) ([]*x509.Certificate, error) {
	transport, err := s.Proxy.Transport(s.NoProxyList(a.ProxmoxURL))
	if err != nil {
		return nil, fmt.Errorf("the proxy setting: %w", err)
	}
	if !s.Proxy.Enabled() {
		transport.Proxy = http.ProxyFromEnvironment
	}
	if err = trustSiteCA(transport, s.siteTrust()); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", host, err)
	}
	proxy, err := transport.Proxy(req)
	if err != nil {
		return nil, fmt.Errorf("%s: the proxy: %w", host, err)
	}
	// Go's transport hands the proxy's handshake and the host's the same TLS configuration, the
	// proxy's first.
	verifyProxy := proxy != nil && proxy.Scheme == "https"
	proxyName := ""
	if verifyProxy {
		proxyName = proxy.Hostname()
	}
	var proxyRoots *x509.CertPool
	if transport.TLSClientConfig != nil {
		proxyRoots = transport.TLSClientConfig.RootCAs
	}
	var mu sync.Mutex
	var presented []*x509.Certificate
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection verifies the proxy, and judgeChains judges the host's chain
		VerifyConnection: func(cs tls.ConnectionState) error {
			mu.Lock()
			defer mu.Unlock()
			if verifyProxy {
				verifyProxy = false
				return verifyPeer(cs.PeerCertificates, proxyRoots, proxyName)
			}
			presented = cs.PeerCertificates
			return nil
		},
	}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(presented) > 0 {
		return presented, nil
	}
	if err == nil {
		err = errors.New("no certificate presented")
	}
	return nil, fmt.Errorf("%s: %w", host, err)
}

// verifyPeer is the check Go's TLS client makes of a server: its chain to roots, the system's when
// nil, and its name.
func verifyPeer(peer []*x509.Certificate, roots *x509.CertPool, name string) error {
	if len(peer) == 0 {
		return errors.New("the proxy presented no certificate")
	}
	intermediates := x509.NewCertPool()
	for _, c := range peer[1:] {
		intermediates.AddCert(c)
	}
	_, err := peer[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: name})
	if err != nil {
		return fmt.Errorf("the proxy's certificate: %w", err)
	}
	return nil
}

// hostChain is the chain one host presented, leaf first.
type hostChain struct {
	host  string
	chain []*x509.Certificate
}

// judgeChains adds to the authorities the renamed roots that make Go's verifier, as the nodes run
// it, accept each chain that no public authority signs. Go then checks the roots' path length, name
// constraints and validity itself. A chain it still refuses is a fault, and the hosts with the same
// fault share its line. A fault that is a presented root the Site CA lacks also hands that root
// over, unless it is a public root, which the nodes hold already.
func judgeChains(authorities []*siteAuthority, public *x509.CertPool, chains []hostChain, now time.Time) error {
	type labelled struct{ label, fault string }
	var faults []labelled
	hosts := map[labelled][]string{}
	var roots []*x509.Certificate
	for _, hc := range chains {
		if verifyChain(hc.chain, public, now) == nil {
			continue
		}
		if fault := nodesRefusal(authorities, public, hc.chain, now); fault.text != "" {
			label := authorityOf(authorities, hc.chain).label
			key := labelled{label, fault.text}
			if _, seen := hosts[key]; !seen {
				faults = append(faults, key)
			}
			hosts[key] = append(hosts[key], hc.host)
			if fault.root != nil && label == siteCALabel && !slices.ContainsFunc(roots, fault.root.Equal) {
				roots = append(roots, fault.root)
			}
		}
	}
	if len(faults) == 0 {
		return nil
	}
	out := SiteChainError{Lines: make([]string, 0, len(faults))}
	for _, key := range faults {
		out.Lines = append(out.Lines, key.label+" "+strings.Join(hosts[key], ", ")+": "+key.fault)
	}
	out.Roots = missingRoots(roots)
	return out
}

// missingRoots are the roots to hand over, each named for a button or a prompt. The start of the
// fingerprint tells apart two roots of one name.
func missingRoots(roots []*x509.Certificate) []MissingRoot {
	out := make([]MissingRoot, 0, len(roots))
	for _, r := range roots {
		name := shortName(r)
		if slices.ContainsFunc(roots, func(o *x509.Certificate) bool { return !o.Equal(r) && shortName(o) == name }) {
			name += " (" + Fingerprint(r)[:11] + ")"
		}
		out = append(out, MissingRoot{Name: name, PEM: string(pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: r.Raw}))})
	}
	return out
}

// nodesRefusal is what keeps the nodes from trusting chain (judgeChain). Where the chain leads to
// one of the nodes' public roots for some usage, or ends at one, which the nodes hold already, the
// fault is in the chain and no Site CA entry mends it.
func nodesRefusal(authorities []*siteAuthority, public *x509.CertPool, chain []*x509.Certificate, now time.Time) refusal {
	fault := judgeChain(authorities, chain, now)
	if fault.text != "" && (leadsToPublicRoot(chain, public, now) || fault.root != nil && isPublicRoot(fault.root, public, now)) {
		return publicRefusal(chain, public, now)
	}
	return fault
}

// leadsToPublicRoot says whether chain leads to one of the nodes' public roots for any usage.
func leadsToPublicRoot(chain []*x509.Certificate, public *x509.CertPool, now time.Time) bool {
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{Roots: public, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err == nil
}

// isPublicRoot says whether root is one of the nodes' public roots.
func isPublicRoot(root *x509.Certificate, public *x509.CertPool, now time.Time) bool {
	_, err := root.Verify(x509.VerifyOptions{Roots: public, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err == nil
}

// publicRefusal says why the nodes refuse a chain that ends at one of their public roots, which
// adding that root to the Site CA would not change.
func publicRefusal(chain []*x509.Certificate, public *x509.CertPool, now time.Time) refusal {
	if invalid, ok := errors.AsType[x509.CertificateInvalidError](verifyChain(chain, public, now)); ok {
		if fault := verifyFault(invalid); fault != "" {
			return refusal{text: fault}
		}
	}
	return refusal{text: fmt.Sprintf("the nodes refuse %q. Check the chain on the device.", subjectOf(chain[0]))}
}

// authorityOf is the authority a refused chain is about: the one whose root a certificate of the
// chain names as its issuer or names the key of, else the proxy's, which re-signs what it carries.
func authorityOf(authorities []*siteAuthority, chain []*x509.Certificate) *siteAuthority {
	for _, c := range chain {
		for _, auth := range authorities {
			for _, r := range slices.Concat(auth.roots, auth.copies) {
				if canonicalName(c.RawIssuer) == canonicalName(r.RawSubject) || (len(c.AuthorityKeyId) > 0 && bytes.Equal(c.AuthorityKeyId, r.SubjectKeyId)) {
					return auth
				}
			}
		}
	}
	return authorities[len(authorities)-1]
}

// refusal is what keeps the nodes from trusting a chain, and the root it ends at where the host
// presented it and the pasted authority lacks it.
type refusal struct {
	text string
	root *x509.Certificate
}

// field is how the messages name the setting the authority was pasted into.
func (s *siteAuthority) field() string {
	if s.label == proxyCALabel {
		return "the proxy's CA"
	}
	return "the Site CA"
}

// lackingRoot says that the chain ends at root, which the host presented and the authority lacks.
// The fingerprint tells it from another root of the same name.
func (s *siteAuthority) lackingRoot(root *x509.Certificate) refusal {
	return refusal{fmt.Sprintf("Chain ends at %q (SHA-256 %s). Add that root to %s.", subjectOf(root), Fingerprint(root), s.field()), root}
}

// lackingIssuer says that nothing the host presented or the authority holds signs top, the
// topmost certificate of the path. Where a presented certificate the path does not reach leads to
// a pasted root, the issuer is an intermediate between the two, which the device can send. Else
// nothing tells a root from an intermediate the device leaves out, and adding either to the
// authority makes the nodes accept the chain.
func (s *siteAuthority) lackingIssuer(top *x509.Certificate, anchors, presented, path []*x509.Certificate, now time.Time) refusal {
	roots, intermediates := poolOf(anchors), poolOf(presented)
	above := slices.ContainsFunc(presented, func(p *x509.Certificate) bool {
		if slices.ContainsFunc(path, p.Equal) {
			return false
		}
		_, err := p.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		return err == nil
	})
	if above {
		return refusal{text: fmt.Sprintf("Have the device send %q, or add it to %s.", issuerOf(top), s.field())}
	}
	return refusal{text: fmt.Sprintf("Add %q to %s.", issuerOf(top), s.field())}
}

// judgeChain adds the renamed roots that make Go accept one chain, or names what keeps it from
// doing so.
func judgeChain(authorities []*siteAuthority, chain []*x509.Certificate, now time.Time) refusal {
	anchors := anchorsOf(authorities)
	err := verifyChain(chain, poolOf(anchors), now)
	if err == nil {
		return refusal{}
	}
	found := renamedRoots(authorities, chain)
	if len(found) > 0 {
		for _, f := range found {
			anchors = append(anchors, f.cert)
		}
		if err = verifyChain(chain, poolOf(anchors), now); err == nil {
			for _, f := range found {
				f.auth.add(f.cert)
			}
			return refusal{}
		}
	}
	return chainFault(chain, anchors, authorityOf(authorities, chain), err, now)
}

// anchorsOf are the pasted roots and the copies found so far.
func anchorsOf(authorities []*siteAuthority) []*x509.Certificate {
	var out []*x509.Certificate
	for _, auth := range authorities {
		out = append(out, auth.roots...)
		out = append(out, auth.copies...)
	}
	return out
}

func poolOf(certs []*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	return pool
}

// verifyChain is the nodes' check: Go's verifier with the presented intermediates, for a server.
func verifyChain(chain []*x509.Certificate, roots *x509.CertPool, now time.Time) error {
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err //nolint:wrapcheck // chainFault reads the verifier's own error
}

// renamedRoot is a copy of a pasted root under the name a presented certificate gives its issuer.
type renamedRoot struct {
	auth *siteAuthority
	cert *x509.Certificate
}

// renamedRoots are the copies of the pasted roots for each presented certificate that a root signs
// and that names the root in other bytes, as RFC 5280 and OpenSSL compare names and Go does not.
func renamedRoots(authorities []*siteAuthority, chain []*x509.Certificate) []renamedRoot {
	var out []renamedRoot
	for _, c := range chain {
		for _, auth := range authorities {
			for _, r := range auth.roots {
				if bytes.Equal(c.RawIssuer, r.RawSubject) || canonicalName(c.RawIssuer) != canonicalName(r.RawSubject) || c.CheckSignatureFrom(r) != nil {
					continue
				}
				if renamed, err := withSubject(r, c.RawIssuer); err == nil {
					out = append(out, renamedRoot{auth: auth, cert: renamed})
				}
			}
		}
	}
	return out
}

// chainFault names the certificate that breaks a chain Go refuses, and what to do about it. It
// follows the chain up by names as OpenSSL compares them, then reads the verifier's error.
func chainFault(chain, anchors []*x509.Certificate, auth *siteAuthority, verifyErr error, now time.Time) refusal {
	cur, path := chain[0], []*x509.Certificate{chain[0]}
	for {
		if fault := lapse(cur, now); fault != "" {
			return refusal{text: fault}
		}
		if slices.ContainsFunc(anchors, cur.Equal) {
			break
		}
		parent, fault := issuerIn(cur, anchors, chain[1:], path, auth, now)
		if fault.text != "" {
			return fault
		}
		if fault := presentedFault(cur, parent, anchors, auth); fault.text != "" {
			return fault
		}
		path = append(path, parent)
		cur = parent
	}
	if invalid, ok := errors.AsType[x509.CertificateInvalidError](verifyErr); ok {
		if fault := verifyFault(invalid); fault != "" {
			return refusal{text: fault}
		}
	}
	return refusal{text: fmt.Sprintf("the nodes refuse %q. Check the chain on the device.", subjectOf(chain[0]))}
}

// verifyFault names a refusal of Go's verifier that following the chain by names does not find,
// empty for another.
func verifyFault(invalid x509.CertificateInvalidError) string {
	reason, c := invalid.Reason, invalid.Cert
	if reason == x509.TooManyIntermediates {
		return fmt.Sprintf("%q allows fewer intermediates than presented. Shorten the chain on the device.", subjectOf(c))
	}
	if reason == x509.CANotAuthorizedForThisName {
		return fmt.Sprintf("%q is outside its issuers' name constraints. Check them on the device.", subjectOf(c))
	}
	if reason == x509.NotAuthorizedToSign {
		return notCA(c)
	}
	if reason == x509.IncompatibleUsage {
		return fmt.Sprintf("%q is not for servers. Reissue it for server authentication.", subjectOf(c))
	}
	return ""
}

// presentedFault says what keeps a presented parent of cur from carrying the chain to a pasted
// root: it is another root, or cur names it in other bytes. Empty for a pasted root or a copy.
func presentedFault(cur, parent *x509.Certificate, anchors []*x509.Certificate, auth *siteAuthority) refusal {
	switch {
	case slices.ContainsFunc(anchors, parent.Equal):
		return refusal{}
	case canonicalName(parent.RawIssuer) == canonicalName(parent.RawSubject) && !slices.ContainsFunc(anchors, sameKey(parent)):
		return auth.lackingRoot(parent)
	case !bytes.Equal(cur.RawIssuer, parent.RawSubject):
		return refusal{text: fmt.Sprintf("%q encodes its issuer's name %q differently. Reissue it on the device.", subjectOf(cur), subjectOf(parent))}
	}
	return refusal{}
}

// issuerIn finds the certificate that signs cur among the anchors and the presented certificates,
// those already on the path left out, or says why none does.
func issuerIn(cur *x509.Certificate, anchors, presented, path []*x509.Certificate, auth *siteAuthority, now time.Time) (*x509.Certificate, refusal) {
	if bytes.Equal(cur.RawIssuer, cur.RawSubject) {
		return nil, auth.lackingRoot(cur)
	}
	issuer := canonicalName(cur.RawIssuer)
	fault := ""
	for _, p := range slices.Concat(anchors, presented) {
		if slices.ContainsFunc(path, p.Equal) || canonicalName(p.RawSubject) != issuer {
			continue
		}
		err := cur.CheckSignatureFrom(p)
		_, insecure := errors.AsType[x509.InsecureAlgorithmError](err)
		switch {
		case err == nil:
			return p, refusal{}
		case insecure:
			return nil, refusal{text: fmt.Sprintf("%q is signed with %s. Nodes refuse that. Reissue it with SHA-256.", subjectOf(cur), cur.SignatureAlgorithm)}
		case !p.BasicConstraintsValid || !p.IsCA:
			fault = notCA(p)
		case p.KeyUsage != 0 && p.KeyUsage&x509.KeyUsageCertSign == 0:
			fault = fmt.Sprintf("%q may not sign certificates. Reissue it with the certificate signing key usage.", subjectOf(p))
		case slices.ContainsFunc(anchors, p.Equal):
			// The device's root was made again under the same name, with another key.
			fault = fmt.Sprintf("%q is signed by a new %q. Paste the new root.", subjectOf(cur), subjectOf(p))
		}
	}
	if fault == "" {
		return nil, auth.lackingIssuer(cur, anchors, presented, path, now)
	}
	return nil, refusal{text: fault}
}

// sameKey matches a certificate that carries c's key.
func sameKey(c *x509.Certificate) func(*x509.Certificate) bool {
	return func(o *x509.Certificate) bool {
		return bytes.Equal(o.RawSubjectPublicKeyInfo, c.RawSubjectPublicKeyInfo)
	}
}

func notCA(c *x509.Certificate) string {
	return fmt.Sprintf("%q is no CA. Reissue it as a CA on the device.", subjectOf(c))
}

// lapse says that c is not valid at now, empty while it is.
func lapse(c *x509.Certificate, now time.Time) string {
	switch {
	case now.Before(c.NotBefore):
		return fmt.Sprintf("%q is valid from %s. Check the device's clock.", subjectOf(c), c.NotBefore.Format(time.DateOnly))
	case now.After(c.NotAfter):
		return fmt.Sprintf("%q expired on %s. Renew it on the device.", subjectOf(c), c.NotAfter.Format(time.DateOnly))
	}
	return ""
}

// parseCertificates reads the certificates of a PEM bundle.
func parseCertificates(bundle string) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != pemCertificate {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("holds a certificate that does not parse: %w", err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, errors.New("holds no PEM certificate")
	}
	return certs, nil
}

func subjectOf(c *x509.Certificate) string { return nameText(c.Subject.String(), c.RawSubject) }

func issuerOf(c *x509.Certificate) string { return nameText(c.Issuer.String(), c.RawIssuer) }

// nameText is a distinguished name for a person to read: Go's text of it, else the values as
// canonicalName decodes them, since Go leaves out values in UniversalString.
func nameText(goText string, raw []byte) string {
	if goText != "" {
		return goText
	}
	rdns, ok := nameAttributes(raw)
	if !ok {
		return ""
	}
	seq := make(pkix.RDNSequence, 0, len(rdns))
	for _, rdn := range rdns {
		set := make(pkix.RelativeDistinguishedNameSET, 0, len(rdn))
		for _, atv := range rdn {
			var value any = atv.Value
			if text, canon, ok := nameString(atv.Value); canon && ok {
				value = text
			}
			set = append(set, pkix.AttributeTypeAndValue{Type: atv.Type, Value: value})
		}
		seq = append(seq, set)
	}
	return seq.String()
}

// nameAttribute is one attribute of a distinguished name, with its value as encoded.
type nameAttribute struct {
	Type  asn1.ObjectIdentifier
	Value asn1.RawValue
}

// nameAttributesSET is one relative distinguished name. Go's asn1 package reads a slice type whose
// name ends in SET as a SET OF.
type nameAttributesSET []nameAttribute

func nameAttributes(raw []byte) ([]nameAttributesSET, bool) {
	var rdns []nameAttributesSET
	rest, err := asn1.Unmarshal(raw, &rdns)
	return rdns, err == nil && len(rest) == 0
}

// canonicalName is a distinguished name as OpenSSL compares it, after x509_name_canon and
// asn1_string_canon in crypto/x509/x_name.c. A value of a string type in ASN1_MASK_CANON becomes
// UTF-8 folded by canonicalValue, so its type no longer counts. A value of any other type keeps its
// tag and bytes, so it matches only the same type. A name OpenSSL cannot load matches no other. Go
// compares the encoded bytes instead.
func canonicalName(raw []byte) string {
	unloadable := "!" + hex.EncodeToString(raw)
	rdns, ok := nameAttributes(raw)
	if !ok {
		return unloadable
	}
	var sb strings.Builder
	for _, rdn := range rdns {
		parts := make([]string, 0, len(rdn))
		for _, atv := range rdn {
			text, canon, ok := nameString(atv.Value)
			switch {
			case !ok:
				return unloadable
			case canon:
				parts = append(parts, atv.Type.String()+"="+strconv.Quote(canonicalValue(text)))
			default:
				parts = append(parts, atv.Type.String()+"=#"+hex.EncodeToString(atv.Value.FullBytes))
			}
		}
		// The canonical encoding is a DER SET OF, whose order is fixed.
		slices.Sort(parts)
		sb.WriteString(strings.Join(parts, "+"))
		sb.WriteByte(';')
	}
	return sb.String()
}

// The ASN.1 string types Go's asn1 package has no constant for.
const (
	tagVisibleString   = 26
	tagUniversalString = 28
)

// The bytes a character of BMPString and of UniversalString takes.
const (
	bmpWidth       = 2
	universalWidth = 4
)

// nameString decodes a value of a string type in OpenSSL's ASN1_MASK_CANON to UTF-8, as
// ASN1_STRING_to_UTF8 does: the one-byte types as Latin-1, BMPString as UCS-2 and UniversalString as
// UCS-4, both big-endian. The first flag is false for a value of another type. The second is false
// for one OpenSSL cannot convert, and for VisibleString, which OpenSSL's name entries cannot hold
// (their value is an ASN1_PRINTABLE, whose B_ASN1_PRINTABLE leaves VisibleString out).
func nameString(v asn1.RawValue) (string, bool, bool) {
	if v.Class != asn1.ClassUniversal || v.IsCompound {
		return "", false, true
	}
	switch v.Tag {
	case asn1.TagUTF8String:
		return string(v.Bytes), true, utf8.Valid(v.Bytes)
	case asn1.TagPrintableString, asn1.TagT61String, asn1.TagIA5String:
		runes := make([]rune, len(v.Bytes))
		for i, b := range v.Bytes {
			runes[i] = rune(b)
		}
		return string(runes), true, true
	case asn1.TagBMPString:
		text, ok := ucs(v.Bytes, bmpWidth)
		return text, true, ok
	case tagUniversalString:
		text, ok := ucs(v.Bytes, universalWidth)
		return text, true, ok
	case tagVisibleString:
		return "", true, false
	}
	return "", false, true
}

// ucs decodes big-endian code points of width bytes each, refusing surrogates and values past
// Unicode as OpenSSL's UTF8_putc does.
func ucs(b []byte, width int) (string, bool) {
	if len(b)%width != 0 {
		return "", false
	}
	var out strings.Builder
	for i := 0; i < len(b); i += width {
		var r uint32
		if width == bmpWidth {
			r = uint32(binary.BigEndian.Uint16(b[i:]))
		} else {
			r = binary.BigEndian.Uint32(b[i:])
		}
		if r > unicode.MaxRune || (0xD800 <= r && r <= 0xDFFF) {
			return "", false
		}
		out.WriteRune(rune(r))
	}
	return out.String(), true
}

// canonicalValue folds a value as OpenSSL's asn1_string_canon does: ASCII letters to lower case,
// each run of ASCII white space to one space and none at the ends. Every other character stays, so
// names that differ in the case of a non-ASCII letter stay apart, as OpenSSL keeps them.
func canonicalValue(s string) string {
	var out strings.Builder
	space := false
	for i := range len(s) {
		c := s[i]
		if c == ' ' || ('\t' <= c && c <= '\r') {
			space = out.Len() > 0
			continue
		}
		if space {
			out.WriteByte(' ')
			space = false
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out.WriteByte(c)
	}
	return out.String()
}

// withSubject is a copy of c whose subject is the encoded name, with the same key, extensions and
// signature. The signature no longer covers the certificate, which Go never checks for a root it is
// given.
func withSubject(c *x509.Certificate, name []byte) (*x509.Certificate, error) {
	var outer struct{ TBS, Algorithm, Signature asn1.RawValue }
	if rest, err := asn1.Unmarshal(c.Raw, &outer); err != nil || len(rest) > 0 {
		return nil, errors.New("holds a certificate that does not parse")
	}
	var fields []asn1.RawValue
	for body := outer.TBS.Bytes; len(body) > 0; {
		var f asn1.RawValue
		var err error
		if body, err = asn1.Unmarshal(body, &f); err != nil {
			return nil, errors.New("holds a certificate that does not parse")
		}
		fields = append(fields, f)
	}
	// A version 3 certificate leads with its version, then the serial number, the signature
	// algorithm, the issuer and the validity.
	subject := 4
	if len(fields) > 0 && fields[0].Class == asn1.ClassContextSpecific && fields[0].Tag == 0 {
		subject = 5
	}
	if len(fields) <= subject || !bytes.Equal(fields[subject].FullBytes, c.RawSubject) {
		return nil, errors.New("holds a certificate whose name cannot be copied under another encoding")
	}
	var tbs []byte
	for i, f := range fields {
		if i == subject {
			tbs = append(tbs, name...)
			continue
		}
		tbs = append(tbs, f.FullBytes...)
	}
	sequence := func(body []byte) ([]byte, error) {
		return asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: body})
	}
	tbsDER, err := sequence(tbs)
	if err != nil {
		return nil, errors.New("holds a certificate whose name cannot be copied under another encoding")
	}
	der, err := sequence(slices.Concat(tbsDER, outer.Algorithm.FullBytes, outer.Signature.FullBytes))
	if err != nil {
		return nil, errors.New("holds a certificate whose name cannot be copied under another encoding")
	}
	renamed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errors.New("holds a certificate that does not parse")
	}
	return renamed, nil
}
