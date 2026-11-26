package ui

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// siteCert is a generated certificate and its key.
type siteCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func (c siteCert) pem() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw}))
}

// issueSiteCert signs a certificate named cn with parent, or itself when parent is nil.
func issueSiteCert(t *testing.T, cn string, parent *siteCert, ca bool) siteCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: ca, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, DNSNames: []string{cn},
	}
	signer, signKey := tmpl, key
	if parent != nil {
		signer, signKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return siteCert{cert: cert, key: key}
}

// normalisedSiteCA is the Site CA as saving rewrites it.
func normalisedSiteCA(t *testing.T, ca string) string {
	t.Helper()
	s := appliance.Settings{SiteCA: ca}
	if err := s.ValidateSiteDefaults(); err != nil {
		t.Fatal(err)
	}
	return s.SiteCA
}

func TestSiteCARollPreviewComparesTheBundleTheClustersRun(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	saved := normalisedSiteCA(t, issueSiteCert(t, "Inspect Root", nil, true).pem())
	bundle := saved + issueSiteCert(t, "Inspect Root", nil, true).pem()
	dir := filepath.Join(h.server.Appliance.Dir, "management")
	raw, err := json.Marshal(appliance.Settings{SiteCA: saved, SiteBundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	running, err := json.Marshal([]string{strings.TrimSpace(bundle)})
	if err != nil {
		t.Fatal(err)
	}
	c := cluster("team-a", "web")
	c.Status.Conditions = []metav1.Condition{{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue}}
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: provisioner.SiteCAsVariable, Value: apiextensionsv1.JSON{Raw: running}})
	if err = h.kube.Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		siteCA string
		want   string
	}{
		"the saved Site CA": {saved, `"now":0`},
		"another Site CA":   {issueSiteCert(t, "Other Root", nil, true).pem(), `"now":1`},
	} {
		body, err := json.Marshal(map[string]string{"siteCA": tc.siteCA})
		if err != nil {
			t.Fatal(err)
		}
		rec := b.do("POST", "/api/setup/site-ca-roll", string(body))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"inherits":1`) || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s, want %s", name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// TestSetupCheckNamesTheFaultsOfTheSiteCAChainAsTheSaveDoes runs the chain check in the live check,
// which then shows the fault the save refuses with.
func TestSetupCheckNamesTheFaultsOfTheSiteCAChainAsTheSaveDoes(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	root := issueSiteCert(t, "Inspect Root", nil, true)
	intermediate := issueSiteCert(t, "Inspect Intermediate", &root, true)
	leaf := issueSiteCert(t, "host.example", &intermediate, false)
	h.server.Appliance.SiteChains = func(context.Context, *appliance.Settings, string) ([]*x509.Certificate, error) {
		return []*x509.Certificate{leaf.cert}, nil
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(preflightApply), &settings); err != nil {
		t.Fatal(err)
	}
	inner, ok := settings["settings"].(map[string]any)
	if !ok {
		t.Fatal("the settings are no object")
	}
	inner["siteCA"] = root.pem()
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	const want = `Site CA: factory.talos.dev, ghcr.io, registry.k8s.io, tuf-repo-cdn.sigstore.dev: Intermediate "CN=Inspect Intermediate" is not presented. Have the device send it.`
	check, err := json.Marshal(map[string]any{"settings": settings["settings"]})
	if err != nil {
		t.Fatal(err)
	}
	f := checkSettings(t, b, string(check))
	if len(f.Errors) != 1 || f.Errors[0] != want {
		t.Errorf("the live check gave %q, want %q", f.Errors, want)
	}
	rec := b.do("POST", "/api/setup", string(raw))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Have the device send it.") || h.server.Appliance.Current() != nil {
		t.Errorf("the save answered %d %s", rec.Code, rec.Body.String())
	}
}

// TestSetupCheckHandsOverTheRootAChainEndsAtAndTheSiteCALacks returns the presented root by name and as
// PEM, from the live check and from the refused save, for the interface and the CLI to append to the
// Site CA, and finds nothing wrong once it is there.
func TestSetupCheckHandsOverTheRootAChainEndsAtAndTheSiteCALacks(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	root := issueSiteCert(t, "Inspect Root", nil, true)
	intermediate := issueSiteCert(t, "Inspect Intermediate", &root, true)
	leaf := issueSiteCert(t, "host.example", &intermediate, false)
	h.server.Appliance.SiteChains = func(context.Context, *appliance.Settings, string) ([]*x509.Certificate, error) {
		return []*x509.Certificate{leaf.cert, intermediate.cert, root.cert}, nil
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(preflightApply), &settings); err != nil {
		t.Fatal(err)
	}
	inner, ok := settings["settings"].(map[string]any)
	if !ok {
		t.Fatal("the settings are no object")
	}
	check := func(siteCA string) appliance.Findings {
		t.Helper()
		inner["siteCA"] = siteCA
		body, err := json.Marshal(map[string]any{"settings": inner})
		if err != nil {
			t.Fatal(err)
		}
		return checkSettings(t, b, string(body))
	}
	other := issueSiteCert(t, "Other Root", nil, true)
	f := check(other.pem())
	want := []appliance.MissingRoot{{Name: "Inspect Root", PEM: root.pem()}}
	if len(f.Errors) != 1 || !strings.Contains(f.Errors[0], "Chain ends at \"CN=Inspect Root\" (SHA-256 ") || !slices.Equal(f.MissingRoots, want) {
		t.Fatalf("the live check gave errors %q and roots %q", f.Errors, f.MissingRoots)
	}
	saved, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	rec := b.do("POST", "/api/setup", string(saved))
	var refused struct {
		Error        string                  `json:"error"`
		MissingRoots []appliance.MissingRoot `json:"missingRoots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil || rec.Code != http.StatusUnprocessableEntity ||
		refused.Error != f.Errors[0] || !slices.Equal(refused.MissingRoots, want) {
		t.Errorf("the save answered %d %s", rec.Code, rec.Body.String())
	}
	if again := check(appliance.WithRoots(other.pem(), f.MissingRoots)); len(again.Errors) != 0 || len(again.MissingRoots) != 0 {
		t.Errorf("with the root added the live check gave errors %q and roots %q", again.Errors, again.MissingRoots)
	}
}

