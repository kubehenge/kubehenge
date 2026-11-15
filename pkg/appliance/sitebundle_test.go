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
