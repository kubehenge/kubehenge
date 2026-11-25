package appliance

import (
	"context"
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

// rejectedAuthority finds the certificate Go names as the candidate authority it tried.
var rejectedAuthority = regexp.MustCompile(`candidate authority certificate "([^"]+)"`)

// explainCertificates names, after a failed run, each management node whose log says it could not
// verify a host's certificate, with the host and the certificate's issuer, and tells the person to
// check the Site CA. The error names the first such node. It returns err as it was where no node
// says so.
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
		said = append(said, fmt.Sprintf("Node %s rejected the certificate %s presents, issued by %s", node, rejected.host, a.issuerOf(ctx, s, rejected.host, rejected.authority)))
	}
	if len(said) == 0 {
		return err
	}
	for _, line := range said {
		j.Add(line + ". Check the Site CA.")
	}
	// The log names every node, so that the error stays two sentences.
	return fmt.Errorf("%s, so check the Site CA. The run failed: %w", said[0], err)
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

// issuerOf names who issued the certificate a host presents, as the host shows it now, else as the
// node's log names it.
func (a *Appliance) issuerOf(ctx context.Context, s *Settings, host, logged string) string {
	if logged != "" {
		return logged
	}
	if a.SiteChains != nil {
		dial, cancel := context.WithTimeout(ctx, siteDialTimeout)
		defer cancel()
		if chain, err := a.SiteChains(dial, s, host); err == nil && len(chain) > 0 {
			return issuerOf(chain[0])
		}
	}
	return "an authority the node does not know"
}

// rejection is a certificate a node's log says it could not verify: the host it was presented by,
// and the authority the line names where it does.
type rejection struct{ host, authority string }

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
		found := rejection{host: m[1]}
		if a := rejectedAuthority.FindStringSubmatch(line); a != nil {
			found.authority = a[1]
		}
		return found, true
	}
	return rejection{}, false
}
