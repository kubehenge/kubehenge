package appliance

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

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
		entered, release := make(chan struct{}), make(chan struct{})
		var first sync.Once
		a.OpenVolume = func(ctx context.Context) (provisioner.ObjectStore, error) {
			first.Do(func() { close(entered) })
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return vol, nil
			}
		}
		backup := make(chan error, 1)
		go func() {
			_, err := a.TakeEtcdSnapshot(context.Background())
			backup <- err
		}()
		<-entered
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
