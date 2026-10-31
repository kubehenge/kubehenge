//go:build integration

package provisioner

// The integration tier: the charts and the controllers against a real API server, envtest's
// kube-apiserver and etcd with no kubelet and no other controller, carrying the CRDs of the
// releases the management cluster installs (test/integration/assets.sh). It checks what the unit
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
	"regexp"
	"slices"
	"strconv"
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
	"k8s.io/client-go/rest"
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
	"kubehenge-talos":   {"talos-bootstrap-system", "talos-control-plane-system"},
}

func TestMain(m *testing.M) {
	if err := ownChildren(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	m.Run()
}

// startAPI starts an API server with the CRDs and installs the providers' chart and Kubehenge's in
// it, as the management-cluster module does, but for the chart's webhook configurations: they fail
// closed while nothing answers them, which only startValidatingAPI's tests do.
func startAPI(t *testing.T) client.Client {
	t.Helper()
	objects, _ := apiObjects(t)
	_, c, _ := startAPIWith(t, objects, nil)
	return c
}

// startValidatingAPI is startAPI with the chart's webhook configurations, pointing at a local
// server whose address and certificates the options hold; the test serves them.
func startValidatingAPI(t *testing.T) (client.Client, *envtest.WebhookInstallOptions) {
	t.Helper()
	objects, webhooks := apiObjects(t)
	_, c, w := startAPIWith(t, objects, webhooks)
	return c, w
}

// startAPIConfig is startAPI with the administrator's configuration too, for clients of other
// identities, and Kubehenge's chart installed with the module's values and then those of values.
func startAPIConfig(t *testing.T, values ...string) (*rest.Config, client.Client) {
	t.Helper()
	objects, _ := apiObjects(t, values...)
	cfg, c, _ := startAPIWith(t, objects, nil)
	return cfg, c
}

// apiObjects renders the providers' chart and Kubehenge's with the module's values and then those
// of values, and parts out Kubehenge's webhook configurations.
func apiObjects(t *testing.T, values ...string) ([]*unstructured.Unstructured, []*admissionregistrationv1.ValidatingWebhookConfiguration) {
	t.Helper()
	// The providers' release comes first, as in the module, and carries no webhooks of its own.
	objects := renderChart(t, "cluster-api-providers", "../../management-cluster/charts/cluster-api-providers", "capi-operator-system",
		"testdata/cluster-api-providers-values.yaml")
	var webhooks []*admissionregistrationv1.ValidatingWebhookConfiguration
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, append([]string{"testdata/integration-values.yaml"}, values...)...) {
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
	return objects, webhooks
}

func startAPIWith(t *testing.T, objects []*unstructured.Unstructured, webhooks []*admissionregistrationv1.ValidatingWebhookConfiguration) (*rest.Config, client.Client, *envtest.WebhookInstallOptions) {
	t.Helper()
	crds := os.Getenv("KUBEHENGE_INTEGRATION_CRDS")
	if crds == "" || os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS and KUBEHENGE_INTEGRATION_CRDS are unset: run the tier with `make integration`")
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
	return cfg, c, &env.WebhookInstallOptions
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
	c := startAPI(t)
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
	c := startAPI(t)
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
	c, w := startValidatingAPI(t)
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
	// The chart refuses every cluster while nothing answers the webhook, so the test waits until it does.
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

	many := &clusterv1.Cluster{
		Namespace: "team-a", Name: "many",
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-rke2", Namespace: classNamespace},
			Version:  "v1.36.5+rke2r1",
			Workers: clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{
				{Class: "worker", Name: "md-0", Replicas: new(int32(100))},
				{Class: "worker", Name: "md-1", Replicas: new(int32(100))},
				{Class: "worker", Name: "md-2", Replicas: new(int32(100))},
			}},
		}},
	}
	if err := c.Create(ctx, many); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "with 300 workers, control planes need 4 vCPUs and 8192 MiB") {
		t.Fatalf("300 workers on the class's default control planes: %v", err)
	}

	checkInterfaceRulesAdmission(ctx, t, c)
	checkAbandonAdmission(ctx, t, c)

	workerless := &clusterv1.Cluster{
		Namespace: "team-a", Name: "workerless",
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-k3s", Namespace: classNamespace},
			Version:  "v1.36.5+k3s1",
		}},
	}
	if err := c.Create(ctx, workerless); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "without workers, control planes need 3 vCPUs and 5120 MiB") {
		t.Fatalf("a k3s cluster without workers on the class's default control planes: %v", err)
	}

	// Each class's sizing guide starts at the least its control-plane machines may be, and without
	// workers they need its least worker on top.
	oneWorker := &clusterv1.Cluster{Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
		Workers: clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{{Replicas: new(int32(1))}}},
	}}}
	for _, name := range []string{"kubehenge-kubeadm", "kubehenge-k3s", "kubehenge-rke2"} {
		class := &clusterv1.ClusterClass{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: name}, class); err != nil {
			t.Fatal(err)
		}
		need, _, err := controlPlaneFit(oneWorker, class)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		minimum := func(variable, field string) int {
			for _, v := range class.Spec.Variables {
				if m := v.Schema.OpenAPIV3Schema.Properties[field].Minimum; v.Name == variable && m != nil {
					return int(*m)
				}
			}
			t.Fatalf("%s: %s sets no least %s", name, variable, field)
			return 0
		}
		if need.Cores != minimum("controlPlaneMachine", "cores") || need.MemoryMiB != minimum("controlPlaneMachine", "memoryMiB") {
			t.Errorf("%s: its guide starts at %d vCPUs and %d MiB, its control-plane machines at %d and %d",
				name, need.Cores, need.MemoryMiB, minimum("controlPlaneMachine", "cores"), minimum("controlPlaneMachine", "memoryMiB"))
		}
		alone, _, err := controlPlaneFit(&clusterv1.Cluster{}, class)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if alone.Cores != need.Cores+minimum("workerMachine", "cores") || alone.MemoryMiB != need.MemoryMiB+minimum("workerMachine", "memoryMiB") {
			t.Errorf("%s: without workers its control planes need %d vCPUs and %d MiB", name, alone.Cores, alone.MemoryMiB)
		}
		checkClassAddonRoom(t, class, oneWorker, alone)
	}

	small := &clusterv1.Cluster{
		Namespace: "team-a", Name: "small",
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-k3s", Namespace: classNamespace},
			Version:  "v1.36.5+k3s1",
			Workers: clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{
				{Class: "worker", Name: "md-0", Replicas: new(int32(1))},
			}},
		}},
	}
	withAddons(small, map[string]string{"workerMachine": `{"memoryMiB":1024}`, "monitoring": `{"enabled":true}`})
	if err := c.Create(ctx, small); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "add-ons (monitoring, cert-manager, Rancher's agent) need 2240 MiB of memory on workers") {
		t.Fatalf("monitoring on a k3s worker at its least: %v", err)
	}

	hooks.DatastoreAllocate = testDatastoreAllocate
	checkDatastoreAllocateAdmission(ctx, t, c)
	checkSysAuditAdmission(ctx, t, c, hooks)
}

