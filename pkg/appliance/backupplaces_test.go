package appliance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// A bucket set to a day apart keeps its two snapshots a day apart, while the volume backs up every
// 6 hours beside it.
func TestABucketKeepsItsOwnScheduleBesideTheVolume(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, vol, _ := volumeAppliance(t, &now)
	s := EtcdBackupSettings{Target: "minio", EveryHours: 24, Keep: 2}
	if err := a.ConfigureEtcdBackups(ctx, &s, "AKID", "secret", passphrase); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if a.EtcdBackupDue(now) {
			if _, err := a.TakeScheduledEtcdSnapshot(ctx, now); err != nil {
				t.Fatal(err)
			}
		}
		now = now.Add(6 * time.Hour)
	}
	inBucket, err := a.ListEtcdSnapshots(ctx)
	if err != nil || len(inBucket) != 2 || inBucket[0].Time.Sub(inBucket[1].Time) != 24*time.Hour {
		t.Fatalf("the bucket keeps %+v (%v), want 2 snapshots a day apart", inBucket, err)
	}
	onVolume, err := provisioner.ListEtcdSnapshots(ctx, vol, provisioner.ManagementEtcdFolder(""))
	if err != nil || len(onVolume) != 12 {
		t.Fatalf("the volume keeps %d snapshots (%v), want one every 6 hours", len(onVolume), err)
	}
}

// removed is a with its management cluster removed, the backup volume kept or deleted with it.
func removed(t *testing.T, a *Appliance, deleteVolume bool) {
	t.Helper()
	if err := a.Remove(passphrase, deleteVolume, ticket(), func(context.Context, *Job) error { return nil }, func() {}); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("remove: %+v", j)
	}
}

// Once the management cluster is removed, no backup of it is due or taken.
func TestRemoveStopsTheVolumeBackups(t *testing.T) {
	t.Parallel()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, _, _ := volumeAppliance(t, &now)
	if _, err := a.TakeEtcdSnapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	removed(t, a, false)
	for _, at := range []time.Time{now, now.Add(volumeBackupEvery), now.Add(30 * 24 * time.Hour)} {
		if a.EtcdBackupDue(at) {
			t.Errorf("a backup is due at %s after the removal", at)
		}
	}
	if a.EtcdBackups().Volume != nil {
		t.Error("the removed cluster's backup volume still shows")
	}
	if _, err := a.TakeEtcdSnapshot(t.Context()); !errors.Is(err, ErrNoEtcdBackups) {
		t.Errorf("a snapshot after the removal: %v", err)
	}
}

// Switching the bucket's backups off leaves the volume's backups their own credential.
func TestStoppingTheBucketKeepsTheVolumeBackups(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, vol, _ := volumeAppliance(t, &now)
	s := EtcdBackupSettings{Target: "minio", EveryHours: 24, Keep: 2}
	if err := a.ConfigureEtcdBackups(ctx, &s, "AKID", "secret", passphrase); err != nil {
		t.Fatal(err)
	}
	if err := a.StopEtcdBackups(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.TakeScheduledEtcdSnapshot(ctx, now); err != nil {
		t.Fatalf("a volume backup after the bucket's were switched off: %v", err)
	}
	if list, err := provisioner.ListEtcdSnapshots(ctx, vol, provisioner.ManagementEtcdFolder("")); err != nil || len(list) != 1 {
		t.Fatalf("the volume holds %d snapshots (%v)", len(list), err)
	}
	if st := a.EtcdBackups().Volume; st == nil || st.Error != "" {
		t.Fatalf("the volume's backups show %+v", st)
	}
}
