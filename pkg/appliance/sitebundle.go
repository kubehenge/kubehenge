package appliance

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// siteCALabel and proxyCALabel open the messages about the pasted authorities, as the interface
// labels their fields.
const (
	siteCALabel  = "Site CA:"
	proxyCALabel = "Proxy certificate:"
)

// siteDialTimeout bounds the read of one host's chain, so that a host that does not answer holds a
// save up for no longer.
const siteDialTimeout = 6 * time.Second

// siteRegistries are the registries the management nodes pull from, each read in its mirrors' place
// where it has mirrors.
var siteRegistries = []string{"ghcr.io", "registry.k8s.io"}

// siteCheckTimeout bounds the whole check of the Site CA at a save, the hosts being read in parallel.
const siteCheckTimeout = 2 * siteDialTimeout

// noteSite tells the job's log where the chains were read from, which hosts did not answer, and
// that the management nodes' network was not checked where the appliance sits outside it, since a
// device may treat the two differently.
func (a *Appliance) noteSite(j *Job, s *Settings, unchecked []string) {
	if !s.pastesAuthority() || a.SiteChains == nil {
		return
	}
	own, _ := a.addresses()
	if len(own) > 0 {
		j.Add("The Site CA was checked from the appliance at " + strings.Join(own, ", "))
		if !withinNodeNetwork(own, s) {
			j.Add("The management nodes' network was not checked")
		}
	}
	if len(unchecked) > 0 {
		j.Add("The Site CA was not checked against " + strings.Join(unchecked, ", ") + ", which did not answer")
	}
}

