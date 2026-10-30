//go:build integration

package provisioner

// The integration tier: the charts and the controllers against a real API server, envtest's
// kube-apiserver and etcd with no kubelet and no other controller, carrying the CRDs of the
// releases the management cluster installs (test/integration/assets.sh). It checks what the unit
// tests' fake client cannot: every object against its CRD's schema, server-side apply, the status
// and scale subresources, finalizers and deletion. Run it with `make integration`.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/kubehenge/kubehenge/test/integration"
)

const classNamespace = integration.ClassNamespace

// classProviders is testdata/integration-values.yaml's providerNamespaces.
var classProviders = map[string][]string{
	"kubehenge-kubeadm": {"capi-kubeadm-bootstrap-system", "capi-kubeadm-control-plane-system"},
	"kubehenge-rke2":    {"rke2-bootstrap-system", "rke2-control-plane-system"},
	"kubehenge-k3s":     {"k3s-bootstrap-system", "k3s-control-plane-system"},
	"kubehenge-talos":   {"talos-bootstrap-system", "talos-control-plane-system"},
}

func TestMain(m *testing.M) {
	if err := integration.OwnChildren(); err != nil {
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
	_, c, _ := startAPIWith(t, objects, integration.Webhooks{})
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
	cfg, c, _ := startAPIWith(t, objects, integration.Webhooks{})
	return cfg, c
}

// apiObjects renders the providers' chart and Kubehenge's with the module's values and then those
// of values, and parts out Kubehenge's webhook configurations.
func apiObjects(t *testing.T, values ...string) ([]*unstructured.Unstructured, integration.Webhooks) {
	t.Helper()
	return integration.Charts(t, "../..", values...)
}

// startAPIWith starts an API server with the CRDs and installs objects in it, the namespaces the
// tests use made first, with webhooks pointing at a local server the test serves.
func startAPIWith(t *testing.T, objects []*unstructured.Unstructured, webhooks integration.Webhooks) (*rest.Config, client.Client, *envtest.WebhookInstallOptions) {
	t.Helper()
	api := integration.Start(t, &integration.Options{Objects: objects, Namespaces: apiNamespaces(), Webhooks: webhooks})
	return api.Config, api.Client, api.Webhooks
}

// apiNamespaces are the class namespace, a team's and the providers'.
func apiNamespaces() []string {
	namespaces := []string{classNamespace, "team-a"}
	for _, ns := range classProviders {
		namespaces = append(namespaces, ns...)
	}
	return namespaces
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

// serveAdmission serves the handlers where the chart's webhook configurations call them until the
// test ends, and returns once the server answers: the chart refuses every cluster until it does.
func serveAdmission(t *testing.T, w *envtest.WebhookInstallOptions, handlers map[string]http.Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := webhook.NewServer(webhook.Options{Host: w.LocalServingHost, Port: w.LocalServingPort, CertDir: w.LocalServingCertDir})
	for path, h := range handlers {
		srv.Register(path, h)
	}
	go func() {
		if err := srv.Start(ctx); err != nil {
			t.Error(err)
		}
	}()
	started := srv.StartedChecker()
	for deadline := time.Now().Add(30 * time.Second); started(nil) != nil; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the webhook server did not start")
		}
	}
}

