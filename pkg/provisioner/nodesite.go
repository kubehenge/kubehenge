package provisioner

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	registryMirrorsVariable = "registryMirrors"
	trustedCAsVariable      = "trustedCAs"
	// The limits of the class's schemas.
	maxRegistryMirrors = 16
	maxMirrorEndpoints = 4
	maxTrustedCAs      = 8
	maxNodePEMBytes    = 65536
	certificateBlock   = "CERTIFICATE"
	mirrorAPIPath      = "/v2"
)

// RegistryMirror is one entry of a cluster's registryMirrors variable: where the nodes pull a
// registry's images from, before the registry itself.
type RegistryMirror struct {
	// Registry is a host name with its port, or * for every registry.
	Registry string `json:"registry"`
	// Endpoints are tried in order. A path, which ends in /v2, is used as given.
	Endpoints []string `json:"endpoints"`
	// CABundle is the PEM authority that signed the endpoints' certificates.
	CABundle string `json:"caBundle,omitempty"`
}

// registryName holds a registry to a lower-case host name, as the chart's schema does.
var registryName = regexp.MustCompile(`^(\*|[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:\d{1,5})?)$`)

// validateNodeSite holds the site settings a cluster's nodes take, the mirrors and the trusted
// authorities, to the rules the schema cannot state: certificates that parse and have not
// expired, endpoints that are addresses, and a mirror host that has one authority. On an update
// each setting is checked only when it changes, since a certificate expires without one.
func validateNodeSite(old, c *clusterv1.Cluster, now time.Time) field.ErrorList {
	variables := field.NewPath("spec", "topology", "variables")
	changed := func(name string) bool { return old == nil || variableChanged(old, c, name) }
	var errs field.ErrorList
	if changed(registryMirrorsVariable) {
		mirrors, _, err := optionalVariable[[]RegistryMirror](c, registryMirrorsVariable)
		if err != nil {
			return field.ErrorList{field.Invalid(variables.Key(registryMirrorsVariable), nil, err.Error())}
		}
		errs = append(errs, checkMirrors(mirrors, variables.Key(registryMirrorsVariable), now)...)
	}
	if changed(trustedCAsVariable) {
		cas, _, err := optionalVariable[[]string](c, trustedCAsVariable)
		if err != nil {
			return append(errs, field.Invalid(variables.Key(trustedCAsVariable), nil, err.Error()))
		}
		path := variables.Key(trustedCAsVariable)
		if len(cas) > maxTrustedCAs {
			errs = append(errs, field.TooMany(path, len(cas), maxTrustedCAs))
		}
		for i, ca := range cas {
			if caErr := checkCertificates(ca, now); caErr != nil {
				errs = append(errs, field.Invalid(path.Index(i), nil, caErr.Error()))
			}
		}
	}
	if !changed(releaseMirrorVariable) {
		return errs
	}
	release, set, err := ClusterReleaseMirror(c)
	switch {
	case err != nil:
		errs = append(errs, field.Invalid(variables.Key(releaseMirrorVariable), nil, err.Error()))
	case set && release.URL == "":
		errs = append(errs, field.Required(variables.Key(releaseMirrorVariable).Child("url"), "a release mirror needs its address"))
	default:
		// The address stays out of the error, as it may hold a password.
		if checkErr := release.Check(now); checkErr != nil {
			errs = append(errs, field.Invalid(variables.Key(releaseMirrorVariable), nil, checkErr.Error()))
		}
	}
	return errs
}

