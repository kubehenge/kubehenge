//go:build integration

package provisioner

// The integration tier: the charts and the controllers against a real API server, envtest's
// kube-apiserver and etcd with no kubelet and no other controller, carrying the CRDs of the
// releases the management cluster installs (hack/integration-assets.sh). It checks what the unit
// tests' fake client cannot: every object against its CRD's schema, server-side apply, the status
// and scale subresources, finalizers and deletion. Run it with `make integration`.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const classNamespace = "kubehenge-system"

// classProviders is testdata/integration-values.yaml's providerNamespaces.
var classProviders = map[string][]string{
	"kubehenge-kubeadm": {"capi-kubeadm-bootstrap-system", "capi-kubeadm-control-plane-system"},
	"kubehenge-rke2":    {"rke2-bootstrap-system", "rke2-control-plane-system"},
	"kubehenge-k3s":     {"k3s-bootstrap-system", "k3s-control-plane-system"},
}

func TestMain(m *testing.M) {
	if err := ownChildren(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	m.Run()
}

// startAPI starts an API server with the CRDs and installs the providers' chart and Kubehenge's in
// it, as the management-cluster module does. The chart's webhook configurations point at a local server
// instead, whose address and certificates the options hold; nothing serves them until a test does.
func startAPI(t *testing.T) (client.Client, *envtest.WebhookInstallOptions) {
	t.Helper()
	crds := os.Getenv("KUBEHENGE_INTEGRATION_CRDS")
	if crds == "" || os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS and KUBEHENGE_INTEGRATION_CRDS are unset: run the tier with `make integration`")
	}
	// The providers' release comes first, as in the module, and carries no webhooks of its own.
	objects := renderChart(t, "cluster-api-providers", "../../management-cluster/charts/cluster-api-providers", "capi-operator-system",
		"testdata/cluster-api-providers-values.yaml")
	var webhooks []*admissionregistrationv1.ValidatingWebhookConfiguration
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "ValidatingWebhookConfiguration" {
			objects = append(objects, o)
			continue
		}
		w := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, w); err != nil {
			t.Fatal(err)
		}
		webhooks = append(webhooks, w)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, clusterv1.AddToScheme, addonsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crds},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{ValidatingWebhooks: webhooks},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopAPI(t, env) })
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	installChart(t, c, objects)
	return c, &env.WebhookInstallOptions
}

// stopAPI stops the API server and etcd, ending them first where envtest cannot.
func stopAPI(t *testing.T, env *envtest.Environment) {
	t.Helper()
	if err := endChildren(); err != nil {
		t.Error(err)
		return
	}
	if err := env.Stop(); err != nil && !endsChildrenItself {
		t.Error(err)
	}
}

// renderChart is what `helm template` makes of a chart with the module's values for it, hooks
// included.
func renderChart(t *testing.T, release, chart, namespace string, values ...string) []*unstructured.Unstructured {
	t.Helper()
	args := make([]string, 0, 5+2*len(values))
	args = append(args, "template", release, chart, "--namespace", namespace)
	for _, v := range values {
		args = append(args, "-f", v)
	}
	helm := exec.CommandContext(t.Context(), "helm", args...) //nolint:gosec // the test's own charts and values
	var stderr bytes.Buffer
	helm.Stderr = &stderr
	out, err := helm.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	return decodeAll(t, out)
}

// installChart applies the chart's objects, in the namespaces they name.
func installChart(t *testing.T, c client.Client, objects []*unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	namespaces := []string{classNamespace, "team-a"}
	for _, ns := range classProviders {
		namespaces = append(namespaces, ns...)
	}
	for _, o := range objects {
		if ns := o.GetNamespace(); ns != "" {
			namespaces = append(namespaces, ns)
		}
	}
	slices.Sort(namespaces)
	for _, ns := range slices.Compact(namespaces) {
		if err := c.Create(ctx, &corev1.Namespace{Name: ns}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	for _, o := range objects {
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), client.FieldOwner("helm"), client.ForceOwnership); err != nil {
			t.Errorf("the API server refuses the chart's %s %s: %v", o.GetKind(), o.GetName(), err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

func decodeAll(t *testing.T, data []byte) []*unstructured.Unstructured {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		if len(u.Object) > 0 {
			out = append(out, u)
		}
	}
}

// startProviders creates a class's provider Deployments, scaled to none as the controller leaves them.
func startProviders(ctx context.Context, t *testing.T, c client.Client, class string) []types.NamespacedName {
	t.Helper()
	out := make([]types.NamespacedName, 0, len(classProviders[class]))
	for _, ns := range classProviders[class] {
		labels := map[string]string{"app": "manager"}
		dep := &appsv1.Deployment{
			Namespace: ns, Name: "controller-manager", Labels: map[string]string{providerLabel: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: new(int32(0)),
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "registry.example/manager:v1"}}},
				},
			},
		}
		if err := c.Create(ctx, dep); err != nil {
			t.Fatal(err)
		}
		out = append(out, client.ObjectKeyFromObject(dep))
	}
	return out
}

