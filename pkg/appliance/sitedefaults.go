package appliance

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// checkSiteDefaults holds the site CA to PEM certificates valid now, the site time zone to an
// IANA name and the VM ID range to Proxmox VE's limits. A site CA that passes is rewritten as the chart and the module take it.
func (s *Settings) checkSiteDefaults(p *problems, now time.Time) {
	if s.SiteCA != "" {
		plain, err := provisioner.NormaliseCertificateBundle(s.SiteCA)
		if err == nil {
			err = provisioner.CheckCertificateBundle(plain, now)
		}
		if err != nil {
			p.addf("Site CA: the authority %s", err)
		} else {
			s.SiteCA = plain
		}
	}
	if s.TimeZone != "" {
		if err := provisioner.CheckTimeZone(s.TimeZone); err != nil {
			p.addf("Time zone: %s", err)
		}
	}
	if s.WorkloadVMIDRange != nil {
		if err := s.WorkloadVMIDRange.Check(); err != nil {
			p.addf("%s", err)
		}
	}
}

// ValidateSiteDefaults refuses a site CA, time zone or VM ID range that Validate refuses, with its
// message.
func (s *Settings) ValidateSiteDefaults() error {
	var p problems
	s.checkSiteDefaults(&p, time.Now())
	return p.err()
}

// EffectiveReleaseMirror is the release mirror as the nodes and the interface use it: an https
// mirror without an authority of its own is verified with the site CA.
func (s *Settings) EffectiveReleaseMirror() provisioner.ReleaseMirror {
	return s.ReleaseMirror.Mirror().WithSiteCA(s.SiteCA)
}

// siteCA is the saved site CA, empty when none is saved.
func (a *Appliance) siteCA() string {
	if saved := a.SavedSettings(); saved != nil {
		return saved.SiteCA
	}
	return ""
}

// OutboundCA is the authorities the management side's downloads trust beside the public ones: the
// site CA, then the proxy's. Either may be empty, and the proxy's counts only while there is a proxy.
func (s *Settings) OutboundCA() string {
	var parts []string
	if s.SiteCA != "" {
		parts = append(parts, s.SiteCA)
	}
	if s.Proxy.Enabled() && s.Proxy.CABundle != "" {
		parts = append(parts, s.Proxy.CABundle)
	}
	return strings.Join(parts, "\n")
}

// trustSiteCA has a transport trust the site CA beside the authorities it trusts already.
func trustSiteCA(t *http.Transport, siteCA string) error {
	if err := (provisioner.ReleaseMirror{CABundle: siteCA}).Trust(t); err != nil {
		return fmt.Errorf("trusting the site CA: %w", err)
	}
	return nil
}
