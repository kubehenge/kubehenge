package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	formLogs       = `{"enabled":true,"collector":"node","retentionDays":7,"sizeGiB":10}`
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
	users := []string{monitoringProxy(c), lokiProxy(c), alloyProxy(c), alloyNodeProxy(c)}
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
		if !proxyExists(kube, c, name) {
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

// An add-on installed already gets its changes while cert-manager is upgraded.
func TestInstalledAddonsChangeWhileCertManagerUpgrades(t *testing.T) {
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
	current := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	for i := range current.Spec.Topology.Variables {
		if v := &current.Spec.Topology.Variables[i]; v.Name == "monitoring" {
			v.Value = apiextensionsv1.JSON{Raw: []byte(`{"enabled":true,"hostname":"grafana.example.org"}`)}
		}
	}
	if err := kube.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, current); err != nil {
		t.Fatal(err)
	}
	monitoring := &unstructured.Unstructured{}
	monitoring.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: monitoringProxy(c)}, monitoring); err != nil {
		t.Fatal(err)
	}
	if values := specText(monitoring.Object["spec"], "valuesTemplate"); !strings.Contains(values, "grafana.example.org") {
		t.Error("monitoring's change waited while cert-manager upgraded")
	}
}

// Rancher, monitoring, Loki and both Alloys each wait for cert-manager's release.
func TestEachCertManagerUserWaits(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	r, _ := newReconciler(t, &fakeCloud{}, c)
	for _, name := range []string{rancherProxy(c), monitoringProxy(c), lokiProxy(c), alloyProxy(c), alloyNodeProxy(c)} {
		addons := &Addons{}
		waits, err := r.chartWaits(context.Background(), c, name, addons)
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.TrimPrefix(name, c.Name+"-"); !waits || !slices.Equal(addons.certManagerWaiting, []string{want}) {
			t.Errorf("%s: waits %v, waiting for cert-manager %v", name, waits, addons.certManagerWaiting)
		}
	}
}

// helmRelease is a deployed revision of a Helm release in kube-system, as Helm stores it.
func helmRelease(name string) *corev1.Secret {
	return &corev1.Secret{
		Namespace: metav1.NamespaceSystem, Name: "sh.helm.release.v1." + name + ".v1",
		Labels: map[string]string{"owner": "helm", "name": name, "status": "deployed", "version": "1"},
	}
}

// The add-ons that wait for cert-manager say so on the cluster, with what cert-manager waits for,
// and the line goes once cert-manager is deployed.
func TestAddonsWaitingSayWhatCertManagerWaitsFor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		annotated   bool
		initialised time.Duration
		workload    []client.Object
		reason      string
		says        string
	}{
		"installing":          {annotated: true, reason: AddonsWaitingForCertManager, says: "monitoring, loki, alloy and alloy-node wait for cert-manager to be deployed"},
		"gateway, new":        {initialised: time.Minute, reason: AddonsWaitingForCertManager, says: "wait for cert-manager, which waits for the cluster's Gateway API definitions"},
		"gateway, no chart":   {initialised: time.Hour, reason: AddonsWaitingForCertManager, says: "Gateway API definitions from rke2-traefik-crd"},
		"gateway, unrecorded": {initialised: time.Hour, workload: []client.Object{helmRelease("rke2-traefik-crd")}, reason: AddonsWaitingGatewayAPIMissing, says: "rke2-traefik-crd is deployed, yet none were recorded in 10 minutes"},
	} {
		c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring, "logs": formLogs})
		if tc.annotated {
			c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
		}
		if tc.initialised > 0 {
			c.Status.Conditions = []metav1.Condition{{
				Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(now.Add(-tc.initialised)), Reason: "Initialised",
			}}
		}
		r, kube := newReconciler(t, &fakeCloud{}, c)
		r.Now = func() time.Time { return now }
		workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(tc.workload...).Build()
		r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
		got := &clusterv1.Cluster{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, AddonsWaitingCondition)
		if cond == nil || cond.Reason != tc.reason || !strings.Contains(cond.Message, tc.says) {
			t.Errorf("%s: condition %+v", name, cond)
			continue
		}
		if !tc.annotated {
			continue
		}
		setCertManagerReady(t, kube, c)
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
			t.Fatal(err)
		}
		if cond := meta.FindStatusCondition(got.Status.Conditions, AddonsWaitingCondition); cond != nil {
			t.Errorf("%s: still waiting once cert-manager is deployed: %+v", name, cond)
		}
	}
}

// A cert-manager of the cluster's own holds Kubehenge's back, and its users with it: one line names
// what was found and what waits.
func TestForeignCertManagerHoldsItsUsersInOneLine(t *testing.T) {
	t.Parallel()
	c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring, "logs": formLogs})
	c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	r, kube := newReconciler(t, &fakeCloud{}, c)
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(crd("certificates.cert-manager.io", nil)).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{certManagerProxy(c), monitoringProxy(c), lokiProxy(c)} {
		if proxyExists(kube, c, name) {
			t.Errorf("%s installed beside the cluster's own cert-manager", name)
		}
	}
	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	conflict := meta.FindStatusCondition(got.Status.Conditions, AddonConflictCondition)
	if conflict == nil || !strings.Contains(conflict.Message, "certificates.cert-manager.io") || !strings.Contains(conflict.Message, "monitoring, loki, alloy and alloy-node wait") {
		t.Errorf("conflict %+v", conflict)
	}
	if waiting := meta.FindStatusCondition(got.Status.Conditions, AddonsWaitingCondition); waiting != nil {
		t.Errorf("the wait is said twice: %+v", waiting)
	}
}

// A cluster created with the retired useExisting.certManager keeps reconciling: the variable comes
// off, and Kubehenge installs cert-manager for its monitoring and logs.
func TestClusterThatUsedAnExistingCertManagerGetsKubehengesOwn(t *testing.T) {
	t.Parallel()
	c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring, "logs": formLogs, "useExisting": `{"certManager":true}`})
	c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(got.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool { return v.Name == "useExisting" }) {
		t.Error("the class no longer defines useExisting, and the cluster still sets it")
	}
	if !proxyExists(kube, c, certManagerProxy(c)) {
		t.Fatal("no cert-manager for monitoring and logs")
	}
	setCertManagerReady(t, kube, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{monitoringProxy(c), lokiProxy(c), alloyProxy(c), alloyNodeProxy(c)} {
		if !proxyExists(kube, c, name) {
			t.Errorf("%s not installed once cert-manager is deployed", name)
		}
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
	if addons.certManager() {
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

// Each add-on that makes cert-manager objects gets them from a cert-manager the cluster installs.
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
