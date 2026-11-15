//go:build integration

package provisioner

import (
	"context"
	"os"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/test/integration"
)

// TestIntegrationHandKeptBundlesAreStoredAsCertificatesAlone creates clusters as kubectl does whose
// trustedCAs, registry mirror and release mirror hold a bundle laid out as curl's cacert.pem, once
// Cluster API has passed their classes and so checks their variables against the schemas. Kubehenge's
// webhook writes each as its certificates alone before Cluster API's sees it, on create and update,
// and refuses a damaged one by its line without repeating it.
func TestIntegrationHandKeptBundlesAreStoredAsCertificatesAlone(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	api, _ := startClusterAPI(t, "testdata/clusterapi-values.yaml")
	c := api.Client
	ctx := context.Background()
	for _, class := range []string{"kubehenge-kubeadm", "kubehenge-k3s"} {
		integration.ApplyClassTemplates(t, c, classNamespace, class, templatesKey)
		integration.WaitReconciled(t, c, classNamespace, class)
	}
	raw, err := os.ReadFile("testdata/curl-cacert.txt")
	if err != nil {
		t.Fatal(err)
	}
	curl := string(raw)
	plain, err := NormalizeCertificateBundle(curl)
	if err != nil {
		t.Fatal(err)
	}
	mirror := func(bundle string) string {
		return jsonOf(t, []map[string]any{{"registry": "docker.io", "endpoints": []string{"https://mirror.example.com"}, "caBundle": bundle}})
	}
	release := func(bundle string) string {
		return jsonOf(t, map[string]string{"url": "https://mirror.example.com", "caBundle": bundle})
	}
	stored := func(name string) *clusterv1.Cluster {
		got := &clusterv1.Cluster{}
		if getErr := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, got); getErr != nil {
			t.Fatal(getErr)
		}
		return got
	}
	for i, tc := range []struct{ name, class, variable, value, want string }{
		{"trusted", "kubehenge-kubeadm", trustedCAsVariable, jsonOf(t, []string{curl}), jsonOf(t, []string{plain})},
		{"mirror", "kubehenge-kubeadm", registryMirrorsVariable, mirror(curl), mirror(plain)},
		{"release", "kubehenge-k3s", releaseMirrorVariable, release(curl), release(plain)},
	} {
		if err = c.Create(ctx, clusterOf(tc.class, tc.name, 10+i, map[string]string{tc.variable: tc.value})); err != nil {
			t.Errorf("%s: a hand-kept bundle created as kubectl does: %v", tc.name, err)
			continue
		}
		if got := storedVariable(stored(tc.name), tc.variable); got != tc.want {
			t.Errorf("%s: stored %s, want the certificates alone", tc.name, got)
		}
	}

	// An update that sets a bundle with other comments is stored the same way.
	live := stored("trusted")
	withVariable(live, trustedCAsVariable, jsonOf(t, []string{"# Site roots\n" + curl}))
	if err = c.Update(ctx, live); err != nil {
		t.Fatalf("an update with a hand-kept bundle: %v", err)
	}
	if got := storedVariable(stored("trusted"), trustedCAsVariable); got != jsonOf(t, []string{plain}) {
		t.Errorf("updated to %s, want the certificates alone", got)
	}

	damaged := strings.Replace(curl, "-----BEGIN CERTIFICATE-----", "----BEGIN CERTIFICATE-----", 1)
	err = c.Create(ctx, clusterOf("kubehenge-kubeadm", "damaged", 20, map[string]string{trustedCAsVariable: jsonOf(t, []string{damaged})}))
	if err == nil || !strings.Contains(err.Error(), "spec.topology.variables[trustedCAs][0]") || !strings.Contains(err.Error(), "has a damaged BEGIN or END line on line 15") ||
		strings.Contains(err.Error(), "Bundle of CA Root Certificates") {
		t.Errorf("a damaged bundle: %v, want it refused by its line without its text", err)
	}
}
