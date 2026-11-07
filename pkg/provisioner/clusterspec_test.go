package provisioner

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClusterSpecRoundTripsAndKeepsNoSecretValue(t *testing.T) {
	t.Parallel()
	c := cluster(variableOf("storage", `"local-lvm"`))
	c.ResourceVersion, c.UID = "42", "uid-1"
	c.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "x", "kubehenge.dev/created-by": "root@pam"}
	c.Status.Phase = "Provisioned"
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{ClusterSpecClusterLabel: "web"}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{Name: "web-token", Namespace: "team-a", Labels: labels, Data: map[string][]byte{"token": []byte("hunter2")}},
		&corev1.ConfigMap{Name: "web-settings", Namespace: "team-a", Labels: labels, Data: map[string]string{"a": "b"}},
		&corev1.ConfigMap{Name: "other", Namespace: "team-a"},
	).Build()
	objects, err := ClusterSpecObjects(context.Background(), kube, c)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ClusterSpecYAML(c, objects)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, bad := range []string{"hunter2", "aHVudGVyMg", "uid-1", "last-applied", "Provisioned", "other"} {
		if strings.Contains(text, bad) {
			t.Errorf("the spec holds %q:\n%s", bad, text)
		}
	}
	for _, want := range []string{"kind: Cluster", "local-lvm", "web-settings", "kind: Secret", "created-by"} {
		if !strings.Contains(text, want) {
			t.Errorf("the spec lacks %q:\n%s", want, text)
		}
	}
	back, err := ParseClusterSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Name != "web" || back.Namespace != "team-a" || back.Spec.Topology.Version != c.Spec.Topology.Version || len(back.Spec.Topology.Variables) != 1 {
		t.Fatalf("read back %+v", back)
	}
	again, _ := ClusterSpecYAML(c, objects)
	if ClusterSpecSum(raw) != ClusterSpecSum(again) {
		t.Fatal("the same cluster gave different bytes")
	}
	c.Spec.Topology.Version = "v1.37.2+rke2r1"
	changed, _ := ClusterSpecYAML(c, objects)
	if ClusterSpecSum(raw) == ClusterSpecSum(changed) {
		t.Fatal("a changed cluster gave the same sum")
	}
}

func TestParseClusterSpecRefusesWhatIsNoCluster(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "kind: ConfigMap\nmetadata:\n  name: a\n", "kind: Cluster\nspec: {}\n", "kind: Cluster\nmetadata:\n  name: a\nbogus: 1\n"} {
		if _, err := ParseClusterSpec([]byte(raw)); err == nil {
			t.Errorf("%q was taken for a cluster", raw)
		}
	}
}