// markServing reports a Deployment's pods up, as its controller would; envtest runs none.
func markServing(ctx context.Context, t *testing.T, c client.Client, key types.NamespacedName) {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	n := *dep.Spec.Replicas
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: n, UpdatedReplicas: n, ReadyReplicas: n, AvailableReplicas: n}
	if err := c.Status().Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
}

func scaledTo(ctx context.Context, t *testing.T, c client.Client, key types.NamespacedName) int32 {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, key, dep); err != nil {
		t.Fatal(err)
	}
	return *dep.Spec.Replicas
}

// appliedTemplates lists the objects the controller applied for a class, by kind.
func appliedTemplates(ctx context.Context, t *testing.T, c client.Client, class string, templates []*unstructured.Unstructured) []string {
	t.Helper()
	var out []string
	for _, tpl := range templates {
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(tpl.GroupVersionKind())
		err := c.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: tpl.GetName()}, got)
		if apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		if got.GetLabels()[ClassLabel] != class {
			t.Errorf("%s %s is not labelled for %s", got.GetKind(), got.GetName(), class)
		}
		out = append(out, got.GetKind())
	}
	slices.Sort(out)
	return out
}

func TestIntegrationDistributionTemplatesAndScaling(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	c, _ := startAPI(t)
	ctx := context.Background()
	d := &Distributions{Client: c, ClassNamespace: classNamespace, Providers: classProviders}
	reconcileClass := func(t *testing.T, class string) {
		t.Helper()
		if _, err := d.Reconcile(ctx, ctrl.Request{Namespace: classNamespace, Name: class}); err != nil {
			t.Fatalf("%s: %v", class, err)
		}
	}

	for class := range classProviders {
		t.Run(class, func(t *testing.T) {
			providers := startProviders(ctx, t, c, class)
			templates, _, err := d.templates(ctx, class)
			if err != nil || len(templates) == 0 {
				t.Fatalf("the chart's templates for %s: %d, %v", class, len(templates), err)
			}

			cluster := &clusterv1.Cluster{
				Namespace: "team-a", Name: class,
				Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
					ClassRef: clusterv1.ClusterClassRef{Name: class, Namespace: classNamespace},
					Version:  "v1.37.1",
				}},
			}
			if err := c.Create(ctx, cluster); err != nil {
				t.Fatal(err)
			}
			reconcileClass(t, class)
			for _, p := range providers {
				if scaledTo(ctx, t, c, p) != 1 {
					t.Fatalf("%s not started for the first cluster", p)
				}
				markServing(ctx, t, c, p)
			}
			// Every template has to pass its CRD's schema here; in a management cluster the providers'
			// webhooks check it further.
			reconcileClass(t, class)
			want := make([]string, 0, len(templates))
			for _, tpl := range templates {
				want = append(want, tpl.GetKind())
			}
			slices.Sort(want)
			if got := appliedTemplates(ctx, t, c, class, templates); !slices.Equal(got, want) {
				t.Fatalf("applied %v, want %v", got, want)
			}
			cc := &clusterv1.ClusterClass{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: class}, cc); err != nil || cc.Annotations[TemplatesAnnotation] == "" {
				t.Fatalf("the class was not marked for its templates: %v", err)
			}

			if err := c.Delete(ctx, cluster); err != nil {
				t.Fatal(err)
			}
			reconcileClass(t, class)
			if got := appliedTemplates(ctx, t, c, class, templates); len(got) != 0 {
				t.Fatalf("templates left after the last cluster: %v", got)
			}
			for _, p := range providers {
				if scaledTo(ctx, t, c, p) != 0 {
					t.Fatalf("%s still runs after the last cluster", p)
				}
			}
		})
	}
}

