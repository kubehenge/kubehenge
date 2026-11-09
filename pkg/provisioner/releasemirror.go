package provisioner

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// ReleaseMirror serves the releases of RKE2 and k3s in place of GitHub, on GitHub's own paths:
// <URL>/<repository>/releases/download/<version>/<file> for a release's files and
// <URL>/<repository>/raw/<version>/install.sh for its install script. The pins still decide what a
// node installs, so a mirror can make a node fail but never run other files.
type ReleaseMirror struct {
	// URL is an http:// or https:// address without a trailing slash. Empty, releases come from GitHub.
	URL string
	// CABundle is the PEM authority of a mirror the usual authorities do not sign.
	CABundle string
}

// releaseMirrorCAPath is where a node keeps the mirror's authority for its download.
const releaseMirrorCAPath = "/etc/kubehenge/release-mirror-ca.pem"

// releaseMirrorVariable is an RKE2 or k3s cluster's own release mirror, which its nodes take in
// place of the management cluster's.
const releaseMirrorVariable = "releaseMirror"

// ClusterReleaseMirror is the cluster's own release mirror, and whether it names one.
func ClusterReleaseMirror(c *clusterv1.Cluster) (ReleaseMirror, bool, error) {
	v, set, err := optionalVariable[struct {
		URL      string `json:"url"`
		CABundle string `json:"caBundle"`
	}](c, releaseMirrorVariable)
	return ReleaseMirror{URL: v.URL, CABundle: v.CABundle}, set, err
}

// releaseMirrorAddress holds a mirror to what a node's command line carries unquoted.
var releaseMirrorAddress = regexp.MustCompile(`^[A-Za-z0-9._~:/\[\]-]+$`)

// Enabled reports whether releases come from the mirror.
func (m ReleaseMirror) Enabled() bool { return m.URL != "" }

// Check refuses a mirror that is no address a node can be handed, and an authority that is no
// certificate valid now. The address is left out of the messages: it may hold a password.
func (m ReleaseMirror) Check(now time.Time) error {
	if m.URL == "" {
		if m.CABundle != "" {
			return errors.New("the release mirror's authority needs a release mirror")
		}
		return nil
	}
	u, err := url.Parse(m.URL)
	switch {
	case err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) || u.Host == "":
		return errors.New("the release mirror is no http:// or https:// address with a host")
	case u.User != nil:
		return errors.New("the release mirror holds credentials. Give the address alone")
	case !releaseMirrorAddress.MatchString(m.URL):
		return errors.New("the release mirror has a query, a fragment or characters other than letters, digits and - . _ ~ : / [ ]")
	case strings.HasSuffix(m.URL, "/"):
		return errors.New("the release mirror ends in /. Give it without")
	case !proxyHost.MatchString(u.Host):
		return errors.New("the release mirror's host is no name or address with an optional port")
	}
	if m.CABundle == "" {
		return nil
	}
	if u.Scheme != schemeHTTPS {
		return errors.New("the release mirror's authority is for an https:// mirror")
	}
	if err := checkCertificates(m.CABundle, now); err != nil {
		return fmt.Errorf("the release mirror's authority %w", err)
	}
	return nil
}

// Trust has a transport trust the mirror's authority beside those it trusts already.
func (m ReleaseMirror) Trust(t *http.Transport) error {
	if m.CABundle == "" {
		return nil
	}
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	pool := t.TLSClientConfig.RootCAs
	if pool == nil {
		system, err := x509.SystemCertPool()
		if err != nil {
			return fmt.Errorf("the system's authorities: %w", err)
		}
		pool = system
	} else {
		pool = pool.Clone()
	}
	if !pool.AppendCertsFromPEM([]byte(m.CABundle)) {
		return errors.New("the release mirror's authority holds no PEM certificate")
	}
	t.TLSClientConfig.RootCAs = pool
	return nil
}

// releaseURL is where a release's file comes from.
func (m ReleaseMirror) releaseURL(github, repository, version, name string) string {
	if m.Enabled() {
		github = m.URL
	}
	return github + "/" + repository + "/releases/download/" + version + "/" + name
}

// scriptURL is where a release's install script comes from.
func (m ReleaseMirror) scriptURL(raw, repository, version string) string {
	if m.Enabled() {
		return m.URL + "/" + repository + "/raw/" + version + "/install.sh"
	}
	return raw + "/" + repository + "/" + version + "/install.sh"
}