// TestSetupCheckWarnsOfAChainFaultWhileTheSiteCAIsTheSaved gives the fault as a warning where the
// Site CA is the saved one, since the save then keeps the last run's certificates.
func TestSetupCheckWarnsOfAChainFaultWhileTheSiteCAIsTheSaved(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	root := issueSiteCert(t, "Inspect Root", nil, true)
	intermediate := issueSiteCert(t, "Inspect Intermediate", &root, true)
	leaf := issueSiteCert(t, "host.example", &intermediate, false)
	h.server.Appliance.SiteChains = func(context.Context, *appliance.Settings, string) ([]*x509.Certificate, error) {
		return []*x509.Certificate{leaf.cert}, nil
	}
	siteCA := normalisedSiteCA(t, root.pem())
	dir := filepath.Join(h.server.Appliance.Dir, "management")
	saved, err := json.Marshal(appliance.Settings{SiteCA: siteCA})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "settings.json"), saved, 0o600); err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err = json.Unmarshal([]byte(preflightApply), &settings); err != nil {
		t.Fatal(err)
	}
	inner, ok := settings["settings"].(map[string]any)
	if !ok {
		t.Fatal("the settings are no object")
	}
	inner["siteCA"] = siteCA
	check, err := json.Marshal(map[string]any{"settings": inner})
	if err != nil {
		t.Fatal(err)
	}
	const want = `Site CA: factory.talos.dev, ghcr.io, registry.k8s.io, tuf-repo-cdn.sigstore.dev: Intermediate "CN=Inspect Intermediate" is not presented. Have the device send it.`
	f := checkSettings(t, b, string(check))
	if slices.Contains(f.Errors, want) || !slices.Contains(f.Warnings, want) {
		t.Errorf("the live check gave errors %q and warnings %q, want the warning %q", f.Errors, f.Warnings, want)
	}
}
