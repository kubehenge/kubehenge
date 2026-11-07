package provisioner

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// ChartMirrors maps a chart repository's host (ghcr.io, charts.jetstack.io) to the address of a
// mirror. A mirror serves the same charts under the same digests, so that the pins hold: an oci://
// address for a host that serves OCI charts, whose digest names a manifest, and an http(s):// one
// for a chart repository, whose digest names an archive.
type ChartMirrors map[string]string

// ParseChartMirrors reads <host>=<address> pairs.
func ParseChartMirrors(pairs []string) (ChartMirrors, error) {
	mirrors := ChartMirrors{}
	for _, pair := range pairs {
		host, address, ok := strings.Cut(pair, "=")
		if !ok || host == "" {
			return nil, fmt.Errorf("%q is not <host>=<address>", pair)
		}
		if _, dup := mirrors[host]; dup {
			return nil, fmt.Errorf("%s has two mirrors", host)
		}
		u, err := url.Parse(address)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(address, "/") || u.User != nil {
			// The address is left out of the message: it may hold a password.
			return nil, fmt.Errorf("the mirror of %s is no oci://, https:// or http:// address with a host, without a trailing slash, credentials, query or fragment", host)
		}
		if u.Scheme != "oci" && u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP {
			return nil, fmt.Errorf("the mirror of %s is no oci://, https:// or http:// address", host)
		}
		mirrors[host] = address
	}
	return mirrors, nil
}

// isOCI reports whether a repository address is an OCI registry's.
func isOCI(address string) bool { return strings.HasPrefix(address, "oci://") }

// Repository is where a chart repository is fetched from: its mirror's address with the
// repository's own path beneath it, or the repository itself without a mirror.
func (m ChartMirrors) Repository(repository string) string {
	u, err := url.Parse(repository)
	if err != nil {
		return repository
	}
	mirror, ok := m[u.Host]
	if !ok {
		return repository
	}
	return mirror + u.Path
}

// Check refuses a mirror that no chart uses, which is a misspelt host, and one that serves another
// kind of chart than its host does, whose digests would never match the pins.
func (m ChartMirrors) Check(charts []Chart) error {
	kinds := map[string]bool{}
	for i := range charts {
		if u, err := url.Parse(charts[i].Repository); err == nil && u.Host != "" {
			kinds[u.Host] = isOCI(charts[i].Repository)
		}
	}
	var problems []string
	for _, host := range slices.Sorted(maps.Keys(m)) {
		oci, used := kinds[host]
		switch {
		case !used:
			problems = append(problems, fmt.Sprintf("no chart comes from %s (the hosts are %s)", host, strings.Join(slices.Sorted(maps.Keys(kinds)), ", ")))
		case oci != isOCI(m[host]):
			kind := "an http(s):// address, as it serves a chart repository"
			if oci {
				kind = "an oci:// address, as it serves OCI charts"
			}
			problems = append(problems, fmt.Sprintf("the mirror of %s has to be %s", host, kind))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Pinned are all the charts, among them those a cluster installs only for some distributions or
// settings.
func (c *Charts) Pinned() []Chart {
	return []Chart{
		c.CCM, c.CSI, c.CSINewest, c.SnapshotController, c.Cilium, c.Calico, c.Headlamp, c.CertManager,
		c.Rancher, c.Monitoring, c.Loki, c.Alloy, c.Velero, c.Kyverno, c.KyvernoPolicies,
		c.Istio, c.IstioBase, c.IstioCNI, c.IstioZtunnel, c.Traefik, c.MetalLB, c.KubeVip,
		c.KubeVipCloudProvider, c.ArgoCD, c.FluxOperator,
	}
}

// String is the chart as the controller's flags name it: <repository>/<name>:<version>@sha256:<digest>.
func (c Chart) String() string {
	return c.Repository + "/" + c.Name + ":" + c.Version + "@sha256:" + c.Digest
}

// ChartsInValues reads the charts a Kubehenge chart's values pin under addons, by the add-on's key,
// so that the charts a site has to mirror can be listed from the release without a controller.
func ChartsInValues(raw []byte) (map[string]Chart, error) {
	var values struct {
		Addons map[string]struct {
			Repository string `json:"repository"`
			Chart      string `json:"chart"`
			Version    string `json:"version"`
		} `json:"addons"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("reading the values: %w", err)
	}
	charts := map[string]Chart{}
	for key, a := range values.Addons {
		if a.Repository == "" || a.Chart == "" || a.Version == "" {
			continue
		}
		chart, err := ParseChart(a.Repository + "/" + a.Chart + ":" + a.Version)
		if err != nil {
			return nil, fmt.Errorf("addons.%s: %w", key, err)
		}
		charts[key] = chart
	}
	if len(charts) == 0 {
		return nil, errors.New("the values pin no chart under addons. Pass the values of Kubehenge's chart")
	}
	return charts, nil
}