// The API server asks the controller about Kubehenge's clusters through the chart's webhook
// configuration, and the class's own schema supplies the vCPUs a cluster leaves to its default.
func TestIntegrationClusterValidator(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	c, w := startValidatingAPI(t)
	ctx := context.Background()
	hooks := &Hooks{Reader: c, ClassNamespace: classNamespace, RancherKubernetes: KubernetesMinors{Min: 34, Max: 37}}
	serveAdmission(t, w, AdmissionWebhooks(c.Scheme(), hooks))

	rancherCluster := func(name, controlPlane string) *clusterv1.Cluster {
		vars := []clusterv1.ClusterVariable{{Name: "rancher", Value: apiextensionsv1.JSON{Raw: []byte(`{"install":true,"hostname":"rancher.example.org"}`)}}}
		if controlPlane != "" {
			vars = append(vars, clusterv1.ClusterVariable{Name: "controlPlaneMachine", Value: apiextensionsv1.JSON{Raw: []byte(controlPlane)}})
		}
		return placed(&clusterv1.Cluster{
			Namespace: "team-a", Name: name,
			Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
				ClassRef:  clusterv1.ClusterClassRef{Name: "kubehenge-rke2", Namespace: classNamespace},
				Version:   "v1.36.5+rke2r1",
				Variables: vars,
			}},
		})
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
	if err := c.Create(ctx, placed(many)); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "with 300 workers, control planes need 4 vCPUs and 8192 MiB") {
		t.Fatalf("300 workers on the class's default control planes: %v", err)
	}

	checkInterfaceRulesAdmission(ctx, t, c)

	workerless := &clusterv1.Cluster{
		Namespace: "team-a", Name: "workerless",
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-k3s", Namespace: classNamespace},
			Version:  "v1.36.5+k3s1",
		}},
	}
	if err := c.Create(ctx, placed(workerless)); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "without workers, control planes need 3 vCPUs and 5120 MiB") {
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
	if err := c.Create(ctx, placed(small)); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "add-ons (monitoring, cert-manager, Rancher's agent) need 2240 MiB of memory on workers") {
		t.Fatalf("monitoring on a k3s worker at its least: %v", err)
	}

	hooks.DatastoreAllocate = testDatastoreAllocate
	checkDatastoreAllocateAdmission(ctx, t, c)
	checkSysAuditAdmission(ctx, t, c, hooks)
}

