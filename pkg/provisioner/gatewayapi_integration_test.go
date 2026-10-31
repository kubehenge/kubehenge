//go:build integration

package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
	"github.com/kubehenge/kubehenge/test/integration"
)

// The Gateway API's release as a workload cluster's API server takes it: every CRD established,
// and its admission policy refusing older and experimental CRDs.
func TestIntegrationGatewayAPI(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is unset: run the tier with `make integration`")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { integration.Stop(t, env) })
	workload, err := client.New(cfg, client.Options{Scheme: remoteScheme})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := LoadGatewayAPI()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &GatewayAPIReconciler{Bundle: bundle}
	key := types.NamespacedName{Namespace: "team-a", Name: "web"}
	if _, applied, ensureErr := r.ensure(ctx, key, workload, ""); ensureErr != nil || !applied {
		t.Fatalf("applied %v, %v", applied, ensureErr)
	}
	oldest, applied, err := r.ensure(ctx, key, workload, "")
	if err != nil || applied || oldest != bundle.Version {
		t.Fatalf("once applied: oldest %q, applied %v, %v", oldest, applied, err)
	}
	for _, o := range bundle.objects {
		if o.gvk != crdGVK {
			continue
		}
		until(t, 30*time.Second, o.name+" established", func() bool {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := workload.Get(ctx, client.ObjectKey{Name: o.name}, crd); err != nil {
				t.Fatal(err)
			}
			for _, c := range crd.Status.Conditions {
				if c.Type == apiextensionsv1.Established {
					return c.Status == apiextensionsv1.ConditionTrue
				}
			}
			return false
		})
	}

	// The API server takes a CRD in a kubernetes.io group only with its API review's approval,
	// which the older CRD borrows from a real one so that only the policy can refuse it.
	const approval = "api-approved.kubernetes.io"
	approved := &apiextensionsv1.CustomResourceDefinition{}
	if err := workload.Get(ctx, client.ObjectKey{Name: "httproutes.gateway.networking.k8s.io"}, approved); err != nil {
		t.Fatal(err)
	}
	older := &apiextensionsv1.CustomResourceDefinition{
		Name: "olds.gateway.networking.k8s.io",
		Annotations: map[string]string{
			bundleVersionAnnotation: "v1.4.0", "gateway.networking.k8s.io/channel": "standard",
			approval: approved.Annotations[approval],
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "gateway.networking.k8s.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "olds", Kind: "Old", ListKind: "OldList", Singular: "old"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"}},
			}},
		},
	}
	// The API server enforces a new policy a moment after it is created.
	until(t, 30*time.Second, "the policy refuses a release before v1.5.0", func() bool {
		created := workload.Create(ctx, older.DeepCopy())
		if created == nil {
			if err := workload.Delete(ctx, older.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			return false
		}
		if !strings.Contains(created.Error(), "prohibited") {
			t.Fatalf("an older CRD: %v", created)
		}
		return true
	})
	experimental := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"gateway.networking.k8s.io/channel":"experimental"}}}`))
	routes := &metav1.PartialObjectMetadata{}
	routes.SetGroupVersionKind(crdGVK)
	routes.SetName("httproutes.gateway.networking.k8s.io")
	if err := workload.Patch(ctx, routes, experimental); err == nil || !strings.Contains(err.Error(), "prohibited") {
		t.Errorf("an experimental CRD over a standard one: %v", err)
	}
}

// The distribution's own Gateway API chart is found by its HelmChart, through a kind an API server
// has only once helm-controller's CRD is there, and Kubehenge waits until its release is installed.
func TestIntegrationGatewayAPIFindsTheDistributionsChart(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is unset: run the tier with `make integration`")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { integration.Stop(t, env) })
	workload, err := client.New(cfg, client.Options{Scheme: remoteScheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	held := func() (bool, string) {
		t.Helper()
		ready, owner, err := distributionGatewayAPI(ctx, workload, templates.RKE2)
		if err != nil {
			t.Fatal(err)
		}
		return ready, owner
	}
	if err := workload.Create(ctx, helmRevision(t, "rke2-traefik-crd", 1, "deployed", "")); err != nil {
		t.Fatal(err)
	}
	if ready, owner := held(); !ready || owner != "" {
		t.Errorf("without HelmCharts: ready %v, held by %q", ready, owner)
	}

	helmCharts := &apiextensionsv1.CustomResourceDefinition{
		Name: "helmcharts.helm.cattle.io",
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: helmChartGVK.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "helmcharts", Kind: "HelmChart", ListKind: "HelmChartList", Singular: "helmchart"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: helmChartGVK.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: new(true)}},
			}},
		},
	}
	if err := workload.Create(ctx, helmCharts); err != nil {
		t.Fatal(err)
	}
	chart := &unstructured.Unstructured{}
	chart.SetGroupVersionKind(helmChartGVK)
	chart.SetNamespace("kube-system")
	chart.SetName("rke2-gateway-api-crd")
	until(t, 30*time.Second, "a HelmChart created", func() bool { return workload.Create(ctx, chart.DeepCopy()) == nil })
	if ready, owner := held(); ready || owner != "kube-system/rke2-gateway-api-crd" {
		t.Errorf("while the distribution's Gateway API chart installs: ready %v, held by %q", ready, owner)
	}
	if err := workload.Create(ctx, helmRevision(t, "rke2-gateway-api-crd", 1, "deployed", "")); err != nil {
		t.Fatal(err)
	}
	if ready, owner := held(); !ready || owner != "kube-system/rke2-gateway-api-crd" {
		t.Errorf("once the distribution's Gateway API chart is installed: ready %v, held by %q", ready, owner)
	}
}

// until polls cond until it holds and fails the test after limit.
func until(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(limit); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// The RKE2 and k3s classes switch their Traefik's Gateway API provider on through the
// HelmChartConfig they write on the control planes, as Cluster API renders it for a cluster, and
// leave the cluster's default certificate to the values Secret it names, so that no certificates
// setting changes the file and replaces the control planes.
func TestIntegrationClassesConfigureTraefik(t *testing.T) {
	t.Parallel()
	found := map[string]bool{}
	for _, o := range integration.RenderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml") {
		if o.GetKind() != "ClusterClass" {
			continue
		}
		patches, _, err := unstructured.NestedSlice(o.Object, "spec", "patches")
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range jsonPatches(t, patches) {
			raw, _, _ := unstructured.NestedString(op, "valueFrom", "template")
			if !strings.Contains(raw, "traefik-config.yaml") {
				continue
			}
			found[o.GetName()] = true
			checkTraefikConfig(t, o.GetName(), raw)
		}
	}
	if !found["kubehenge-rke2"] || !found["kubehenge-k3s"] || found["kubehenge-kubeadm"] {
		t.Errorf("classes configuring their distribution's Traefik: %v", found)
	}
}

// helmChartConfig is the part of an RKE2 or k3s HelmChartConfig that sets a chart's values.
type helmChartConfig struct {
	Kind string              `json:"kind"`
	Spec helmChartConfigSpec `json:"spec"`
}

type helmChartConfigSpec struct {
	ValuesContent string              `json:"valuesContent"`
	ValuesSecrets []helmValuesSecrets `json:"valuesSecrets"`
}

type helmValuesSecrets struct {
	Name          string   `json:"name"`
	Keys          []string `json:"keys"`
	IgnoreUpdates bool     `json:"ignoreUpdates"`
}

// The certificates variable as clusters set it, "" for a cluster without one.
var traefikCertificateCases = []string{
	"",
	`{"enabled": false, "issuer": "ca", "domain": "lab.example.org"}`,
	`{"enabled": true, "issuer": "acme-dns01"}`,
	`{"enabled": true, "issuer": "acme-http01", "domain": "lab.example.org"}`,
	`{"enabled": true, "domain": "lab.example.org"}`,
	`{"enabled": true, "issuer": "acme-dns01", "domain": "lab.example.org"}`,
	`{"enabled": true, "issuer": "ca", "domain": "lab.example.org"}`,
}

func checkTraefikConfig(t *testing.T, class, raw string) {
	t.Helper()
	tpl, err := template.New(class).Funcs(sprig.HermeticTxtFuncMap()).Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var first []byte
	for _, certificates := range traefikCertificateCases {
		// As Cluster API hands a template its variables: decoded from their JSON.
		data := map[string]any{"controlPlaneEndpoint": "192.0.2.50"}
		if certificates != "" {
			var v any
			if err := json.Unmarshal([]byte(certificates), &v); err != nil {
				t.Fatal(err)
			}
			data[certificatesVariable] = v
		}
		var out bytes.Buffer
		if err := tpl.Execute(&out, data); err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = out.Bytes()
		} else if !bytes.Equal(out.Bytes(), first) {
			t.Errorf("%s: certificates %s change the file on the control planes:\n%s", class, certificates, out.Bytes())
		}
	}
	config, values := traefikConfigValues(t, class, first)
	checkTraefikValues(t, class, values)
	_, websecure, _ := unstructured.NestedFieldNoCopy(values, "gateway", "listeners", "websecure")
	if _, store := values["tlsStore"]; store || websecure {
		t.Errorf("%s: the file gives Traefik a default certificate: %v", class, values)
	}
	want := []helmValuesSecrets{{Name: traefikValuesSecret, Keys: []string{traefikValuesKey}}}
	if !reflect.DeepEqual(config.Spec.ValuesSecrets, want) {
		t.Errorf("%s: Traefik's values Secrets %+v, want %+v", class, config.Spec.ValuesSecrets, want)
	}
}

// traefikConfigValues are the HelmChartConfig in a rendered file patch and its values.
func traefikConfigValues(t *testing.T, class string, rendered []byte) (helmChartConfig, map[string]any) {
	t.Helper()
	var file struct {
		Content string `json:"content"`
	}
	var config helmChartConfig
	var values map[string]any
	if err := yaml.Unmarshal(rendered, &file); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(file.Content), &config); err != nil {
		t.Fatalf("%s: %v in\n%s", class, err, file.Content)
	}
	if err := yaml.Unmarshal([]byte(config.Spec.ValuesContent), &values); err != nil {
		t.Fatalf("%s: %v in\n%s", class, err, config.Spec.ValuesContent)
	}
	if config.Kind != "HelmChartConfig" {
		t.Fatalf("%s: a %s", class, config.Kind)
	}
	return config, values
}

// checkTraefikValues holds a class's Traefik to what every cluster's needs.
func checkTraefikValues(t *testing.T, class string, values map[string]any) {
	t.Helper()
	gateway, _, _ := unstructured.NestedMap(values, "providers", "kubernetesGateway")
	ip, _, _ := unstructured.NestedString(gateway, "statusAddress", "ip")
	if enabled, _, _ := unstructured.NestedBool(gateway, "enabled"); !enabled || ip != "192.0.2.50" {
		t.Errorf("%s: Traefik's Gateway provider %v", class, gateway)
	}
	if from, _, _ := unstructured.NestedString(values, "gateway", "listeners", "web", "namespacePolicy", "from"); from != "All" {
		t.Errorf("%s: Traefik's Gateway takes routes from %q", class, from)
	}
	if strict, _, _ := unstructured.NestedBool(values, "core", "strictTLSOptions"); !strict {
		t.Errorf("%s: %s", class, fmt.Sprint(values))
	}
	if cpu, _, _ := unstructured.NestedString(values, "resources", "requests", "cpu"); cpu == "" {
		t.Errorf("%s: Traefik requests no CPU", class)
	}
}
