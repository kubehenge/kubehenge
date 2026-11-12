package installer

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
)

// APICandidate is a name or address of this cluster that the Proxmox VE API's certificate is valid
// for. The lower the rank, the better.
type APICandidate struct {
	Rank int
	Host string
	What string
}

// certNames returns the names and addresses a certificate carries, DNS names first.
func certNames(c *x509.Certificate) ([]string, []string) {
	var dns, ips []string
	for _, d := range c.DNSNames {
		dns = append(dns, strings.ToLower(d))
	}
	for _, ip := range c.IPAddresses {
		if a, ok := netip.AddrFromSlice(ip); ok {
			ips = append(ips, a.Unmap().String())
		}
	}
	return dns, ips
}

func parseCertFile(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}

// servedCertificate reads the certificate Proxmox VE serves its API with.
func (n *Node) servedCertificate() (*x509.Certificate, string, error) {
	file := pveServedPEM
	data, err := n.readFile(file)
	if err != nil {
		file = pveNodePEM
		if data, err = n.readFile(file); err != nil {
			return nil, file, err
		}
	}
	c, err := parseCertFile(data)
	return c, file, err
}

func (*Node) memberNode(f *Facts, address string) string {
	for _, m := range f.Members {
		if m.Address == address {
			return m.Node
		}
	}
	return ""
}

// nameNodes returns the nodes a name resolves to, "-" standing for an address that is none of
// theirs, nothing when it does not resolve.
func (n *Node) nameNodes(ctx context.Context, f *Facts, name string) []string {
	addrs, err := n.LookupHost(ctx, name)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, a := range addrs {
		p, err := netip.ParseAddr(a)
		if err != nil || p.Is4In6() {
			continue
		}
		owner := n.memberNode(f, p.String())
		if owner == "" {
			owner = "-"
		}
		set[owner] = true
	}
	owners := make([]string, 0, len(set))
	for o := range set {
		owners = append(owners, o)
	}
	slices.Sort(owners)
	return owners
}

// APICandidates lists each name and address in the API's certificate that is this cluster's, best
// first: a name of several of its nodes, this node's own name, another name of it, its address on
// the bridge, another address of it, a name without a domain, then other nodes' names and addresses.
// A name that also leads to a host outside the cluster is none of them.
func (n *Node) APICandidates(ctx context.Context, f *Facts, bridge string) []APICandidate {
	cert, _, err := n.servedCertificate()
	if err != nil {
		return nil
	}
	dns, ips := certNames(cert)
	list := n.addressCandidates(f, ips, n.bridgeAddress(ctx, bridge), bridge)
	for _, name := range dns {
		if c, ok := n.nameCandidate(ctx, f, name); ok {
			list = append(list, c)
		}
	}
	slices.SortStableFunc(list, func(a, b APICandidate) int { return cmp.Compare(a.Rank, b.Rank) })
	return firstPerHost(list)
}

// bridgeAddress is the address of the bridge, IPv4 first.
func (n *Node) bridgeAddress(ctx context.Context, bridge string) string {
	if p := n.bridgeAddress4(ctx, bridge); p.IsValid() {
		return p.Addr().String()
	}
	return n.bridgeAddress6(ctx, bridge)
}

// addressCandidates ranks the certificate's addresses that belong to a cluster node.
func (n *Node) addressCandidates(f *Facts, ips []string, bridgeAddr, bridge string) []APICandidate {
	var list []APICandidate
	for _, ip := range ips {
		owner := n.memberNode(f, ip)
		switch {
		case owner == "":
			continue
		case owner != f.Node:
			list = append(list, APICandidate{8, ip, "an address of node " + owner})
		case ip == bridgeAddr:
			list = append(list, APICandidate{4, ip, "this node's address on " + bridge})
		default:
			list = append(list, APICandidate{5, ip, "an address of this node"})
		}
	}
	return list
}

