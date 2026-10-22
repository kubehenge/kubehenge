package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func velero(kind, name string, labels map[string]string, fields map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: fields}
	o.SetAPIVersion("velero.io/v1")
	o.SetKind(kind)
	o.SetNamespace(provisioner.VeleroNamespace)
	o.SetName(name)
	o.SetLabels(labels)
	return o
}

// backingUp is a cluster that backs up, with the identities' Secrets beside it.
func backingUp(name string, expires time.Time) []client.Object {
	c := cluster("default", name)
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: "backups", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true,"target":"store"}`)},
	})
	ids := []provisioner.Identity{provisioner.IdentityBackups, provisioner.IdentityRestores}
	out := make([]client.Object, 0, 1+len(ids))
	out = append(out, c)
	for _, id := range ids {
		secret := provisioner.IdentitySecretName(name, id)
		out = append(out, &corev1.Secret{
			Namespace: "default", Name: secret,
			Annotations: map[string]string{provisioner.IdentityExpiresAnnotation: expires.UTC().Format(time.RFC3339)},
			Data: map[string][]byte{
				provisioner.IdentityServerKey: []byte("https://" + name + ".example:6443"), provisioner.IdentityCAKey: []byte("CA"),
				provisioner.IdentityTokenKey: []byte(secret + "-token"),
			},
		})
	}
	return out
}

// veleroCluster is what the fake Velero holds, and the tokens it was reached with.
type veleroCluster struct {
	client *dynamicfake.FakeDynamicClient
	tokens []string
}

func newVelero(t *testing.T, h *harness) *veleroCluster {
	t.Helper()
	gvrs := map[schema.GroupVersionResource]string{
		veleroBackups: "BackupList", veleroRestores: "RestoreList", veleroSchedule: "ScheduleList",
		veleroLocation: "BackupStorageLocationList", veleroVolumes: "PodVolumeRestoreList",
	}
	objects := []runtime.Object{
		velero("BackupStorageLocation", "default", nil, map[string]any{"status": map[string]any{"phase": "Available"}}),
		velero("Schedule", provisioner.VeleroSchedule, nil, map[string]any{
			"spec":   map[string]any{"schedule": "CRON_TZ=UTC 0 2 * * *", "template": map[string]any{"ttl": "168h0m0s", "excludedNamespaces": []any{"kube-system"}}},
			"status": map[string]any{"lastBackup": "2026-10-13T02:00:00Z"},
		}),
		velero("Backup", "velero-scheduled-20261012020000", map[string]string{"velero.io/schedule-name": provisioner.VeleroSchedule}, map[string]any{
			"status": map[string]any{
				"phase": "Completed", "startTimestamp": "2026-10-12T02:00:00Z", "expiration": "2026-10-19T02:00:00Z",
				"progress": map[string]any{"itemsBackedUp": int64(120), "totalItems": int64(120)},
			},
		}),
		velero("Backup", "velero-scheduled-20261013020000", map[string]string{"velero.io/schedule-name": provisioner.VeleroSchedule}, map[string]any{
			"status": map[string]any{"phase": "InProgress", "startTimestamp": "2026-10-13T02:00:00Z"},
		}),
		velero("Restore", "velero-scheduled-20261012020000-a", nil, map[string]any{
			"spec":   map[string]any{"backupName": "velero-scheduled-20261012020000", "includedNamespaces": []any{"shop"}},
			"status": map[string]any{"phase": "InProgress", "startTimestamp": "2026-10-13T08:00:00Z"},
		}),
		velero("PodVolumeRestore", "pvr-1", map[string]string{"velero.io/restore-name": "velero-scheduled-20261012020000-a"}, map[string]any{
			"status": map[string]any{"phase": "InProgress", "progress": map[string]any{"bytesDone": int64(512), "totalBytes": int64(2048)}},
		}),
	}
	v := &veleroCluster{client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrs, objects...)}
	v.client.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		o, ok := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if ok && o.GetName() == "" {
			o.SetName(o.GetGenerateName() + "made")
		}
		return false, nil, nil
	})
	h.server.ConnectWorkload = func(server string, ca []byte, token string) (*Workload, error) {
		if !strings.HasPrefix(server, "https://") || string(ca) != "CA" {
			t.Errorf("reached %s trusting %q", server, ca)
		}
		v.tokens = append(v.tokens, token)
		return &Workload{Dynamic: v.client}, nil
	}
	return v
}

func backupsHarness(t *testing.T) (*harness, *veleroCluster) {
	t.Helper()
	h := newHarness(t, backingUp("one", time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC))...)
	return h, newVelero(t, h)
}

func backupsViewOf(t *testing.T, body []byte) backupsView {
	t.Helper()
	var v backupsView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

func TestClusterBackupsView(t *testing.T) {
	t.Parallel()
	h, v := backupsHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	view := backupsViewOf(t, rec.Body.Bytes())
	if !view.Enabled || !view.Reachable || view.Location != "Available" || view.LastBackup == nil || !view.CanBackUp || !view.CanRestore {
		t.Fatalf("view %+v", view)
	}
	if len(view.Backups) != 2 || view.Backups[0].Phase != "InProgress" || !view.Backups[1].Scheduled || view.Backups[1].Items != 120 {
		t.Errorf("backups %+v", view.Backups)
	}
	if len(view.Restores) != 1 || view.Restores[0].Volumes != 1 || view.Restores[0].BytesDone != 512 || view.Restores[0].Namespaces[0] != "shop" {
		t.Errorf("restores %+v", view.Restores)
	}
	if len(v.tokens) != 1 || v.tokens[0] != "one-backups-token" {
		t.Errorf("read with %v, not the backups identity alone", v.tokens)
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Errorf("the view gives a token away: %s", rec.Body)
	}

	viewer := h.browser()
	viewer.signIn("viewer@pve")
	rec = viewer.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	if view := backupsViewOf(t, rec.Body.Bytes()); view.CanBackUp || view.CanRestore || !view.Reachable {
		t.Errorf("a viewer's view %+v", view)
	}
	expect(t, viewer.do("POST", "/api/clusters/default/one/backups", ""), http.StatusForbidden)
	expect(t, viewer.do("POST", "/api/clusters/default/one/restores", `{"backup":"velero-scheduled-20261012020000"}`), http.StatusForbidden)
}

func TestClusterBackupsWhileSettingUp(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: "backups", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true,"target":"store"}`)},
	})
	h := newHarness(t, c)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	if view := backupsViewOf(t, rec.Body.Bytes()); view.Reachable || !strings.Contains(view.Message, "access key") {
		t.Errorf("view without a credential %+v", view)
	}
	expect(t, b.do("POST", "/api/clusters/default/one/backups", ""), http.StatusConflict)

	shared := &corev1.Secret{Namespace: "default", Name: provisioner.CredentialName("", "backup")}
	if err := h.kube.Create(t.Context(), shared); err != nil {
		t.Fatal(err)
	}
	rec = b.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	if view := backupsViewOf(t, rec.Body.Bytes()); view.Reachable || !strings.Contains(view.Message, "being set up") {
		t.Errorf("view with the shared credential %+v", view)
	}

	off := newHarness(t, cluster("default", "one"))
	ob := off.browser()
	ob.signIn("dev@pve")
	rec = ob.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	if view := backupsViewOf(t, rec.Body.Bytes()); view.Enabled {
		t.Errorf("view of a cluster without backups %+v", view)
	}
}

