package provisioner

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

func rancherConfig(t *testing.T, workload client.Client) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := workload.Get(context.Background(), client.ObjectKey{Namespace: rancherNamespace, Name: rancherConfigMap}, cm); err != nil {
		t.Fatal(err)
	}
	return cm
}

// Rancher's chart installs only once Rancher's ConfigMap names Turtles' images by digest, and Helm
// can take the ConfigMap over.
func TestRancherInstallsOnlyOnceTurtlesIsPinned(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "rancher", Value: apiextensionsv1.JSON{
		Raw: []byte(`{"install":true,"hostname":"rancher.example.org"}`),
	}})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	key := client.ObjectKey{Namespace: c.Namespace, Name: rancherProxy(c)}

	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), key, hcp); err == nil {
		t.Fatal("Rancher was installed before its ConfigMap could be written")
	}

	workload := fake.NewClientBuilder().WithScheme(remoteScheme).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), key, hcp); err != nil {
		t.Fatalf("Rancher once its ConfigMap is written: %v", err)
	}
	cm := rancherConfig(t, workload)
	if cm.Labels["app.kubernetes.io/managed-by"] != "Helm" || cm.Annotations["meta.helm.sh/release-name"] != "rancher" ||
		cm.Annotations["meta.helm.sh/release-namespace"] != rancherNamespace {
		t.Errorf("Helm cannot take over the ConfigMap: labels %v, annotations %v", cm.Labels, cm.Annotations)
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(cm.Data[turtlesChart]), &values); err != nil {
		t.Fatal(err)
	}
	image, _, _ := unstructured.NestedStringMap(values, "image")
	if image["repository"] != "docker.io/rancher/turtles" || image["tag"] != "sha256:"+testCharts.Turtles.Digest {
		t.Errorf("Turtles' image %v, want it by digest", image)
	}
	core, _, _ := unstructured.NestedString(values, "cluster-api-operator", "cluster-api", "core", "imageUrl")
	if core != testCharts.TurtlesClusterAPI.String() {
		t.Errorf("Cluster API's image %q, want %q", core, testCharts.TurtlesClusterAPI.String())
	}
}

// Rancher's own keys stay when the ConfigMap is Helm's already, and a changed image replaces the
// one written before.
func TestRancherConfigKeepsRanchersKeys(t *testing.T) {
	t.Parallel()
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(&corev1.ConfigMap{
		Namespace: rancherNamespace, Name: rancherConfigMap,
		Data: map[string]string{"priorityClassName": "rancher-critical", turtlesChart: "image: {}\n"},
	}).Build()
	values, err := turtlesValues(testCharts.Turtles, testCharts.TurtlesClusterAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRancherConfig(context.Background(), workload, values); err != nil {
		t.Fatal(err)
	}
	cm := rancherConfig(t, workload)
	if cm.Data["priorityClassName"] != "rancher-critical" || cm.Data[turtlesChart] != values {
		t.Errorf("ConfigMap's data %v", cm.Data)
	}
}
