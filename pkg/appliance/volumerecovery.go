package appliance

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// Where a recovery read its snapshot.
const (
	FromBackupVolume  = "the backup volume"
	FromApplianceCopy = "the appliance's copy"
)

// RecoverManagement makes a lost management cluster again from Kubehenge's own backups. It applies
// the module whole first, which brings the backup volume back to the new nodes when it survived.
// It then restores the newest snapshot of the volume and the appliance's copy, says which in the
// log and why, and applies the module again. ready is called as for Apply.
func (a *Appliance) RecoverManagement(passphrase string, ticket *pve.Ticket, ready func(ctx context.Context, kubeconfig []byte) error) error {
	if a.Talos == nil {
		return errors.New("this appliance does not reach Talos nodes")
	}
	settings := a.SavedSettings()
	if settings == nil || !a.HasState() {
		return errors.New("there is no management cluster state to recover it from: import an export first")
	}
	keys := a.volumeBackupKeys()
	if keys == nil {
		return errors.New("this appliance holds no backups of its own: recreate the management cluster from a backup target's snapshot")
	}
	sealed, err := base64.StdEncoding.DecodeString(keys.Sealed)
	if err != nil {
		return fmt.Errorf("the snapshots' sealed key: %w", err)
	}
	identity, err := openIdentity(strings.NewReader(string(sealed)), passphrase)
	if err != nil {
		return ErrPassphrase
	}
	// Read before the run, which the state's own reads wait for. The machine secrets, and so the
	// nodes' addresses, stay the same across the apply.
	admin, err := a.Credential(context.Background(), passphrase, "talosconfig")
	if err != nil {
		return err
	}
	nodes, err := provisioner.TalosEndpoints(admin)
	if err != nil {
		return fmt.Errorf("the management cluster's talosconfig: %w", err)
	}
	return a.start(ticket, "recover", func(ctx context.Context, j *Job) error {
		j.Add("Applying the management cluster, so that its backup volume can be read")
		if _, err := a.run(ctx, j, settings, passphrase, ticket); err != nil {
			return err
		}
		file, name, from, err := a.newestBackup(ctx, j, identity, passphrase)
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(file) }()
		j.Add("Restoring the management cluster's etcd from " + name + ", read from " + from)
		open := func() (io.ReadCloser, error) { return os.Open(file) } //nolint:gosec // the appliance's own file
		if err := a.Talos.Restore(ctx, admin, nodes, open, j.Add); err != nil {
			return fmt.Errorf("restoring etcd: %w", err)
		}
		j.Add("Applying the management cluster whole")
		kubeconfig, err := a.run(ctx, j, settings, passphrase, ticket)
		if err != nil {
			return err
		}
		if err := ready(ctx, kubeconfig); err != nil {
			return fmt.Errorf("the management cluster is up, but the interface cannot use it: %w", err)
		}
		return nil
	})
}

const (
	// volumeWait bounds how long a recovery waits for the backup volume's store, which starts only
	// once the apply's nodes run and the volume is attached to one of them.
	volumeWait  = 10 * time.Minute
	volumeRetry = 10 * time.Second
)

// errNoSnapshot is a store that answers and holds no snapshot.
var errNoSnapshot = errors.New("it holds no snapshot")

// newestBackup decrypts the newest snapshot of the backup volume and the appliance's copy into a
// file, and says which it read and why. It waits for the volume's store first, so that a store not
// started yet never hands the restore to an older copy.
func (a *Appliance) newestBackup(ctx context.Context, j *Job, identity *age.X25519Identity, passphrase string) (string, string, string, error) {
	folder := provisioner.ManagementEtcdFolder("")
	store, onVolume, volErr := a.waitForVolume(ctx, j, folder)
	copied := a.applianceCopy()
	var inCopy *provisioner.EtcdSnapshot
	copyErr := a.checkCopyOwner(ctx, identity.Recipient().String())
	if copyErr == nil {
		inCopy, copyErr = newestIn(ctx, copied, folder)
	}
	if volErr == nil && (copyErr != nil || !inCopy.Time.After(onVolume.Time)) {
		j.Add("Reading the backup volume, which holds the newest snapshot")
		file, err := a.decryptWithKeys(ctx, store, folder, onVolume, identity, passphrase)
		if err == nil {
			return file, onVolume.Name, FromBackupVolume, nil
		}
		volErr = err
	}
	if copyErr != nil {
		return "", "", "", fmt.Errorf("neither the backup volume nor the appliance's copy gives a snapshot: %w", errors.Join(fmt.Errorf("the backup volume: %w", volErr), fmt.Errorf("the appliance's copy: %w", copyErr)))
	}
	if volErr != nil {
		j.Add("Reading the appliance's copy, since the backup volume gives no snapshot: " + volErr.Error())
	} else {
		j.Add("Reading the appliance's copy, which holds a snapshot newer than the backup volume's " + onVolume.Name)
	}
	file, err := a.decryptSnapshot(ctx, copied, folder, inCopy, identity.String())
	if err != nil {
		return "", "", "", fmt.Errorf("the appliance's copy, snapshot %s: %w", inCopy.Name, err)
	}
	return file, inCopy.Name, FromApplianceCopy, nil
}