// checkInterfaceRulesAdmission has the API server refuse what the interface refuses, for a cluster
// applied with kubectl.
func checkInterfaceRulesAdmission(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	offered := func(name string, controlPlanes int32, addresses, endpoint string) *clusterv1.Cluster {
		cluster := &clusterv1.Cluster{
			Namespace: "team-a", Name: name,
			Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
				ClassRef:     clusterv1.ClusterClassRef{Name: "kubehenge-k3s", Namespace: classNamespace},
				Version:      "v1.36.5+k3s1",
				ControlPlane: clusterv1.ControlPlaneTopology{Replicas: new(controlPlanes)},
				Workers: clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{
					{Class: "worker", Name: "md-0", Replicas: new(int32(1))},
				}},
			}},
		}
		return withAddons(cluster, map[string]string{
			"network":              `{"bridge":"vmbr0","addresses":["` + addresses + `"],"prefix":24,"gateway":"10.0.0.1","dnsServers":["10.0.0.1"]}`,
			"controlPlaneEndpoint": `"` + endpoint + `"`,
		})
	}
	if err := c.Create(ctx, offered(strings.Repeat("a", MaxClusterName+1), 1, "10.0.0.10-10.0.0.20", "10.0.0.5")); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "up to 40") {
		t.Fatalf("a name too long for the machines' host names: %v", err)
	}
	if err := c.Create(ctx, offered("even", 2, "10.0.0.10-10.0.0.20", "10.0.0.5")); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "1, 3, 5 or 7") {
		t.Fatalf("two control planes: %v", err)
	}
	if err := c.Create(ctx, offered("first", 1, "10.0.0.10-10.0.0.20", "10.0.0.5")); err != nil {
		t.Fatalf("a cluster on its own addresses: %v", err)
	}
	if err := c.Create(ctx, offered("taker", 1, "10.0.0.20-10.0.0.30", "10.0.0.6")); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "overlap 10.0.0.10-10.0.0.20 of cluster team-a/first") {
		t.Fatalf("a cluster on another's node addresses: %v", err)
	}
	unpinned := withAddons(offered("unpinned", 1, "10.0.0.40-10.0.0.50", "10.0.0.7"), map[string]string{
		gitopsVariable: `{"enabled":true,"auth":"deployKey","repository":{"url":"ssh://git@git.example.org/a/b.git"}}`,
	})
	if err := c.Create(ctx, unpinned); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "knownHosts") {
		t.Fatalf("a repository over SSH without its host's keys: %v", err)
	}
}

