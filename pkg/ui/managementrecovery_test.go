package ui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/notify"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const exportSecret = "an export passphrase of its own"

// countingStore counts what is written to the bucket.
type countingStore struct {
	*uiStore

	puts atomic.Int32
}

func (s *countingStore) Put(ctx context.Context, key string, r io.Reader, metadata map[string]string) (int64, error) {
	s.puts.Add(1)
	return s.uiStore.Put(ctx, key, r, metadata)
}

// managementWithTarget is a management cluster whose settings name a backup target, and the bucket.
func managementWithTarget(t *testing.T, h *harness) (*appliance.Appliance, *managementTalos, *countingStore) {
	t.Helper()
	a := withManagementCluster(t, h)
	talos := &managementTalos{}
	store := &countingStore{uiStore: &uiStore{objects: map[string]provisioner.StoredObject{}, data: map[string][]byte{}}}
	a.Talos = talos
	a.OpenStore = func(*provisioner.BackupTarget, map[string][]byte) (provisioner.ObjectStore, error) { return store, nil }
	saved := a.SavedSettings()
	saved.BackupTargets = []appliance.BackupTarget{{Name: "minio", Endpoint: "https://minio.lab.internal", Bucket: "backups"}}
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Dir, "management", "settings.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return a, talos, store
}

func setUpBackups(t *testing.T, b *browser) {
	t.Helper()
	expect(t, b.do("PUT", "/api/appliance/etcd-backups", `{"target":"minio","everyHours":6,"keep":3,"accessKeyID":"AKID","secretAccessKey":"secret","passphrase":"`+statePassphrase+`"}`), http.StatusOK)
}

func TestBackupWarningStandsAndIsDismissedWithWhoAndWhen(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	managementWithTarget(t, h)
	admin := h.browser()
	admin.signIn("admin@pve")
	dev := h.browser()
	dev.signIn("dev@pve")

	v := managementBackupsOf(t, admin)
	if !v.Warning.Shown() {
		t.Fatalf("backups are off, yet the warning does not stand: %+v", v.Warning)
	}
	expect(t, dev.do("POST", "/api/appliance/etcd-backups/warning/dismissal", ""), http.StatusForbidden)
	if !managementBackupsOf(t, admin).Warning.Shown() {
		t.Fatal("a user without the privilege silenced the warning")
	}
	rec := admin.do("POST", "/api/appliance/etcd-backups/warning/dismissal", "")
	expect(t, rec, http.StatusOK)
	d := managementBackupsOf(t, admin).Warning.Dismissal
	if d == nil || d.By != "admin@pve" || d.Until.Sub(d.At) != appliance.EtcdBackupWarningSnooze || time.Since(d.At) > time.Minute {
		t.Fatalf("dismissal %+v", d)
	}
	setUpBackups(t, admin)
	expect(t, admin.do("POST", "/api/appliance/etcd-backups/warning/dismissal", ""), http.StatusConflict)
	if managementBackupsOf(t, admin).Warning != nil {
		t.Fatal("the warning stands with backups on")
	}
}

func TestNotificationsRepeatTheBackupWarningWeekly(t *testing.T) {
	t.Parallel()
	st := &notifyState{}
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	found := func() *notifyFound {
		return &notifyFound{
			read: map[string]bool{}, listed: true,
			problems: []notify.Notice{{Key: notifyEtcdBackupsKey, Severity: notify.Warning, Title: "not backed up"}, {Key: "management/node/a", Severity: notify.Error, Title: "node"}},
		}
	}
	told := func(at time.Time) map[string]int {
		out := map[string]int{}
		for _, n := range st.step(at, found()) {
			out[n.Key]++
		}
		return out
	}
	told(now)
	if got := told(now.Add(notifyHold)); got[notifyEtcdBackupsKey] != 1 || got["management/node/a"] != 1 {
		t.Fatalf("first telling: %v", got)
	}
	if got := told(now.Add(3 * 24 * time.Hour)); len(got) != 0 {
		t.Fatalf("told again after 3 days: %v", got)
	}
	got := told(now.Add(notifyHold + notifyRepeatEvery))
	if got[notifyEtcdBackupsKey] != 1 || got["management/node/a"] != 0 {
		t.Fatalf("after a week: %v, want only the backup warning again", got)
	}
}