// waitForVolume lists the backup volume's newest snapshot, retrying while its store does not
// answer, up to volumeWait. A store that answers with no snapshot ends the wait.
func (a *Appliance) waitForVolume(ctx context.Context, j *Job, folder string) (provisioner.ObjectStore, *provisioner.EtcdSnapshot, error) {
	wait, retry := volumeWait, volumeRetry
	if a.waitVolume > 0 {
		wait, retry = a.waitVolume, a.waitVolume/10
	}
	deadline := time.Now().Add(wait)
	for said := false; ; said = true {
		store, err := a.openVolume(ctx)
		if err == nil {
			var newest *provisioner.EtcdSnapshot
			if newest, err = newestIn(ctx, store, folder); err == nil || errors.Is(err, errNoSnapshot) {
				return store, newest, err
			}
		}
		if time.Now().Add(retry).After(deadline) {
			return nil, nil, fmt.Errorf("it did not answer within %s: %w", wait, err)
		}
		if !said {
			j.Add("Waiting for the backup volume's store to answer: " + err.Error())
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err() //nolint:wrapcheck // the run's own end
		case <-time.After(retry):
		}
	}
}

func newestIn(ctx context.Context, store provisioner.ObjectStore, folder string) (*provisioner.EtcdSnapshot, error) {
	list, err := provisioner.ListEtcdSnapshots(ctx, store, folder)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller names the store
	}
	if len(list) == 0 {
		return nil, errNoSnapshot
	}
	return &list[0], nil
}

// decryptWithKeys decrypts snapshot newest in store into a file of the appliance's and checks its
// checksum. It opens it with identity, or with a key the store keeps that the passphrase opens,
// which an earlier appliance left.
func (a *Appliance) decryptWithKeys(ctx context.Context, store provisioner.ObjectStore, folder string, newest *provisioner.EtcdSnapshot, identity *age.X25519Identity, passphrase string) (string, error) {
	ids := []string{identity.String()}
	if keys, err := newestFirst(ctx, store, "management/identities"); err == nil {
		for _, k := range keys {
			if r, err := store.Get(ctx, k); err == nil {
				if other, err := openIdentity(r, passphrase); err == nil && other.String() != identity.String() {
					ids = append(ids, other.String())
				}
				_ = r.Close()
			}
		}
	}
	var errs []error
	for _, id := range ids {
		file, err := a.decryptSnapshot(ctx, store, folder, newest, id)
		if err == nil {
			return file, nil
		}
		errs = append(errs, err)
	}
	return "", fmt.Errorf("snapshot %s: %w", newest.Name, errors.Join(errs...))
}

func (a *Appliance) decryptSnapshot(ctx context.Context, store provisioner.ObjectStore, folder string, snap *provisioner.EtcdSnapshot, identity string) (string, error) {
	sealed, err := store.Get(ctx, folder+"/"+snap.Name)
	if err != nil {
		return "", fmt.Errorf("reading it: %w", err)
	}
	defer func() { _ = sealed.Close() }()
	f, err := a.plainFile("etcd-restore-*.db")
	if err != nil {
		return "", fmt.Errorf("making room for it: %w", err)
	}
	sum, _, err := provisioner.DecryptEtcdSnapshot(&ctxReader{ctx: ctx, r: sealed}, identity, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && snap.SHA256 != "" && sum != snap.SHA256 {
		err = errors.New("it does not match its checksum")
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err //nolint:wrapcheck // DecryptEtcdSnapshot names what failed
	}
	return f.Name(), nil
}