func TestIntegrationClusterLifecycle(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	c, _ := startAPI(t)
	ctx := context.Background()
	cloud := &fakeCloud{}
	r := &Reconciler{
		Client:         c,
		Cloud:          cloud,
		WorkloadPool:   "kubehenge",
		ClassNamespace: classNamespace,
		Endpoint:       Endpoint{URL: "https://pve.example.org:8006", CA: "PEM"},
		Charts:         testCharts,
	}
	cluster := storageCluster()
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}
	reconcileCluster := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}

	// Twice: the second must find everything in place and change nothing.
	reconcileCluster()
	reconcileCluster()
	got := &clusterv1.Cluster{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Finalizers, Finalizer) {
		t.Fatalf("finalizers %v", got.Finalizers)
	}
	creds := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "web-proxmox-token"}, creds); err != nil {
		t.Fatal(err)
	}
	if !isReady(creds) || len(creds.OwnerReferences) != 1 || creds.OwnerReferences[0].UID != got.UID {
		t.Fatalf("credentials not ready or not owned by the cluster: %v %v", creds.Data, creds.OwnerReferences)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "web-proxmox"}, &addonsv1.ClusterResourceSet{}); err != nil {
		t.Fatalf("ClusterResourceSet: %v", err)
	}
	proxies := &unstructured.UnstructuredList{}
	proxies.SetGroupVersionKind(helmChartProxyGVK.GroupVersion().WithKind(helmChartProxyGVK.Kind + "List"))
	if err := c.List(ctx, proxies, client.InNamespace("team-a")); err != nil {
		t.Fatal(err)
	}
	if len(proxies.Items) < 2 {
		t.Fatalf("%d HelmChartProxies, want the cloud controller manager's and the CSI plugin's at least", len(proxies.Items))
	}
	if cloud.created != 1 {
		t.Fatalf("volume owner created %d times", cloud.created)
	}

	// A machine left holds the Proxmox VE objects, and with them the cluster.
	machine := &clusterv1.Machine{
		Namespace: "team-a", Name: "web-cp", Labels: map[string]string{clusterv1.ClusterNameLabel: "web"},
		Spec: clusterv1.MachineSpec{
			ClusterName: "web",
			Bootstrap:   clusterv1.Bootstrap{DataSecretName: new("web-cp-bootstrap")},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "ProxmoxMachine", Name: "web-cp",
			},
		},
	}
	if err := c.Create(ctx, machine); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcileCluster()
	if len(cloud.removed) != 0 {
		t.Fatal("removed the Proxmox VE objects while a machine remains")
	}
	if err := c.Delete(ctx, machine); err != nil {
		t.Fatal(err)
	}
	reconcileCluster()
	if len(cloud.removed) != 1 {
		t.Fatalf("Proxmox VE objects removed %d times", len(cloud.removed))
	}
	if err := c.Get(ctx, req.NamespacedName, &clusterv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the cluster is still there once its finalizer is gone: %v", err)
	}
}

