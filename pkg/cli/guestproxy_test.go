package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

// savedGuest gives an appliance the saved settings and returns what the template builds give their guests.
func savedGuest(t *testing.T, saved map[string]any) *appliance.Appliance {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "management"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "management", "settings.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return &appliance.Appliance{Dir: dir}
}

func TestBakesTrustTheSavedSiteCAWithoutAProxy(t *testing.T) {
	t.Parallel()
	_, site := authorityFile(t, "site")
	guest := savedGuestProxy(savedGuest(t, map[string]any{"siteCA": site}))
	if guest == nil || guest.Enabled() || guest.CABundle != site {
		t.Errorf("guest = %+v, want the site CA and no proxy", guest)
	}
}

func TestBakesTrustTheSiteCAAndTheProxyAuthority(t *testing.T) {
	t.Parallel()
	_, site := authorityFile(t, "site")
	_, proxyCA := authorityFile(t, "proxy")
	guest := savedGuestProxy(savedGuest(t, map[string]any{"siteCA": site, "proxy": map[string]any{"url": "http://proxy.example:3128", "caBundle": proxyCA}}))
	if guest == nil || !guest.Enabled() || !strings.Contains(guest.CABundle, site) || !strings.Contains(guest.CABundle, proxyCA) {
		t.Errorf("guest = %+v, want the proxy and both authorities", guest)
	}
}

func TestBakesGetNothingWithNeither(t *testing.T) {
	t.Parallel()
	if guest := savedGuestProxy(savedGuest(t, map[string]any{})); guest != nil {
		t.Errorf("guest = %+v, want none", guest)
	}
}
