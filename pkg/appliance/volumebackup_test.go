package appliance

import (
	"context"
	"encoding/base64"
	"errors"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// volumeAppliance is an appliance with a management cluster whose backup volume is vol, and no
// backup target set up.
func volumeAppliance(t *testing.T, now *time.Time) (*Appliance, backupstore.Dir, *memoryStore) {
	t.Helper()
	bucket := newMemoryStore()
	a, _ := backupAppliance(t, bucket)
	vol := backupstore.Dir{Root: t.TempDir()}
	a.OpenVolume = func(context.Context) (provisioner.ObjectStore, error) { return vol, nil }
	a.ExportObjects = func(context.Context) ([]byte, error) { return []byte(exportedObjects), nil }
	a.Now = func() time.Time { return *now }
	admin, err := a.Credential(t.Context(), passphrase, "talosconfig")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.setUpVolumeBackups(t.Context(), admin, passphrase); err != nil {
		t.Fatal(err)
	}
	return a, vol, bucket
}

func TestVolumeBackupsRunWithNoInput(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, vol, bucket := volumeAppliance(t, &now)
	if a.EtcdBackups().Settings != nil {
		t.Fatal("the volume's backups need settings")
	}
	if !a.EtcdBackupDue(now) {
		t.Fatal("the first snapshot onto the volume is not due at once")
	}
	var last *provisioner.EtcdSnapshot
	for range volumeBackupKeep + 2 {
		taken, err := a.TakeEtcdSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		last = taken
		now = now.Add(volumeBackupEvery)
	}
	list, err := provisioner.ListEtcdSnapshots(ctx, vol, provisioner.ManagementEtcdFolder(""))
	if err != nil || len(list) != volumeBackupKeep || list[0].Name != last.Name {
		t.Fatalf("the volume keeps %d snapshots, newest %v (%v), want %d, newest %s", len(list), list[0].Name, err, volumeBackupKeep, last.Name)
	}
	if len(bucket.objects) != 0 {
		t.Errorf("a target with no settings got %d objects", len(bucket.objects))
	}
	keys := a.volumeBackupKeys()
	r, err := vol.Get(ctx, volumeIdentityKey(keys.Recipient))
	if err != nil {
		t.Fatalf("the volume holds no key beside the snapshots: %v", err)
	}
	identity, err := openIdentity(r, passphrase)
	_ = r.Close()
	if err != nil || identity.Recipient().String() != keys.Recipient {
		t.Fatalf("the passphrase does not open the volume's key: %v", err)
	}
	st := a.EtcdBackups().Volume
	if st == nil || st.Claim != "kubehenge-system/kubehenge-backups" || st.Last == nil || st.Last.Name != last.Name || st.Error != "" {
		t.Fatalf("the status shows %+v", st)
	}
	if a.EtcdBackupDue(last.Time.Add(volumeBackupEvery - time.Minute)) {
		t.Error("due before its 6 hours")
	}
}

const exportedObjects = "apiVersion: cluster.x-k8s.io/v1beta2\nkind: Cluster\nmetadata:\n  name: web\n"

func TestExportGoesWithEachSnapshot(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, vol, _ := volumeAppliance(t, &now)
	var last *provisioner.EtcdSnapshot
	for range 2 {
		taken, err := a.TakeEtcdSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		last = taken
		now = now.Add(volumeBackupEvery)
	}
	identity, err := openIdentity(strings.NewReader(mustDecode(t, a.volumeBackupKeys().Sealed)), passphrase)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		store provisioner.ObjectStore
		count int
	}{{vol, 2}, {a.applianceCopy(), 1}} {
		keys, err := newestFirst(ctx, s.store, exportFolder)
		if err != nil || len(keys) != s.count || keys[0] != exportKey(last.Name) {
			t.Fatalf("exports %v (%v), want %d, newest for %s", keys, err, s.count, last.Name)
		}
		r, err := s.store.Get(ctx, keys[0])
		if err != nil {
			t.Fatal(err)
		}
		e, err := openExport(r, identity)
		_ = r.Close()
		if err != nil || e.Objects != exportedObjects || e.Snapshot != last.Name || e.Settings == nil || len(e.Settings.Nodes) == 0 {
			t.Fatalf("the export reads back as %+v, %v", e, err)
		}
	}
	if st := a.EtcdBackups().Volume; st.Export == nil || st.Export.Name != path.Base(exportKey(last.Name)) {
		t.Fatalf("the status shows the export as %+v", st.Export)
	}

	a.ExportObjects = func(context.Context) ([]byte, error) { return nil, errors.New("clusters: forbidden") }
	if _, err := a.TakeEtcdSnapshot(ctx); err == nil {
		t.Fatal("a backup whose export failed succeeded")
	}
	if st := a.EtcdBackups().Volume; !strings.Contains(st.Error, "exporting Kubehenge's objects: clusters: forbidden") || st.Failed == nil {
		t.Fatalf("a failed export shows as %+v", st)
	}
}

