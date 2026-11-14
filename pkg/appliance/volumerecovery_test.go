package appliance

import (
	"bytes"
	"context"
	"errors"
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

	if err := a.RecoverManagement("the wrong passphrase entirely", ticket(), ready); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("a wrong passphrase: %v", err)
	}

	recover := func(want string) {
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
	recover(FromBackupVolume)

	// The volume was lost with the cluster, and the apply made an empty one.
	empty := backupstore.Dir{Root: t.TempDir()}
	a.OpenVolume = func(context.Context) (provisioner.ObjectStore, error) { return empty, nil }
	recover(FromApplianceCopy)
}
