package appliance

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestRecoveryReadsTheVolumeElseTheCopy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, _, _ := volumeAppliance(t, &now)
	talos := &fakeManagementTalos{}
	a.Talos = talos
	var last *provisioner.EtcdSnapshot
	for range 2 {
		taken, err := a.TakeEtcdSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		last = taken
		now = now.Add(volumeBackupEvery)
	}
	ready := func(context.Context, []byte) error { return nil }

	requirePassphrase(t, a)
	if err := a.RecoverManagement("the wrong passphrase entirely", ticket(), ready); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("a wrong passphrase: %v", err)
	}

	recoverFrom := func(want string) {
		t.Helper()
		talos.restored = nil
		if err := a.RecoverManagement(passphrase, ticket(), ready); err != nil {
			t.Fatal(err)
		}
		j := waitFor(t, a)
		log := strings.Join(j.Log, "\n")
		if j.State != StateSucceeded || !strings.Contains(log, "from "+last.Name+", read from "+want) {
			t.Fatalf("recovery %s (%s):\n%s", j.State, j.Error, log)
		}
		if !bytes.Equal(talos.restored, etcdDatabase()) {
			t.Fatalf("restored %q", talos.restored)
		}
	}
	recoverFrom(FromBackupVolume)

	// The volume was lost with the cluster, and the apply made an empty one.
	empty := backupstore.Dir{Root: t.TempDir()}
	a.OpenVolume = func(context.Context) (provisioner.ObjectStore, error) { return empty, nil }
	recoverFrom(FromApplianceCopy)
}

// The bootstrap of a lost first node never runs again from the state, so a whole apply onto lost VMs
// waits for etcd forever. The recovery makes the VMs and their node config first, restores, and only
// then applies the whole module.
func TestRecoveryMakesTheVMsBeforeTheRestoreAndAppliesWholeAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, _, _ := volumeAppliance(t, &now)
	if _, err := a.TakeEtcdSnapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	traced := &tracedTalos{tofuLog: filepath.Join(a.Dir, "fake-tofu.log")}
	a.Talos = traced
	earlier := len(appliesIn(traced.tofuLog))
	if err := a.RecoverManagement(passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("recovery %s: %s", j.State, j.Error)
	}
	before := traced.applies[earlier:]
	if len(before) != 1 || !strings.Contains(before[0], "-target=module.management.proxmox_virtual_environment_vm.node") ||
		!strings.Contains(before[0], "-target=module.management.module.talos_node_0") {
		t.Fatalf("before the restore, applies were %q: only the VMs and nodes belong there", before)
	}
	all := appliesIn(traced.tofuLog)[earlier:]
	if len(all) != 2 || strings.Contains(all[1], "-target") {
		t.Fatalf("applies of the recovery: %q, want the targeted one, then a whole one", all)
	}
}

// Recovery waits for the volume's store before it reads the appliance's copy, never restores a copy
// older than the volume's newest, and says which it read and why.
func TestRecoveryWaitsForTheVolumeAndTakesTheNewest(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a, vol, _ := volumeAppliance(t, &now)
	talos := &fakeManagementTalos{}
	a.Talos = talos
	a.waitVolume = 200 * time.Millisecond
	first, err := a.TakeEtcdSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The second goes onto the volume alone: the copy is over its cap.
	now = now.Add(volumeBackupEvery)
	a.CopyCap = 8
	second, err := a.TakeEtcdSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var asked int
	notReady := func(answerFrom int) {
		asked = 0
		a.OpenVolume = func(context.Context) (provisioner.ObjectStore, error) {
			asked++
			if answerFrom < 0 || asked < answerFrom {
				return nil, errors.New("the backup volume answered 503 Service Unavailable")
			}
			return vol, nil
		}
	}
	recoverFrom := func(snapshot, from, why string) {
		t.Helper()
		talos.restored = nil
		if err := a.RecoverManagement(passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
		j := waitFor(t, a)
		log := strings.Join(j.Log, "\n")
		if j.State != StateSucceeded || !strings.Contains(log, "from "+snapshot+", read from "+from) || !strings.Contains(log, why) {
			t.Fatalf("recovery %s (%s), want %s from %s because %q:\n%s", j.State, j.Error, snapshot, from, why, log)
		}
	}

	notReady(3)
	recoverFrom(second.Name, FromBackupVolume, "Waiting for the backup volume's store to answer: the backup volume answered 503")
	if asked < 3 {
		t.Errorf("the volume was asked %d times", asked)
	}

	notReady(-1)
	recoverFrom(first.Name, FromApplianceCopy, "since the backup volume gives no snapshot: it did not answer within 200ms")

	// A copy newer than the volume's newest, whose store failed on the last backup.
	newer := backupstore.Dir{Root: t.TempDir()}
	a.OpenVolume = func(context.Context) (provisioner.ObjectStore, error) { return newer, nil }
	a.CopyCap = 0
	now = now.Add(volumeBackupEvery)
	third, _ := a.TakeEtcdSnapshot(ctx)
	if third == nil {
		t.Fatal("no third snapshot")
	}
	a.OpenVolume = func(context.Context) (provisioner.ObjectStore, error) { return vol, nil }
	recoverFrom(third.Name, FromApplianceCopy, "which holds a snapshot newer than the backup volume's "+second.Name)
}
