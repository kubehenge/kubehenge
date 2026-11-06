package appliance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// spellchecker:disable-next-line
const chartAuthority = "-----BEGIN CERTIFICATE-----\nMIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw\nDgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow\nEjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d\n7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B\n5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr\nBgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1\nNDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l\nWf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc\n6MF9+Yw1Yy0t\n-----END CERTIFICATE-----\n"

func TestChartRepositoriesValidation(t *testing.T) {
	t.Parallel()
	good := []*ChartRepositories{
		nil,
		{},
		{Mirrors: map[string]string{"ghcr.io": "oci://registry.example.org/ghcr"}},
		{Mirrors: map[string]string{"charts.jetstack.io": "https://charts.example.org:8443/jetstack", "helm.cilium.io": "http://10.0.0.5:8080"}, CABundle: chartAuthority},
	}
	for _, c := range good {
		s := settings("pve1")
		s.ChartRepositories = c
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := map[string]*ChartRepositories{
		"no address":         {Mirrors: map[string]string{"ghcr.io": "registry.example.org/ghcr"}},
		"a trailing slash":   {Mirrors: map[string]string{"ghcr.io": "oci://registry.example.org/ghcr/"}},
		"credentials":        {Mirrors: map[string]string{"ghcr.io": "oci://u:p@registry.example.org/ghcr"}}, //nolint:gosec // a fake address
		"another scheme":     {Mirrors: map[string]string{"ghcr.io": "ftp://registry.example.org/ghcr"}},
		"an authority alone": {CABundle: chartAuthority},
		"no certificate":     {Mirrors: map[string]string{"ghcr.io": "oci://r.example.org"}, CABundle: "hello"},
		"a private key":      {Mirrors: map[string]string{"ghcr.io": "oci://r.example.org"}, CABundle: chartAuthority + "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"},
		"too large":          {Mirrors: map[string]string{"ghcr.io": "oci://r.example.org"}, CABundle: chartAuthority + strings.Repeat("x", maxChartCABundle)},
	}
	for name, c := range bad {
		s := settings("pve1")
		s.ChartRepositories = c
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), "Chart mirror") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestChartRepositoriesReachTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	s := settings("pve1")
	s.ChartRepositories = &ChartRepositories{Mirrors: map[string]string{"ghcr.io": "oci://registry.example.org/ghcr"}, CABundle: chartAuthority}
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), "chart_repositories     = var.settings.chartRepositories") {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
	vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
	if err != nil || !strings.Contains(string(vars), `"ghcr.io": "oci://registry.example.org/ghcr"`) || !strings.Contains(string(vars), `"ca_bundle": "-----BEGIN CERTIFICATE-----`) {
		t.Fatalf("tfvars without the mirror: %v\n%s", err, vars)
	}
}

func TestNoChartRepositoriesGivesTheModuleNone(t *testing.T) {
	t.Parallel()
	got := moduleChartRepositories(nil)
	mirrors, ok := got["mirrors"].(map[string]string)
	if !ok || len(mirrors) != 0 || got[moduleCABundle] != "" {
		t.Errorf("no setting gave %v", got)
	}
}
