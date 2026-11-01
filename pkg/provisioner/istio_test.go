package provisioner

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// proxySpecs are the specs of the cluster's proxies, by name, with their values.
func proxySpecs(t *testing.T, distribution templates.Distribution, cni CNI, addons *Addons) (map[string]map[string]any, map[string]map[string]any) {
	t.Helper()
	proxies, err := helmChartProxies(cluster(), &testCharts, distribution, "local-lvm", cni, addons)
	if err != nil {
		t.Fatal(err)
	}
	specs, values := map[string]map[string]any{}, map[string]map[string]any{}
	for _, p := range proxies {
		spec := lookup[map[string]any](t, p.Object, "spec")
		v := map[string]any{}
		if err := yaml.Unmarshal([]byte(lookup[string](t, spec, "valuesTemplate")), &v); err != nil {
			t.Fatal(err)
		}
		specs[p.GetName()], values[p.GetName()] = spec, v
	}
	return specs, values
}

func TestIstioInstallsInAmbientMode(t *testing.T) {
	t.Parallel()
	istio := &Addons{Istio: Istio{Enabled: true}}
	specs, values := proxySpecs(t, templates.RKE2, CNI{}, istio)
	for name, want := range map[string]struct {
		chart     Chart
		namespace string
	}{
		"web-istio-base": {testCharts.IstioBase, "istio-system"},
		"web-istiod":     {testCharts.Istio, "istio-system"},
		"web-istio-cni":  {testCharts.IstioCNI, "istio-node"},
		"web-ztunnel":    {testCharts.IstioZtunnel, "istio-node"},
	} {
		spec := specs[name]
		if spec == nil {
			t.Fatalf("no proxy %s among %v", name, specs)
		}
		if spec["chartName"] != want.chart.Name || spec["namespace"] != want.namespace || spec["repoURL"] != want.chart.Repository ||
			spec["version"] != "1.31.2@sha256:"+want.chart.Digest {
			t.Errorf("%s installs %v/%v %v into %v", name, spec["repoURL"], spec["chartName"], spec["version"], spec["namespace"])
		}
	}
	for _, name := range []string{"web-istiod", "web-istio-cni", "web-ztunnel"} {
		if values[name]["profile"] != "ambient" {
			t.Errorf("%s is not in ambient mode: %v", name, values[name])
		}
	}
	for _, name := range []string{"web-istio-cni", "web-ztunnel"} {
		if lookup[string](t, values[name], "seLinuxOptions", "type") != "spc_t" || lookup[string](t, values[name], "seLinuxOptions", "level") != "s0" {
			t.Errorf("%s cannot write its sockets on an SELinux node: %v", name, values[name])
		}
	}
	istiod := values["web-istiod"]
	if lookup[string](t, istiod, "pilot", "trustedZtunnelNamespace") != "istio-node" {
		t.Errorf("istiod does not trust the ztunnel beside istio-cni: %v", istiod)
	}
	for _, path := range [][]any{{"pilot", "seccompProfile", "type"}, {"gateways", "seccompProfile", "type"}} {
		if lookup[string](t, istiod, path...) != "RuntimeDefault" {
			t.Errorf("%v short of Pod Security's restricted level: %v", path, istiod)
		}
	}
	// The addon-memory figure for Istio counts on what these two ask for.
	if lookup[string](t, istiod, "pilot", "resources", "requests", "memory") != "256Mi" ||
		lookup[string](t, values["web-ztunnel"], "resources", "requests", "memory") != "128Mi" {
		t.Errorf("istiod or ztunnel asks for other than the figure counts: %v %v", istiod, values["web-ztunnel"])
	}
	if _, ok := values["web-istio-cni"]["global"]; ok {
		t.Errorf("istio-cni looks for RKE2's CNI configuration where k3s keeps its own: %v", values["web-istio-cni"])
	}

	off, _ := proxySpecs(t, templates.RKE2, CNI{}, &Addons{})
	for _, name := range istioProxies(cluster()) {
		if off[name] != nil {
			t.Errorf("Istio off installs %s", name)
		}
	}
}

func TestIstioFindsTheCNIConfiguration(t *testing.T) {
	t.Parallel()
	istio := &Addons{Istio: Istio{Enabled: true}}
	_, flannel := proxySpecs(t, templates.K3s, CNI{}, istio)
	if lookup[string](t, flannel["web-istio-cni"], "global", "platform") != "k3s" {
		t.Errorf("on k3s's own flannel, istio-cni misses k3s's CNI directories: %v", flannel["web-istio-cni"])
	}
	cilium := CNI{Name: cniCilium, Pods: "10.42.0.0/16"}
	_, with := proxySpecs(t, templates.K3s, cilium, istio)
	if _, ok := with["web-istio-cni"]["global"]; ok {
		t.Errorf("with Cilium, k3s keeps the standard CNI directories: %v", with["web-istio-cni"])
	}
	if lookup[bool](t, with["web-cni"], "cni", "exclusive") {
		t.Errorf("Cilium takes istio-cni's plugin out: %v", with["web-cni"])
	}
	_, without := proxySpecs(t, templates.K3s, cilium, &Addons{})
	if !reflect.DeepEqual(without["web-cni"]["cni"], with["web-cni"]["cni"]) {
		t.Errorf("switching Istio changes Cilium's CNI settings, which restarts every agent: %v, then %v", without["web-cni"]["cni"], with["web-cni"]["cni"])
	}
}

