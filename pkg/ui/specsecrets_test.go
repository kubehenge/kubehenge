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

// typedSecrets are the credentials a user types into the interface for the cluster web. Their
// values must reach the spec sealed and come back with the cluster.
var typedSecrets = map[string]string{
	"/api/clusters/default/web/credentials/backup":      `{"values":{"accessKeyID":"AKIA-web","secretAccessKey":"backup-secret-value"}}`,
	"/api/clusters/default/web/credentials/gitops":      `{"values":{"username":"deploy","password":"gitops-token-value"}}`,
	"/api/clusters/default/web/credentials/oidc-client": `{"values":{"clientSecret":"oidc-secret-value"}}`,
}

// controllerSecrets are what the controller makes beside a cluster and a rebuilt one cannot do
// without, and the Secrets it makes again, which a spec must leave out.
var (
	controllerSecrets = map[string]string{
		"web-gitops": "deploy-key-value", "web-backup-keys": "backup-keys-value",
		"web-encryption": "etcd-encryption-key-value", "web-ingress-ca": "ingress-ca-key-value",
	}
	regenerated = map[string]string{"web-kubeconfig": "kubeconfig-value", "web-ca": "capi-ca-value", "web-credentials": "pve-token-value"}
)

func cleanWeb(t *testing.T, h *harness, names []string) {
	t.Helper()
	ctx := t.Context()
	if err := h.kube.Delete(ctx, &clusterv1.Cluster{Namespace: "default", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := h.kube.Delete(ctx, &corev1.Secret{Namespace: "default", Name: name}); client.IgnoreNotFound(err) != nil {
			t.Fatal(err)
		}
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
	var names []string
	for path, body := range typedSecrets {
		expect(t, admin.do("PUT", path, body), http.StatusOK)
		names = append(names, "web."+path[strings.LastIndex(path, "/")+1:])
	}
	for _, group := range []map[string]string{controllerSecrets, regenerated} {
		for name, value := range group {
			if err := h.kube.Create(ctx, &corev1.Secret{Namespace: "default", Name: name, Data: map[string][]byte{"value": []byte(value)}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name := range controllerSecrets {
		names = append(names, name)
	}
	for name := range regenerated {
		names = append(names, name)
	}
	h.server.storeSpecsOnce(ctx, map[string]string{})

	stored := string(store.data["clusters/default.web/cluster.yaml"])
	for _, value := range []string{"backup-secret-value", "gitops-token-value", "oidc-secret-value", "AKIA-web", "deploy-key-value",
		"backup-keys-value", "etcd-encryption-key-value", "ingress-ca-key-value"} {
		if strings.Contains(stored, value) || strings.Contains(stored, base64.StdEncoding.EncodeToString([]byte(value))) {
			t.Fatalf("the backup target holds %q in plain text:\n%s", value, stored)
		}
	}
	if !strings.HasPrefix(stored, "# Secrets: sealed, 7\n") || len(sealedValue.FindAllString(stored, -1)) != 7 {
		t.Fatalf("the stored spec does not hold seven sealed Secrets:\n%s", stored)
	}
	for name := range regenerated {
		if strings.Contains(stored, "name: "+name+"\n") {
			t.Errorf("the spec holds %s, which the controller makes again", name)
		}
	}
	rec := dev.do("GET", "/api/clusters/default/web/spec", "")
	expect(t, rec, http.StatusOK)
	spec := rec.Body.String()
	if !strings.HasPrefix(spec, "# Secrets: sealed, 7\n") {
		t.Fatalf("the download: %s", spec)
	}

	// The same appliance, and so its key, restores every Secret with the cluster.
	cleanWeb(t, h, names)
	expect(t, dev.do("POST", "/api/cluster-specs", specRequest(t, spec, false)), http.StatusCreated)
	want := map[string]string{
		"web.backup": "backup-secret-value", "web.gitops": "gitops-token-value", "web.oidc-client": "oidc-secret-value",
		"web-gitops": "deploy-key-value", "web-backup-keys": "backup-keys-value",
		"web-encryption": "etcd-encryption-key-value", "web-ingress-ca": "ingress-ca-key-value",
	}
	for name, value := range want {
		got := &corev1.Secret{}
		if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, got); err != nil {
			t.Errorf("Secret %s was not restored: %v", name, err)
			continue
		}
		found := false
		for _, v := range got.Data {
			found = found || string(v) == value
		}
		if !found {
			t.Errorf("Secret %s holds %v", name, got.Data)
		}
	}
	typed := &corev1.Secret{}
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "web.backup"}, typed); err != nil {
		t.Fatal(err)
	}
	if typed.Type != provisioner.CredentialType || typed.Labels[provisioner.CredentialClusterLabel] != "web" || typed.Annotations[provisioner.CredentialSetBy] != "admin@pve" {
		t.Errorf("the restored credential lost its type, labels or annotations: %+v", typed.ObjectMeta)
	}
	for name := range regenerated {
		if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &corev1.Secret{}); err == nil {
			t.Errorf("Secret %s, which the controller makes again, was restored", name)
		}
	}

	// A Secret of another cluster in a spec is refused, before anything is written.
	cleanWeb(t, h, names)
	foreign := spec + "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: other-gitops\n  namespace: default\n"
	expect(t, dev.do("POST", "/api/cluster-specs", specRequest(t, foreign, true)), http.StatusUnprocessableEntity)
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "web-gitops"}, &corev1.Secret{}); err == nil {
		t.Error("a refused request left Secrets behind")
	}

	// Another appliance's key, and no key at all, refuse and name the Secrets.
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
	otherKey := sealedValue.ReplaceAllString(spec, provisioner.SealedSecretAnnotation+": "+base64.StdEncoding.EncodeToString(sealed.Bytes()))
	rec = dev.do("POST", "/api/cluster-specs", specRequest(t, otherKey, false))
	expect(t, rec, http.StatusUnprocessableEntity)
	for name := range want {
		if !strings.Contains(rec.Body.String(), "Secret default/"+name+" ") {
			t.Errorf("the refusal does not name %s: %s", name, rec.Body)
		}
	}
	if !strings.Contains(rec.Body.String(), "another recovery key") {
		t.Errorf("the refusal does not say why: %s", rec.Body)
	}
	if err = h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "web"}, &clusterv1.Cluster{}); err == nil {
		t.Error("a cluster was created although its Secrets could not be restored")
	}
	if err = os.Remove(filepath.Join(a.Dir, "etcd-backup", "spec-recovery.key")); err != nil {
		t.Fatal(err)
	}
	rec = dev.do("POST", "/api/cluster-specs", specRequest(t, spec, false))
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "Secret default/web-gitops") || !strings.Contains(rec.Body.String(), "no recovery key") {
		t.Errorf("a missing key is not named: %s", rec.Body)
	}
	expect(t, dev.do("POST", "/api/cluster-specs", specRequest(t, spec, true)), http.StatusCreated)
	if err = h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "web-gitops"}, &corev1.Secret{}); err == nil {
		t.Error("withoutSecrets restored a Secret")
	}
}