// The API server asks the controller about Kubehenge's clusters through the chart's webhook
// configuration, and the class's own schema supplies the vCPUs a cluster leaves to its default.
func TestIntegrationClusterValidator(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	c, w := startAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := webhook.NewServer(webhook.Options{Host: w.LocalServingHost, Port: w.LocalServingPort, CertDir: w.LocalServingCertDir})
	hooks := &Hooks{Reader: c, ClassNamespace: classNamespace, RancherKubernetes: KubernetesMinors{Min: 34, Max: 37}}
	srv.Register("/validate-cluster", admission.WithValidator[*clusterv1.Cluster](c.Scheme(), &ClusterValidator{Hooks: hooks}))
	go func() {
		if err := srv.Start(ctx); err != nil {
			t.Error(err)
		}
	}()
	// The chart ignores the webhook while nothing answers, so the test waits until it does.
	started := srv.StartedChecker()
	for deadline := time.Now().Add(30 * time.Second); started(nil) != nil; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the webhook server did not start")
		}
	}

	rancherCluster := func(name, controlPlane string) *clusterv1.Cluster {
		vars := []clusterv1.ClusterVariable{{Name: "rancher", Value: apiextensionsv1.JSON{Raw: []byte(`{"install":true,"hostname":"rancher.example.org"}`)}}}
		if controlPlane != "" {
			vars = append(vars, clusterv1.ClusterVariable{Name: "controlPlaneMachine", Value: apiextensionsv1.JSON{Raw: []byte(controlPlane)}})
		}
		return &clusterv1.Cluster{
			Namespace: "team-a", Name: name,
			Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
				ClassRef:  clusterv1.ClusterClassRef{Name: "kubehenge-rke2", Namespace: classNamespace},
				Version:   "v1.36.5+rke2r1",
				Variables: vars,
			}},
		}
	}
	err := c.Create(ctx, rancherCluster("defaults", ""))
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "at least 4 vCPUs") || !strings.Contains(err.Error(), "control planes have 2") {
		t.Fatalf("Rancher on the class's default vCPUs: %v", err)
	}
	ok := rancherCluster("rancher", `{"cores":4,"memoryMiB":8192}`)
	if err := c.Create(ctx, ok); err != nil {
		t.Fatalf("Rancher on four vCPUs: %v", err)
	}
	smaller := ok.DeepCopy()
	smaller.Spec.Topology.Variables[1].Value.Raw = []byte(`{"cores":2,"memoryMiB":8192}`)
	if err := c.Update(ctx, smaller); err == nil || !strings.Contains(err.Error(), "at least 4 vCPUs") {
		t.Fatalf("Rancher's control planes cut to two vCPUs: %v", err)
	}
	if err := c.Create(ctx, rancherCluster("second", `{"cores":4,"memoryMiB":8192}`)); err == nil || !strings.Contains(err.Error(), "already runs Rancher") {
		t.Fatalf("a second Rancher: %v", err)
	}
}

// The end-to-end tests shorten the workers' health check for their own cluster, so the classes' own
// checks are held here.
func TestIntegrationClassHealthChecks(t *testing.T) {
	t.Parallel()
	const checks = `"checks":{"nodeStartupTimeoutSeconds":1200,"unhealthyNodeConditions":[` +
		`{"status":"Unknown","timeoutSeconds":300,"type":"Ready"},{"status":"False","timeoutSeconds":300,"type":"Ready"}]}`
	asJSON := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	var classes []string
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "ClusterClass" {
			continue
		}
		classes = append(classes, o.GetName())
		controlPlane, _, err := unstructured.NestedMap(o.Object, "spec", "controlPlane", "healthCheck")
		if err != nil || asJSON(controlPlane) != "{"+checks+"}" {
			t.Errorf("%s: control plane health check %v, %v", o.GetName(), controlPlane, err)
		}
		workers, _, err := unstructured.NestedSlice(o.Object, "spec", "workers", "machineDeployments")
		if err != nil || len(workers) == 0 {
			t.Errorf("%s: worker classes %v, %v", o.GetName(), workers, err)
		}
		for _, w := range workers {
			worker, ok := w.(map[string]any)
			if !ok {
				t.Fatalf("%s: worker class %v", o.GetName(), w)
			}
			if got := worker["healthCheck"]; asJSON(got) != "{"+checks+`,"remediation":{"maxInFlight":1}}` {
				t.Errorf("%s: worker class %v health check %v", o.GetName(), worker["class"], got)
			}
		}
	}
	slices.Sort(classes)
	if want := slices.Sorted(maps.Keys(classProviders)); !slices.Equal(classes, want) {
		t.Fatalf("classes %v, want %v", classes, want)
	}
}

// Only a management cluster with Proxmox VE HA hands the controller the HA token and offers the
// classes' proxmoxHA variable in the interface.
func TestIntegrationChartProxmoxHA(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		values := []string{"testdata/integration-values.yaml"}
		if on {
			values = append(values, "testdata/proxmox-ha-values.yaml")
		}
		var token bool
		classes := 0
		for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, values...) {
			switch o.GetKind() {
			case "ClusterClass":
				if got := o.GetAnnotations()["kubehenge.dev/proxmox-ha"] == "true"; got != on {
					t.Errorf("HA %t: %s offers proxmoxHA: %t", on, o.GetName(), got)
				}
				classes++
			case "Deployment":
				containers, _, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
				if err != nil {
					t.Fatal(err)
				}
				token = token || strings.Contains(fmt.Sprint(containers), "KUBEHENGE_PROXMOX_HA_TOKEN")
			}
		}
		if token != on || classes != len(classProviders) {
			t.Errorf("HA %t: the controller gets the HA token: %t; %d classes", on, token, classes)
		}
	}
}