func TestUnenrolTakesEverythingOutOfTheMesh(t *testing.T) {
	t.Parallel()
	ambient := map[string]string{istioDataplaneMode: "ambient"}
	redirected := map[string]string{istioRedirection: "enabled"}
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(
		&corev1.Namespace{Name: "shop", Labels: ambient},
		&corev1.Namespace{Name: "plain", Labels: map[string]string{"team": "a"}},
		&corev1.Pod{Namespace: "shop", Name: "cart", Annotations: redirected},
		&corev1.Pod{Namespace: "plain", Name: "one", Labels: map[string]string{istioDataplaneMode: "ambient", "app": "one"}},
	).Build()
	ctx := context.Background()
	waiting, err := unenrol(ctx, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !waiting {
		t.Error("does not wait for a pod istio-cni still redirects")
	}
	for _, o := range []client.Object{&corev1.Namespace{}, &corev1.Pod{}} {
		key := client.ObjectKey{Name: "shop"}
		if _, ok := o.(*corev1.Pod); ok {
			key = client.ObjectKey{Namespace: "plain", Name: "one"}
		}
		if err := workload.Get(ctx, key, o); err != nil {
			t.Fatal(err)
		}
		if _, ok := o.GetLabels()[istioDataplaneMode]; ok {
			t.Errorf("%s is still enrolled: %v", key, o.GetLabels())
		}
	}
	pod := &corev1.Pod{}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: "plain", Name: "one"}, pod); err != nil || pod.Labels["app"] != "one" {
		t.Errorf("a pod's other labels went with the mesh's: %v %v", pod.Labels, err)
	}
	plain := &corev1.Namespace{}
	if err := workload.Get(ctx, client.ObjectKey{Name: "plain"}, plain); err != nil || plain.Labels["team"] != "a" {
		t.Errorf("a namespace outside the mesh changed: %v %v", plain.Labels, err)
	}

	// Once istio-cni has taken the redirection away, nothing waits.
	cart := &corev1.Pod{}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "cart"}, cart); err != nil {
		t.Fatal(err)
	}
	cart.Annotations = nil
	if err := workload.Update(ctx, cart); err != nil {
		t.Fatal(err)
	}
	if waiting, err := unenrol(ctx, workload); err != nil || waiting {
		t.Errorf("waits with nothing redirected: %v %v", waiting, err)
	}
}

// Istio stays while its workloads cannot be taken out of the mesh, as long as the cluster cannot be
// reached. The cluster's other add-ons do not wait with it.
func TestReconcileKeepsIstioUntilItsWorkloadsLeaveTheMesh(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "istio", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)}})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	r.Remotes = &Remotes{Reader: kube}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	exists := func(name string) bool {
		hcp := &unstructured.Unstructured{}
		hcp.SetGroupVersionKind(helmChartProxyGVK)
		return kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, hcp) == nil
	}
	for _, name := range istioProxies(c) {
		if !exists(name) {
			t.Fatalf("Istio switched on, no %s", name)
		}
	}

	current := &clusterv1.Cluster{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Topology.Variables[len(current.Spec.Topology.Variables)-1].Value.Raw = []byte(`{"enabled":false}`)
	current.Spec.Topology.Variables = append(current.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "headlamp", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)}})
	if err := kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, current); err != nil {
		t.Fatal(err)
	}
	for _, name := range istioProxies(c) {
		if !exists(name) {
			t.Errorf("%s removed before the cluster's workloads left the mesh", name)
		}
	}
	if !exists("web-headlamp") {
		t.Error("Headlamp waits on Istio's removal")
	}

	// A cluster whose API does not answer fails the removal, after the rest is applied.
	unreachable := &corev1.Secret{
		Namespace: "team-a", Name: "web-kubeconfig",
		Data: map[string][]byte{"value": []byte(`apiVersion: v1
kind: Config
clusters: [{name: web, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: web, user: {token: t}}]
contexts: [{name: web, context: {cluster: web, user: web}}]
current-context: web
`)},
	}
	if err := kube.Create(ctx, unreachable); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Topology.Variables = append(current.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "monitoring", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)}})
	if err := kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err == nil {
		t.Error("an unreachable cluster's workloads count as out of the mesh")
	}
	if !exists("web-monitoring") {
		t.Error("monitoring waits on a cluster Istio's removal cannot reach")
	}
	for _, name := range istioProxies(c) {
		if !exists(name) {
			t.Errorf("%s removed from a cluster that cannot be reached", name)
		}
	}
}