// checkAbandonAdmission has the API server ask the validator about the name that abandons a
// cluster's deletion, also once the cluster is being deleted and the validator leaves it alone
// otherwise.
func checkAbandonAdmission(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	cluster := &clusterv1.Cluster{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "first"}, cluster); err != nil {
		t.Fatal(err)
	}
	patch := func(p string) error {
		return c.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, []byte(p)))
	}
	annotate := func(value string) error {
		return patch(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, AbandonAnnotation, value))
	}
	if err := annotate("first"); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "only a cluster being deleted") {
		t.Fatalf("abandoning the deletion of a cluster not being deleted: %v", err)
	}
	// A finalizer of the test's own keeps the cluster while it is being deleted.
	if err := patch(`{"metadata":{"finalizers":["kubehenge.dev/test-hold"]}}`); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := annotate("not-first"); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "must be the cluster's name") {
		t.Fatalf("abandoning the deletion with another cluster's name: %v", err)
	}
	if err := annotate("first"); err != nil {
		t.Fatalf("abandoning the deletion with the cluster's name: %v", err)
	}
	if err := patch(`{"metadata":{"finalizers":null}}`); err != nil {
		t.Fatalf("releasing the deleted cluster: %v", err)
	}
}

// checkSysAuditAdmission has the API server ask the validator about clusters whose token is to hold
// Sys.Audit on /, before and after the administrator lets clusters hold it.
func checkSysAuditAdmission(ctx context.Context, t *testing.T, c client.Client, hooks *Hooks) {
	t.Helper()
	asking := func(name string) *clusterv1.Cluster {
		cluster := &clusterv1.Cluster{
			Namespace: "team-a", Name: name,
			Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
				ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-k3s", Namespace: classNamespace},
				Version:  "v1.36.5+k3s1",
				Workers: clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{
					{Class: "worker", Name: "md-0", Replicas: new(int32(1))},
				}},
			}},
		}
		return withAddons(cluster, map[string]string{"storage": `"local-lvm"`, ccmVariable: `{"haGroups":true}`})
	}
	hooks.SysAuditGroup = ""
	if err := c.Create(ctx, asking("unaudited")); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "clusters may not hold Sys.Audit on /") {
		t.Fatalf("Sys.Audit on / where no cluster may hold it: %v", err)
	}
	hooks.SysAuditGroup = "kubehenge-clusters-audit"
	if err := c.Create(ctx, asking("audited")); err != nil {
		t.Fatalf("Sys.Audit on / where clusters may hold it: %v", err)
	}
}

