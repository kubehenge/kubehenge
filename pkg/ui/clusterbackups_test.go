package ui

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"slices"
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

// backingUp is the cluster one, which backs up, with the identities' Secrets beside it.
func backingUp(expires time.Time) []client.Object {
	const name = "one"
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

// veleroKinds are the lists of Velero's resources the interface reads.
var veleroKinds = map[schema.GroupVersionResource]string{
	veleroBackups: "BackupList", veleroRestores: "RestoreList", veleroSchedule: "ScheduleList",
	veleroLocation: "BackupStorageLocationList", veleroVolumes: "PodVolumeRestoreList",
	veleroDeletions: "DeleteBackupRequestList", veleroDownloads: "DownloadRequestList",
}

// location is a backup location; source names the cluster whose backups it holds, for one of
// another cluster's.
func location(name, source string, volumeData bool) *unstructured.Unstructured {
	l := velero("BackupStorageLocation", name, nil, map[string]any{"status": map[string]any{"phase": "Available"}})
	if source != "" {
		l.SetAnnotations(map[string]string{
			provisioner.BackupSourceAnnotation: source, provisioner.VolumeDataAnnotation: map[bool]string{true: "true", false: "false"}[volumeData],
		})
	}
	return l
}

// backupAt is a backup in a location, started at a time and in a phase.
func backupAt(name, where, phase, started string) *unstructured.Unstructured {
	return velero("Backup", name, nil, map[string]any{
		"spec":   map[string]any{"storageLocation": where},
		"status": map[string]any{"phase": phase, "startTimestamp": started},
	})
}

// veleroObjects are what a cluster's Velero holds: its own location and shop's, its schedule, two
// backups of its own, one of shop's, one it was asked to delete in vain, and a restore under way.
func veleroObjects() []runtime.Object {
	return []runtime.Object{
		location("default", "", false),
		location("cluster-shop", "shop", false),
		velero("Schedule", provisioner.VeleroSchedule, nil, map[string]any{
			"metadata": map[string]any{"creationTimestamp": "2026-10-01T00:00:00Z"},
			"spec":     map[string]any{"schedule": "CRON_TZ=UTC 0 2 * * *", "template": map[string]any{"ttl": "168h0m0s", "excludedNamespaces": []any{"kube-system"}}},
			"status":   map[string]any{"lastBackup": "2026-10-13T02:00:00Z"},
		}),
		velero("Backup", "velero-scheduled-20261012020000", map[string]string{"velero.io/schedule-name": provisioner.VeleroSchedule}, map[string]any{
			"spec": map[string]any{"storageLocation": "default"},
			"status": map[string]any{
				"phase": "Completed", "startTimestamp": "2026-10-12T02:00:00Z", "expiration": "2026-10-19T02:00:00Z",
				"progress": map[string]any{"itemsBackedUp": int64(120), "totalItems": int64(120)},
			},
		}),
		velero("Backup", "velero-scheduled-20261013020000", map[string]string{"velero.io/schedule-name": provisioner.VeleroSchedule}, map[string]any{
			"spec":   map[string]any{"storageLocation": "default"},
			"status": map[string]any{"phase": "InProgress", "startTimestamp": "2026-10-13T02:00:00Z"},
		}),
		backupAt("shop-20261011", "cluster-shop", "Completed", "2026-10-11T02:00:00Z"),
		backupAt("kept", "default", "Completed", "2026-10-10T02:00:00Z"),
		velero("DeleteBackupRequest", "kept-x", nil, map[string]any{
			"metadata": map[string]any{"creationTimestamp": "2026-10-12T10:00:00Z"},
			"spec":     map[string]any{"backupName": "kept"},
			"status":   map[string]any{"phase": "Processed", "errors": []any{"cannot reach the store"}},
		}),
		velero("Restore", "velero-scheduled-20261012020000-a", nil, map[string]any{
			"spec":   map[string]any{"backupName": "velero-scheduled-20261012020000", "includedNamespaces": []any{"shop"}},
			"status": map[string]any{"phase": "InProgress", "startTimestamp": "2026-10-13T08:00:00Z"},
		}),
		velero("PodVolumeRestore", "pvr-1", map[string]string{"velero.io/restore-name": "velero-scheduled-20261012020000-a"}, map[string]any{
			"status": map[string]any{"phase": "InProgress", "progress": map[string]any{"bytesDone": int64(512), "totalBytes": int64(2048)}},
		}),
	}
}

func newVelero(t *testing.T, h *harness, objects ...runtime.Object) *veleroCluster {
	t.Helper()
	if len(objects) == 0 {
		objects = veleroObjects()
	}
	v := &veleroCluster{client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), veleroKinds, objects...)}
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
	h := newHarness(t, backingUp(time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC))...)
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