func TestClusterBackupsOnceTheTokenRanOut(t *testing.T) {
	t.Parallel()
	h, v := backupsHarness(t)
	h.clock = h.clock.Add(2 * time.Hour)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	if view := backupsViewOf(t, rec.Body.Bytes()); view.Reachable || !strings.Contains(view.Message, "run out") || len(v.tokens) != 0 {
		t.Errorf("view %+v, reached with %v", view, v.tokens)
	}
	expect(t, b.do("POST", "/api/clusters/default/one/backups", ""), http.StatusConflict)
}

func TestBackUpNow(t *testing.T) {
	t.Parallel()
	h, v := backupsHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("POST", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusCreated)
	made, err := v.client.Resource(veleroBackups).Namespace(provisioner.VeleroNamespace).Get(t.Context(), "kubehenge-made", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if made.GetAnnotations()[requestedBy] != "dev@pve" {
		t.Errorf("annotations %v", made.GetAnnotations())
	}
	if ttl, _, _ := unstructured.NestedString(made.Object, "spec", "ttl"); ttl != "168h0m0s" {
		t.Errorf("not the schedule's template: %v", made.Object["spec"])
	}
}

func TestRestore(t *testing.T) {
	t.Parallel()
	h, v := backupsHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	for body, status := range map[string]int{
		`{"backup":"velero-scheduled-20261013020000"}`: http.StatusConflict,
		`{"backup":"nothing"}`:                         http.StatusNotFound,
		`{"backup":""}`:                                http.StatusUnprocessableEntity,
		`{"backup":"velero-scheduled-20261012020000","namespaces":["Shop"]}`:        http.StatusUnprocessableEntity,
		`{"backup":"velero-scheduled-20261012020000","namespaces":["shop","shop"]}`: http.StatusUnprocessableEntity,
	} {
		expect(t, b.do("POST", "/api/clusters/default/one/restores", body), status)
	}
	rec := b.do("POST", "/api/clusters/default/one/restores", `{"backup":"velero-scheduled-20261012020000","namespaces":["shop","web"]}`)
	expect(t, rec, http.StatusCreated)
	made, err := v.client.Resource(veleroRestores).Namespace(provisioner.VeleroNamespace).Get(t.Context(), "velero-scheduled-20261012020000-made", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	namespaces, _, _ := unstructured.NestedStringSlice(made.Object, "spec", "includedNamespaces")
	if strings.Join(namespaces, ",") != "shop,web" || made.GetAnnotations()[requestedBy] != "dev@pve" {
		t.Errorf("restore %v", made.Object)
	}
	if v.tokens[len(v.tokens)-1] != "one-restores-token" {
		t.Errorf("restored with %v, not the restores identity", v.tokens)
	}
}
