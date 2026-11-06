package appliance

import (
	"crypto/x509"
	"maps"
	"slices"
	"strings"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// maxChartCABundle bounds the authority of the chart mirrors, as the proxy's is bounded.
const maxChartCABundle = 256 * kibibyte

// moduleCABundle is the key of a PEM authority in the module's proxy and chart_repositories variables.
const moduleCABundle = "ca_bundle"

// ChartRepositories are the mirrors that clusters fetch their Helm charts from when the charts' own
// repositories are out of reach. Registry mirrors reach only the images.
type ChartRepositories struct {
	// Mirrors are addresses by repository host, such as ghcr.io. A mirror serves the same charts under
	// the same digests: an oci:// address for a host that serves OCI charts, an http(s):// one for a
	// chart repository.
	Mirrors map[string]string `json:"mirrors,omitempty"`
	// CABundle is the PEM authority of mirrors the usual authorities do not sign.
	CABundle string `json:"caBundle,omitempty"`
}

// Enabled reports whether any mirror is set.
func (c *ChartRepositories) Enabled() bool { return c != nil && len(c.Mirrors) > 0 }

// pairs are the mirrors as the controller's --chart-mirror takes them.
func (c *ChartRepositories) pairs() []string {
	pairs := make([]string, 0, len(c.Mirrors))
	for _, host := range slices.Sorted(maps.Keys(c.Mirrors)) {
		pairs = append(pairs, host+"="+c.Mirrors[host])
	}
	return pairs
}

func (c *ChartRepositories) check(p *problems) {
	if c == nil {
		return
	}
	if _, err := provisioner.ParseChartMirrors(c.pairs()); err != nil {
		p.addf("Chart mirrors: %s", err)
	}
	if c.CABundle == "" {
		return
	}
	switch {
	case len(c.Mirrors) == 0:
		p.addf("Chart mirror certificate authority: needs a mirror")
	case len(c.CABundle) > maxChartCABundle:
		p.addf("Chart mirror certificate authority: at most %d KiB", maxChartCABundle/kibibyte)
	case strings.Contains(c.CABundle, "PRIVATE KEY"):
		p.addf("Chart mirror certificate authority: holds a private key. Give the authority's certificate only")
	case !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CABundle)):
		p.addf("Chart mirror certificate authority: holds no PEM certificate")
	}
}

// moduleChartRepositories is the module's chart_repositories variable.
func moduleChartRepositories(c *ChartRepositories) map[string]any {
	if c == nil {
		return map[string]any{"mirrors": map[string]string{}, moduleCABundle: ""}
	}
	mirrors := c.Mirrors
	if mirrors == nil {
		mirrors = map[string]string{}
	}
	return map[string]any{"mirrors": mirrors, moduleCABundle: c.CABundle}
}
