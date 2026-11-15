package appliance

import (
	"fmt"
	"net/http"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// checkSiteDefaults holds the site CA to PEM certificates valid now and the site time zone to an
// IANA name. A site CA that passes is rewritten as the chart and the module take it.
func (s *Settings) checkSiteDefaults(p *problems, now time.Time) {
	if s.SiteCA != "" {
		if err := provisioner.CheckCertificateBundle(s.SiteCA, now); err != nil {
			p.addf("Site CA: the authority %s", err)
		} else {
			s.SiteCA = provisioner.NormalizeCertificateBundle(s.SiteCA)
		}
	}
	if s.TimeZone != "" {
		if err := provisioner.CheckTimeZone(s.TimeZone); err != nil {
			p.addf("Time zone: %s", err)
		}
	}
}

// ValidateSiteDefaults refuses a site CA or time zone that Validate refuses, with its message.
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

// trustSiteCA has a transport trust the site CA beside the authorities it trusts already.
func trustSiteCA(t *http.Transport, siteCA string) error {
	if err := (provisioner.ReleaseMirror{CABundle: siteCA}).Trust(t); err != nil {
		return fmt.Errorf("trusting the site CA: %w", err)
	}
	return nil
}
