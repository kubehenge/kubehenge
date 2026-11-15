package appliance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestSiteDefaultsValidation(t *testing.T) {
	t.Parallel()
	ca := validAuthority(t)
	for name, tc := range map[string]struct{ ca, zone string }{
		"nothing set":     {},
		"a site CA":       {ca: ca},
		"a time zone":     {zone: "Europe/Zurich"},
		"both":            {ca: ca, zone: "UTC"},
		"a CA bundle":     {ca: ca + ca},
		"a zone with art": {zone: "America/Argentina/Buenos_Aires"},
	} {
		s := settings("pve1")
		s.SiteCA, s.TimeZone = tc.ca, tc.zone
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct{ ca, zone, want string }{
		"no certificate": {ca: "hello", want: "Site CA: the authority holds no PEM certificate"},
		"an expired one": {ca: chartAuthority, want: "Site CA"},
		"text after it":  {ca: ca + "extra", want: "Site CA"},
		"not a zone":     {zone: "Mars/Olympus", want: "Time zone"},
		"local time":     {zone: "Local", want: "Time zone"},
	} {
		s := settings("pve1")
		s.SiteCA, s.TimeZone = tc.ca, tc.zone
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestSiteDefaultsReachTheModuleOnlyWhenSet(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	s := settings("pve1")
	s.SiteCA, s.TimeZone = validAuthority(t), "Europe/Zurich"
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), "site_ca                = try(var.settings.siteCA, \"\")") || !strings.Contains(string(main), "time_zone              = try(var.settings.timeZone, \"\")") {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
	vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
	if err != nil || !strings.Contains(string(vars), `"siteCA"`) || !strings.Contains(string(vars), `"timeZone": "Europe/Zurich"`) {
		t.Fatalf("tfvars without the site defaults: %v\n%s", err, vars)
	}
	plain, _ := newAppliance(t)
	if err = plain.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, plain); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	vars, err = os.ReadFile(filepath.Join(plain.Dir, "management", "terraform.tfvars.json"))
	if err != nil || strings.Contains(string(vars), "siteCA") || strings.Contains(string(vars), "timeZone") {
		t.Fatalf("unset defaults reached the module: %v\n%s", err, vars)
	}
}

func TestReleaseMirrorInheritsTheSiteCAUnlessItHasItsOwn(t *testing.T) {
	t.Parallel()
	site, own := validAuthority(t), validAuthority(t)
	for name, tc := range map[string]struct {
		settings Settings
		want     string
	}{
		"no site CA":          {Settings{ReleaseMirror: &ReleaseMirror{URL: "https://m.example.org"}}, ""},
		"a site CA":           {Settings{SiteCA: site, ReleaseMirror: &ReleaseMirror{URL: "https://m.example.org"}}, site},
		"an own CA wins":      {Settings{SiteCA: site, ReleaseMirror: &ReleaseMirror{URL: "https://m.example.org", CABundle: own}}, own},
		"plain HTTP":          {Settings{SiteCA: site, ReleaseMirror: &ReleaseMirror{URL: "http://m.example.org"}}, ""},
		"no mirror":           {Settings{SiteCA: site}, ""},
		"a mirror from nil":   {Settings{SiteCA: site, ReleaseMirror: nil}, ""},
		"trusted by the user": {Settings{SiteCA: site, ReleaseMirror: &ReleaseMirror{URL: "https://m.example.org"}}, site},
	} {
		if got := tc.settings.EffectiveReleaseMirror().CABundle; got != tc.want {
			t.Errorf("%s: authority %q, want %q", name, got, tc.want)
		}
	}
}

func TestEtcdBackupStoreTrustsTheSiteCAUnlessTheTargetHasItsOwn(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	s := settings("pve1")
	s.SiteCA = validAuthority(t)
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	var got *provisioner.BackupTarget
	a.OpenStore = func(target *provisioner.BackupTarget, _ map[string][]byte) (provisioner.ObjectStore, error) {
		got = target
		return nil, nil //nolint:nilnil // only the target is read
	}
	for name, tc := range map[string]struct {
		target BackupTarget
		want   string
	}{
		"an endpoint without a CA": {BackupTarget{Name: "s3", Endpoint: "https://s3.example.org", Bucket: "b"}, s.SiteCA},
		"its own CA":               {BackupTarget{Name: "s3", Endpoint: "https://s3.example.org", Bucket: "b", CACertificate: "own"}, "own"},
		"no endpoint":              {BackupTarget{Name: "aws", Bucket: "b"}, ""},
	} {
		if _, err := a.openEtcdStore(&tc.target, &etcdBackupKeys{}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.CACertificate != tc.want {
			t.Errorf("%s: authority %q, want %q", name, got.CACertificate, tc.want)
		}
	}
}