// checkDatastoreAllocateAdmission has the API server ask the validator about clusters holding
// Datastore.Allocate on their storage.
func checkDatastoreAllocateAdmission(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	onStorage := func(name, storage, csi string) *clusterv1.Cluster {
		cluster := &clusterv1.Cluster{
			Namespace: "team-a", Name: name,
			Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
				ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-k3s", Namespace: classNamespace},
				Version:  "v1.36.5+k3s1",
				Workers: clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{
					{Class: "worker", Name: "md-0", Replicas: new(int32(1))},
				}},
			}},
		}
		vars := map[string]string{"storage": `"` + storage + `"`}
		if csi != "" {
			vars[csiVariable] = csi
		}
		return withAddons(cluster, vars)
	}
	if err := c.Create(ctx, onStorage("allocating", "ceph", `{"datastoreAllocate":true}`)); err != nil {
		t.Fatalf("Datastore.Allocate on a storage of its own: %v", err)
	}
	if err := c.Create(ctx, onStorage("sharing", "ceph", "")); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "holds Datastore.Allocate on storage ceph") {
		t.Fatalf("a second cluster on the storage another holds Datastore.Allocate on: %v", err)
	}
	if err := c.Create(ctx, onStorage("management", "local-lvm", `{"datastoreAllocate":true}`)); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "storage local-lvm, which the management cluster uses") {
		t.Fatalf("Datastore.Allocate on the management cluster's storage: %v", err)
	}
}