// nameCandidate ranks one name of the certificate, false when it leads outside the cluster.
func (n *Node) nameCandidate(ctx context.Context, f *Facts, name string) (APICandidate, bool) {
	if strings.HasPrefix(name, "*.") {
		if i := strings.Index(f.FQDN, "."); i < 0 || f.FQDN[i+1:] != name[2:] {
			return APICandidate{}, false
		}
		name = f.FQDN
	}
	owners := n.nameNodes(ctx, f, name)
	if len(owners) == 0 || slices.Contains(owners, "-") {
		return APICandidate{}, false
	}
	switch {
	case !strings.Contains(name, "."):
		if len(owners) == 1 && owners[0] == f.Node {
			return APICandidate{6, name, "this node's name without a domain"}, true
		}
		return APICandidate{9, name, "a name without a domain"}, true
	case len(owners) > 1:
		return APICandidate{1, name, "a name of nodes " + strings.Join(owners, ", ")}, true
	case owners[0] != f.Node:
		return APICandidate{7, name, "a name of node " + owners[0]}, true
	case name == f.FQDN:
		return APICandidate{2, name, "this node's name"}, true
	default:
		return APICandidate{3, name, "a name of this node"}, true
	}
}

// firstPerHost keeps the best-ranked candidate of each host in a ranked list.
func firstPerHost(list []APICandidate) []APICandidate {
	seen := map[string]bool{}
	out := list[:0]
	for _, c := range list {
		if !seen[c.Host] {
			seen[c.Host] = true
			out = append(out, c)
		}
	}
	return out
}

// TiedCandidates returns the best-ranked candidates, more than one when the choice is a tie.
func TiedCandidates(list []APICandidate) []APICandidate {
	var tied []APICandidate
	for _, c := range list {
		if c.Rank == list[0].Rank {
			tied = append(tied, c)
		}
	}
	return tied
}

// APIURL turns a host into the API's URL.
func APIURL(host string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host + ":8006"
}

// splitAPI returns the host and port of an API URL.
func splitAPI(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse: %w", err)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u.Hostname(), port, nil
}

// CAKind says where the CA the API is checked against comes from.
type CAKind int

// The sources of the API's CA, in the order they are tried.
const (
	CAGiven CAKind = iota
	CACluster
	CASystem
	CAServed
	CAInsecure
)

// CAChoice is the CA the appliance and the clusters check the API's certificate against.
type CAChoice struct {
	Kind        CAKind
	PEM         []byte
	Subject     string
	Issuer      string
	Fingerprint string
	// NeedsTrust is set for a served CA nothing vouches for yet: the user decides.
	NeedsTrust bool
}

func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func encodeCerts(certs ...*x509.Certificate) []byte {
	out := make([]byte, 0, len(certs)*certPEMSize)
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

func poolOf(data []byte) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(data)
	return pool
}

// verifyAPI checks the served chain for host against the pool, every certificate in it a trust
// anchor as in Go's own clients. It returns the anchor the chain rests on.
func verifyAPI(served []*x509.Certificate, host string, pool *x509.CertPool) (*x509.Certificate, bool) {
	if len(served) == 0 {
		return nil, false
	}
	inter := x509.NewCertPool()
	for _, c := range served[1:] {
		inter.AddCert(c)
	}
	chains, err := served[0].Verify(x509.VerifyOptions{DNSName: host, Roots: pool, Intermediates: inter})
	if err != nil || len(chains) == 0 {
		return nil, false
	}
	chain := chains[0]
	return chain[len(chain)-1], true
}

// servedAuthority picks the certificate to trust of those the API serves: the self-signed root,
// else the topmost CA certificate, else its own.
func servedAuthority(served []*x509.Certificate) *x509.Certificate {
	var root, top *x509.Certificate
	for _, c := range served[1:] {
		if c.IsCA {
			top = c
		}
		if c.CheckSignatureFrom(c) == nil && bytes.Equal(c.RawIssuer, c.RawSubject) {
			root = c
		}
	}
	for _, c := range []*x509.Certificate{root, top, served[0]} {
		if c != nil {
			return c
		}
	}
	return served[0]
}

