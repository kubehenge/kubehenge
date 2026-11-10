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
// expired, endpoints that are addresses, and a mirror host that has one authority.
func validateNodeSite(c *clusterv1.Cluster, now time.Time) field.ErrorList {
	variables := field.NewPath("spec", "topology", "variables")
	var errs field.ErrorList
	mirrors, _, err := optionalVariable[[]RegistryMirror](c, registryMirrorsVariable)
	if err != nil {
		return field.ErrorList{field.Invalid(variables.Key(registryMirrorsVariable), nil, err.Error())}
	}
	errs = append(errs, checkMirrors(mirrors, variables.Key(registryMirrorsVariable), now)...)
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
			errs = append(errs, field.Invalid(at.Child("registry"), m.Registry, "is no lower-case host name with an optional port, and no *"))
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
// as Talos and the nodes' trust stores would otherwise refuse it only after a node was built.
func checkCertificates(bundle string, now time.Time) error {
	if len(bundle) > maxNodePEMBytes {
		return fmt.Errorf("at most %d KiB", maxNodePEMBytes/kibibyte)
	}
	rest := []byte(bundle)
	found := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != certificateBlock {
			return fmt.Errorf("holds a %s. Give certificates only", strings.ToLower(block.Type))
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("certificate %d is no valid X.509 certificate", found+1)
		}
		switch {
		case now.After(cert.NotAfter):
			return fmt.Errorf("certificate %d (%s) expired on %s", found+1, cert.Subject.CommonName, cert.NotAfter.UTC().Format(time.DateOnly))
		case now.Before(cert.NotBefore):
			return fmt.Errorf("certificate %d (%s) is valid only from %s", found+1, cert.Subject.CommonName, cert.NotBefore.UTC().Format(time.DateOnly))
		}
		found++
	}
	switch {
	case found == 0:
		return errors.New("holds no PEM certificate")
	case len(bytes.TrimSpace(rest)) > 0:
		return errors.New("holds text besides its certificates")
	}
	return nil
}
