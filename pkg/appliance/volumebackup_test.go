package appliance

import (
	"context"
	"errors"
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
