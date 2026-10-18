package provisioner

import (
	"context"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
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

// setKyvernoReady sets the Ready condition of the web cluster's Kyverno HelmChartProxy as the Add-on
// Provider for Helm does, for the generation behind generations before its current one.
func setKyvernoReady(t *testing.T, kube client.Client, status string, behind int64) {
	t.Helper()
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-kyverno"}, hcp); err != nil {
		t.Fatal(err)
	}
	hcp.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": status, "observedGeneration": hcp.GetGeneration() - behind},
	}}
	if err := kube.Update(context.Background(), hcp); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileInstallsKyvernoPoliciesOnceKyvernoIsReady(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "podSecurity", Value: apiextensionsv1.JSON{Raw: []byte(`{"kyverno":"restricted"}`)}})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	ctx := context.Background()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}
	policies := func() error {
		hcp := &unstructured.Unstructured{}
		hcp.SetGroupVersionKind(helmChartProxyGVK)
		return kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "web-kyverno-policies"}, hcp)
	}
	kyverno := &unstructured.Unstructured{}
	kyverno.SetGroupVersionKind(helmChartProxyGVK)

	for _, ready := range []func(){
		func() {},
		// Ready for an earlier spec says nothing of this one.
		func() { setKyvernoReady(t, kube, "True", 1) },
		func() { setKyvernoReady(t, kube, "False", 0) },
	} {
		ready()
		res, err := r.Reconcile(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "web-kyverno"}, kyverno); err != nil {
			t.Fatalf("Kyverno is not being installed: %v", err)
		}
		if err := policies(); !apierrors.IsNotFound(err) {
			t.Fatalf("Kyverno's policies are installed before Kyverno is ready: %v", err)
		}
		if res.RequeueAfter != waitInterval {
			t.Errorf("waiting on Kyverno, the cluster is looked at again after %v", res.RequeueAfter)
		}
	}

	setKyvernoReady(t, kube, "True", 0)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if err := policies(); err != nil {
		t.Fatalf("Kyverno is ready and its policies are not installed: %v", err)
	}
	// Kyverno being upgraded leaves its policies where they are.
	setKyvernoReady(t, kube, "False", 0)
	res, err := r.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := policies(); err != nil {
		t.Errorf("Kyverno's policies went while Kyverno was not ready: %v", err)
	}
	if res.RequeueAfter != resync {
		t.Errorf("with nothing to wait on, the cluster is looked at again after %v", res.RequeueAfter)
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
	setKyvernoReady(t, kube, "True", 0)
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
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(current)})
	if err != nil {
		t.Fatal(err)
	}
	// Removing the policies changes nothing the controller watches.
	if res.RequeueAfter != waitInterval {
		t.Errorf("waiting on Kyverno's policies, the cluster is looked at again after %v", res.RequeueAfter)
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