// placed gives a cluster what every class requires and it does not set itself: a storage, and an
// endpoint and node addresses on a network of its own, which its name picks.
func placed(c *clusterv1.Cluster) *clusterv1.Cluster {
	set := map[string]bool{}
	for _, v := range c.Spec.Topology.Variables {
		set[v.Name] = true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(c.Name))
	net := fmt.Sprintf("10.%d.%d", h.Sum32()>>8%250, h.Sum32()%250)
	for name, raw := range map[string]string{
		"storage":              `"local-lvm"`,
		"controlPlaneEndpoint": `"` + net + `.5"`,
		"network":              `{"bridge":"vmbr0","addresses":["` + net + `.10-` + net + `.30"],"prefix":24,"gateway":"` + net + `.1","dnsServers":["` + net + `.1"]}`,
	} {
		if !set[name] {
			withAddons(c, map[string]string{name: raw})
		}
	}
	return c
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
		return placed(withAddons(cluster, map[string]string{
			"network":              `{"bridge":"vmbr0","addresses":["` + addresses + `"],"prefix":24,"gateway":"10.0.0.1","dnsServers":["10.0.0.1"]}`,
			"controlPlaneEndpoint": `"` + endpoint + `"`,
		}))
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
		return placed(withAddons(cluster, map[string]string{"storage": `"local-lvm"`, ccmVariable: `{"haGroups":true}`}))
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
		return placed(withAddons(cluster, vars))
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
// kind; a worker and control plane sized as the end-to-end tests size them from the figures holding
// every add-on but Rancher, logs and backups, and a MiB less refused; the default worker's CPU
// holding every add-on but Rancher; and the least control plane Rancher runs on without workers,
// at the memory its figures ask, holding Rancher.
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
		if nodes, _, figuresErr := classFigures(class, annotation); figuresErr != nil || nodes["controlPlane"] <= 0 || nodes["worker"] <= 0 {
			t.Errorf("%s: %s %v, %v", class.Name, annotation, nodes, figuresErr)
		}
	}
	defaults := withAddons(oneWorker.DeepCopy(), everyAddon)
	room, err := addonRoomOf(addonMemory, defaults, class)
	if err != nil {
		t.Fatal(err)
	}
	least := classMinimumSize(class, "workerMachine").MemoryMiB
	_, worker, err := nodeSize(defaults, class, "workerMachine", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, controlPlane, err := nodeSize(defaults, class, "controlPlaneMachine", nil)
	if err != nil {
		t.Fatal(err)
	}
	sized := func(workerMiB, controlPlaneMiB int) *clusterv1.Cluster {
		return withAddons(defaults.DeepCopy(), map[string]string{
			"workerMachine":       fmt.Sprintf(`{"memoryMiB":%d}`, workerMiB),
			"controlPlaneMachine": fmt.Sprintf(`{"memoryMiB":%d}`, controlPlaneMiB),
		})
	}
	workerMiB, controlPlaneMiB := max(least, room.Base+room.Need, worker), max(room.ControlPlaneNeed, controlPlane)
	if err = checkRoomFor(addonMemory, nil, sized(workerMiB, controlPlaneMiB), class); err != nil {
		t.Errorf("%s: every add-on on a worker of %d MiB and control plane of %d MiB: %v", class.Name, workerMiB, controlPlaneMiB, err)
	}
	if workerMiB > max(least, worker) && checkRoomFor(addonMemory, nil, sized(workerMiB-1, controlPlaneMiB), class) == nil {
		t.Errorf("%s: every add-on on a worker of %d MiB, a MiB short, admitted", class.Name, workerMiB-1)
	}
	if controlPlaneMiB > controlPlane && checkRoomFor(addonMemory, nil, sized(workerMiB, controlPlaneMiB-1), class) == nil {
		t.Errorf("%s: every add-on with a control plane of %d MiB, a MiB short, admitted", class.Name, controlPlaneMiB-1)
	}
	all := withAddons(withAddons(oneWorker.DeepCopy(), everyAddon), map[string]string{
		"logs": `{"enabled":true}`, "backups": `{"enabled":true,"target":"store"}`, "gitops": `{"enabled":true,"engine":"flux"}`,
		"loadBalancer": `{"enabled":true,"implementation":"kube-vip","addresses":["192.0.2.100-192.0.2.110"]}`,
	})
	if err = checkRoomFor(addonCPU, nil, all, class); err != nil {
		t.Errorf("%s: every add-on but Rancher on the default worker's CPU: %v", class.Name, err)
	}
	rancherOn := map[string]string{"rancher": `{"install":true,"hostname":"rancher.example.org"}`}
	room, err = addonRoomOf(addonMemory, withAddons(&clusterv1.Cluster{}, rancherOn), class)
	if err != nil {
		t.Fatal(err)
	}
	rancherOn["controlPlaneMachine"] = fmt.Sprintf(`{"cores":%d,"memoryMiB":%d}`,
		max(alone.Cores, RancherMinimumCores), max(alone.MemoryMiB, room.Base+room.Need))
	rancher := withAddons(&clusterv1.Cluster{}, rancherOn)
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
	for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
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
	for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		switch {
		case o.GetKind() == "ConfigMap" && o.GetLabels()["kubehenge.dev/class"] != "":
			data, _, err := unstructured.NestedString(o.Object, "data", "templates.yaml")
			if err != nil {
				t.Fatal(err)
			}
			for _, tpl := range integration.Decode(t, []byte(data)) {
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
		for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, values...) {
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
		for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, values...) {
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
		for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, values...) {
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
	for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
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

// Only a management cluster that restores through the guest agent hands the controller the restore
// token, and its classes say so for the interface to offer the restore. A Talos cluster is restored
// through its nodes' Talos API, so its class says so either way.
func TestIntegrationChartEtcdRestore(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		values := []string{"testdata/integration-values.yaml"}
		if on {
			values = append(values, "testdata/etcd-restore-values.yaml")
		}
		var token bool
		classes := 0
		for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, values...) {
			switch o.GetKind() {
			case "ClusterClass":
				want := on || o.GetName() == "kubehenge-talos"
				if got := o.GetAnnotations()[AutomaticRestoreAnnotation]; got != strconv.FormatBool(want) {
					t.Errorf("restore %t: %s says %q", on, o.GetName(), got)
				}
				classes++
			case "Deployment":
				containers, _, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
				if err != nil {
					t.Fatal(err)
				}
				token = token || strings.Contains(fmt.Sprint(containers), "KUBEHENGE_PROXMOX_RESTORE_TOKEN")
			}
		}
		if token != on || classes != len(classProviders) {
			t.Errorf("restore %t: the controller gets the restore token: %t; %d classes", on, token, classes)
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
	for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
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
