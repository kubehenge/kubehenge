package appliance

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// nodeLogTimeout bounds the read of one node's log after a failed run.
const nodeLogTimeout = 10 * time.Second

// rejectedHost finds the host a node's log says it could not verify, in the address of the request
// the error ends.
var rejectedHost = regexp.MustCompile(`https?://([A-Za-z0-9.-]+(?::\d+)?)`)

// rejectedAuthority finds the certificate Go names as the candidate authority it tried, also in a
// line that quotes the error as a JSON string.
var rejectedAuthority = regexp.MustCompile(`candidate authority certificate \\?"([^"\\]+)\\?"`)

// rejectedReason finds Go's reason for refusing the certificate, up to its details.
var rejectedReason = regexp.MustCompile(`x509: ([^(\\":]+)`)

// unknownAuthority is Go's reason for a chain that no root the node trusts signs.
const unknownAuthority = "certificate signed by unknown authority"

// explainCertificates names, after a failed run, each management node whose log says it could not
// verify a host's certificate, with the host and what to do about it (refusalLine). The error names
// the first such node. It returns err as it was where no node says so.
func (a *Appliance) explainCertificates(ctx context.Context, j *Job, env []string, s *Settings, err error) error {
	if a.TalosLogs == nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), waitsTimeout)
	defer cancel()
	pulled, readErr := a.read(ctx, env, "state", "pull")
	if readErr != nil {
		return err
	}
	admin := stateTalosconfig(pulled)
	nodes, endpointsErr := provisioner.TalosEndpoints(admin)
	if endpointsErr != nil {
		return err
	}
	var said []string
	for _, node := range nodes {
		lines := a.nodeLog(ctx, admin, node)
		rejected, found := rejectedCertificate(lines)
		if !found {
			continue
		}
		said = append(said, a.refusalLine(ctx, s, node, rejected))
	}
	if len(said) == 0 {
		return err
	}
	for _, line := range said {
		j.Add(line)
	}
	// The log names every node, so that the error stays two sentences.
	return fmt.Errorf("%s The run failed: %w", said[0], err)
}

// nodeLog is a node's kernel and Talos messages, or none where the node does not answer.
func (a *Appliance) nodeLog(ctx context.Context, admin []byte, node string) []string {
	ctx, cancel := context.WithTimeout(ctx, nodeLogTimeout)
	defer cancel()
	logs, err := a.TalosLogs(ctx, admin, node)
	if err != nil {
		return nil
	}
	defer func() { _ = logs.Close() }()
	lines, err := logs.Dmesg(ctx)
	if err != nil {
		return nil
	}
	return lines
}

// refusalLine says that the node refused the host's certificate, and what to do about it. The
// node's own log decides first: a reason other than an unknown authority, or a candidate authority
// that is one of the nodes' public roots, is no matter of the Site CA. A candidate the Site CA
// holds signs no longer, as a device's root made again does. With no candidate logged, a root to
// add is named only where the host presents it, self-signed, and nothing the nodes trust signs the
// chain or holds that root. Every other case says to check the Site CA, since the node may meet
// another device than the appliance does, and an issuer the host leaves out may be any root.
func (a *Appliance) refusalLine(ctx context.Context, s *Settings, node string, r rejection) string {
	refused := fmt.Sprintf("Node %s refused %s's certificate", node, r.host)
	plain := fmt.Sprintf("%s: %s.", refused, r.reason)
	switch {
	case r.reason != unknownAuthority || r.authority != "" && publicName(r.authority):
		return plain
	case r.authority != "":
		return fmt.Sprintf("%s, signed by a new %q. Paste the new root.", refused, r.authority)
	}
	check := plain + " Check the Site CA."
	chain := a.liveChain(ctx, s, r.host)
	if chain == nil {
		return check
	}
	top, presented := endOf(chain)
	public, now := a.publicRoots(), a.now()
	switch {
	case !presented || verifyChain(chain, public, now) == nil || nodesTrust(s, chain, top, now):
		return check
	case isPublicRoot(top, public, now):
		return plain
	}
	return fmt.Sprintf("%s. Add its root %q (SHA-256 %s) to the Site CA.", refused, subjectOf(top), Fingerprint(top))
}

// nodesTrust says whether the authorities the nodes trust beside the public roots, the saved Site CA
// and the proxy's CA, sign chain or hold the root top it ends at.
func nodesTrust(s *Settings, chain []*x509.Certificate, top *x509.Certificate, now time.Time) bool {
	var trusted []*x509.Certificate
	bundles := []string{s.SiteCA}
	if s.Proxy.Enabled() {
		bundles = append(bundles, s.Proxy.CABundle)
	}
	for _, bundle := range bundles {
		if certs, err := parseCertificates(bundle); err == nil {
			trusted = append(trusted, certs...)
		}
	}
	if len(trusted) == 0 {
		return false
	}
	return verifyChain(chain, poolOf(trusted), now) == nil || slices.ContainsFunc(trusted, func(c *x509.Certificate) bool {
		return c.Equal(top) || sameKey(top)(c)
	})
}

// liveChain is the chain the host presents now, or none where it cannot be read.
func (a *Appliance) liveChain(ctx context.Context, s *Settings, host string) []*x509.Certificate {
	if a.SiteChains == nil {
		return nil
	}
	dial, cancel := context.WithTimeout(ctx, siteDialTimeout)
	defer cancel()
	chain, err := a.SiteChains(dial, s, host)
	if err != nil || len(chain) == 0 {
		return nil
	}
	return chain
}

// endOf follows a presented chain from its leaf by issuer names and returns the certificate it stops
// at. That is a self-signed root where the host presented one, else the topmost certificate, whose
// issuer is the root the host left out.
func endOf(chain []*x509.Certificate) (*x509.Certificate, bool) {
	cur, path := chain[0], []*x509.Certificate{chain[0]}
	for !bytes.Equal(cur.RawIssuer, cur.RawSubject) {
		next := slices.IndexFunc(chain, func(p *x509.Certificate) bool {
			return !slices.ContainsFunc(path, p.Equal) && canonicalName(p.RawSubject) == canonicalName(cur.RawIssuer) && cur.CheckSignatureFrom(p) == nil
		})
		if next < 0 {
			return cur, false
		}
		cur = chain[next]
		path = append(path, cur)
	}
	return cur, true
}

// rejection is a certificate a node's log says it could not verify: the host it was presented by,
// Go's reason, and the authority the line names where it does.
type rejection struct{ host, reason, authority string }

// rejectedCertificate finds the last line that says a certificate could not be verified.
func rejectedCertificate(lines []string) (rejection, bool) {
	for _, line := range slices.Backward(lines) {
		if !strings.Contains(line, "x509:") {
			continue
		}
		m := rejectedHost.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		found := rejection{host: m[1], reason: unknownAuthority}
		if reason := rejectedReason.FindStringSubmatch(line); reason != nil {
			found.reason = strings.TrimSpace(reason[1])
		}
		if a := rejectedAuthority.FindStringSubmatch(line); a != nil {
			found.authority = a[1]
		}
		return found, true
	}
	return rejection{}, false
}
