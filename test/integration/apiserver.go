//go:build integration

// Package integration runs the integration tier's API server: envtest's kube-apiserver and etcd,
// with no kubelet, carrying the CRDs of the releases the management cluster installs (assets.sh) and
// the objects a test installs, and where a test asks, Cluster API's own Cluster and ClusterClass
// webhooks and ClusterClass controller beside them, set up as the management cluster runs them. Its
// tests run it through `make integration`.
package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1beta1 "sigs.k8s.io/cluster-api/api/core/v1beta1" //nolint:staticcheck // Cluster API's webhooks still convert from it
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimev1 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/cluster-api/core/reconcilers/clusterclass"
	capiadmission "sigs.k8s.io/cluster-api/core/webhooks/admission"
	runtimeclient "sigs.k8s.io/cluster-api/exp/runtime/client"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// Options are what Start runs beside the API server.
type Options struct {
	// Objects are applied once the webhooks answer, after the namespaces they name and Namespaces.
	Objects    []*unstructured.Unstructured
	Namespaces []string
	// Webhooks are validating webhook configurations whose services point at a local server. With
	// ClusterAPI or Serve, Start serves it; without, nothing does until the test serves it at the
	// address and with the certificates API.Webhooks holds.
	Webhooks []*admissionregistrationv1.ValidatingWebhookConfiguration
	// ClusterAPI serves Cluster API's Cluster and ClusterClass webhooks, from the configurations its
	// release ships, and runs its ClusterClass controller, with the feature gates the management
	// cluster sets: its defaulting and its checks of the classes and of the clusters' variables
	// against them, their CEL rules included, then apply as they do in a management cluster.
	ClusterAPI bool
	// Serve returns more handlers for the same server by path, such as those of Webhooks, given the
	// administrator's client.
	Serve func(client.Client) map[string]http.Handler
}

// API is a running API server.
type API struct {
	// Config is the administrator's.
	Config   *rest.Config
	Client   client.Client
	Webhooks *envtest.WebhookInstallOptions
}

// Scheme holds the kinds the tier's tests read and write as typed objects.
func Scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme, admissionregistrationv1.AddToScheme,
		apiextensionsv1.AddToScheme, clusterv1.AddToScheme, addonsv1.AddToScheme, runtimev1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

const (
	// serverStart bounds the wait for the webhook server, which the API server's webhooks need
	// before anything is installed.
	serverStart = 30 * time.Second
	// classReconcile bounds the wait for Cluster API to pass a class.
	classReconcile = time.Minute
	// poll is how often a wait looks again.
	poll = 100 * time.Millisecond
	// decodeBuffer is how far the decoder looks ahead to tell YAML from JSON.
	decodeBuffer = 4096
)