// withinNodeNetwork tells whether any of the addresses lies in a subnet the management nodes have an
// address in.
func withinNodeNetwork(addresses []string, s *Settings) bool {
	for _, text := range addresses {
		own, err := netip.ParseAddr(strings.TrimSuffix(text, text[strings.Index(text+"/", "/"):]))
		if err != nil {
			continue
		}
		for _, node := range slices.Concat(s.NodeAddresses, s.NodeAddressesIPv6) {
			if prefix, err := netip.ParsePrefix(node); err == nil && prefix.Contains(own) {
				return true
			}
		}
	}
	return false
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

// pastesAuthority tells whether the settings carry an authority the hosts' chains are checked
// against: the Site CA, or the CA of a proxy that re-signs TLS.
func (s *Settings) pastesAuthority() bool {
	return s.SiteCA != "" || (s.Proxy.Enabled() && s.Proxy.CABundle != "")
}

// SiteTrustFor is the authority the nodes get for the Site CA ca: the saved bundle while ca is the
// saved Site CA, else ca as pasted.
func (a *Appliance) SiteTrustFor(ca string) string {
	if saved := a.SavedSettings(); saved != nil && saved.SiteCA == ca {
		return saved.siteTrust()
	}
	return ca
}

// SiteChainErrors are what keeps the nodes from trusting the chains the hosts present, one line each.
type SiteChainErrors []string

func (f SiteChainErrors) Error() string { return strings.Join(f, "\n") }

// ResolveSite derives the bundles the nodes and downloads trust from the pasted Site CA and proxy
// CA and the chains the hosts present, and sets them on s. The pasted authorities stay as they are.
// A host that does not answer is returned in unchecked, and fails nothing. A chain the nodes would
// refuse with the bundles fails with SiteChainErrors.
func (a *Appliance) ResolveSite(ctx context.Context, s *Settings) ([]string, error) {
	authorities, err := a.siteAuthorities(s)
	s.SiteBundle = ""
	if s.Proxy != nil {
		s.Proxy.Bundle = ""
	}
	if err != nil {
		return nil, err
	}
	if len(authorities) == 0 || a.SiteChains == nil {
		return nil, nil
	}
	answered, unchecked := a.readChains(ctx, s)
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if err := judgeChains(authorities, a.publicRoots(), answered, now); err != nil {
		return unchecked, err
	}
	for _, auth := range authorities {
		*auth.bundle = auth.derived()
	}
	return unchecked, nil
}

// resolveSiteUnattended is ResolveSite for a run nobody watches. A chain the nodes would refuse
// keeps the bundles of the last run that passed, and the log says why, since nobody is there to fix
// the chain and the nodes go on as they are.
func (a *Appliance) resolveSiteUnattended(ctx context.Context, j *Job, s *Settings) ([]string, error) {
	site, proxy := s.SiteBundle, ""
	if s.Proxy != nil {
		proxy = s.Proxy.Bundle
	}
	unchecked, err := a.ResolveSite(ctx, s)
	faults, ok := errors.AsType[SiteChainErrors](err)
	if !ok {
		return unchecked, err
	}
	s.SiteBundle = site
	if s.Proxy != nil {
		s.Proxy.Bundle = proxy
	}
	j.Add("Kept the certificates of the last run, since the nodes would refuse these chains:")
	for _, fault := range faults {
		j.Add(fault)
	}
	return unchecked, nil
}

// CheckSite runs the check ResolveSite runs, on a copy of s, for the setup to show its faults before
// a save. It returns no line where the nodes would trust every host that answered.
func (a *Appliance) CheckSite(ctx context.Context, s *Settings) []string {
	c := *s
	if s.Proxy != nil {
		proxy := *s.Proxy
		c.Proxy = &proxy
	}
	_, err := a.ResolveSite(ctx, &c)
	if faults, ok := errors.AsType[SiteChainErrors](err); ok {
		return faults
	}
	if err != nil {
		return []string{err.Error()}
	}
	return nil
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
		return nil, fmt.Errorf("%s the authority %w", label, err)
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
		out.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	return out.String()
}

// publicRoots are the system's authorities, whose chains are no concern of the pasted ones.
func (a *Appliance) publicRoots() *x509.CertPool {
	if a.sitePublic != nil {
		return a.sitePublic
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil
	}
	return pool
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
// fault share its line.
func judgeChains(authorities []*siteAuthority, public *x509.CertPool, chains []hostChain, now time.Time) error {
	type labelled struct{ label, fault string }
	var faults []labelled
	hosts := map[labelled][]string{}
	for _, hc := range chains {
		if public != nil && verifyChain(hc.chain, public, now) == nil {
			continue
		}
		if fault := judgeChain(authorities, hc.chain, now); fault != "" {
			key := labelled{authorityOf(authorities, hc.chain).label, fault}
			if _, seen := hosts[key]; !seen {
				faults = append(faults, key)
			}
			hosts[key] = append(hosts[key], hc.host)
		}
	}
	if len(faults) == 0 {
		return nil
	}
	lines := make(SiteChainErrors, 0, len(faults))
	for _, key := range faults {
		lines = append(lines, key.label+" "+strings.Join(hosts[key], ", ")+": "+key.fault)
	}
	return lines
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

// judgeChain adds the renamed roots that make Go accept one chain, or names what keeps it from
// doing so.
func judgeChain(authorities []*siteAuthority, chain []*x509.Certificate, now time.Time) string {
	anchors := anchorsOf(authorities)
	err := verifyChain(chain, poolOf(anchors), now)
	if err == nil {
		return ""
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
			return ""
		}
	}
	return chainFault(chain, anchors, err, now)
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
func chainFault(chain, anchors []*x509.Certificate, verifyErr error, now time.Time) string {
	cur, path := chain[0], []*x509.Certificate{chain[0]}
	for {
		if fault := lapse(cur, now); fault != "" {
			return fault
		}
		if slices.ContainsFunc(anchors, cur.Equal) {
			break
		}
		parent, fault := issuerIn(cur, anchors, chain[1:], path)
		if fault != "" {
			return fault
		}
		if fault := presentedFault(cur, parent, anchors); fault != "" {
			return fault
		}
		path = append(path, parent)
		cur = parent
	}
	if invalid, ok := errors.AsType[x509.CertificateInvalidError](verifyErr); ok {
		if fault := verifyFault(invalid); fault != "" {
			return fault
		}
	}
	return fmt.Sprintf("the nodes refuse %q. Check the chain on the device.", subjectOf(chain[0]))
}

// verifyFault names a refusal of Go's verifier that following the chain by names does not find,
// empty for another.
func verifyFault(invalid x509.CertificateInvalidError) string {
	reason, c := invalid.Reason, invalid.Cert
	if reason == x509.TooManyIntermediates {
		return fmt.Sprintf("%q allows fewer intermediates than the host presents. Shorten the chain on the device.", subjectOf(c))
	}
	if reason == x509.CANotAuthorizedForThisName {
		return fmt.Sprintf("%q names a host its issuers may not sign for. Check their name constraints.", subjectOf(c))
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
func presentedFault(cur, parent *x509.Certificate, anchors []*x509.Certificate) string {
	switch {
	case slices.ContainsFunc(anchors, parent.Equal):
		return ""
	case canonicalName(parent.RawIssuer) == canonicalName(parent.RawSubject) && !slices.ContainsFunc(anchors, sameKey(parent)):
		return fmt.Sprintf("%q is a root that was not pasted. Paste that root.", subjectOf(parent))
	case !bytes.Equal(cur.RawIssuer, parent.RawSubject):
		return fmt.Sprintf("%q encodes its issuer's name %q differently. Reissue it on the device.", subjectOf(cur), subjectOf(parent))
	}
	return ""
}

// issuerIn finds the certificate that signs cur among the anchors and the presented certificates,
// those already on the path left out, or says why none does.
func issuerIn(cur *x509.Certificate, anchors, presented, path []*x509.Certificate) (*x509.Certificate, string) {
	if bytes.Equal(cur.RawIssuer, cur.RawSubject) {
		return nil, fmt.Sprintf("%q is a root that was not pasted. Paste that root.", subjectOf(cur))
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
			return p, ""
		case insecure:
			return nil, fmt.Sprintf("%q is signed with %s, which nodes refuse. Reissue it with SHA-256.", subjectOf(cur), cur.SignatureAlgorithm)
		case !p.BasicConstraintsValid || !p.IsCA:
			fault = notCA(p)
		case p.KeyUsage != 0 && p.KeyUsage&x509.KeyUsageCertSign == 0:
			fault = fmt.Sprintf("%q may not sign certificates. Reissue it with certificate signing.", subjectOf(p))
		case slices.ContainsFunc(anchors, p.Equal):
			// The device's root was made again under the same name, with another key.
			fault = fmt.Sprintf("%q is signed by a new %q. Paste it.", subjectOf(cur), subjectOf(p))
		}
	}
	if fault == "" {
		fault = fmt.Sprintf("%q is missing. Paste it with the root.", issuerOf(cur))
	}
	return nil, fault
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
		if block.Type != "CERTIFICATE" {
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

func subjectOf(c *x509.Certificate) string { return c.Subject.String() }

func issuerOf(c *x509.Certificate) string { return c.Issuer.String() }

// canonicalName is a distinguished name as OpenSSL compares it (x509_name_canon in
// crypto/x509/x_name.c): each value folded by canonicalValue, whatever its string type. Go compares
// the encoded bytes instead.
func canonicalName(raw []byte) string {
	var rdns pkix.RDNSequence
	if rest, err := asn1.Unmarshal(raw, &rdns); err != nil || len(rest) > 0 {
		return string(raw)
	}
	var sb strings.Builder
	for _, rdn := range rdns {
		parts := make([]string, 0, len(rdn))
		for _, atv := range rdn {
			value := fmt.Sprint(atv.Value)
			if s, ok := atv.Value.(string); ok {
				value = canonicalValue(s)
			}
			parts = append(parts, atv.Type.String()+"="+value)
		}
		slices.Sort(parts)
		sb.WriteString(strings.Join(parts, "+"))
		sb.WriteByte(';')
	}
	return sb.String()
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
		return nil, errors.New("does not parse to re-encode its name")
	}
	var fields []asn1.RawValue
	for body := outer.TBS.Bytes; len(body) > 0; {
		var f asn1.RawValue
		var err error
		if body, err = asn1.Unmarshal(body, &f); err != nil {
			return nil, errors.New("does not parse to re-encode its name")
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
		return nil, errors.New("holds its subject where it cannot be re-encoded")
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
		return nil, errors.New("cannot be re-encoded")
	}
	der, err := sequence(slices.Concat(tbsDER, outer.Algorithm.FullBytes, outer.Signature.FullBytes))
	if err != nil {
		return nil, errors.New("cannot be re-encoded")
	}
	renamed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errors.New("does not parse once its name is re-encoded")
	}
	return renamed, nil
}