func backupNamed(t *testing.T, view *backupsView, name string) backupView {
	t.Helper()
	i := slices.IndexFunc(view.Backups, func(b backupView) bool { return b.Name == name })
	if i < 0 {
		t.Fatalf("no backup %s in %+v", name, view.Backups)
	}
	return view.Backups[i]
}

func TestClusterBackupsView(t *testing.T) {
	t.Parallel()
	h, v := backupsHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	view := backupsViewOf(t, rec.Body.Bytes())
	if !view.Enabled || !view.Reachable || view.Location != "Available" || view.LastBackup == nil || !view.CanBackUp || !view.CanDelete || !view.CanRestore {
		t.Fatalf("view %+v", view)
	}
	checkBackupsListed(t, &view)
	checkSourcesAndRestores(t, &view)
	if len(view.Attention) != 0 {
		t.Errorf("a cluster backed up yesterday needs attention: %+v", view.Attention)
	}
	if len(v.tokens) != 1 || v.tokens[0] != "one-backups-token" {
		t.Errorf("read with %v, not the backups identity alone", v.tokens)
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Errorf("the view gives a token away: %s", rec.Body)
	}
}

// checkBackupsListed checks the backups of veleroObjects in a view: newest first, shop's told
// apart, and a deletion Velero failed at shown on the backup.
func checkBackupsListed(t *testing.T, view *backupsView) {
	t.Helper()
	if len(view.Backups) != 4 || view.Backups[0].Phase != "InProgress" || !view.Backups[1].Scheduled || view.Backups[1].Items != 120 {
		t.Errorf("backups %+v", view.Backups)
	}
	if shop := backupNamed(t, view, "shop-20261011"); shop.From != "shop" {
		t.Errorf("shop's backup %+v", shop)
	}
	if kept := backupNamed(t, view, "kept"); kept.Deleting || len(kept.DeleteErrors) != 1 || kept.From != "" {
		t.Errorf("a backup Velero did not delete %+v", kept)
	}
}

// checkSourcesAndRestores checks shop's location and the restore under way of veleroObjects in a
// view.
func checkSourcesAndRestores(t *testing.T, view *backupsView) {
	t.Helper()
	if len(view.Sources) != 1 || view.Sources[0].Cluster != "shop" || view.Sources[0].Phase != "Available" || view.Sources[0].VolumeData {
		t.Errorf("sources %+v", view.Sources)
	}
	if len(view.Restores) != 1 || view.Restores[0].Volumes != 1 || view.Restores[0].BytesDone != 512 || view.Restores[0].Namespaces[0] != "shop" || !view.Restores[0].VolumeData {
		t.Errorf("restores %+v", view.Restores)
	}
}

