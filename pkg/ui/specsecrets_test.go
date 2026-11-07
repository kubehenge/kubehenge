package ui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

var sealedValue = regexp.MustCompile(provisioner.SealedSecretAnnotation + `: (\S+)`)

func deleteWeb(t *testing.T, h *harness) {
	t.Helper()
	ctx := t.Context()
	c := &clusterv1.Cluster{Namespace: "default", Name: "web"}
	if err := h.kube.Delete(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := h.kube.Delete(ctx, &corev1.Secret{Namespace: "default", Name: "web-deploy-key"}); err != nil {
		t.Fatal(err)
	}
}

func specRequest(t *testing.T, spec string, withoutSecrets bool) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"spec": spec, "withoutSecrets": withoutSecrets})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSpecSecretsAreSealedStoredAndRestoredWithTheCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a, _, store := managementWithTarget(t, h)
	admin := h.browser()
	admin.signIn("admin@pve")
	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("POST", "/api/clusters", createBody), http.StatusCreated)
	ctx := t.Context()
	setUpBackups(t, admin)
	secret := &corev1.Secret{
		Namespace: "default", Name: "web-deploy-key", Labels: map[string]string{provisioner.ClusterSpecClusterLabel: "web"},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"ssh-privatekey": []byte("hunter2-key-material")},
	}
	if err := h.kube.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	h.server.storeSpecsOnce(ctx, map[string]string{})

	stored := string(store.data["clusters/default.web/cluster.yaml"])
	for _, bad := range []string{"hunter2-key-material", base64.StdEncoding.EncodeToString([]byte("hunter2-key-material"))} {
		if strings.Contains(stored, bad) {
			t.Fatalf("the backup target holds a Secret value in plain text:\n%s", stored)
		}
	}
	if !strings.HasPrefix(stored, "# Secrets: sealed, 1\n") || sealedValue.FindStringSubmatch(stored) == nil {
		t.Fatalf("the stored spec does not say it holds sealed Secrets:\n%s", stored)
	}
	rec := dev.do("GET", "/api/clusters/default/web/spec", "")
	expect(t, rec, http.StatusOK)
	spec := rec.Body.String()
	if !strings.HasPrefix(spec, "# Secrets: sealed, 1\n") || strings.Contains(spec, "hunter2-key-material") {
		t.Fatalf("the download: %s", spec)
	}

	// The same appliance, and so its key, restores the Secret with the cluster.
	deleteWeb(t, h)
	expect(t, dev.do("POST", "/api/cluster-specs", specRequest(t, spec, false)), http.StatusCreated)
	got := &corev1.Secret{}
	if err := h.kube.Get(ctx, client.ObjectKeyFromObject(secret), got); err != nil {
		t.Fatalf("the Secret was not restored: %v", err)
	}
	if string(got.Data["ssh-privatekey"]) != "hunter2-key-material" || got.Labels[provisioner.ClusterSpecClusterLabel] != "web" || got.Type != corev1.SecretTypeOpaque {
		t.Fatalf("restored %+v", got)
	}

	// Another appliance's key, and no key at all, refuse and name the Secret.
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	w, err := age.Encrypt(&sealed, other.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`{}`))
	_ = w.Close()
	foreign := sealedValue.ReplaceAllString(spec, provisioner.SealedSecretAnnotation+": "+base64.StdEncoding.EncodeToString(sealed.Bytes()))
	deleteWeb(t, h)
	rec = dev.do("POST", "/api/cluster-specs", specRequest(t, foreign, false))
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "Secret default/web-deploy-key") || !strings.Contains(rec.Body.String(), "another recovery key") {
		t.Errorf("the refusal does not name the Secret and why: %s", rec.Body)
	}
	if err = h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "web"}, &clusterv1.Cluster{}); err == nil {
		t.Error("a cluster was created although its Secret could not be restored")
	}
	if err = os.Remove(filepath.Join(a.Dir, "etcd-backup", "spec-recovery.key")); err != nil {
		t.Fatal(err)
	}
	rec = dev.do("POST", "/api/cluster-specs", specRequest(t, spec, false))
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "Secret default/web-deploy-key") || !strings.Contains(rec.Body.String(), "no recovery key") {
		t.Errorf("a missing key is not named: %s", rec.Body)
	}
	expect(t, dev.do("POST", "/api/cluster-specs", specRequest(t, spec, true)), http.StatusCreated)
	if err = h.kube.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); err == nil {
		t.Error("withoutSecrets restored a Secret")
	}
}