func mustDecode(t *testing.T, s string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestApplySetsUpVolumeBackups(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	a.Talos = &fakeManagementTalos{}
	if a.EtcdBackupDue(time.Now()) {
		t.Fatal("a snapshot is due before any apply")
	}
	if err := a.Apply(settings("pve1"), passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("apply: %+v", j)
	}
	keys := a.volumeBackupKeys()
	if keys == nil || len(keys.Nodes) != 2 || keys.Recipient == "" {
		t.Fatalf("the apply set the volume's backups up as %+v", keys)
	}
	if !a.EtcdBackupDue(time.Now()) {
		t.Fatal("no snapshot onto the volume is due after the first apply")
	}
}

func TestVolumeBackupFailureShows(t *testing.T) {
	t.Parallel()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, _, _ := volumeAppliance(t, &now)
	a.OpenVolume = func(context.Context) (provisioner.ObjectStore, error) {
		return nil, errors.New("the backup volume's token: secrets \"kubehenge-backup-store\" not found")
	}
	if _, err := a.TakeEtcdSnapshot(t.Context()); err == nil {
		t.Fatal("a snapshot with the volume out of reach succeeded")
	}
	st := a.EtcdBackups().Volume
	if st == nil || !strings.Contains(st.Error, "kubehenge-backup-store") || st.Failed == nil || !st.Next.Equal(now.Add(etcdBackupRetry)) {
		t.Fatalf("the failure shows as %+v", st)
	}
}

func TestApplianceKeepsACopyOfTheLatest(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, _, _ := volumeAppliance(t, &now)
	var names []string
	for range 3 {
		taken, err := a.TakeEtcdSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, taken.Name)
		now = now.Add(volumeBackupEvery)
	}
	copies, err := provisioner.ListEtcdSnapshots(ctx, a.applianceCopy(), provisioner.ManagementEtcdFolder(""))
	if err != nil || len(copies) != 1 || copies[0].Name != names[2] {
		t.Fatalf("the appliance keeps %+v (%v), want only %s", copies, err, names[2])
	}
	st := a.EtcdBackups().Volume.Copy
	if st == nil || st.Snapshot != names[2] || st.Bytes == 0 || st.Bytes > st.Cap || st.Cap != ApplianceCopyCap || st.Error != "" {
		t.Fatalf("the copy shows as %+v", st)
	}

	// A snapshot over the cap goes to the volume alone, and the copy before it stays.
	a.CopyCap = 8
	taken, err := a.TakeEtcdSnapshot(ctx)
	if err != nil {
		t.Fatalf("a snapshot over the copy's cap failed on the volume: %v", err)
	}
	copies, _ = provisioner.ListEtcdSnapshots(ctx, a.applianceCopy(), provisioner.ManagementEtcdFolder(""))
	st = a.EtcdBackups().Volume.Copy
	if len(copies) != 1 || copies[0].Name != names[2] || !strings.Contains(st.Error, taken.Name) {
		t.Fatalf("over the cap the copy holds %+v and shows %+v", copies, st)
	}
}
