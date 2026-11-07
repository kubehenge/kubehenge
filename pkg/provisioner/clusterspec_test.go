package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	raw, err := ClusterSpecYAML(c, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state, n := SpecSecretsState(raw); state != SpecSecretsOmitted || n != 1 {
		t.Errorf("a spec made without a sealer says %q, %d", state, n)
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
	sum, _ := ClusterSpecSum(c, objects)
	if again, _ := ClusterSpecSum(c, objects); sum != again {
		t.Fatal("the same cluster gave different sums")
	}
	c.Spec.Topology.Version = "v1.37.2+rke2r1"
	if changed, _ := ClusterSpecSum(c, objects); sum == changed {
		t.Fatal("a changed cluster gave the same sum")
	}
	c.Spec.Topology.Version = cluster(variableOf("storage", `"local-lvm"`)).Spec.Topology.Version
	rotated, ok := objects[len(objects)-1].(*corev1.Secret)
	if !ok {
		t.Fatalf("the last object is %T", objects[len(objects)-1])
	}
	rotated.Data["token"] = []byte("rotated")
	if changed, _ := ClusterSpecSum(c, objects); sum == changed {
		t.Fatal("a rotated Secret value gave the same sum")
	}
}

func TestClusterSpecSealsSecretValuesAndLeavesNoneInPlainText(t *testing.T) {
	t.Parallel()
	c := cluster()
	labels := map[string]string{ClusterSpecClusterLabel: "web"}
	secret := &corev1.Secret{
		Name: "web-deploy-key", Namespace: "team-a", Labels: labels, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"ssh-privatekey": []byte("hunter2")},
	}
	var plains [][]byte
	seal := func(plain []byte) (string, error) {
		plains = append(plains, plain)
		sum := sha256.Sum256(plain)
		return "sealed-" + hex.EncodeToString(sum[:]), nil
	}
	raw, err := ClusterSpecYAML(c, []client.Object{secret}, seal)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"hunter2", "aHVudGVyMg"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("the stored spec holds %q in plain text:\n%s", bad, raw)
		}
	}
	if string(secret.Data["ssh-privatekey"]) != "hunter2" {
		t.Error("sealing emptied the caller's Secret")
	}
	if state, n := SpecSecretsState(raw); state != SpecSecretsSealed || n != 1 {
		t.Errorf("state %q, %d", state, n)
	}
	if len(plains) != 1 || !strings.Contains(string(plains[0]), "ssh-privatekey") {
		t.Fatalf("the sealer was given %q", plains)
	}
	found, err := ParseSpecSecrets(raw)
	if err != nil || len(found) != 1 || found[0].Name != "web-deploy-key" || found[0].Namespace != "team-a" ||
		!strings.HasPrefix(found[0].Sealed, "sealed-") || found[0].Labels[ClusterSpecClusterLabel] != "web" {
		t.Fatalf("read back %+v, %v", found, err)
	}
}

func TestClusterSpecObjectsFollowTheClusterAndNotALabel(t *testing.T) {
	t.Parallel()
	c := cluster()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := func(name string, labels map[string]string) *corev1.Secret {
		return &corev1.Secret{Name: name, Namespace: "team-a", Labels: labels, Data: map[string][]byte{"k": []byte("v")}}
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		secret("web-gitops", nil), secret("web-backup-keys", nil), secret("web-encryption", nil), secret("web-ingress-ca", nil),
		secret("web.backup", nil), secret("web.dns01", nil), secret("web.gitops", nil), secret("web.oidc-client", nil),
		secret("web-extra", map[string]string{ClusterSpecClusterLabel: "web"}),
		secret("web-kubeconfig", nil), secret("web-ca", nil), secret("web-sa", nil), secret("web-credentials", nil),
		secret("kubehenge-backup", nil), secret("kubehenge-webhook", nil), secret("other-gitops", nil),
	).Build()
	objects, err := ClusterSpecObjects(context.Background(), kube, c)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(objects))
	for _, o := range objects {
		got = append(got, o.GetName())
	}
	want := "web-backup-keys web-encryption web-extra web-gitops web-ingress-ca web.backup web.dns01 web.gitops web.oidc-client"
	if strings.Join(got, " ") != want {
		t.Errorf("collected\n%s\nwant\n%s", strings.Join(got, " "), want)
	}
	for _, name := range []string{"web-gitops", "web-extra"} {
		if !SpecSecretBelongs(c, &SpecSecret{Namespace: "team-a", Name: name, Labels: map[string]string{ClusterSpecClusterLabel: "web"}}) {
			t.Errorf("%s does not belong to its cluster", name)
		}
	}
	for _, s := range []SpecSecret{{Namespace: "team-a", Name: "other-gitops"}, {Namespace: "kube-system", Name: "web-gitops"}, {Namespace: "team-a", Name: "web-kubeconfig"}} {
		if SpecSecretBelongs(c, &s) {
			t.Errorf("%s/%s was taken for the cluster's", s.Namespace, s.Name)
		}
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
