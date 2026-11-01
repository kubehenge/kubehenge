package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func credentialOf(t *testing.T, body *bytes.Buffer) credentialView {
	t.Helper()
	var v credentialView
	if err := json.Unmarshal(body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

func storedSecret(t *testing.T, h *harness, name string) (*corev1.Secret, bool) {
	t.Helper()
	s := &corev1.Secret{}
	err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, s)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return s, true
}

// checkStoredBackup checks the Secret of cluster one's backup credential, as dev@pve first stored
// it: owned by the cluster, so that deleting the cluster deletes it.
func checkStoredBackup(t *testing.T, h *harness) {
	t.Helper()
	secret, ok := storedSecret(t, h, "one.backup")
	if !ok {
		t.Fatal("no Secret one.backup")
	}
	owner := &clusterv1.Cluster{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "one"}, owner); err != nil {
		t.Fatal(err)
	}
	refs := secret.OwnerReferences
	if secret.Type != provisioner.CredentialType ||
		secret.Labels[provisioner.CredentialLabel] != "backup" || secret.Labels[provisioner.CredentialClusterLabel] != "one" ||
		secret.Annotations[provisioner.CredentialSetBy] != "dev@pve" ||
		string(secret.Data["accessKeyID"]) != "AKIA-first" || string(secret.Data["secretAccessKey"]) != "first-secret" ||
		len(refs) != 1 || refs[0].Kind != "Cluster" || refs[0].Name != "one" || refs[0].UID != owner.UID {
		t.Fatalf("Secret = %+v", secret)
	}
}

func TestClusterCredentials(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"))
	var logged bytes.Buffer
	h.server.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	b := h.browser()
	b.signIn("dev@pve")
	const path = "/api/clusters/default/one/credentials/backup"

	rec := b.do("GET", path, "")
	expect(t, rec, http.StatusOK)
	if v := credentialOf(t, rec.Body); v.Set || !v.CanChange || v.Purpose != "backup" {
		t.Fatalf("before storing: %+v", v)
	}

	before := time.Now().UTC().Add(-time.Second)
	rec = b.do("PUT", path, `{"values":{"accessKeyID":"AKIA-first","secretAccessKey":"first-secret"}}`)
	expect(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "first-secret") || strings.Contains(rec.Body.String(), "AKIA-first") {
		t.Fatalf("the answer gives a value back: %s", rec.Body)
	}
	v := credentialOf(t, rec.Body)
	if !v.Set || v.SetBy != "dev@pve" || v.SetAt == nil || v.SetAt.Before(before) || !v.CanChange {
		t.Fatalf("stored: %+v", v)
	}
	checkStoredBackup(t, h)

	rec = b.do("GET", path, "")
	expect(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "first-secret") || !credentialOf(t, rec.Body).Set {
		t.Fatalf("after storing: %s", rec.Body)
	}

	expect(t, b.do("PUT", path, `{"values":{"accessKeyID":"AKIA-second","secretAccessKey":"second-secret"}}`), http.StatusOK)
	if secret, _ := storedSecret(t, h, "one.backup"); string(secret.Data["secretAccessKey"]) != "second-secret" || len(secret.Data) != 2 {
		t.Fatalf("replaced: %v", secret.Data)
	}

	expect(t, b.do("DELETE", path, ""), http.StatusNoContent)
	if _, ok := storedSecret(t, h, "one.backup"); ok {
		t.Fatal("the credential is still there")
	}
	expect(t, b.do("DELETE", path, ""), http.StatusNoContent)
	if v := credentialOf(t, b.do("GET", path, "").Body); v.Set {
		t.Fatalf("after removing: %+v", v)
	}

	for _, value := range []string{"first-secret", "second-secret", "AKIA"} {
		if strings.Contains(logged.String(), value) {
			t.Fatalf("the log holds a value: %s", logged.String())
		}
	}
	if !strings.Contains(logged.String(), `"credential":"backup of default/one"`) {
		t.Fatalf("the log names no change: %s", logged.String())
	}
}