// checkClassAddonRoom holds a class's memory and CPU figures: one for every add-on and every node
// kind, a cluster at the class's defaults holding every add-on but Rancher, logs and backups, the
// default worker's CPU holding every add-on but Rancher, as the end-to-end tests have it, and the
// least cluster Rancher runs on, without workers, holding Rancher.
func checkClassAddonRoom(t *testing.T, class *clusterv1.ClusterClass, oneWorker *clusterv1.Cluster, alone controlPlaneSize) {
	t.Helper()
	addons, _, err := classFigures(class, AddonMemoryAnnotation)
	if err != nil {
		t.Fatal(err)
	}
	cpu, _, err := classFigures(class, AddonCPUAnnotation)
	if err != nil {
		t.Fatal(err)
	}
	for key := range addonNames {
		if addons[key] <= 0 {
			t.Errorf("%s: no memory figure for %s", class.Name, key)
		}
		// Some add-ons' pods ask for no CPU at all, so a figure of 0 is one.
		if _, ok := cpu[key]; !ok {
			t.Errorf("%s: no CPU figure for %s", class.Name, key)
		}
	}
	for _, annotation := range []string{NodeMemoryAnnotation, NodeCPUAnnotation} {
		if nodes, _, err := classFigures(class, annotation); err != nil || nodes["controlPlane"] <= 0 || nodes["worker"] <= 0 {
			t.Errorf("%s: %s %v, %v", class.Name, annotation, nodes, err)
		}
	}
	if err := checkAddonRoom(nil, withAddons(oneWorker.DeepCopy(), everyAddon), class); err != nil {
		t.Errorf("%s: every add-on on the default worker: %v", class.Name, err)
	}
	all := withAddons(withAddons(oneWorker.DeepCopy(), everyAddon), map[string]string{
		"logs": `{"enabled":true}`, "backups": `{"enabled":true,"target":"store"}`, "gitops": `{"enabled":true,"engine":"flux"}`,
		"loadBalancer": `{"enabled":true,"implementation":"kube-vip","addresses":["192.0.2.100-192.0.2.110"]}`,
	})
	if err := checkRoomFor(addonCPU, nil, all, class); err != nil {
		t.Errorf("%s: every add-on but Rancher on the default worker's CPU: %v", class.Name, err)
	}
	rancher := withAddons(&clusterv1.Cluster{}, map[string]string{
		"rancher":             `{"install":true,"hostname":"rancher.example.org"}`,
		"controlPlaneMachine": fmt.Sprintf(`{"cores":%d,"memoryMiB":%d}`, max(alone.Cores, RancherMinimumCores), alone.MemoryMiB),
	})
	if err := checkAddonRoom(nil, rancher, class); err != nil {
		t.Errorf("%s: Rancher on the least control plane without workers: %v", class.Name, err)
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

// Every argument the classes hand a component, in their templates or their patches, is one
// name=value: a list written in YAML's flow style splits a value that holds commas into several,
// and the component then refuses to start on a flag it does not know.
func TestIntegrationClassArguments(t *testing.T) {
	t.Parallel()
	argument := regexp.MustCompile(`^[a-z][a-z0-9-]*=\S`)
	isArgs := func(key string) bool { return strings.HasSuffix(key, "Args") || strings.HasSuffix(key, "Arg") }
	checked := 0
	check := func(where, s string) {
		checked++
		if !argument.MatchString(s) {
			t.Errorf("%s: %q is not one name=value", where, s)
		}
	}
	var walk func(where string, v any)
	walk = func(where string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, e := range v {
				if list, ok := e.([]any); ok && isArgs(key) {
					for _, a := range list {
						if s, ok := a.(string); ok {
							check(where+" "+key, s)
						}
					}
				}
				walk(where, e)
			}
		case []any:
			for _, e := range v {
				walk(where, e)
			}
		}
	}
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		switch {
		case o.GetKind() == "ConfigMap" && o.GetLabels()["kubehenge.dev/class"] != "":
			data, _, err := unstructured.NestedString(o.Object, "data", "templates.yaml")
			if err != nil {
				t.Fatal(err)
			}
			for _, tpl := range decodeAll(t, []byte(data)) {
				walk(tpl.GetKind(), tpl.Object)
			}
		case o.GetKind() == "ClusterClass":
			patches, _, err := unstructured.NestedSlice(o.Object, "spec", "patches")
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range jsonPatches(t, patches) {
				path := fmt.Sprint(op["path"])
				if s, ok := op["value"].(string); ok && isArgs(strings.TrimSuffix(path, "/-")) {
					check(o.GetName()+" "+path, s)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no arguments found")
	}
}

// jsonPatches are the operations of a ClusterClass's patches, every definition's in turn.
func jsonPatches(t *testing.T, patches []any) []map[string]any {
	t.Helper()
	var ops []map[string]any
	for _, p := range patches {
		patch, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("patch %v", p)
		}
		definitions, _, err := unstructured.NestedSlice(patch, "definitions")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range definitions {
			definition, ok := d.(map[string]any)
			if !ok {
				t.Fatalf("definition %v", d)
			}
			list, _, err := unstructured.NestedSlice(definition, "jsonPatches")
			if err != nil {
				t.Fatal(err)
			}
			for _, j := range list {
				op, ok := j.(map[string]any)
				if !ok {
					t.Fatalf("operation %v", j)
				}
				ops = append(ops, op)
			}
		}
	}
	return ops
}

// tokenJobOf tells, of a rendered chart, whether the controller is told to use the token Job
// template, whether a PodTemplate of that name gets the token's Secret, and whether a Deployment
// does, which none may.
func tokenJobOf(t *testing.T, objs []*unstructured.Unstructured, flag, template, secret string) (bool, bool, bool) {
	t.Helper()
	var told, job, deployment bool
	for _, o := range objs {
		switch {
		case o.GetKind() == "Deployment":
			raw, err := json.Marshal(o.Object["spec"])
			if err != nil {
				t.Fatal(err)
			}
			deployment = deployment || strings.Contains(string(raw), `"`+secret+`"`)
			told = told || o.GetName() == "kubehenge-controller" && strings.Contains(string(raw), `"`+flag+`"`)
		case o.GetKind() == "PodTemplate" && o.GetName() == template:
			raw, err := json.Marshal(o.Object["template"])
			if err != nil {
				t.Fatal(err)
			}
			job = strings.Contains(string(raw), `"name":"`+secret+`"`)
		}
	}
	return told, job, deployment
}

// Only a management cluster with Proxmox VE HA has the controller write HA, through Jobs that alone
// get the HA token, and offers the classes' proxmoxHA variable in the interface.
func TestIntegrationChartProxmoxHA(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		values := []string{"testdata/integration-values.yaml"}
		if on {
			values = append(values, "testdata/proxmox-ha-values.yaml")
		}
		classes := 0
		objs := renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, values...)
		for _, o := range objs {
			if o.GetKind() == "ClusterClass" {
				if got := o.GetAnnotations()["kubehenge.dev/proxmox-ha"] == "true"; got != on {
					t.Errorf("HA %t: %s offers proxmoxHA: %t", on, o.GetName(), got)
				}
				classes++
			}
		}
		told, job, deployment := tokenJobOf(t, objs, "--proxmox-ha", HAJobTemplate, "kubehenge-proxmox-ha")
		if told != on || job != on || deployment || classes != len(classProviders) {
			t.Errorf("HA %t: the controller writes HA: %t; a Job gets the HA token: %t; a Deployment does: %t; %d classes", on, told, job, deployment, classes)
		}
	}
}

