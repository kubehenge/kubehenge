package provisioner

import (
	"bytes"
	"context"
	"encoding/base64"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestReconcileKeepsTheKubeadmEncryptionKey(t *testing.T) {
	t.Parallel()
	c := cluster(
		clusterv1.ClusterVariable{Name: "storage", Value: apiextensionsv1.JSON{Raw: []byte(`"local-lvm"`)}},
		clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"cilium"`)}},
	)
	c.Spec.Topology.ClassRef.Name = "kubehenge-kubeadm"
	r, kube := newReconciler(t, &fakeCloud{}, c)
	read := func() []byte {
		t.Helper()
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
		s := &corev1.Secret{}
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-encryption"}, s); err != nil {
			t.Fatal(err)
		}
		if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != "web" {
			t.Errorf("the key outlives its cluster: %v", s.OwnerReferences)
		}
		return s.Data[encryptionConfigKey]
	}
	first := read()
	config := map[string]any{}
	if err := yaml.Unmarshal(first, &config); err != nil {
		t.Fatalf("%v:\n%s", err, first)
	}
	key := lookup[map[string]any](t, config, "resources", 0, "providers", 0, "secretbox", "keys", 0)
	if raw, err := base64.StdEncoding.DecodeString(lookup[string](t, key, "secret")); err != nil || len(raw) != secretboxKeyBytes {
		t.Errorf("a secretbox key of %d bytes: %v", len(raw), err)
	}
	key["secret"] = "the key"
	want := map[string]any{
		"apiVersion": "apiserver.config.k8s.io/v1",
		"kind":       "EncryptionConfiguration",
		"resources": []any{map[string]any{
			"resources": []any{"secrets"},
			"providers": []any{map[string]any{"secretbox": map[string]any{"keys": []any{
				map[string]any{"name": "key1", "secret": "the key"},
			}}}},
		}},
	}
	if !reflect.DeepEqual(config, want) {
		t.Errorf("configuration:\n%s", first)
	}
	if again := read(); !bytes.Equal(again, first) {
		t.Error("the key was replaced, and with it every Secret the cluster wrote became unreadable")
	}
}

func TestReconcileMakesNoEncryptionKeyWhereTheDistributionHasItsOwn(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-encryption"}, &corev1.Secret{})
	if err == nil {
		t.Error("an RKE2 cluster got a key it does not use")
	}
}