func TestCredentialValues(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	const path = "/api/clusters/default/one/credentials/backup"
	// A credential takes every value of its purpose, and only those.
	for _, body := range []string{
		`{"values":{"accessKeyID":"AKIA"}}`,
		`{"values":{"accessKeyID":"AKIA","secretAccessKey":""}}`,
		`{"values":{"accessKeyID":"a","secretAccessKey":"b","region":"c"}}`,
		`{"values":{"accessKeyID":"AKIA","secretAccessKey":"` + strings.Repeat("x", maxCredentialValue+1) + `"}}`,
	} {
		expect(t, b.do("PUT", path, body), http.StatusUnprocessableEntity)
	}
	if _, ok := storedSecret(t, h, "one.backup"); ok {
		t.Fatal("a credential stored from values it refused")
	}
	expect(t, b.do("GET", "/api/clusters/default/one/credentials/ssh", ""), http.StatusNotFound)
	expect(t, b.do("PUT", "/api/clusters/default/one/credentials/gitops", `{"values":{"username":"git","password":"ghp-token"}}`), http.StatusOK)
	if secret, ok := storedSecret(t, h, "one.gitops"); !ok || string(secret.Data["password"]) != "ghp-token" {
		t.Fatalf("one.gitops: %v", secret)
	}
}

// Without Sys.Modify on its pool, a user sees whether a cluster's credential is set, and no more.
func TestCredentialPrivileges(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"))
	b := h.browser()
	b.signIn("dev@pve")
	const values = `{"values":{"accessKeyID":"a","secretAccessKey":"b"}}`
	rec := b.do("GET", "/api/clusters/default/two/credentials/backup", "")
	expect(t, rec, http.StatusOK)
	if v := credentialOf(t, rec.Body); v.CanChange || v.Set {
		t.Fatalf("another cluster's: %+v", v)
	}
	expect(t, b.do("PUT", "/api/clusters/default/two/credentials/backup", values), http.StatusForbidden)
	expect(t, b.do("DELETE", "/api/clusters/default/two/credentials/backup", ""), http.StatusForbidden)

	const path = "/api/clusters/default/one/credentials/backup"
	expect(t, b.do("PUT", path, values), http.StatusOK)
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	rec = viewer.do("GET", path, "")
	expect(t, rec, http.StatusOK)
	if v := credentialOf(t, rec.Body); v.CanChange || !v.Set || v.SetBy != "dev@pve" {
		t.Fatalf("a viewer's: %+v", v)
	}
	expect(t, viewer.do("PUT", path, values), http.StatusForbidden)
	expect(t, viewer.do("DELETE", path, ""), http.StatusForbidden)
	expect(t, viewer.do("GET", "/api/clusters/default/two/credentials/backup", ""), http.StatusNotFound)
	if _, ok := storedSecret(t, h, "one.backup"); !ok {
		t.Fatal("a viewer removed the credential")
	}
}

func TestCredentialNameTakenByAnotherSecret(t *testing.T) {
	t.Parallel()
	other := &corev1.Secret{Namespace: "default", Name: "one.backup", Data: map[string][]byte{"key": []byte("theirs")}}
	h := newHarness(t, cluster("default", "one"), other)
	b := h.browser()
	b.signIn("dev@pve")
	const path = "/api/clusters/default/one/credentials/backup"
	expect(t, b.do("GET", path, ""), http.StatusConflict)
	expect(t, b.do("PUT", path, `{"values":{"accessKeyID":"a","secretAccessKey":"b"}}`), http.StatusConflict)
	expect(t, b.do("DELETE", path, ""), http.StatusConflict)
	if secret, ok := storedSecret(t, h, "one.backup"); !ok || string(secret.Data["key"]) != "theirs" || len(secret.Data) != 1 {
		t.Fatalf("the other Secret: %v", secret)
	}
}

func TestCredentialsOfClustersElsewhereOrGoing(t *testing.T) {
	t.Parallel()
	going := cluster("default", "one")
	going.Finalizers = []string{"kubehenge.dev/test"}
	going.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)}
	h := newHarness(t, going, cluster("team-a", "elsewhere"))
	b := h.browser()
	b.signIn("admin@pve")
	expect(t, b.do("PUT", "/api/clusters/default/one/credentials/backup", `{"values":{"accessKeyID":"a","secretAccessKey":"b"}}`), http.StatusConflict)
	expect(t, b.do("GET", "/api/clusters/default/one/credentials/backup", ""), http.StatusOK)
	expect(t, b.do("GET", "/api/clusters/team-a/elsewhere/credentials/backup", ""), http.StatusForbidden)
	expect(t, b.do("PUT", "/api/clusters/team-a/elsewhere/credentials/backup", `{"values":{"accessKeyID":"a","secretAccessKey":"b"}}`), http.StatusForbidden)
}

