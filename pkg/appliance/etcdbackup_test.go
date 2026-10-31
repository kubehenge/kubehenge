package appliance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

const passphrase = "correct horse battery staple"

// memoryStore is a bucket in memory.
type memoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	meta    map[string]map[string]string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: map[string][]byte{}, meta: map[string]map[string]string{}}
}

func (m *memoryStore) Put(_ context.Context, key string, r io.Reader, metadata map[string]string) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key], m.meta[key] = data, metadata
	return int64(len(data)), nil
}

func (m *memoryStore) SetMetadata(_ context.Context, key string, metadata map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meta[key] = metadata
	return nil
}

func (m *memoryStore) List(_ context.Context, prefix string) ([]provisioner.StoredObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []provisioner.StoredObject
	for k, v := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, provisioner.StoredObject{Key: k, Size: int64(len(v)), Metadata: m.meta[k]})
		}
	}
	slices.SortFunc(out, func(a, b provisioner.StoredObject) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

func (m *memoryStore) Stat(_ context.Context, key string) (provisioner.StoredObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.objects[key]
	if !ok {
		return provisioner.StoredObject{}, fmt.Errorf("%s: %w", key, provisioner.ErrNoObject)
	}
	return provisioner.StoredObject{Key: key, Size: int64(len(v)), Metadata: m.meta[key]}, nil
}

func (m *memoryStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if _, err := m.Stat(ctx, key); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return io.NopCloser(bytes.NewReader(m.objects[key])), nil
}

func (m *memoryStore) Remove(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (*memoryStore) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", errors.New("no links")
}

// fakeManagementTalos hands out a backup talosconfig and a snapshot ending in etcd's checksum, and
// records a restore.
type fakeManagementTalos struct {
	mu       sync.Mutex
	asked    []string
	restored []byte
	nodes    []string
}

// etcdDatabase is a snapshot as etcd sends it: the database, then its SHA-256.
func etcdDatabase() []byte {
	db := []byte("etcd database")
	sum := sha256.Sum256(db)
	return append(db, sum[:]...)
}

func (f *fakeManagementTalos) BackupConfig(_ context.Context, admin []byte, address string, _ time.Duration) ([]byte, error) {
	if !strings.Contains(string(admin), "endpoints") {
		return nil, errors.New("not the admin talosconfig")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, address)
	return []byte("backup talosconfig"), nil
}

func (*fakeManagementTalos) Snapshot(_ context.Context, talosconfig []byte, nodes []string) (io.ReadCloser, error) {
	if string(talosconfig) != "backup talosconfig" || len(nodes) != 2 {
		return nil, fmt.Errorf("snapshot with %q from %v", talosconfig, nodes)
	}
	return io.NopCloser(bytes.NewReader(etcdDatabase())), nil
}

func (f *fakeManagementTalos) Restore(_ context.Context, admin []byte, nodes []string, open func() (io.ReadCloser, error), log func(string)) error {
	r, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	log("restored on " + nodes[0])
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored, f.nodes = data, nodes
	if !strings.Contains(string(admin), "endpoints") {
		return errors.New("not the admin talosconfig")
	}
	return nil
}

// backupAppliance is an appliance with a management cluster, state and a backup target, and the
// bucket of that target.
func backupAppliance(t *testing.T, store *memoryStore) (*Appliance, *fakeManagementTalos) {
	t.Helper()
	a, _ := newAppliance(t)
	talos := &fakeManagementTalos{}
	a.Talos = talos
	a.OpenStore = func(target *provisioner.BackupTarget, credential map[string][]byte) (provisioner.ObjectStore, error) {
		if target.Bucket != "backups" || string(credential["accessKeyID"]) != "AKID" || string(credential["secretAccessKey"]) != "secret" {
			return nil, fmt.Errorf("opened %+v with %q", target, credential)
		}
		return store, nil
	}
	s := settings("pve1")
	s.BackupTargets = []BackupTarget{{Name: "minio", Endpoint: "https://minio.lab.internal", Bucket: "backups", Prefix: "kubehenge"}}
	work := filepath.Join(a.Dir, "management")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(work, "settings.json"), s); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{filepath.Join(work, "terraform.tfstate"): "{}", a.KubeconfigPath(): "kubeconfig"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return a, talos
}

func TestEtcdBackups(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	a, talos := backupAppliance(t, store)
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	ctx := context.Background()
	settings := EtcdBackupSettings{Target: "minio", EveryHours: 6, Keep: 2, By: "root@pam", Since: now}

	if err := a.ConfigureEtcdBackups(ctx, settings, "AKID", "secret", "wrong passphrase, long enough"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	bad := settings
	bad.Target, bad.Keep = "elsewhere", 0
	if err := a.ConfigureEtcdBackups(ctx, bad, "AKID", "secret", passphrase); err == nil ||
		!strings.Contains(err.Error(), "not one of the backup targets") || !strings.Contains(err.Error(), "keep:") {
		t.Fatalf("bad settings: %v", err)
	}
	if err := a.ConfigureEtcdBackups(ctx, settings, "AKID", "secret", passphrase); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(talos.asked, []string{"10.0.0.11"}) {
		t.Errorf("asked %v for the backup talosconfig", talos.asked)
	}
	if _, ok := store.objects["kubehenge/management/identity.age"]; !ok {
		t.Fatal("the target holds no key")
	}
	if !a.EtcdBackupDue(now) {
		t.Fatal("the first snapshot is not due at once")
	}
	names := make([]string, 0, 3)
	for i := range 3 {
		now = now.Add(time.Duration(i+1) * time.Hour)
		taken, err := a.TakeEtcdSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, taken.Name)
		if bytes.Contains(store.objects["kubehenge/management/etcd/"+taken.Name], []byte("etcd database")) {
			t.Fatal("the snapshot is stored in the clear")
		}
	}
	if a.EtcdBackupDue(now.Add(5 * time.Hour)) {
		t.Error("due before its 6 hours")
	}
	list, err := a.ListEtcdSnapshots(ctx)
	if err != nil || len(list) != 2 || list[0].Name != names[2] || list[1].Name != names[1] {
		t.Fatalf("kept %+v, %v; took %v", list, err, names)
	}

	// A wrong passphrase starts nothing; the right one restores the checked snapshot.
	if err := a.RestoreEtcd(ctx, list[1].Name, "wrong passphrase, long enough", ticket()); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if err := a.RestoreEtcd(ctx, "../identity.age", passphrase, ticket()); err == nil {
		t.Fatal("restored from something that is no snapshot")
	}
	if err := a.RestoreEtcd(ctx, list[1].Name, passphrase, ticket()); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	if j.State != "succeeded" || !bytes.Equal(talos.restored, etcdDatabase()) || !slices.Equal(talos.nodes, []string{"10.0.0.11", "10.0.0.12"}) {
		t.Fatalf("restore %+v, restored %q on %v", j, talos.restored, talos.nodes)
	}
	if !slices.Contains(j.Log, "restored on 10.0.0.11") {
		t.Errorf("log %q", j.Log)
	}
	if left, _ := filepath.Glob(filepath.Join(a.Dir, "etcd-restore-*")); len(left) > 0 {
		t.Errorf("left %v", left)
	}

	// Another appliance on the same target needs the passphrase the key was stored with.
	other, _ := backupAppliance(t, store)
	if err := other.ConfigureEtcdBackups(ctx, settings, "AKID", "secret", passphrase); err != nil {
		t.Fatalf("same passphrase: %v", err)
	}

	if err := a.StopEtcdBackups(); err != nil {
		t.Fatal(err)
	}
	if a.EtcdBackups().Settings != nil || a.EtcdBackupKeysStored() {
		t.Error("still set up")
	}
	if _, err := a.TakeEtcdSnapshot(ctx); !errors.Is(err, ErrNoEtcdBackups) {
		t.Errorf("snapshot once off: %v", err)
	}
}

// A snapshot that fails says why and is tried again after a while, not at once.
func TestEtcdBackupFailure(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	a, _ := backupAppliance(t, store)
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	ctx := context.Background()
	if err := a.ConfigureEtcdBackups(ctx, EtcdBackupSettings{Target: "minio", EveryHours: 1, Keep: 3, Since: now}, "AKID", "secret", passphrase); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.etcdBackupFile("talosconfig"), []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.TakeEtcdSnapshot(ctx); err == nil {
		t.Fatal("took a snapshot with the wrong talosconfig")
	}
	st := a.EtcdBackups()
	if st.Error == "" || st.Failed == nil || !st.Next.Equal(now.Add(etcdBackupRetry)) || a.EtcdBackupDue(now.Add(time.Minute)) {
		t.Fatalf("status %+v", st)
	}
}

func TestRestoreNeedsBackups(t *testing.T) {
	t.Parallel()
	a, _ := backupAppliance(t, newMemoryStore())
	if err := a.RestoreEtcd(context.Background(), "20270301T120000Z-management.db.age", passphrase, &pve.Ticket{Username: "root@pam", Issued: time.Now()}); !errors.Is(err, ErrNoEtcdBackups) {
		t.Errorf("restore without backups: %v", err)
	}
}