// Start starts an API server for the test and stops it once the test ends.
func Start(t *testing.T, o *Options) *API {
	t.Helper()
	crds := os.Getenv("KUBEHENGE_INTEGRATION_CRDS")
	if crds == "" || os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS and KUBEHENGE_INTEGRATION_CRDS are unset: run the tier with `make integration`")
	}
	install := envtest.WebhookInstallOptions{ValidatingWebhooks: o.Webhooks}
	if o.ClusterAPI {
		mutating, validating := clusterAPIWebhooks(t, filepath.Join(crds, "cluster-api.yaml"))
		install.MutatingWebhooks = append(install.MutatingWebhooks, mutating...)
		install.ValidatingWebhooks = append(install.ValidatingWebhooks, validating...)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crds},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: install,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Stop(t, env) })
	c, err := client.New(cfg, client.Options{Scheme: Scheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	if o.ClusterAPI || o.Serve != nil {
		serve(t, cfg, c, &env.WebhookInstallOptions, o)
	}
	Install(t, c, o.Namespaces, o.Objects)
	return &API{Config: cfg, Client: c, Webhooks: &env.WebhookInstallOptions}
}

// Stop stops an API server and its etcd, ending them first where envtest cannot.
func Stop(t *testing.T, env *envtest.Environment) {
	t.Helper()
	if err := EndChildren(); err != nil {
		t.Error(err)
		return
	}
	if err := env.Stop(); err != nil && !EndsChildrenItself {
		t.Error(err)
	}
}

// clusterAPIWebhooks are the webhook configurations of Cluster API's release for Clusters and
// ClusterClasses, as envtest points them at the local server.
func clusterAPIWebhooks(t *testing.T, components string) ([]*admissionregistrationv1.MutatingWebhookConfiguration, []*admissionregistrationv1.ValidatingWebhookConfiguration) {
	t.Helper()
	data, err := os.ReadFile(components) //nolint:gosec // the tier's own assets
	if err != nil {
		t.Fatal(err)
	}
	ours := func(rules []admissionregistrationv1.RuleWithOperations) bool {
		return slices.ContainsFunc(rules, func(r admissionregistrationv1.RuleWithOperations) bool {
			return slices.Contains(r.Resources, "clusters") || slices.Contains(r.Resources, "clusterclasses")
		})
	}
	var mutating []*admissionregistrationv1.MutatingWebhookConfiguration
	var validating []*admissionregistrationv1.ValidatingWebhookConfiguration
	for _, o := range Decode(t, data) {
		switch o.GetKind() {
		case "MutatingWebhookConfiguration":
			w := &admissionregistrationv1.MutatingWebhookConfiguration{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, w); err != nil {
				t.Fatal(err)
			}
			w.Webhooks = slices.DeleteFunc(w.Webhooks, func(h admissionregistrationv1.MutatingWebhook) bool { return !ours(h.Rules) })
			mutating = append(mutating, w)
		case "ValidatingWebhookConfiguration":
			w := &admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, w); err != nil {
				t.Fatal(err)
			}
			w.Webhooks = slices.DeleteFunc(w.Webhooks, func(h admissionregistrationv1.ValidatingWebhook) bool { return !ours(h.Rules) })
			validating = append(validating, w)
		}
	}
	if len(mutating) != 1 || len(mutating[0].Webhooks) != 1 || len(validating) != 1 || len(validating[0].Webhooks) != 2 {
		t.Fatalf("%s: Cluster API's webhooks for Clusters and ClusterClasses are not the three expected", components)
	}
	return mutating, validating
}

// noExtensions stands for Cluster API's Runtime SDK client, which its ClusterClass controller calls
// only for a class whose patches discover variables; none of Kubehenge's do, so any call panics.
type noExtensions struct {
	runtimeclient.Client
}