func checkMirrors(mirrors []RegistryMirror, path *field.Path, now time.Time) field.ErrorList {
	var errs field.ErrorList
	if len(mirrors) > maxRegistryMirrors {
		errs = append(errs, field.TooMany(path, len(mirrors), maxRegistryMirrors))
	}
	registries := map[string]bool{}
	// authorities is the PEM each endpoint host was given, since a node trusts one per host.
	authorities := map[string]string{}
	for i, m := range mirrors {
		at := path.Index(i)
		switch {
		case !registryName.MatchString(m.Registry):
			errs = append(errs, field.Invalid(at.Child("registry"), m.Registry, "is no lowercase host name with an optional port, and no *"))
		case registries[m.Registry]:
			errs = append(errs, field.Duplicate(at.Child("registry"), m.Registry))
		}
		registries[m.Registry] = true
		errs = append(errs, checkEndpoints(m, at, authorities)...)
		if m.CABundle != "" {
			if err := checkCertificates(m.CABundle, now); err != nil {
				errs = append(errs, field.Invalid(at.Child("caBundle"), nil, err.Error()))
			}
		}
	}
	return errs
}

func checkEndpoints(m RegistryMirror, at *field.Path, authorities map[string]string) field.ErrorList {
	var errs field.ErrorList
	if len(m.Endpoints) == 0 || len(m.Endpoints) > maxMirrorEndpoints {
		errs = append(errs, field.Invalid(at.Child("endpoints"), len(m.Endpoints), fmt.Sprintf("takes 1 to %d addresses", maxMirrorEndpoints)))
	}
	secure := false
	seen := map[string]bool{}
	for j, endpoint := range m.Endpoints {
		ep := at.Child("endpoints").Index(j)
		u, err := mirrorAddress(endpoint)
		if err != nil {
			errs = append(errs, field.Invalid(ep, endpoint, err.Error()))
			continue
		}
		if seen[endpoint] {
			errs = append(errs, field.Duplicate(ep, endpoint))
		}
		seen[endpoint] = true
		secure = secure || u.Scheme == schemeHTTPS
		if prior, ok := authorities[u.Host]; ok && prior != strings.TrimSpace(m.CABundle) {
			errs = append(errs, field.Invalid(ep, endpoint, "its host has another authority under another registry. A node trusts one authority per mirror host"))
		}
		authorities[u.Host] = strings.TrimSpace(m.CABundle)
	}
	if m.CABundle != "" && !secure && len(m.Endpoints) > 0 {
		errs = append(errs, field.Invalid(at.Child("caBundle"), nil, "is for https:// endpoints, and none of these is"))
	}
	return errs
}

// mirrorAddress parses a mirror endpoint: http:// or https://, a host, and no credentials or
// query. A path has to end in /v2, which is how a registry's API starts and what the nodes'
// container runtimes take as given.
func mirrorAddress(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	switch {
	case err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) || u.Host == "":
		return nil, errors.New("is no http:// or https:// address")
	case u.User != nil:
		return nil, errors.New("holds credentials. Give the address alone")
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(endpoint, " \t\r\n'\"`$\\"):
		return nil, errors.New("has a query, a fragment or characters other than letters, digits and . _ - : /")
	case u.Path != "" && !strings.HasSuffix(u.Path, mirrorAPIPath):
		return nil, fmt.Errorf("has a path that does not end in %s", mirrorAPIPath)
	case !proxyHost.MatchString(u.Host) || strings.HasPrefix(u.Host, "["):
		return nil, errors.New("has a host that is no name or IPv4 address with an optional port")
	}
	return u, nil
}

// checkCertificates holds a PEM bundle to certificates alone, none of them expired or not yet valid,
// as Talos and the nodes' trust stores would otherwise refuse it only after a node was built. A zero
// now leaves the validity unchecked.
func checkCertificates(bundle string, now time.Time) error {
	if len(bundle) > maxNodePEMBytes {
		return fmt.Errorf("at most %d KiB", maxNodePEMBytes/kibibyte)
	}
	blocks, err := pemBlocks(bundle)
	if err != nil {
		return err
	}
	found := 0
	for _, block := range blocks {
		if block.Type != certificateBlock {
			return fmt.Errorf("holds a %s. Give certificates only", strings.ToLower(block.Type))
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("certificate %d is no valid X.509 certificate", found+1)
		}
		switch {
		case now.IsZero():
		case now.After(cert.NotAfter):
			return fmt.Errorf("certificate %d (%s) expired on %s", found+1, cert.Subject.CommonName, cert.NotAfter.UTC().Format(time.DateOnly))
		case now.Before(cert.NotBefore):
			return fmt.Errorf("certificate %d (%s) is valid only from %s", found+1, cert.Subject.CommonName, cert.NotBefore.UTC().Format(time.DateOnly))
		}
		found++
	}
	if found == 0 {
		return errors.New("holds no PEM certificate")
	}
	return nil
}