func TestClusterBackupsForAViewer(t *testing.T) {
	t.Parallel()
	h, _ := backupsHarness(t)
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	rec := viewer.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	if view := backupsViewOf(t, rec.Body.Bytes()); view.CanBackUp || view.CanDelete || view.CanRestore || !view.Reachable {
		t.Errorf("a viewer's view %+v", view)
	}
	expect(t, viewer.do("POST", "/api/clusters/default/one/backups", ""), http.StatusForbidden)
	expect(t, viewer.do("POST", "/api/clusters/default/one/restores", `{"backup":"velero-scheduled-20261012020000"}`), http.StatusForbidden)
	expect(t, viewer.do("DELETE", "/api/clusters/default/one/backups/kept", `{"confirm":"kept"}`), http.StatusForbidden)
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
	if view := backupsViewOf(t, rec.Body.Bytes()); view.Reachable || !strings.Contains(view.Message, "own access key") {
		t.Errorf("view with only the shared credential %+v", view)
	}
	own := &corev1.Secret{Namespace: "default", Name: provisioner.ClusterBackupCredentialName("one")}
	if err := h.kube.Create(t.Context(), own); err != nil {
		t.Fatal(err)
	}
	rec = b.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	if view := backupsViewOf(t, rec.Body.Bytes()); view.Reachable || !strings.Contains(view.Message, "Setting up Velero") {
		t.Errorf("view with the cluster's own credential %+v", view)
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
	if view := backupsViewOf(t, rec.Body.Bytes()); view.Reachable || !strings.Contains(view.Message, "expired") || len(v.tokens) != 0 {
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

func TestDeleteBackup(t *testing.T) {
	t.Parallel()
	h, v := backupsHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	for path, tc := range map[string]struct {
		body   string
		status int
		says   string
	}{
		"kept":                            {`{"confirm":"kep"}`, http.StatusUnprocessableEntity, "type the backup's name"},
		"velero-scheduled-20261013020000": {`{"confirm":"velero-scheduled-20261013020000"}`, http.StatusConflict, "still running"},
		"shop-20261011":                   {`{"confirm":"shop-20261011"}`, http.StatusConflict, "another cluster's"},
		"nothing":                         {`{"confirm":"nothing"}`, http.StatusNotFound, "no such backup"},
		"Bad_Name":                        {`{"confirm":"Bad_Name"}`, http.StatusNotFound, "no such backup"},
	} {
		rec := b.do("DELETE", "/api/clusters/default/one/backups/"+path, tc.body)
		expect(t, rec, tc.status)
		if !strings.Contains(rec.Body.String(), tc.says) {
			t.Errorf("deleting %s: %s, want it to say %q", path, rec.Body, tc.says)
		}
	}
	rec := b.do("DELETE", "/api/clusters/default/one/backups/kept", `{"confirm":"kept"}`)
	expect(t, rec, http.StatusAccepted)
	var answered backupView
	if err := json.Unmarshal(rec.Body.Bytes(), &answered); err != nil || !answered.Deleting || answered.DeleteErrors != nil {
		t.Errorf("answered %s: %v", rec.Body, err)
	}
	made, err := v.client.Resource(veleroDeletions).Namespace(provisioner.VeleroNamespace).Get(t.Context(), "kept-made", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if name, _, _ := unstructured.NestedString(made.Object, "spec", "backupName"); name != "kept" || made.GetLabels()[veleroBackupName] != "kept" || made.GetAnnotations()[requestedBy] != "dev@pve" {
		t.Errorf("deletion %v", made.Object)
	}
	if v.tokens[len(v.tokens)-1] != "one-backups-token" {
		t.Errorf("deleted with %v, not the backups identity", v.tokens)
	}

	// Asked once, it shows as being deleted until Velero is done, and is not asked for again.
	rec = b.do("GET", "/api/clusters/default/one/backups", "")
	expect(t, rec, http.StatusOK)
	after := backupsViewOf(t, rec.Body.Bytes())
	if kept := backupNamed(t, &after, "kept"); !kept.Deleting || len(kept.DeleteErrors) != 0 {
		t.Errorf("a backup being deleted %+v", kept)
	}
	expect(t, b.do("DELETE", "/api/clusters/default/one/backups/kept", `{"confirm":"kept"}`), http.StatusConflict)
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
		`{"backup":"velero-scheduled-20261012020000","namespaces":["Shop"]}`:                                   http.StatusUnprocessableEntity,
		`{"backup":"velero-scheduled-20261012020000","namespaces":["shop","shop"]}`:                            http.StatusUnprocessableEntity,
		`{"backup":"velero-scheduled-20261012020000","namespaces":["shop"],"excludedNamespaces":["shop"]}`:     http.StatusUnprocessableEntity,
		`{"backup":"velero-scheduled-20261012020000","resources":["Deployments"]}`:                             http.StatusUnprocessableEntity,
		`{"backup":"velero-scheduled-20261012020000","resources":["pods"],"volumeData":false}`:                 http.StatusUnprocessableEntity,
		`{"backup":"velero-scheduled-20261012020000","resources":["secrets"],"excludedResources":["secrets"]}`: http.StatusUnprocessableEntity,
	} {
		expect(t, b.do("POST", "/api/clusters/default/one/restores", body), status)
	}
	rec := b.do("POST", "/api/clusters/default/one/restores", `{"backup":"velero-scheduled-20261012020000","namespaces":["shop","web"]}`)
	expect(t, rec, http.StatusCreated)
	restores := v.client.Resource(veleroRestores).Namespace(provisioner.VeleroNamespace)
	made, err := restores.Get(t.Context(), "velero-scheduled-20261012020000-made", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	namespaces, _, _ := unstructured.NestedStringSlice(made.Object, "spec", "includedNamespaces")
	restorePVs, _, _ := unstructured.NestedBool(made.Object, "spec", "restorePVs")
	if strings.Join(namespaces, ",") != "shop,web" || !restorePVs || made.GetAnnotations()[requestedBy] != "dev@pve" {
		t.Errorf("restore %v", made.Object)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(made.Object, "spec", "excludedResources"); found {
		t.Errorf("a whole restore leaves resources out: %v", made.Object["spec"])
	}
	if v.tokens[len(v.tokens)-1] != "one-restores-token" {
		t.Errorf("restored with %v, not the restores identity", v.tokens)
	}
	if err = restores.Delete(t.Context(), made.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	// Chosen in part and without the volumes' data: the pods stay out for their controllers.
	rec = b.do("POST", "/api/clusters/default/one/restores", `{"backup":"velero-scheduled-20261012020000",`+
		`"excludedNamespaces":["cache"],"resources":["deployments.apps","persistentvolumeclaims"],"excludedResources":["secrets"],"volumeData":false}`)
	expect(t, rec, http.StatusCreated)
	made, err = restores.Get(t.Context(), "velero-scheduled-20261012020000-made", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(made.Object, "spec")
	got, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"backupName":"velero-scheduled-20261012020000","excludedNamespaces":["cache"],`+
		`"excludedResources":["secrets","pods"],"includedNamespaces":["*"],"includedResources":["deployments.apps","persistentvolumeclaims"],"restorePVs":false}` {
		t.Errorf("restore %s", got)
	}
	if x := restoreOf(made, nil); x.VolumeData || len(x.Resources) != 2 || len(x.ExcludedNamespaces) != 1 {
		t.Errorf("as listed %+v", x)
	}
}

// Another cluster's backup comes back only for those who see that cluster, and without the volumes'
// data where the cluster's Velero cannot read it.
func TestRestoreAnotherClustersBackup(t *testing.T) {
	t.Parallel()
	h, _ := backupsHarness(t)
	h.accounts.passwords["ops@pve"] = "secret"
	h.accounts.grants["ops@pve"] = map[string][]string{"/pool/kubehenge/default.one": {"VM.Audit", "Sys.Modify"}}
	ops := h.browser()
	ops.signIn("ops@pve")
	rec := ops.do("POST", "/api/clusters/default/one/restores", `{"backup":"shop-20261011","volumeData":false}`)
	expect(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "VM.Audit on /pool/kubehenge/default.shop") {
		t.Errorf("refused saying %s", rec.Body)
	}

	b := h.browser()
	b.signIn("dev@pve")
	rec = b.do("POST", "/api/clusters/default/one/restores", `{"backup":"shop-20261011"}`)
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "volumes' data of shop's backups") {
		t.Errorf("refused saying %s", rec.Body)
	}
	rec = b.do("POST", "/api/clusters/default/one/restores", `{"backup":"shop-20261011","namespaces":["shop"],"volumeData":false}`)
	expect(t, rec, http.StatusCreated)
	var x restoreView
	if err := json.Unmarshal(rec.Body.Bytes(), &x); err != nil || x.From != "shop" || x.VolumeData {
		t.Errorf("restore %s: %v", rec.Body, err)
	}

	shared := newHarness(t, backingUp(time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC))...)
	objects := veleroObjects()
	objects[1] = location("cluster-shop", "shop", true)
	newVelero(t, shared, objects...)
	sb := shared.browser()
	sb.signIn("dev@pve")
	expect(t, sb.do("POST", "/api/clusters/default/one/restores", `{"backup":"shop-20261011"}`), http.StatusCreated)

	foreign := newHarness(t, backingUp(time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC))...)
	objects = veleroObjects()
	objects[1] = location("cluster-shop", "", false)
	newVelero(t, foreign, objects...)
	fb := foreign.browser()
	fb.signIn("dev@pve")
	rec = fb.do("POST", "/api/clusters/default/one/restores", `{"backup":"shop-20261011","volumeData":false}`)
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "did not set up") {
		t.Errorf("a location of someone else's: %s", rec.Body)
	}
}

// resultsStore is a target that holds a restore's results, encrypted with key, and the class that
// names it.
func resultsStore(t *testing.T, h *harness, key, results string) {
	t.Helper()
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Key") != base64.StdEncoding.EncodeToString([]byte(key)) {
			http.Error(w, "wrong key", http.StatusBadRequest)
			return
		}
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(results))
		_ = zw.Close()
	}))
	t.Cleanup(store.Close)
	class := &clusterv1.ClusterClass{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	ca, err := json.Marshal(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: store.Certificate().Raw})))
	if err != nil {
		t.Fatal(err)
	}
	class.Annotations[provisioner.BackupTargetsAnnotation] = `[{"name":"store","endpoint":"` + store.URL + `","bucket":"backups","caCertificate":` + string(ca) + `}]`
	if err = h.kube.Update(t.Context(), class); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "shop"} {
		keys := &corev1.Secret{Namespace: "default", Name: provisioner.BackupKeysSecretName(name), Data: map[string][]byte{
			provisioner.BackupKeysKeySSEC: []byte(strings.Repeat(name[:1], 32)),
		}}
		if err = h.kube.Create(t.Context(), keys); err != nil {
			t.Fatal(err)
		}
	}
	link := store.URL + "/backups/default.shop/velero/restores/x/restore-x-results.gz"
	h.server.ConnectWorkload = wrapDownloads(h.server.ConnectWorkload, link)
}

// wrapDownloads has the fake Velero sign every DownloadRequest at once with link.
func wrapDownloads(connect func(string, []byte, string) (*Workload, error), link string) func(string, []byte, string) (*Workload, error) {
	return func(server string, ca []byte, token string) (*Workload, error) {
		w, err := connect(server, ca, token)
		if err != nil {
			return nil, err
		}
		if fake, ok := w.Dynamic.(*dynamicfake.FakeDynamicClient); ok {
			fake.PrependReactor("create", "downloadrequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
				o, ok := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
				if ok {
					_ = unstructured.SetNestedMap(o.Object, map[string]any{"phase": "Processed", "downloadURL": link}, "status")
				}
				return false, nil, nil
			})
		}
		return w, nil
	}
}

func TestRestoreResults(t *testing.T) {
	t.Parallel()
	h, v := backupsHarness(t)
	finished := velero("Restore", "shop-20261011-a", nil, map[string]any{
		"spec":   map[string]any{"backupName": "shop-20261011"},
		"status": map[string]any{"phase": "PartiallyFailed", "startTimestamp": "2026-10-13T08:00:00Z", "warnings": int64(3), "errors": int64(1)},
	})
	if _, err := v.client.Resource(veleroRestores).Namespace(provisioner.VeleroNamespace).Create(t.Context(), finished, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// shop's backups are in its folder, encrypted with its key: its results are too.
	resultsStore(t, h, strings.Repeat("s", 32), `{"warnings":{"velero":["slow"],"namespaces":{"web":["service web already exists"],"api":["a","b"]}},`+
		`"errors":{"cluster":["could not restore crd widgets"]}}`)
	b := h.browser()
	b.signIn("viewer@pve")
	rec := b.do("GET", "/api/clusters/default/one/restores/shop-20261011-a/results", "")
	expect(t, rec, http.StatusOK)
	var got restoreResults
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings.Namespaces) != 2 || got.Warnings.Namespaces[0].Namespace != "api" || got.Warnings.Velero[0] != "slow" ||
		got.Errors.Cluster[0] != "could not restore crd widgets" || len(got.Errors.Namespaces) != 0 || got.Warnings.More != 0 {
		t.Errorf("results %s", rec.Body)
	}
	expect(t, b.do("GET", "/api/clusters/default/one/restores/velero-scheduled-20261012020000-a/results", ""), http.StatusConflict)
	expect(t, b.do("GET", "/api/clusters/default/one/restores/nothing/results", ""), http.StatusNotFound)
}

func TestResultsAreBounded(t *testing.T) {
	t.Parallel()
	many := make([]string, maxResultMessages+20)
	for i := range many {
		many[i] = "already exists"
	}
	v := resultsOf(veleroResult{Velero: []string{"x"}, Namespaces: map[string][]string{"a": many, "b": {"y"}}})
	// One from Velero and 499 of a's carried; 21 of a's and b's one left over.
	if v.More != 22 || len(v.Velero) != 1 || len(v.Namespaces) != 1 || len(v.Namespaces[0].Messages) != maxResultMessages-1 {
		t.Errorf("results %d velero, %d namespaces, %d more", len(v.Velero), len(v.Namespaces), v.More)
	}
	if empty := resultsOf(veleroResult{}); empty.Velero == nil || empty.Cluster == nil || empty.Namespaces == nil {
		t.Errorf("empty results carry nulls: %+v", empty)
	}
}

// A cluster may read another's backups only for those who could read that cluster's Secrets.
func TestReadingAnotherClustersBackups(t *testing.T) {
	t.Parallel()
	objects := backingUp(time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC))
	objects = append(objects, cluster("default", "shop"))
	h := newHarness(t, objects...)
	b := h.browser()
	b.signIn("dev@pve")
	change := `{"variables":{"backups":{"enabled":true,"target":"store","readFrom":["shop"]}}}`
	rec := b.do("PATCH", "/api/clusters/default/one", change)
	expect(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "Sys.Modify on /pool/kubehenge/default.shop") {
		t.Errorf("refused saying %s", rec.Body)
	}
	h.accounts.grants["dev@pve"]["/pool/kubehenge/default.shop"] = []string{"Sys.Modify"}
	expect(t, b.do("PATCH", "/api/clusters/default/one", change), http.StatusOK)
	// Keeping a cluster it reads takes no right on that cluster.
	delete(h.accounts.grants["dev@pve"], "/pool/kubehenge/default.shop")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"backups":{"enabled":true,"target":"store","schedule":"0 3 * * *","readFrom":["shop"]}}}`), http.StatusOK)
}

func TestBackupsAttention(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	at := func(s string) *time.Time { t, _ := time.Parse(time.RFC3339, s); return &t }
	schedule := velero("Schedule", provisioner.VeleroSchedule, nil, map[string]any{
		"metadata": map[string]any{"creationTimestamp": "2026-10-01T00:00:00Z"},
		"spec":     map[string]any{"schedule": "CRON_TZ=UTC 0 2 * * *"},
	})
	fresh := velero("Schedule", provisioner.VeleroSchedule, nil, map[string]any{
		"metadata": map[string]any{"creationTimestamp": "2026-10-12T10:00:00Z"},
		"spec":     map[string]any{"schedule": "CRON_TZ=UTC 0 2 * * *"},
	})
	weekly := velero("Schedule", provisioner.VeleroSchedule, nil, map[string]any{
		"metadata": map[string]any{"creationTimestamp": "2026-09-01T00:00:00Z"},
		"spec":     map[string]any{"schedule": "CRON_TZ=Europe/Zurich 0 2 * * 1"},
	})
	for _, tc := range []struct {
		name     string
		schedule *unstructured.Unstructured
		backups  []backupView
		want     []string
	}{
		{"backed up last night", schedule, []backupView{{Name: "a", Phase: "Completed", Started: at("2026-10-13T02:00:00Z")}}, nil},
		{"missed one night", schedule, []backupView{{Name: "a", Phase: "Completed", Started: at("2026-10-12T02:00:00Z")}}, nil},
		{"missed two nights", schedule, []backupView{{Name: "a", Phase: "Completed", Started: at("2026-10-11T02:00:00Z")}}, []string{"warning: Backups overdue"}},
		{"none ever", schedule, nil, []string{"warning: Backups overdue"}},
		{"none yet", fresh, nil, nil},
		{"weekly, last Monday", weekly, []backupView{{Name: "a", Phase: "Completed", Started: at("2026-10-05T00:00:00Z")}}, nil},
		{"last failed", schedule, []backupView{
			{Name: "b", Phase: "Failed", Started: at("2026-10-13T02:00:00Z"), Failure: "the store refused"},
			{Name: "a", Phase: "Completed", Started: at("2026-10-12T02:00:00Z")},
		}, []string{"error: Last backup failed"}},
		{"last partly failed", schedule, []backupView{
			{Name: "b", Phase: "PartiallyFailed", Started: at("2026-10-13T02:00:00Z"), Errors: 2},
			{Name: "a", Phase: "Completed", Started: at("2026-10-11T02:00:00Z")},
		}, []string{"warning: Last backup partly failed", "warning: Backups overdue"}},
		{"another cluster's", schedule, []backupView{
			{Name: "c", Phase: "Failed", Started: at("2026-10-13T03:00:00Z"), From: "shop"},
			{Name: "a", Phase: "Completed", Started: at("2026-10-13T02:00:00Z")},
		}, nil},
		{"running now", schedule, []backupView{
			{Name: "b", Phase: "InProgress", Started: at("2026-10-13T02:00:00Z")},
			{Name: "a", Phase: "Completed", Started: at("2026-10-12T02:00:00Z")},
		}, nil},
		{"no schedule", nil, []backupView{{Name: "b", Phase: "Failed", Started: at("2026-10-13T02:00:00Z")}}, []string{"error: Last backup failed"}},
	} {
		var got []string
		for _, a := range backupsAttention(tc.schedule, tc.backups, now) {
			if a.Section != sectionBackups || a.Message == "" {
				t.Errorf("%s: %+v", tc.name, a)
			}
			got = append(got, a.Severity+": "+a.What)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