// Only a management cluster that lets clusters hold Datastore.Allocate hands the controller the role
// and the management cluster's storages, and offers the classes' csi variable in the interface.
func TestIntegrationChartDatastoreAllocate(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		values := []string{"testdata/integration-values.yaml"}
		if on {
			values = append(values, "testdata/datastore-allocate-values.yaml")
		}
		var args string
		classes := 0
		for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, values...) {
			switch o.GetKind() {
			case "ClusterClass":
				annotations := o.GetAnnotations()
				if got := annotations["kubehenge.dev/datastore-allocate"] == "true"; got != on {
					t.Errorf("Datastore.Allocate %t: %s offers it: %t", on, o.GetName(), got)
				}
				if on && annotations["kubehenge.dev/management-storages"] != "local-lvm,local" {
					t.Errorf("%s names the management cluster's storages %q", o.GetName(), annotations["kubehenge.dev/management-storages"])
				}
				classes++
			case "Deployment":
				containers, _, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
				if err != nil {
					t.Fatal(err)
				}
				args += fmt.Sprint(containers)
			}
		}
		if !strings.Contains(args, "--csi-newest-chart=oci://ghcr.io/sergelogvinov/charts/proxmox-csi-plugin:") {
			t.Errorf("Datastore.Allocate %t: the controller is not given the plugin's newest release", on)
		}
		role := strings.Contains(args, "--datastore-allocate-role=kubehenge-cluster-datastore") && strings.Contains(args, "--management-storages=local-lvm,local")
		if role != on || classes != len(classProviders) {
			t.Errorf("Datastore.Allocate %t: the controller gets the role: %t; %d classes", on, role, classes)
		}
	}
}