func TestExportAndImportOverTheInterface(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, _, store := managementWithTarget(t, h)
	admin := h.browser()
	admin.signIn("admin@pve")
	body := func(state, export string) string {
		return `{"passphrase":"` + state + `","exportPassphrase":"` + export + `"}`
	}
	expect(t, admin.do("POST", "/api/appliance/export", body(statePassphrase, exportSecret)), http.StatusConflict)
	setUpBackups(t, admin)
	expect(t, admin.do("POST", "/api/appliance/etcd-backups/snapshots", ""), http.StatusAccepted)
	var snapshot string
	for deadline := time.Now().Add(10 * time.Second); snapshot == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no snapshot was taken")
		}
		if v := managementBackupsOf(t, admin); !v.Taking && v.Last != nil {
			snapshot = v.Last.Name
			if !v.ExportDue {
				t.Fatal("no export is due after the first snapshot")
			}
		}
	}
	expect(t, admin.do("POST", "/api/appliance/export", body("a wrong passphrase, long enough", exportSecret)), http.StatusForbidden)
	expect(t, admin.do("POST", "/api/appliance/export", body(statePassphrase, "short")), http.StatusUnprocessableEntity)
	rec := admin.do("POST", "/api/appliance/export", body(statePassphrase, exportSecret))
	expect(t, rec, http.StatusOK)
	bundle := rec.Body.Bytes()
	if strings.Contains(string(bundle), "AKID") {
		t.Fatal("the export holds the access key in the clear")
	}
	if managementBackupsOf(t, admin).ExportDue {
		t.Fatal("an export is still due after one was taken")
	}

	h2 := newHarness(t)
	talos := &managementTalos{}
	fresh := &appliance.Appliance{
		Dir: t.TempDir(), KeyDir: t.TempDir(), Tofu: fakeTofu, Log: slog.New(slog.DiscardHandler), KeyWorkFactor: 10, Talos: talos,
		OpenStore: func(*provisioner.BackupTarget, map[string][]byte) (provisioner.ObjectStore, error) { return store, nil },
	}
	h2.server.Appliance = fresh
	admin2 := h2.browser()
	admin2.signIn("admin@pve")
	send := func(file []byte, passphrase string) string {
		raw, err := json.Marshal(map[string]string{"bundle": base64.StdEncoding.EncodeToString(file), "exportPassphrase": passphrase})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	tampered := append([]byte(nil), bundle...)
	tampered[len(tampered)-10] ^= 0xff
	expect(t, admin2.do("POST", "/api/appliance/import", send(bundle, "not the export passphrase")), http.StatusForbidden)
	expect(t, admin2.do("POST", "/api/appliance/import", send(tampered, exportSecret)), http.StatusUnprocessableEntity)
	expect(t, admin2.do("POST", "/api/appliance/import", send([]byte("not an export"), exportSecret)), http.StatusUnprocessableEntity)
	if fresh.HasState() {
		t.Fatal("a refused import left a state")
	}
	expect(t, admin2.do("POST", "/api/appliance/import", send(bundle, exportSecret)), http.StatusOK)
	expect(t, admin2.do("POST", "/api/appliance/import", send(bundle, exportSecret)), http.StatusConflict)
	restore := "/api/appliance/etcd-backups/snapshots/" + snapshot + "/restore"
	expect(t, admin2.do("POST", restore, `{"passphrase":"`+statePassphrase+`"}`), http.StatusAccepted)
	if j := jobEnded(t, fresh); j.State != "succeeded" || !bytes.Equal(talos.restored, etcdStream()) {
		t.Fatalf("restore on the new appliance: %+v", j)
	}
}

func TestRecreateOverTheInterfaceChecksBeforeItStarts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// The run lasts as long as the renewed ticket, which the clock dates.
	h.clock = time.Now()
	a, talos, _ := managementWithTarget(t, h)
	admin := h.browser()
	admin.signIn("admin@pve")
	dev := h.browser()
	dev.signIn("dev@pve")
	setUpBackups(t, admin)
	taken, err := a.TakeEtcdSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/appliance/etcd-backups/snapshots/" + taken.Name + "/recreate"
	expect(t, dev.do("POST", path, `{"passphrase":"`+statePassphrase+`"}`), http.StatusForbidden)
	expect(t, admin.do("POST", path, `{"passphrase":"a wrong passphrase, long enough"}`), http.StatusForbidden)
	expect(t, admin.do("POST", "/api/appliance/etcd-backups/snapshots/..%2Fidentity.age/recreate", `{"passphrase":"`+statePassphrase+`"}`), http.StatusUnprocessableEntity)
	expect(t, admin.do("POST", path, `{"passphrase":"`+statePassphrase+`"}`), http.StatusAccepted)
	if j := jobEnded(t, a); j.State != "succeeded" || j.Kind != "recreate" || !bytes.Equal(talos.restored, etcdStream()) {
		t.Fatalf("recreate: %+v", j)
	}
}

