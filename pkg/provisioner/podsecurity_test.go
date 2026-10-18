package provisioner

import (
	"context"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestKyvernoEnforcesTheChosenLevel(t *testing.T) {
	t.Parallel()
	for _, level := range []string{"baseline", "restricted"} {
		proxies, err := helmChartProxies(cluster(), &testCharts, "kubeadm", "local-lvm", CNI{}, &Addons{PodSecurity: PodSecurity{Kyverno: level}})
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]map[string]any{}
		for _, p := range proxies {
			byName[p.GetName()] = lookup[map[string]any](t, p.Object, "spec")
		}
		kyverno, policies := byName["web-kyverno"], byName["web-kyverno-policies"]
		if kyverno == nil || policies == nil || kyverno["namespace"] != "kyverno" || policies["namespace"] != "kyverno" {
			t.Fatalf("%s: Kyverno and its policies are not installed into the kyverno namespace: %v", level, byName)
		}
		own := map[string]any{}
		if err := yaml.Unmarshal([]byte(lookup[string](t, kyverno, "valuesTemplate")), &own); err != nil {
			t.Fatal(err)
		}
		if !lookup[bool](t, own, "features", "policyExceptions", "enabled") || lookup[string](t, own, "features", "policyExceptions", "namespace") != "kyverno" {
			t.Errorf("%s: Kyverno takes exceptions from anywhere, or none: %v", level, own)
		}
		values := map[string]any{}
		if err := yaml.Unmarshal([]byte(lookup[string](t, policies, "valuesTemplate")), &values); err != nil {
			t.Fatal(err)
		}
		if values["podSecurityStandard"] != level || values["validationFailureAction"] != "Enforce" || values["failurePolicy"] != "Fail" {
			t.Errorf("%s: policies %v", level, values)
		}
		exempt := lookup[[]any](t, values, "vpolExclude", "excludeNamespaces")
		if !slices.Equal(exempt, []any{"kube-system", "node-exporter"}) {
			t.Errorf("%s: policies leave alone %v, not the namespaces Pod Security Admission exempts", level, exempt)
		}
	}
	for _, p := range []PodSecurity{{}, {Kyverno: "off"}, {Admission: "restricted"}} {
		proxies, err := helmChartProxies(cluster(), &testCharts, "kubeadm", "local-lvm", CNI{}, &Addons{PodSecurity: p})
		if err != nil {
			t.Fatal(err)
		}
		for _, proxy := range proxies {
			if proxy.GetName() == "web-kyverno" || proxy.GetName() == "web-kyverno-policies" {
				t.Errorf("%+v installs %s", p, proxy.GetName())
			}
		}
	}
}

func TestReconcileRemovesKyvernoAfterItsPolicies(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "podSecurity", Value: apiextensionsv1.JSON{Raw: []byte(`{"kyverno":"restricted"}`)}})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lookUp := func(name string) error {
		hcp := &unstructured.Unstructured{}
		hcp.SetGroupVersionKind(helmChartProxyGVK)
		return kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, hcp)
	}
	get := func(name string) *unstructured.Unstructured {
		t.Helper()
		hcp := &unstructured.Unstructured{}
		hcp.SetGroupVersionKind(helmChartProxyGVK)
		if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, hcp); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return hcp
	}
	update := func(o client.Object) {
		t.Helper()
		if err := kube.Update(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	policies := get("web-kyverno-policies")
	get("web-kyverno")
	// The Add-on Provider for Helm holds a proxy until its release is uninstalled.
	policies.SetFinalizers([]string{"addons.cluster.x-k8s.io"})
	update(policies)

	current := &clusterv1.Cluster{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Topology.Variables[len(current.Spec.Topology.Variables)-1].Value.Raw = []byte(`{"kyverno":"off"}`)
	update(current)
	if err := reconcile(t, r, current); err != nil {
		t.Fatal(err)
	}
	if err := lookUp("web-kyverno"); err != nil {
		t.Fatalf("Kyverno removed before its policies are uninstalled: %v", err)
	}

	policies = get("web-kyverno-policies")
	if policies.GetDeletionTimestamp() == nil {
		t.Fatal("Kyverno's policies are not being removed")
	}
	policies.SetFinalizers(nil)
	update(policies)
	if err := reconcile(t, r, current); err != nil {
		t.Fatal(err)
	}
	if err := lookUp("web-kyverno"); !apierrors.IsNotFound(err) {
		t.Errorf("Kyverno stays after its policies are gone: %v", err)
	}
}