// serve runs a manager with the webhook server and, with ClusterAPI, Cluster API's webhooks and
// ClusterClass controller, until the test ends.
func serve(t *testing.T, cfg *rest.Config, admin client.Client, w *envtest.WebhookInstallOptions, o *Options) {
	t.Helper()
	srv := webhook.NewServer(webhook.Options{Host: w.LocalServingHost, Port: w.LocalServingPort, CertDir: w.LocalServingCertDir})
	// Cluster API's webhooks convert from the older API version they also serve.
	scheme := Scheme(t)
	if err := clusterv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Logger:                 testr.New(t),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		WebhookServer:          srv,
		// Each test runs a manager of its own in the one binary.
		Controller: ctrlconfig.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if o.ClusterAPI {
		// What the management cluster's Cluster API runs with (management-cluster/cluster-api.tf).
		if err := feature.MutableGates.SetFromMap(map[string]bool{string(feature.ClusterTopology): true, string(feature.RuntimeSDK): true}); err != nil {
			t.Fatal(err)
		}
		if err := (&capiadmission.Cluster{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr); err != nil {
			t.Fatal(err)
		}
		if err := (&capiadmission.ClusterClass{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr); err != nil {
			t.Fatal(err)
		}
		r := &clusterclass.Reconciler{Client: mgr.GetClient(), RuntimeClient: noExtensions{}}
		if err := r.SetupWithManager(ctx, mgr, controller.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if o.Serve != nil {
		for path, h := range o.Serve(admin) {
			srv.Register(path, h)
		}
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	// The webhooks fail closed while nothing answers them.
	started := srv.StartedChecker()
	for deadline := time.Now().Add(serverStart); started(nil) != nil; time.Sleep(poll) {
		if time.Now().After(deadline) {
			t.Fatal("the webhook server did not start")
		}
	}
}

// ClassNamespace is where the charts put Kubehenge's ClusterClasses and controller.
const ClassNamespace = "kubehenge-system"

// Charts renders the providers' chart and Kubehenge's as the management-cluster module installs
// them, from the repository at root, with the tier's values (pkg/provisioner/testdata) and
// Kubehenge's then with those of values, and parts out Kubehenge's webhook configurations, which
// fail closed while nothing answers them.
func Charts(t *testing.T, root string, values ...string) ([]*unstructured.Unstructured, []*admissionregistrationv1.ValidatingWebhookConfiguration) {
	t.Helper()
	testdata := filepath.Join(root, "pkg", "provisioner", "testdata")
	// The providers' release comes first, as in the module, and carries no webhooks of its own.
	objects := RenderChart(t, "cluster-api-providers", filepath.Join(root, "management-cluster", "charts", "cluster-api-providers"), "capi-operator-system",
		filepath.Join(testdata, "cluster-api-providers-values.yaml"))
	var webhooks []*admissionregistrationv1.ValidatingWebhookConfiguration
	for _, o := range RenderChart(t, "kubehenge", filepath.Join(root, "charts", "kubehenge"), ClassNamespace,
		append([]string{filepath.Join(testdata, "integration-values.yaml")}, values...)...) {
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

// RenderChart is what `helm template` makes of a chart with the values files given, hooks included.
func RenderChart(t *testing.T, release, chart, namespace string, values ...string) []*unstructured.Unstructured {
	t.Helper()
	base := []string{"template", release, chart, "--namespace", namespace}
	args := make([]string, 0, len(base)+2*len(values))
	args = append(args, base...)
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
	return Decode(t, out)
}

// Decode reads every object of a stream of YAML or JSON documents.
func Decode(t *testing.T, data []byte) []*unstructured.Unstructured {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), decodeBuffer)
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

// Install applies objects as Helm's server-side apply would, after making namespaces and those the
// objects name.
func Install(t *testing.T, c client.Client, namespaces []string, objects []*unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	namespaces = slices.Clone(namespaces)
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

// ApplyClassTemplates applies the templates a ClusterClass's ConfigMap of the same name holds under
// key, as Kubehenge's controller does once the class's providers serve: Cluster API's ClusterClass
// controller passes a class only once every template it names is there.
func ApplyClassTemplates(t *testing.T, c client.Client, namespace, class, key string) {
	t.Helper()
	ctx := context.Background()
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: class}, cm); err != nil {
		t.Fatalf("the templates of %s: %v", class, err)
	}
	for _, tpl := range Decode(t, []byte(cm.Data[key])) {
		tpl.SetNamespace(namespace)
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(tpl), client.FieldOwner("kubehenge"), client.ForceOwnership); err != nil {
			t.Fatalf("%s's %s %s: %v", class, tpl.GetKind(), tpl.GetName(), err)
		}
	}
}

// WaitReconciled waits until Cluster API's ClusterClass controller has passed a class's current
// generation and its variables, which its Cluster webhook needs before it defaults and checks a
// cluster's variables: until then it lets every cluster of the class through unchecked.
func WaitReconciled(t *testing.T, c client.Client, namespace, class string) *clusterv1.ClusterClass {
	t.Helper()
	cc := &clusterv1.ClusterClass{}
	for deadline := time.Now().Add(classReconcile); ; time.Sleep(poll) {
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: class}, cc); err != nil {
			t.Fatal(err)
		}
		if cc.Status.ObservedGeneration == cc.Generation && conditions.IsTrue(cc, clusterv1.ClusterClassVariablesReadyCondition) {
			return cc
		}
		if time.Now().After(deadline) {
			var why []string
			for _, cond := range cc.Status.Conditions {
				why = append(why, fmt.Sprintf("%s=%s %s: %s", cond.Type, cond.Status, cond.Reason, cond.Message))
			}
			t.Fatalf("Cluster API did not pass ClusterClass %s/%s (generation %d, observed %d): %s",
				namespace, class, cc.Generation, cc.Status.ObservedGeneration, strings.Join(why, "; "))
		}
	}
}