func TestClusterSpecIsStoredOnChangeAndRecreatesTheCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, _, store := managementWithTarget(t, h)
	admin := h.browser()
	admin.signIn("admin@pve")
	dev := h.browser()
	dev.signIn("dev@pve")
	body := strings.Replace(createBody, `"workerReplicas":2,`, `"workerReplicas":2,"workerAutoscaling":{"min":1,"max":4},`, 1)
	expect(t, dev.do("POST", "/api/clusters", body), http.StatusCreated)
	ctx := t.Context()
	stored := map[string]string{}

	h.server.storeSpecsOnce(ctx, stored)
	if store.puts.Load() != 0 {
		t.Fatal("a spec was written with the backups off")
	}
	setUpBackups(t, admin)
	puts := store.puts.Load()
	h.server.storeSpecsOnce(ctx, stored)
	key := "clusters/default.web/cluster.yaml"
	first := string(store.data[key])
	if store.puts.Load() != puts+1 || !strings.Contains(first, "kind: Cluster") {
		t.Fatalf("after the first look: %d puts, spec %q", store.puts.Load()-puts, first)
	}
	h.server.storeSpecsOnce(ctx, stored)
	if store.puts.Load() != puts+1 {
		t.Fatal("an unchanged spec was written again")
	}
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.Topology.Version = "v1.37.9+rke2r1"
	if err := h.kube.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	h.server.storeSpecsOnce(ctx, stored)
	if store.puts.Load() != puts+2 || string(store.data[key]) == first || !strings.Contains(string(store.data[key]), "v1.37.9+rke2r1") {
		t.Fatalf("a changed cluster was not stored: %d puts", store.puts.Load()-puts)
	}

	// The spec is offered for download to those who may see the cluster, and recreates it.
	expect(t, h.browser().do("GET", "/api/clusters/default/web/spec", ""), http.StatusUnauthorized)
	rec := dev.do("GET", "/api/clusters/default/web/spec", "")
	expect(t, rec, http.StatusOK)
	spec := rec.Body.String()
	if spec != string(store.data[key]) {
		t.Fatalf("the download differs from the stored spec:\n%s", spec)
	}
	if err := h.kube.Delete(ctx, c); err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(map[string]string{"spec": spec})
	if err != nil {
		t.Fatal(err)
	}
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("POST", "/api/cluster-specs", string(request)), http.StatusForbidden)
	expect(t, dev.do("POST", "/api/cluster-specs", `{"spec":"kind: ConfigMap"}`), http.StatusUnprocessableEntity)
	expect(t, dev.do("POST", "/api/cluster-specs", string(request)), http.StatusCreated)
	again := &clusterv1.Cluster{}
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "web"}, again); err != nil {
		t.Fatal(err)
	}
	md := again.Spec.Topology.Workers.MachineDeployments[0]
	if again.Spec.Topology.Version != "v1.37.9+rke2r1" || md.Replicas != nil || md.Metadata.Annotations[provisioner.AutoscalerMaxAnnotation] != "4" {
		t.Fatalf("recreated %+v", again.Spec.Topology)
	}
}

func TestSpecAutoscalingTakesOnlySizesThatFitTheRequest(t *testing.T) {
	t.Parallel()
	spec := func(minimum, maximum string) *clusterv1.Cluster {
		c := &clusterv1.Cluster{}
		c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{}}
		c.Spec.Topology.Workers.MachineDeployments[0].Metadata.Annotations = map[string]string{
			provisioner.AutoscalerMinAnnotation: minimum, provisioner.AutoscalerMaxAnnotation: maximum,
		}
		return c
	}
	if got := createRequestOf(spec("1", "4")).WorkerAutoscaling; got == nil || got.Min != 1 || got.Max != 4 {
		t.Errorf("1 to 4 gives %+v", got)
	}
	for _, sizes := range [][2]string{{"1", "4294967297"}, {"-2147483649", "4"}, {"1", "many"}, {"", ""}} {
		if got := createRequestOf(spec(sizes[0], sizes[1])).WorkerAutoscaling; got != nil {
			t.Errorf("%s to %s gives %+v, want no autoscaling", sizes[0], sizes[1], got)
		}
	}
}
