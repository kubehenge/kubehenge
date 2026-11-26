package provisioner

import (
	"context"
	"slices"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// readyCertManager is a cluster's cert-manager proxy with its release deployed.
func readyCertManager(c *clusterv1.Cluster) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	p.SetNamespace(c.Namespace)
	p.SetName(certManagerProxy(c))
	p.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(0)}}}
	return p
}

// setCertManagerReady marks the cert-manager proxy the controller made as deployed.
func setCertManagerReady(t *testing.T, kube client.Client, c *clusterv1.Cluster) {
	t.Helper()
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: certManagerProxy(c)}, p); err != nil {
		t.Fatal(err)
	}
	p.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": p.GetGeneration()}}}
	if err := kube.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func proxyExists(kube client.Client, c *clusterv1.Cluster, name string) bool {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	return kube.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: name}, p) == nil
}

// The request the interface sends for a new RKE2 cluster with monitoring and logs on.
const (
	formMonitoring = `{"enabled":true}`
	formLogs       = `{"enabled":true,"collector":"node"}`
)

// Monitoring and logs make cert-manager objects, which Helm cannot install before cert-manager's
// definitions exist: they wait for its release, and install once it is deployed.
func TestMonitoringAndLogsWaitForCertManager(t *testing.T) {
	t.Parallel()
	c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring, "logs": formLogs})
	c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if !proxyExists(kube, c, certManagerProxy(c)) {
		t.Fatal("monitoring and logs on, no cert-manager")
	}
	users := certManagerUsers(c)
	for _, name := range users {
		if proxyExists(kube, c, name) {
			t.Errorf("%s installed before cert-manager is deployed", name)
		}
	}
	setCertManagerReady(t, kube, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	for _, name := range users {
		if name != rancherProxy(c) && !proxyExists(kube, c, name) {
			t.Errorf("%s not installed once cert-manager is deployed", name)
		}
	}
}

// Without the Gateway API cert-manager is held back, and the add-ons that need it wait for it too.
func TestAddonsWaitWhileCertManagerWaitsForTheGatewayAPI(t *testing.T) {
	t.Parallel()
	c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring, "logs": formLogs})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(certManagerUsers(c), certManagerProxy(c)) {
		if proxyExists(kube, c, name) {
			t.Errorf("%s installed before the Gateway API is there", name)
		}
	}
}

// An add-on installed already stays while cert-manager is upgraded.
func TestInstalledAddonsStayWhileCertManagerUpgrades(t *testing.T) {
	t.Parallel()
	c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring})
	c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	r, kube := newReconciler(t, &fakeCloud{}, c, readyCertManager(c))
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: certManagerProxy(c)}, p); err != nil {
		t.Fatal(err)
	}
	p.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "observedGeneration": int64(0)}}}
	if err := kube.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if !proxyExists(kube, c, monitoringProxy(c)) {
		t.Error("monitoring went while cert-manager was not ready")
	}
}

// cert-manager comes with whichever of its users is on, and goes with the last.
func TestCertManagerComesWithEachOfItsUsers(t *testing.T) {
	t.Parallel()
	for name, variables := range map[string]map[string]string{
		"monitoring":   {"monitoring": formMonitoring},
		"logs":         {"monitoring": formMonitoring, "logs": formLogs},
		"rancher":      {"rancher": `{"install":true,"hostname":"rancher.example.org"}`},
		"certificates": {certificatesVariable: `{"enabled": true, "domain": "lab.example.org"}`},
		"all":          {"monitoring": formMonitoring, "logs": formLogs, "certificates": `{"enabled": true, "domain": "lab.example.org"}`},
	} {
		c := withAddons(storageCluster(), variables)
		addons, err := addonsOf(c)
		if err != nil {
			t.Fatal(err)
		}
		proxies, err := helmChartProxies(c, &testCharts, templates.RKE2, "local-lvm", CNI{}, &addons)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(proxies, func(p *unstructured.Unstructured) bool { return p.GetName() == certManagerProxy(c) }) {
			t.Errorf("%s on, no cert-manager", name)
		}
	}
	none := storageCluster()
	addons, err := addonsOf(none)
	if err != nil {
		t.Fatal(err)
	}
	if addons.certManager() || addons.needsCertManager() {
		t.Error("cert-manager with every add-on off")
	}
}

// Logs off leaves cert-manager to monitoring, and monitoring off after it takes cert-manager too.
func TestCertManagerGoesWithItsLastUser(t *testing.T) {
	t.Parallel()
	c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring, "logs": formLogs})
	c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	setCertManagerReady(t, kube, c)
	set := func(logs, monitoring string) {
		t.Helper()
		current := &clusterv1.Cluster{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), current); err != nil {
			t.Fatal(err)
		}
		for i := range current.Spec.Topology.Variables {
			switch v := &current.Spec.Topology.Variables[i]; v.Name {
			case "logs":
				v.Value = apiextensionsv1.JSON{Raw: []byte(logs)}
			case "monitoring":
				v.Value = apiextensionsv1.JSON{Raw: []byte(monitoring)}
			}
		}
		if err := kube.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		if err := reconcile(t, r, current); err != nil {
			t.Fatal(err)
		}
	}
	set(`{"enabled":false}`, formMonitoring)
	if !proxyExists(kube, c, certManagerProxy(c)) {
		t.Error("switching logs off removed cert-manager while monitoring runs")
	}
	set(`{"enabled":false}`, `{"enabled":false}`)
	if proxyExists(kube, c, certManagerProxy(c)) {
		t.Error("cert-manager stays after its last user is off")
	}
}

// With the cluster's own cert-manager, Kubehenge installs none and the add-ons wait for its
// definitions in the cluster.
func TestAddonsWaitForTheClustersOwnCertManagerDefinitions(t *testing.T) {
	t.Parallel()
	c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring, useExistingVariable: `{"certManager":true}`})
	c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	r, kube := newReconciler(t, &fakeCloud{}, c)
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if proxyExists(kube, c, certManagerProxy(c)) {
		t.Error("cert-manager installed beside the cluster's own")
	}
	if proxyExists(kube, c, monitoringProxy(c)) {
		t.Error("monitoring installed before the cluster serves cert-manager's definitions")
	}
	for _, name := range certManagerCRDs {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK)
		crd.SetName(name)
		if err := workload.Create(context.Background(), crd); err != nil {
			t.Fatal(err)
		}
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
		if got := proxyExists(kube, c, monitoringProxy(c)); got != (name == certManagerCRDs[len(certManagerCRDs)-1]) {
			t.Errorf("after %s, monitoring installed: %v", name, got)
		}
	}
}

// Each add-on that makes cert-manager objects gets them from a cert-manager the cluster installs,
// unless it brings its own.
func TestEveryCertManagerUserHasACertManager(t *testing.T) {
	t.Parallel()
	c := withAddons(storageCluster(), map[string]string{
		"monitoring": formMonitoring, "logs": formLogs, "rancher": `{"install":true,"hostname":"rancher.example.org"}`,
	})
	addons, err := addonsOf(c)
	if err != nil {
		t.Fatal(err)
	}
	proxies, err := helmChartProxies(c, &testCharts, templates.RKE2, "local-lvm", CNI{}, &addons)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(proxies))
	for i, p := range proxies {
		names[i] = p.GetName()
	}
	for _, user := range certManagerUsers(c) {
		if slices.Contains(names, user) && !slices.Contains(names, certManagerProxy(c)) {
			t.Errorf("%s installs without a cert-manager", user)
		}
	}
}
