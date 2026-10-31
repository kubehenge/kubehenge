package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// managementTalos answers the appliance's Talos calls: a snapshot that ends in etcd's checksum,
// and a restore it records.
type managementTalos struct {
	mu       sync.Mutex
	restored []byte
	release  chan struct{}
}

func etcdStream() []byte {
	db := []byte("management etcd")
	sum := sha256.Sum256(db)
	return append(db, sum[:]...)
}

func (*managementTalos) BackupConfig(context.Context, []byte, string, time.Duration) ([]byte, error) {
	return []byte("backup talosconfig"), nil
}

func (m *managementTalos) Snapshot(context.Context, []byte, []string) (io.ReadCloser, error) {
	if m.release != nil {
		<-m.release
	}
	return io.NopCloser(bytes.NewReader(etcdStream())), nil
}

func (m *managementTalos) Restore(_ context.Context, _ []byte, _ []string, open func() (io.ReadCloser, error), _ func(string)) error {
	r, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restored = data
	return err
}

func managementBackupsOf(t *testing.T, b *browser) managementBackupsView {
	t.Helper()
	rec := b.do("GET", "/api/appliance/etcd-backups", "")
	expect(t, rec, http.StatusOK)
	v := managementBackupsView{EtcdBackupStatus: &appliance.EtcdBackupStatus{}}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestManagementBackups(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := withManagementCluster(t, h)
	talos := &managementTalos{release: make(chan struct{})}
	store := &uiStore{objects: map[string]provisioner.StoredObject{}, data: map[string][]byte{}}
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

	// Those who may not set the management cluster up see none of it.
	d := h.browser()
	d.signIn("dev@pve")
	expect(t, d.do("GET", "/api/appliance/etcd-backups", ""), http.StatusForbidden)
	expect(t, d.do("POST", "/api/appliance/etcd-backups/snapshots/x.db.age/restore", `{"passphrase":"`+statePassphrase+`"}`), http.StatusForbidden)

	b := h.browser()
	b.signIn("admin@pve")
	if v := managementBackupsOf(t, b); v.Settings != nil || v.KeysStored || len(v.Targets) != 1 || v.Targets[0] != "minio" {
		t.Fatalf("before: %+v", v)
	}
	expect(t, b.do("POST", "/api/appliance/etcd-backups/snapshots", ""), http.StatusConflict)
	rec := b.do("PUT", "/api/appliance/etcd-backups", `{"target":"nowhere","everyHours":6,"keep":0,"accessKeyID":"AKID","secretAccessKey":"secret","passphrase":"`+statePassphrase+`"}`)
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "not one of the backup targets") || !strings.Contains(rec.Body.String(), "keep:") {
		t.Errorf("refused with %s", rec.Body)
	}
	expect(t, b.do("PUT", "/api/appliance/etcd-backups", `{"target":"minio","everyHours":6,"keep":3,"accessKeyID":"AKID","secretAccessKey":"secret","passphrase":"a wrong passphrase, long enough"}`), http.StatusForbidden)
	expect(t, b.do("PUT", "/api/appliance/etcd-backups", `{"target":"minio","everyHours":6,"keep":3,"accessKeyID":"AKID","secretAccessKey":"secret","passphrase":"`+statePassphrase+`"}`), http.StatusOK)
	if v := managementBackupsOf(t, b); v.Settings == nil || v.Settings.By != "admin@pve" || !v.KeysStored {
		t.Fatalf("set up: %+v", v)
	}

	// Taking one shows at once, and a second is refused while it runs.
	rec = b.do("POST", "/api/appliance/etcd-backups/snapshots", "")
	expect(t, rec, http.StatusAccepted)
	if !strings.Contains(rec.Body.String(), `"taking":true`) {
		t.Errorf("not shown being taken: %s", rec.Body)
	}
	expect(t, b.do("POST", "/api/appliance/etcd-backups/snapshots", ""), http.StatusConflict)
	close(talos.release)
	var name string
	for deadline := time.Now().Add(10 * time.Second); name == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no snapshot was taken")
		}
		if v := managementBackupsOf(t, b); !v.Taking && v.Last != nil {
			name = v.Last.Name
		}
	}
	rec = b.do("GET", "/api/appliance/etcd-backups/snapshots", "")
	expect(t, rec, http.StatusOK)
	var list managementSnapshotsView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Snapshots) != 1 || list.Snapshots[0].Name != name {
		t.Fatalf("listed %s, %v", rec.Body, err)
	}

	restore := "/api/appliance/etcd-backups/snapshots/" + name + "/restore"
	expect(t, b.do("POST", restore, `{"passphrase":"a wrong passphrase, long enough"}`), http.StatusForbidden)
	expect(t, b.do("POST", restore, `{"passphrase":"`+statePassphrase+`"}`), http.StatusAccepted)
	if j := jobEnded(t, a); j.State != "succeeded" || j.Kind != "etcd-restore" {
		t.Fatalf("restore: %+v", j)
	}
	talos.mu.Lock()
	restored := talos.restored
	talos.mu.Unlock()
	if !bytes.Equal(restored, etcdStream()) {
		t.Errorf("restored %q", restored)
	}

	expect(t, b.do("DELETE", "/api/appliance/etcd-backups", ""), http.StatusNoContent)
	if v := managementBackupsOf(t, b); v.Settings != nil || v.KeysStored {
		t.Errorf("still set up: %+v", v)
	}
	if err := a.RestoreEtcd(t.Context(), name, statePassphrase, nil); !errors.Is(err, appliance.ErrNoEtcdBackups) {
		t.Errorf("restore once off: %v", err)
	}
}