// Only a management cluster that lets clusters hold Sys.Audit on / hands the controller the group
// holding it, offers the classes' ccm variable in the interface and switches the HA group labels on
// for each cluster that leaves them unset.
func TestIntegrationChartSysAudit(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		values := []string{"testdata/integration-values.yaml"}
		if on {
			values = append(values, "testdata/sys-audit-values.yaml")
		}
		var args string
		classes := 0
		for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, values...) {
			switch o.GetKind() {
			case "ClusterClass":
				if got := o.GetAnnotations()["kubehenge.dev/sys-audit"] == "true"; got != on {
					t.Errorf("Sys.Audit %t: %s offers it: %t", on, o.GetName(), got)
				}
				variables, _, err := unstructured.NestedSlice(o.Object, "spec", "variables")
				if err != nil {
					t.Fatal(err)
				}
				var schema map[string]any
				for _, v := range variables {
					if v, ok := v.(map[string]any); ok && v["name"] == ccmVariable {
						schema, _, _ = unstructured.NestedMap(v, "schema", "openAPIV3Schema")
					}
				}
				if schema == nil {
					t.Errorf("%s has no ccm variable", o.GetName())
					continue
				}
				whole, _, _ := unstructured.NestedBool(schema, "default", "haGroups")
				property, _, _ := unstructured.NestedBool(schema, "properties", "haGroups", "default")
				if whole != on || property != on {
					t.Errorf("Sys.Audit %t: %s defaults the HA group labels to %t, and the property to %t", on, o.GetName(), whole, property)
				}
				classes++
			case "Deployment":
				containers, _, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
				if err != nil {
					t.Fatal(err)
				}
				args += fmt.Sprint(containers)
			}
		}
		group := strings.Contains(args, "--sys-audit-group=kubehenge-clusters-audit")
		if group != on || strings.Contains(args, "--sys-audit-group") != on || classes != len(classProviders) {
			t.Errorf("Sys.Audit %t: the controller gets the group: %t; %d classes", on, group, classes)
		}
	}
}

// The controller is given the snapshot controller's chart, pinned with its image, and the image of
// the proxy beside the clusters' CSI plugins, its own, so that any cluster may take volume snapshots.
func TestIntegrationChartVolumeSnapshots(t *testing.T) {
	t.Parallel()
	var given strings.Builder
	for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "Deployment" {
			continue
		}
		containers, _, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(&given, containers)
	}
	args := given.String()
	if !strings.Contains(args, "--snapshot-controller-chart=oci://ghcr.io/piraeusdatastore/helm-charts/snapshot-controller:") ||
		!regexp.MustCompile(`"snapshot-controller":\{"controller":\{"image":\{"tag":"v[0-9.]+@sha256:[0-9a-f]{64}"`).MatchString(args) {
		t.Error("the controller is not given the snapshot controller's chart and image")
	}
	if !regexp.MustCompile(`--csi-proxy-image=ghcr\.io/kubehenge/kubehenge@sha256:[0-9a-f]{64}`).MatchString(args) {
		t.Error("the controller is not given its own image for the proxy beside the CSI plugins")
	}
	if strings.Contains(args, "root@pam") || strings.Contains(args, "root-secret") {
		t.Errorf("the controller is given something of root@pam's: %s", args)
	}
}

// Only a management cluster that restores through the guest agent has the controller do so,
// through Jobs that alone get the restore token, and its classes say so for the interface to offer
// the restore. A Talos cluster is restored through its nodes' Talos API, so its class says so either
// way.
func TestIntegrationChartEtcdRestore(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		values := []string{"testdata/integration-values.yaml"}
		if on {
			values = append(values, "testdata/etcd-restore-values.yaml")
		}
		classes := 0
		objs := renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, values...)
		for _, o := range objs {
			if o.GetKind() == "ClusterClass" {
				want := on || o.GetName() == "kubehenge-talos"
				if got := o.GetAnnotations()[AutomaticRestoreAnnotation]; got != strconv.FormatBool(want) {
					t.Errorf("restore %t: %s says %q", on, o.GetName(), got)
				}
				classes++
			}
		}
		told, job, deployment := tokenJobOf(t, objs, "--etcd-restore", GuestJobTemplate, "kubehenge-proxmox-restore")
		if told != on || job != on || deployment || classes != len(classProviders) {
			t.Errorf("restore %t: the controller restores: %t; a Job gets the restore token: %t; a Deployment does: %t; %d classes", on, told, job, deployment, classes)
		}
	}
}

// The HA reconciler's patches against capmox's CRD and Cluster API's: the hook a Machine's
// deletion waits for, and the node a ProxmoxMachine's VM has moved to.
func TestIntegrationProxmoxHA(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	c := startAPI(t)
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
