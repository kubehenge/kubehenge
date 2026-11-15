package appliance

import (
	"strings"
	"testing"
)

func TestSiteCAIsSavedAsTheChartAndModuleTakeIt(t *testing.T) {
	t.Parallel()
	lf := validAuthority(t)
	messy := "Bag Attributes\r\n    friendlyName: site\r\nsubject=CN=site\r\n" + strings.ReplaceAll(lf, "\n", "\r\n")
	s := settings("pve1")
	s.SiteCA = messy
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.SiteCA != lf {
		t.Errorf("site CA %q, want the bare LF bundle %q", s.SiteCA, lf)
	}
	check := Settings{SiteCA: messy}
	if err := check.ValidateSiteDefaults(); err != nil || check.SiteCA != lf {
		t.Errorf("the check left %q (%v), want %q", check.SiteCA, err, lf)
	}
}

// TestProxyAndChartMirrorAuthoritiesAreSavedAsCertificatesAlone holds the two authorities to the
// site CA's rule: what lies between the blocks, such as a key's text commented out, never reaches
// the management nodes' trust roots or the proxy add-on's secret.
func TestProxyAndChartMirrorAuthoritiesAreSavedAsCertificatesAlone(t *testing.T) {
	t.Parallel()
	lf := validAuthority(t)
	const between = "# text that only the comment line holds"
	messy := "subject=CN=site\r\n" + strings.ReplaceAll(lf, "\n", "\r\n") + between + "\n" + between + "\n" + lf
	s := settings("pve1")
	s.Proxy = &Proxy{URL: "http://proxy.example.com:3128", CABundle: messy}
	s.ChartRepositories = &ChartRepositories{Mirrors: map[string]string{"ghcr.io": "oci://r.example.org"}, CABundle: messy}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Proxy.CABundle != lf+lf || s.ChartRepositories.CABundle != lf+lf {
		t.Errorf("proxy authority %q, chart mirror authority %q, want the two certificates alone", s.Proxy.CABundle, s.ChartRepositories.CABundle)
	}
	for what, v := range map[string]any{
		"proxy":              moduleProxy(s, "https://pve1.example.com:8006")[moduleCABundle],
		"chart repositories": moduleChartRepositories(s.ChartRepositories)[moduleCABundle],
	} {
		if got, ok := v.(string); !ok || strings.Contains(got, between) {
			t.Errorf("the module's %s authority holds the text between the blocks: %v", what, v)
		}
	}
	// A refused authority stays as given, for the form to show it again with its problem.
	damaged := strings.Replace(lf, "-----BEGIN", "----BEGIN", 1)
	refused := settings("pve1")
	refused.Proxy = &Proxy{URL: "http://proxy.example.com:3128", CABundle: damaged}
	refused.ChartRepositories = &ChartRepositories{Mirrors: map[string]string{"ghcr.io": "oci://r.example.org"}, CABundle: damaged}
	if err := refused.Validate(); err == nil || refused.Proxy.CABundle != damaged || refused.ChartRepositories.CABundle != damaged {
		t.Errorf("a damaged authority: %v, proxy %q, chart mirror %q, want it refused and left as given", err, refused.Proxy.CABundle, refused.ChartRepositories.CABundle)
	}
}
