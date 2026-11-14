package appliance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
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

// The appliance's copy goes with the backup volume. Kept, it is never restored onto a cluster set
// up after the one it is of.
func TestRemoveDeletesOrFencesTheAppliancesCopy(t *testing.T) {
	t.Parallel()
	for _, deleteVolume := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete volume %v", deleteVolume), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
			a, _, _ := volumeAppliance(t, &now)
			if _, err := a.TakeEtcdSnapshot(ctx); err != nil {
				t.Fatal(err)
			}
			removed(t, a, deleteVolume)
			left, err := a.applianceCopy().Size(ctx)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			if deleteVolume != (left == 0) {
				t.Fatalf("the copy holds %d bytes after a removal that deleted the volume: %v", left, deleteVolume)
			}
			// A setup a month later, whose volume holds nothing yet.
			now = now.Add(30 * 24 * time.Hour)
			if err := os.WriteFile(filepath.Join(a.workdir(), "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			admin, err := a.Credential(ctx, passphrase, "talosconfig")
			if err != nil {
				t.Fatal(err)
			}
			if err := a.setUpVolumeBackups(ctx, admin, passphrase); err != nil {
				t.Fatal(err)
			}
			empty := backupstore.Dir{Root: t.TempDir()}
			a.OpenVolume = func(context.Context) (provisioner.ObjectStore, error) { return empty, nil }
			talos := &fakeManagementTalos{}
			a.Talos = talos
			if err := a.RecoverManagement(passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
				t.Fatal(err)
			}
			j := waitFor(t, a)
			if j.State != StateFailed || talos.restored != nil {
				t.Fatalf("the new cluster was recovered from the removed one's backup: %s, restored %q", j.State, talos.restored)
			}
			if !deleteVolume && !strings.Contains(j.Error, ErrForeignCopy.Error()) {
				t.Errorf("the refusal says %q", j.Error)
			}
		})
	}
}

// The copy's cap counts all it holds, the export included, and a copy over it never replaces the
// last that fit.
func TestTheCopysCapCountsItsExport(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, _, _ := volumeAppliance(t, &now)
	first, err := a.TakeEtcdSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	whole := a.VolumeBackups().Copy.Bytes
	exports, err := newestFirst(ctx, a.applianceCopy(), exportFolder)
	if err != nil || len(exports) != 1 {
		t.Fatalf("the copy holds exports %v (%v)", exports, err)
	}
	// A cap the snapshot fits, but not the copy whole.
	a.CopyCap = whole - 1
	if int64(len(etcdDatabase())) >= a.CopyCap {
		t.Fatal("the snapshot alone does not fit the cap")
	}
	now = now.Add(volumeBackupEvery)
	second, err := a.TakeEtcdSnapshot(ctx)
	if err != nil {
		t.Fatalf("the volume's backup failed with the copy over its cap: %v", err)
	}
	c := a.VolumeBackups().Copy
	if c.Snapshot != first.Name || c.Bytes != whole || !strings.Contains(c.Error, second.Name) ||
		!strings.Contains(c.Error, fmt.Sprintf("%d bytes, more than its cap of %d bytes", whole, whole-1)) {
		t.Fatalf("over the cap the copy shows %+v", c)
	}
	copies, err := provisioner.ListEtcdSnapshots(ctx, a.applianceCopy(), provisioner.ManagementEtcdFolder(""))
	if err != nil || len(copies) != 1 || copies[0].Name != first.Name {
		t.Fatalf("the copy holds %+v (%v), want the last that fit", copies, err)
	}
	if _, err := os.Stat(a.applianceCopy().Root + copyStaging); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused copy is left beside the one in use: %v", err)
	}
}

// A crash between the two renames of a swap leaves the copy it replaced, which the start puts back.
func TestCleanUpPutsBackTheCopyACrashLeftAside(t *testing.T) {
	t.Parallel()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, _, _ := volumeAppliance(t, &now)
	taken, err := a.TakeEtcdSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	live := a.applianceCopy().Root
	if err := os.Rename(live, live+copyReplaced); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(live+copyStaging, "management"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.CleanUp(); err != nil {
		t.Fatal(err)
	}
	copies, err := provisioner.ListEtcdSnapshots(t.Context(), a.applianceCopy(), provisioner.ManagementEtcdFolder(""))
	if err != nil || len(copies) != 1 || copies[0].Name != taken.Name {
		t.Fatalf("after the start the copy holds %+v (%v)", copies, err)
	}
	for _, left := range []string{live + copyStaging, live + copyReplaced} {
		if _, err := os.Stat(left); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is left: %v", filepath.Base(left), err)
		}
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
