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

// siteCALabel opens the messages about the Site CA, as the interface labels its field.
const siteCALabel = "Site CA:"

// siteDialTimeout bounds the read of one host's chain, so that a host that does not answer holds a
// save up for no longer.
const siteDialTimeout = 6 * time.Second

// siteHosts are the hosts the management nodes reach first, whose chains the Site CA has to cover for
// the nodes to come up: the Talos image factory, the registry of the Talos images and Sigstore's
// TUF repository.
var siteHosts = []string{"factory.talos.dev", "ghcr.io", "tuf-repo-cdn.sigstore.dev"}

// siteCheckTimeout bounds the whole check of the Site CA at a save, the hosts being read in parallel.
const siteCheckTimeout = 2 * siteDialTimeout

// noteSite tells the job's log where the chains were read from, which hosts did not answer, and
// that the management nodes' network was not checked where the appliance sits outside it, since a
// device may treat the two differently.
func (a *Appliance) noteSite(j *Job, s *Settings, unchecked []string) {
	if s.SiteCA == "" || a.SiteChains == nil {
		return
	}
	var own []string
	if a.Addresses != nil {
		own, _ = a.Addresses()
	}
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

// hosts are the hosts whose chains the Site CA is checked against: those every node reaches, and
// an https release mirror that has no authority of its own.
func (s *Settings) siteHosts() []string {
	hosts := slices.Clone(siteHosts)
	if m := s.ReleaseMirror; m != nil && m.CABundle == "" {
		if u, err := url.Parse(m.URL); err == nil && u.Scheme == "https" && u.Host != "" && !slices.Contains(hosts, u.Host) {
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}

// ResolveSite derives the bundle the nodes and downloads trust from the pasted Site CA and the chains
// the hosts present, and sets it on s. The pasted Site CA stays as it is. A host that does not answer
// is returned in unchecked, and fails nothing. A chain no bundle makes the nodes verify fails with
// every such fault named.
func (a *Appliance) ResolveSite(ctx context.Context, s *Settings) ([]string, error) {
	s.SiteBundle = ""
	if s.SiteCA == "" || a.SiteChains == nil {
		return nil, nil
	}
	roots, err := parseCertificates(s.SiteCA)
	if err != nil {
		return nil, fmt.Errorf("%s the authority %w", siteCALabel, err)
	}
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
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	bundle, err := buildSiteBundle(s.SiteCA, roots, a.publicRoots(s), answered, now)
	if err != nil {
		return unchecked, err
	}
	if bundle != s.SiteCA {
		s.SiteBundle = bundle
	}
	return unchecked, nil
}

// publicRoots are the authorities that make a chain no concern of the Site CA: the system's, and
// the proxy's.
func (a *Appliance) publicRoots(s *Settings) *x509.CertPool {
	if a.sitePublic != nil {
		return a.sitePublic
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if s.Proxy.Enabled() && s.Proxy.CABundle != "" {
		pool.AppendCertsFromPEM([]byte(s.Proxy.CABundle))
	}
	return pool
}

// DialChain is the SiteChains of the network: it opens a TLS connection to the host, through the
// settings' proxy when there is one, and returns what the host presented without judging it.
func (a *Appliance) DialChain(ctx context.Context, s *Settings, host string) ([]*x509.Certificate, error) {
	transport, err := s.Proxy.Transport(s.NoProxyList(a.ProxmoxURL))
	if err != nil {
		return nil, fmt.Errorf("the proxy setting: %w", err)
	}
	if !s.Proxy.Enabled() {
		transport.Proxy = http.ProxyFromEnvironment
	}
	var mu sync.Mutex
	var presented []*x509.Certificate
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // only the presented chain is wanted, and buildSiteBundle judges it
		VerifyConnection: func(cs tls.ConnectionState) error {
			mu.Lock()
			defer mu.Unlock()
			presented = cs.PeerCertificates
			return nil
		},
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", host, err)
	}
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

// hostChain is the chain one host presented, leaf first.
type hostChain struct {
	host  string
	chain []*x509.Certificate
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

// siteBuilder judges the chains hosts present against the pasted roots.
type siteBuilder struct {
	roots  []*x509.Certificate
	public *x509.CertPool
	now    time.Time
}

// buildSiteBundle is the pasted bundle plus what makes Go's verifier accept every chain that OpenSSL
// accepts with the pasted roots. A chain that reaches no pasted root and that no public authority
// signs, a certificate that has lapsed and a signature Go refuses fail it, each named with its
// subject.
func buildSiteBundle(pasted string, roots []*x509.Certificate, public *x509.CertPool, chains []hostChain, now time.Time) (string, error) {
	b := &siteBuilder{roots: roots, public: public, now: now}
	var extra []*x509.Certificate
	var faults []string
	for _, hc := range chains {
		added, found := b.host(hc)
		extra = append(extra, added...)
		faults = append(faults, found...)
	}
	if len(faults) > 0 {
		return "", errors.New(siteCALabel + " " + strings.Join(faults, "; "))
	}
	bundle := pasted
	var seen [][]byte
	for _, c := range extra {
		if slices.ContainsFunc(seen, func(raw []byte) bool { return bytes.Equal(raw, c.Raw) }) {
			continue
		}
		seen = append(seen, c.Raw)
		if bundle != "" && !strings.HasSuffix(bundle, "\n") {
			bundle += "\n"
		}
		bundle += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	return bundle, nil
}

// host returns the certificates the bundle needs for one host's chain, or what keeps any bundle
// from making the nodes trust it.
func (b *siteBuilder) host(hc hostChain) ([]*x509.Certificate, []string) {
	chain := hc.chain
	if b.publiclyTrusted(chain) {
		return nil, nil
	}
	path := b.climb(chain[0], chain[1:], []*x509.Certificate{chain[0]})
	if path == nil {
		return nil, []string{fmt.Sprintf("%s: no chain from the certificate %q to the Site CA, as %q is unknown. Add the intermediates the host does not present.", hc.host, subjectOf(chain[0]), issuerOf(chain[0]))}
	}
	var faults []string
	for i, c := range path[:len(path)-1] {
		switch {
		case b.now.Before(c.NotBefore):
			faults = append(faults, fmt.Sprintf("%s: the certificate %q is not valid before %s. Check the device's clock.", hc.host, subjectOf(c), c.NotBefore.Format(time.DateOnly)))
		case b.now.After(c.NotAfter):
			faults = append(faults, fmt.Sprintf("%s: the certificate %q expired on %s. Renew it on the device.", hc.host, subjectOf(c), c.NotAfter.Format(time.DateOnly)))
		}
		if _, refused := errors.AsType[x509.InsecureAlgorithmError](c.CheckSignatureFrom(path[i+1])); refused {
			faults = append(faults, fmt.Sprintf("%s: the certificate %q is signed with %s, which the nodes refuse. Reissue it with a stronger signature.", hc.host, subjectOf(c), c.SignatureAlgorithm))
		}
	}
	if len(faults) > 0 || b.goVerifies(chain, b.roots) == nil {
		return nil, faults
	}
	var added []*x509.Certificate
	for i, c := range path[:len(path)-1] {
		parent := path[i+1]
		if i+1 < len(path)-1 {
			added = append(added, parent)
		}
		if !bytes.Equal(c.RawIssuer, parent.RawSubject) {
			renamed, err := withSubject(parent, c.RawIssuer)
			if err != nil {
				return nil, []string{fmt.Sprintf("%s: the certificate %q: %v", hc.host, subjectOf(parent), err)}
			}
			added = append(added, renamed)
		}
	}
	if err := b.goVerifies(chain, slices.Concat(b.roots, added)); err != nil {
		return nil, []string{fmt.Sprintf("%s: the nodes would reject the certificate %q: %v. Check the chain on the device.", hc.host, subjectOf(chain[0]), err)}
	}
	return added, nil
}

// publiclyTrusted tells whether the system's authorities, or the proxy's, sign the chain. Such a
// host is not behind the inspection the Site CA is for.
func (b *siteBuilder) publiclyTrusted(chain []*x509.Certificate) bool {
	if b.public == nil {
		return false
	}
	return verifyChain(chain, b.public, b.now) == nil
}

func verifyChain(chain []*x509.Certificate, roots *x509.CertPool, now time.Time) error {
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err //nolint:wrapcheck // the verifier's own error names the certificate
}

// goVerifies is the nodes' check of the chain: Go's verifier, with the chain's intermediates and
// the anchors. A pasted root that has lapsed since it was saved is the controller's to report, and
// stops nothing here.
func (b *siteBuilder) goVerifies(chain, anchors []*x509.Certificate) error {
	pool := x509.NewCertPool()
	for _, c := range anchors {
		pool.AddCert(c)
	}
	err := verifyChain(chain, pool, b.now)
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) && invalid.Reason == x509.Expired &&
		slices.ContainsFunc(b.roots, func(r *x509.Certificate) bool { return r.Equal(invalid.Cert) }) {
		return nil
	}
	return err
}

// climb finds the path from cert to a pasted root as OpenSSL builds it: each issuer found by its
// name in canonical form, and each signature checked with the issuer's key. The presented
// certificates may come in any order. It returns the path, ending in the root, or nil.
func (b *siteBuilder) climb(cert *x509.Certificate, presented, path []*x509.Certificate) []*x509.Certificate {
	if slices.ContainsFunc(b.roots, cert.Equal) {
		return path
	}
	issuer := canonicalName(cert.RawIssuer)
	for _, r := range b.roots {
		if canonicalName(r.RawSubject) == issuer && signs(r, cert) {
			return append(slices.Clone(path), r)
		}
	}
	for _, p := range presented {
		if slices.ContainsFunc(path, p.Equal) || canonicalName(p.RawSubject) != issuer || !signs(p, cert) {
			continue
		}
		if found := b.climb(p, presented, append(slices.Clone(path), p)); found != nil {
			return found
		}
	}
	return nil
}

// signs tells whether parent signs child as OpenSSL reads it. A signature algorithm Go refuses
// still links the two, and is reported where the path is judged.
func signs(parent, child *x509.Certificate) bool {
	err := child.CheckSignatureFrom(parent)
	_, refused := errors.AsType[x509.InsecureAlgorithmError](err)
	return err == nil || refused
}

func subjectOf(c *x509.Certificate) string { return c.Subject.String() }

func issuerOf(c *x509.Certificate) string { return c.Issuer.String() }

// canonicalName is a distinguished name as RFC 5280 section 7.1 compares it, and OpenSSL: each value
// in lower case with its white space collapsed, whatever its string type. Go compares the encoded
// bytes instead.
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
				value = strings.ToLower(strings.Join(strings.Fields(s), " "))
			}
			parts = append(parts, atv.Type.String()+"="+value)
		}
		slices.Sort(parts)
		sb.WriteString(strings.Join(parts, "+"))
		sb.WriteByte(';')
	}
	return sb.String()
}

// withSubject is a copy of c whose subject is the encoded name, with the same key, extensions and
// signature. The signature no longer covers the certificate, which Go never checks for a root it is
// given, and which a chain's verifier checks only for the certificates below one.
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