var (
	// pemBoundaryWord finds what is left of a BEGIN or END line that lost its dashes.
	pemBoundaryWord = regexp.MustCompile(`\b(BEGIN|END)\b`)
	// pemBodyLine is a line of base64 as long as most of a PEM block's lines, which openssl's own
	// text (attributes, subject, hex dumps with colons) never holds.
	pemBodyLine = regexp.MustCompile(`^[A-Za-z0-9+/=]{40,}$`)
)

// pemBlocks splits a bundle into its PEM blocks, line by line. Decoding the whole bundle at once
// would pass over a block whose BEGIN or END line is damaged and keep the next. Text outside the
// blocks is kept for openssl's output, which people paste with its attributes and text, but a line
// that looks like a damaged boundary or a certificate's body refuses the bundle, naming its line.
func pemBlocks(bundle string) ([]*pem.Block, error) {
	type numbered struct {
		n    int
		text string
	}
	var lines []numbered
	for i, line := range strings.Split(bundle, "\n") {
		line = strings.TrimRight(line, "\r")
		// A block may start on the line another ends on, as when bundles are joined without a break.
		for j := strings.Index(line[min(1, len(line)):], "-----BEGIN "); j >= 0; j = strings.Index(line[1:], "-----BEGIN ") {
			lines = append(lines, numbered{i + 1, line[:j+1]})
			line = line[j+1:]
		}
		lines = append(lines, numbered{i + 1, line})
	}
	var blocks []*pem.Block
	var open []string
	inBlock := false
	// trailing is the first line of text since the last block ended, 0 for none.
	trailing := 0
	for _, l := range lines {
		line := l.text
		switch {
		case inBlock && strings.HasPrefix(line, "-----BEGIN "):
			return nil, fmt.Errorf("block %d is no complete PEM block", len(blocks)+1)
		case inBlock:
			open = append(open, line)
			if !strings.HasPrefix(line, "-----END ") {
				continue
			}
			block, rest := pem.Decode([]byte(strings.Join(open, "\n") + "\n"))
			if block == nil || len(bytes.TrimSpace(rest)) > 0 {
				return nil, fmt.Errorf("block %d is no complete PEM block", len(blocks)+1)
			}
			blocks, open, inBlock = append(blocks, block), nil, false
		case strings.HasPrefix(line, "-----BEGIN "):
			open, inBlock = []string{line}, true
		case strings.Contains(line, "-----") || pemBoundaryWord.MatchString(line):
			return nil, fmt.Errorf("has a damaged BEGIN or END line on line %d", l.n)
		case pemBodyLine.MatchString(strings.TrimSpace(line)):
			return nil, fmt.Errorf("has certificate text without its BEGIN line on line %d", l.n)
		case strings.TrimSpace(line) != "" && trailing == 0:
			trailing = l.n
		}
		if inBlock {
			trailing = 0
		}
	}
	switch {
	case inBlock:
		return nil, fmt.Errorf("block %d is no complete PEM block", len(blocks)+1)
	case trailing > 0 && len(blocks) > 0:
		// openssl writes its text before each block, so text after the last one is no part of it.
		return nil, fmt.Errorf("has text after its last PEM block on line %d", trailing)
	}
	return blocks, nil
}