func subjectOf(c *x509.Certificate) string { return c.Subject.String() }

// DecideCA picks the CA the API's certificate is checked against, as a user would want it:
// --proxmox-ca, else this cluster's CA, else the CA in this node's trust store the chain rests
// on, else the CA the API serves, which needs the user's yes.
func (n *Node) DecideCA(ctx context.Context, cfg *Config, apiURL string) (*CAChoice, error) {
	host, port, err := splitAPI(apiURL)
	if err != nil {
		return nil, userErr("the Proxmox VE API URL "+apiURL+" does not parse", "give another with --proxmox-url")
	}
	cluster, _ := n.readFile(clusterCA)
	if cfg.Insecure {
		return &CAChoice{Kind: CAInsecure, PEM: cluster}, nil
	}
	served, dialErr := n.Dial(ctx, host, port)
	connects := dialErr == nil && len(served) > 0
	check := func(ca []byte) (*x509.Certificate, bool) { return verifyAPI(served, host, poolOf(ca)) }
	if cfg.ProxmoxCA != "" {
		given, err := os.ReadFile(cfg.ProxmoxCA)
		if err != nil {
			return nil, userErr("--proxmox-ca names no readable PEM file", "give the CA that signed the API's certificate")
		}
		anchor, ok := check(given)
		if !ok {
			return nil, apiFailure(connects, apiURL, port, cfg.ProxmoxCA)
		}
		return &CAChoice{Kind: CAGiven, PEM: encodeCerts(anchor), Subject: subjectOf(anchor), Fingerprint: fingerprint(anchor)}, nil
	}
	if !connects {
		return nil, apiFailure(connects, apiURL, port, "")
	}
	if _, ok := check(cluster); ok {
		return &CAChoice{Kind: CACluster, PEM: cluster}, nil
	}
	if system, err := n.readFile(systemCA); err == nil {
		if anchor, ok := check(system); ok {
			return &CAChoice{Kind: CASystem, PEM: encodeCerts(anchor), Subject: subjectOf(anchor), Issuer: anchor.Issuer.String(), Fingerprint: fingerprint(anchor)}, nil
		}
	}
	authority := servedAuthority(served)
	if _, ok := check(encodeCerts(authority)); ok {
		return &CAChoice{
			Kind: CAServed, PEM: encodeCerts(authority), NeedsTrust: true,
			Subject: subjectOf(authority), Issuer: authority.Issuer.String(), Fingerprint: fingerprint(authority),
		}, nil
	}
	return nil, apiFailure(connects, apiURL, port, "")
}

func apiFailure(connects bool, apiURL, port, caFile string) error {
	switch {
	case !connects:
		return userErr("this node cannot connect to the Proxmox VE API at "+apiURL,
			fmt.Sprintf("Open port %s in the node's firewall and set LISTEN_IP in /etc/default/pveproxy, or give another URL with --proxmox-url", port))
	case caFile != "":
		return userErr(fmt.Sprintf("the Proxmox VE API at %s serves no certificate that %s signed for it", apiURL, caFile), "")
	default:
		return userErr(fmt.Sprintf("the Proxmox VE API at %s serves a certificate this cluster's CA did not sign for it", apiURL),
			"Give the CA that did with --proxmox-ca <file>, or another URL with --proxmox-url")
	}
}

// netDial is the real Dial: it reads the chain the API serves without trusting it, since the
// chain is verified by verifyAPI against a CA the user vouched for.
func netDial(ctx context.Context, host, port string) ([]*x509.Certificate, error) {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("dial context: %w", err)
	}
	defer func() { _ = conn.Close() }()
	return handshake(ctx, conn, host)
}
