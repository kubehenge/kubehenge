//go:build integration

package provisioner

import (
	"context"
	"os"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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

// TestIntegrationAClustersRangesAreSetOnCreation sends merge patches, as kubectl patch and edit do,
// to a running cluster. One that leaves the ranges out keeps the ones Kubehenge gave it, with its
// spec and generation as they were. One that changes a range, or moves it onto the nodes' network,
// is refused naming the field. One that changes something else goes through.
func TestIntegrationAClustersRangesAreSetOnCreation(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	api, _ := startClusterAPI(t, "testdata/clusterapi-values.yaml")
	c := api.Client
	ctx := context.Background()
	integration.ApplyClassTemplates(t, c, classNamespace, "kubehenge-k3s", templatesKey)
	integration.WaitReconciled(t, c, classNamespace, "kubehenge-k3s")
	if err := c.Create(ctx, clusterOf("kubehenge-k3s", "ranges", 30, nil)); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "team-a", Name: "ranges"}
	get := func() *clusterv1.Cluster {
		t.Helper()
		got := &clusterv1.Cluster{}
		if err := c.Get(ctx, key, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := get()
	if len(before.Spec.ClusterNetwork.Pods.CIDRBlocks) == 0 || len(before.Spec.ClusterNetwork.Services.CIDRBlocks) == 0 {
		t.Fatalf("created with ranges %+v, want the ones Kubehenge gives", before.Spec.ClusterNetwork)
	}
	patch := func(body string) error {
		return c.Patch(ctx, get(), client.RawPatch(types.MergePatchType, []byte(body)))
	}
	if err := patch(`{"spec":{"clusterNetwork":null}}`); err != nil {
		t.Fatalf("a patch that drops the ranges: %v", err)
	}
	if after := get(); rangesString(after) != rangesString(before) || after.Generation != before.Generation {
		t.Errorf("after a patch that drops the ranges: %s, generation %d, want %s and %d", rangesString(after), after.Generation, rangesString(before), before.Generation)
	}
	for name, tc := range map[string]struct{ body, field string }{
		"a new pod range":                  {`{"spec":{"clusterNetwork":{"pods":{"cidrBlocks":["172.20.0.0/16"]}}}}`, "spec.clusterNetwork.pods.cidrBlocks"},
		"services onto the nodes' network": {`{"spec":{"clusterNetwork":{"services":{"cidrBlocks":["10.0.30.0/24"]}}}}`, "spec.clusterNetwork.services.cidrBlocks"},
	} {
		err := patch(tc.body)
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.field+": Forbidden: set when the cluster is created, and cannot change") {
			t.Errorf("%s: %v, want %s refused as set on creation", name, err, tc.field)
		}
	}
	if err := patch(`{"spec":{"topology":{"controlPlane":{"replicas":3}}}}`); err != nil {
		t.Errorf("a change beside the ranges: %v", err)
	}
	if after := get(); rangesString(after) != rangesString(before) {
		t.Errorf("ranges after the refused patches: %s, want %s", rangesString(after), rangesString(before))
	}
}