// The HA reconciler's patches against capmox's CRD and Cluster API's: the hook a Machine's
// deletion waits for, and the node a ProxmoxMachine's VM has moved to.
func TestIntegrationProxmoxHA(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	c, _ := startAPI(t)
	ctx := context.Background()
	if err := c.Create(ctx, shared()); err != nil {
		t.Fatal(err)
	}
	pve := newFakeHA("pve1", "pve2", "pve3")
	srv := httptest.NewServer(pve)
	t.Cleanup(srv.Close)
	r := &HAReconciler{
		Client: c, Machines: c, ClassNamespace: classNamespace, WorkloadPool: "kubehenge",
		Proxmox: proxmox.NewClient(srv.URL + "/api2/json"),
		HA:      proxmox.NewClient(srv.URL + "/api2/json"),
	}
	// The CRD asks for more than the reconciler reads, so the machines are made as a class makes them.
	var machineSpec map[string]any
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() == "ProxmoxMachineTemplate" {
			machineSpec, _, _ = unstructured.NestedMap(o.Object, "spec", "template", "spec")
			break
		}
	}
	if machineSpec == nil {
		t.Fatal("the chart has no ProxmoxMachineTemplate")
	}
	for i, node := range []string{"pve1", "pve2", "pve3"} {
		vmid := int64(101 + i)
		m, pm := controlPlane(fmt.Sprintf("web-cp-%d", i), vmid, node)
		spec := runtime.DeepCopyJSON(machineSpec)
		spec["virtualMachineID"] = vmid
		pm.Object["spec"] = spec
		m.Spec.Bootstrap = clusterv1.Bootstrap{DataSecretName: new(m.Name + "-bootstrap")}
		machineStatus, status := m.Status, pm.Object["status"]
		createWithStatus(ctx, t, c, m, func() { m.Status = machineStatus })
		createWithStatus(ctx, t, c, pm, func() { pm.Object["status"] = status })
		pve.vms[vmid] = node
	}
	reconcileHA := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{Namespace: "team-a", Name: "web"}); err != nil {
			t.Fatal(err)
		}
	}
	hook := func(name string) string {
		t.Helper()
		m := &clusterv1.Machine{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, m); err != nil {
			t.Fatal(err)
		}
		return m.Annotations[HAHook]
	}

	reconcileHA()
	for i := range 3 {
		if got := hook(fmt.Sprintf("web-cp-%d", i)); got != "kubehenge" {
			t.Errorf("web-cp-%d hook %q", i, got)
		}
	}

	pve.mu.Lock()
	pve.vms[102] = "pve3"
	pve.mu.Unlock()
	reconcileHA()
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "web-cp-1"}, pm); err != nil {
		t.Fatal(err)
	}
	if got := pm.GetAnnotations()[LocateAnnotation]; got != "pve3" {
		t.Errorf("the moved VM's ProxmoxMachine is marked with %q", got)
	}

	if err := c.Delete(ctx, &clusterv1.Machine{Namespace: "team-a", Name: "web-cp-0"}); err != nil {
		t.Fatal(err)
	}
	reconcileHA()
	if got := hook("web-cp-0"); got != "" {
		t.Errorf("the deleted machine is still held: %q", got)
	}
	if _, ok := pve.haResources()["vm:101"]; ok {
		t.Error("the deleted machine's VM is still an HA resource")
	}
}

// createWithStatus creates o and then writes the status restore puts back through the status
// subresource, as a create leaves it out.
func createWithStatus(ctx context.Context, t *testing.T, c client.Client, o client.Object, restore func()) {
	t.Helper()
	if err := c.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	restore()
	if err := c.Status().Update(ctx, o); err != nil {
		t.Fatal(err)
	}
}
