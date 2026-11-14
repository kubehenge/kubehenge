package appliance

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// backupHeld starts a backup that waits at its first open of the volume until release is closed or
// its context ends. Later opens answer at once. It returns once the backup waits there, with the
// channel its result arrives on.
func backupHeld(t *testing.T, a *Appliance, vol provisioner.ObjectStore) (chan struct{}, chan error) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	a.OpenVolume = func(ctx context.Context) (provisioner.ObjectStore, error) {
		held := false
		first.Do(func() { held = true; close(entered) })
		if !held {
			return vol, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return vol, nil
		}
	}
	result := make(chan error, 1)
	go func() {
		_, err := a.TakeEtcdSnapshot(context.Background())
		result <- err
	}()
	<-entered
	return release, result
}

// A backup is under way when the management cluster is removed. The removal stops it first, so
// that nothing it writes outlives the removal, and no snapshot starts beside the removal.
func TestARemovalStopsTheBackupUnderWayFirst(t *testing.T) {
	t.Parallel()
	for _, deleteVolume := range []bool{false, true} {
		now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
		a, vol, _ := volumeAppliance(t, &now)
		// The first snapshot fills the copy and the record, which the removal then clears.
		if _, err := a.TakeEtcdSnapshot(t.Context()); err != nil {
			t.Fatal(err)
		}
		release, backup := backupHeld(t, a, vol)
		var beside error
		before := func(ctx context.Context, _ *Job) error {
			// A snapshot let through would wait for the volume until the removal ends, so it gives up.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			_, beside = a.TakeEtcdSnapshot(ctx)
			return nil
		}
		if err := a.Remove(passphrase, backupsOf[deleteVolume], ticket(), before, func() {}); err != nil {
			t.Fatal(err)
		}
		j := waitFor(t, a)
		// A backup the removal did not stop goes on now, after the removal.
		close(release)
		if err := <-backup; err == nil {
			t.Errorf("delete %v: the backup under way went on past the removal", deleteVolume)
		}
		if j.State != StateSucceeded {
			t.Fatalf("delete %v: remove %s: %s", deleteVolume, j.State, j.Error)
		}
		if !errors.Is(beside, ErrEtcdRemoving) {
			t.Errorf("delete %v: a snapshot during the removal: %v, want refused", deleteVolume, beside)
		}
		if _, err := os.Stat(a.etcdBackupFile("volume-status.json")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("delete %v: the removed cluster's volume-status.json outlives the removal: %v", deleteVolume, err)
		}
		if deleteVolume {
			if left, _ := a.applianceCopy().List(t.Context(), ""); len(left) > 0 {
				t.Errorf("the removal that deleted the volume leaves the copy %v", left)
			}
		}
	}
}

// A removal refused for its passphrase deletes nothing, so it leaves the backup under way to finish.
func TestARefusedRemovalLeavesTheBackupUnderWay(t *testing.T) {
	t.Parallel()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, vol, _ := volumeAppliance(t, &now)
	release, backup := backupHeld(t, a, vol)
	if err := a.Remove("the wrong passphrase entirely", KeepBackups, ticket(), func(context.Context, *Job) error { return nil }, func() {}); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateFailed || j.Error != ErrPassphrase.Error() {
		t.Fatalf("the removal %s: %s, want refused for its passphrase", j.State, j.Error)
	}
	close(release)
	if err := <-backup; err != nil {
		t.Errorf("the backup under way a refused removal met: %v", err)
	}
	if rec := a.volumeBackupRecord(); rec.Error != "" || rec.Last == nil {
		t.Errorf("after the refused removal the volume's record says last %v, error %q", rec.Last, rec.Error)
	}
}

// stallingTalos holds every etcd snapshot until its context ends.
type stallingTalos struct {
	TalosAccess

	entered chan struct{}
	once    sync.Once
}

func (s *stallingTalos) Snapshot(ctx context.Context, _ []byte, _ []string) (io.ReadCloser, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// A recreation stops the backup into the bucket under way. The bucket's status keeps the last
// snapshot taken, with no error.
func TestABucketBackupARecreationStopsIsNoFailure(t *testing.T) {
	t.Parallel()
	a, _, now := backedUp(t)
	configure(t, a, *now)
	taken, err := a.TakeEtcdSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stalling := &stallingTalos{TalosAccess: a.Talos, entered: make(chan struct{})}
	a.Talos = stalling
	backup := make(chan error, 1)
	go func() {
		_, serr := a.TakeEtcdSnapshot(context.Background())
		backup <- serr
	}()
	<-stalling.entered
	if err = a.RecreateManagement(t.Context(), taken.Name, passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("recreate %s: %s", j.State, j.Error)
	}
	if err = <-backup; err == nil {
		t.Fatal("the backup under way went on through the recreation")
	}
	if rec := a.etcdBackupRecord(); rec.Error != "" || rec.Failed != nil || rec.Last == nil || rec.Last.Name != taken.Name {
		t.Errorf("after the recreation the bucket's record says last %v, error %q", rec.Last, rec.Error)
	}
}

// A recovery stops the backup under way. That backup did not fail, so after the recovery the
// backups' status still shows the last one that was taken, with no error.
func TestABackupARecoveryStopsIsNoFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, vol, _ := volumeAppliance(t, &now)
	a.Talos = &fakeManagementTalos{}
	taken, err := a.TakeEtcdSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(volumeBackupEvery)
	_, backup := backupHeld(t, a, vol)
	if err = a.RecoverManagement(passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("recovery %s: %s", j.State, j.Error)
	}
	if err = <-backup; err == nil {
		t.Fatal("the backup under way went on through the recovery")
	}
	rec := a.volumeBackupRecord()
	if rec.Error != "" || rec.Failed != nil || rec.Last == nil || rec.Last.Name != taken.Name {
		t.Errorf("after the recovery the volume's record says last %v, error %q", rec.Last, rec.Error)
	}
	if rec.Copy == nil || rec.Copy.Error != "" || rec.Copy.Snapshot != taken.Name {
		t.Errorf("after the recovery the copy's record is %+v, want %s and no error", rec.Copy, taken.Name)
	}
}