func TestApplianceCredentials(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("GET", "/api/credentials/dns01", "")
	expect(t, rec, http.StatusOK)
	if v := credentialOf(t, rec.Body); v.Set || !v.CanChange {
		t.Fatalf("before storing: %+v", v)
	}
	rec = a.do("PUT", "/api/credentials/dns01", `{"values":{"token":"dns-token"}}`)
	expect(t, rec, http.StatusOK)
	if v := credentialOf(t, rec.Body); !v.Set || v.SetBy != "admin@pve" || strings.Contains(rec.Body.String(), "dns-token") {
		t.Fatalf("stored: %s", rec.Body)
	}
	secret, ok := storedSecret(t, h, "kubehenge-dns01")
	if !ok || secret.Type != provisioner.CredentialType || secret.Labels[provisioner.CredentialLabel] != "dns01" ||
		secret.Labels[provisioner.CredentialClusterLabel] != "" || len(secret.OwnerReferences) != 0 || string(secret.Data["token"]) != "dns-token" {
		t.Fatalf("Secret = %+v", secret)
	}
	// What belongs to one cluster's workloads has no appliance-wide credential.
	expect(t, a.do("GET", "/api/credentials/gitops", ""), http.StatusNotFound)
	expect(t, a.do("PUT", "/api/credentials/oidc-client", `{"values":{"clientSecret":"x"}}`), http.StatusNotFound)

	// Even Sys.Modify on every cluster's pool is not an administrator's privileges.
	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("GET", "/api/credentials/dns01", ""), http.StatusForbidden)
	expect(t, dev.do("PUT", "/api/credentials/dns01", `{"values":{"token":"mine"}}`), http.StatusForbidden)
	expect(t, dev.do("DELETE", "/api/credentials/dns01", ""), http.StatusForbidden)

	expect(t, a.do("DELETE", "/api/credentials/dns01", ""), http.StatusNoContent)
	if _, ok := storedSecret(t, h, "kubehenge-dns01"); ok {
		t.Fatal("the credential is still there")
	}
}

func TestNotificationTargetsAreCheckedAndOnlyTheAppliancesOwn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	a := h.browser()
	a.signIn("admin@pve")
	for _, c := range []struct {
		purpose, body string
		want          int
	}{
		{"notify-ntfy", `{"values":{"url":"https://ntfy.sh/kh"}}`, http.StatusOK},
		{"notify-ntfy", `{"values":{"url":"ntfy.sh/kh","token":"tk"}}`, http.StatusUnprocessableEntity},
		{"notify-email", `{"values":{"server":"smtp.example.com:587","from":"kh@example.com","to":"ops@example.com","username":"","password":""}}`, http.StatusOK},
		{"notify-email", `{"values":{"server":"smtp.example.com:587","from":"kh@example.com","to":"ops@example.com","username":"kh"}}`, http.StatusUnprocessableEntity},
		{"notify-webhook", `{"values":{"url":"https://hooks.example.com/s3cret"}}`, http.StatusOK},
	} {
		expect(t, a.do("PUT", "/api/credentials/"+c.purpose, c.body), c.want)
	}
	if secret, ok := storedSecret(t, h, "kubehenge-notify-ntfy"); !ok || len(secret.Data) != 1 {
		t.Errorf("ntfy without a token = %+v, want its url alone", secret)
	}
	if secret, ok := storedSecret(t, h, "kubehenge-notify-email"); !ok || secret.Data["username"] != nil {
		t.Errorf("email without signing in = %+v, want no username", secret)
	}
	expect(t, a.do("PUT", "/api/clusters/default/one/credentials/notify-webhook", `{"values":{"url":"https://hooks.example.com/x"}}`), http.StatusNotFound)
}

func TestCredentialNames(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, p := range provisioner.CredentialPurposes() {
		if strings.Contains(p.Name, ".") || len(p.Keys) == 0 {
			t.Fatalf("purpose %+v", p)
		}
		for _, name := range []string{provisioner.CredentialName("one", p.Name), provisioner.CredentialName("", p.Name)} {
			if seen[name] {
				t.Fatalf("two credentials named %s", name)
			}
			seen[name] = true
		}
	}
	if provisioner.CredentialName("kubehenge", "backup") == provisioner.CredentialName("", "backup") {
		t.Fatal("a cluster named kubehenge takes the appliance-wide credential's name")
	}
}
