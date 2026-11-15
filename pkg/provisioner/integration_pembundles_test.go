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

// TestIntegrationAReplacedClusterKeepsItsRanges replaces a cluster with the manifest it was created
// from, as kubectl replace and a GitOps sync that replaces do. The manifest names no ranges, and the
// cluster keeps the ones Kubehenge gave it, so its spec and generation stay as they were.
func TestIntegrationAReplacedClusterKeepsItsRanges(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	api, _ := startClusterAPI(t, "testdata/clusterapi-values.yaml")
	c := api.Client
	ctx := context.Background()
	integration.ApplyClassTemplates(t, c, classNamespace, "kubehenge-k3s", templatesKey)
	integration.WaitReconciled(t, c, classNamespace, "kubehenge-k3s")
	// A copy, since Create fills in what it sends. clusterOf orders the variables anew on each call.
	manifest := clusterOf("kubehenge-k3s", "replaced", 30, nil)
	if err := c.Create(ctx, manifest.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "team-a", Name: "replaced"}
	before := &clusterv1.Cluster{}
	if err := c.Get(ctx, key, before); err != nil {
		t.Fatal(err)
	}
	if len(before.Spec.ClusterNetwork.Pods.CIDRBlocks) == 0 || len(before.Spec.ClusterNetwork.Services.CIDRBlocks) == 0 {
		t.Fatalf("created with ranges %+v, want the ones Kubehenge gives", before.Spec.ClusterNetwork)
	}
	manifest.ResourceVersion = before.ResourceVersion
	if err := c.Update(ctx, manifest); err != nil {
		t.Fatalf("replacing the cluster with its manifest: %v", err)
	}
	after := &clusterv1.Cluster{}
	if err := c.Get(ctx, key, after); err != nil {
		t.Fatal(err)
	}
	if rangesString(after) != rangesString(before) || after.Generation != before.Generation {
		t.Errorf("after the replace: ranges %s, generation %d, want %s and %d", rangesString(after), after.Generation, rangesString(before), before.Generation)
	}
}
