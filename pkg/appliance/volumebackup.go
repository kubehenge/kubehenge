package appliance

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	// volumeBackupEvery and volumeBackupKeep are the backup volume's schedule, which needs no input.
	volumeBackupEvery = DefaultEtcdBackupHours * time.Hour
	volumeBackupKeep  = DefaultEtcdBackupKeep
	// ApplianceCopyCap bounds the appliance's copy of the latest backups on its own disk.
	ApplianceCopyCap = 1 << 30
)

// volumeBackupKeys is what the appliance keeps to back up onto the volume with nobody signed in:
// the age recipient the snapshots are encrypted to, its identity sealed with the passphrase, and
// the control-plane nodes.
type volumeBackupKeys struct {
	Recipient string    `json:"recipient"`
	Sealed    string    `json:"sealed"`
	Nodes     []string  `json:"nodes"`
	Since     time.Time `json:"since"`
}

// VolumeBackupStatus is how the backups on the management cluster's volume go.
type VolumeBackupStatus struct {
	// Claim is the backup volume's PersistentVolumeClaim, namespace/name.
	Claim  string                    `json:"claim"`
	Last   *provisioner.EtcdSnapshot `json:"last,omitempty"`
	Next   *time.Time                `json:"next,omitempty"`
	Error  string                    `json:"error,omitempty"`
	Failed *time.Time                `json:"failed,omitempty"`
	// Copy is the appliance's own copy of the latest snapshot.
	Copy *ApplianceCopy `json:"copy,omitempty"`
	// Export is the last export of Kubehenge's objects taken with a snapshot.
	Export *ExportRecord `json:"export,omitempty"`
}

// ApplianceCopy is the copy of the latest backups the appliance keeps on its own disk.
type ApplianceCopy struct {
	Snapshot string `json:"snapshot,omitempty"`
	Bytes    int64  `json:"bytes"`
	Cap      int64  `json:"cap"`
	Error    string `json:"error,omitempty"`
}

type volumeBackupRecord struct {
	Last   *provisioner.EtcdSnapshot `json:"last,omitempty"`
	Error  string                    `json:"error,omitempty"`
	Failed *time.Time                `json:"failed,omitempty"`
	Copy   *ApplianceCopy            `json:"copy,omitempty"`
	Export *ExportRecord             `json:"export,omitempty"`
}

func (a *Appliance) applianceCopy() backupstore.Dir {
	return backupstore.Dir{Root: filepath.Join(a.Dir, "backup-copy")}
}

// volumeIdentityKey is where the volume keeps the sealed identity of the snapshots encrypted to
// recipient. Each appliance that ever backed up there leaves its own.
func volumeIdentityKey(recipient string) string {
	return path.Join("management", "identities", recipient+".age")
}

// setUpVolumeBackups prepares the backups onto the volume after an apply, with no input: a new key
// sealed with the passphrase the first time, and the credential the snapshots are taken with.
func (a *Appliance) setUpVolumeBackups(ctx context.Context, admin []byte, passphrase string) error {
	keys, err := readJSONFile[volumeBackupKeys](a.etcdBackupFile("volume.json"))
	if errors.Is(err, fs.ErrNotExist) {
		keys, err = newVolumeBackupKeys(passphrase, a.KeyWorkFactor)
		if keys != nil {
			keys.Since = a.now()
		}
	}
	if err != nil {
		return err
	}
	var nodes etcdBackupKeys
	talosconfig, err := a.etcdBackupCredential(ctx, admin, &nodes)
	if err != nil {
		return err
	}
	keys.Nodes = nodes.Nodes
	if err := os.MkdirAll(a.etcdBackupDir(), privateDir); err != nil {
		return fmt.Errorf("keeping the backups' settings: %w", err)
	}
	if err := writeFileAtomic(a.etcdBackupFile("talosconfig"), talosconfig); err != nil {
		return err
	}
	return writeJSONFile(a.etcdBackupFile("volume.json"), keys)
}

func newVolumeBackupKeys(passphrase string, workFactor int) (*volumeBackupKeys, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("making the snapshots' key: %w", err)
	}
	sealed, err := sealIdentity(identity, passphrase, workFactor)
	if err != nil {
		return nil, err
	}
	return &volumeBackupKeys{Recipient: identity.Recipient().String(), Sealed: base64.StdEncoding.EncodeToString(sealed)}, nil
}

func (a *Appliance) volumeBackupKeys() *volumeBackupKeys {
	keys, _ := readJSONFile[volumeBackupKeys](a.etcdBackupFile("volume.json"))
	return keys
}

func (a *Appliance) volumeBackupRecord() *volumeBackupRecord {
	rec, _ := readJSONFile[volumeBackupRecord](a.etcdBackupFile("volume-status.json"))
	if rec == nil {
		rec = &volumeBackupRecord{}
	}
	return rec
}

// VolumeBackups returns how the backups on the volume go, or nil before they are set up.
func (a *Appliance) VolumeBackups() *VolumeBackupStatus {
	keys := a.volumeBackupKeys()
	if keys == nil {
		return nil
	}
	rec := a.volumeBackupRecord()
	st := &VolumeBackupStatus{
		Claim: backupstore.Namespace + "/" + backupstore.Claim,
		Last:  rec.Last, Error: rec.Error, Failed: rec.Failed, Copy: rec.Copy, Export: rec.Export,
	}
	// The first snapshot is due from the setup on.
	next := keys.Since
	if rec.Last != nil {
		next = rec.Last.Time.Add(volumeBackupEvery)
	}
	if rec.Failed != nil && (rec.Last == nil || rec.Failed.After(rec.Last.Time)) {
		next = later(next, rec.Failed.Add(etcdBackupRetry))
	}
	st.Next = &next
	return st
}

func (a *Appliance) volumeBackupDue(now time.Time) bool {
	st := a.VolumeBackups()
	return st != nil && !now.Before(*st.Next)
}

// openVolume reaches the store on the backup volume through the management cluster's API, as the
// interface.
func (a *Appliance) openVolume(ctx context.Context) (provisioner.ObjectStore, error) { //nolint:ireturn // tests stand in for the store
	if a.OpenVolume != nil {
		return a.OpenVolume(ctx)
	}
	raw, err := os.ReadFile(a.KubeconfigPath())
	if err != nil {
		return nil, fmt.Errorf("the interface's kubeconfig: %w", err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("the interface's kubeconfig: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("a client for the management cluster: %w", err)
	}
	secret, err := kube.CoreV1().Secrets(backupstore.Namespace).Get(ctx, backupstore.TokenSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("the backup volume's token: %w", err)
	}
	client, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("a client for the management cluster: %w", err)
	}
	return &backupstore.Client{Base: backupstore.ProxyBase(cfg.Host), Token: string(secret.Data[backupstore.TokenKey]), HTTP: client}, nil
}

// storeOnVolume stores the snapshot in file on the volume, encrypted, with the sealed key beside
// it, and keeps the newest volumeBackupKeep. The appliance then keeps a copy of it alone.
func (a *Appliance) storeOnVolume(ctx context.Context, keys *volumeBackupKeys, file string, taken provisioner.EtcdSnapshot) (*provisioner.EtcdSnapshot, error) {
	to, err := age.ParseX25519Recipient(keys.Recipient)
	if err != nil {
		return nil, fmt.Errorf("the snapshots' key: %w", err)
	}
	store, err := a.openVolume(ctx)
	if err != nil {
		return nil, err
	}
	key := volumeIdentityKey(keys.Recipient)
	if _, err := store.Stat(ctx, key); errors.Is(err, provisioner.ErrNoObject) {
		sealed, derr := base64.StdEncoding.DecodeString(keys.Sealed)
		if derr != nil {
			return nil, fmt.Errorf("the snapshots' sealed key: %w", derr)
		}
		if _, err := store.Put(ctx, key, bytes.NewReader(sealed), nil); err != nil {
			return nil, fmt.Errorf("storing the snapshots' key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("the backup volume: %w", err)
	}
	folder := provisioner.ManagementEtcdFolder("")
	if err := storeFile(ctx, store, folder, &taken, file, to); err != nil {
		return nil, fmt.Errorf("storing the snapshot on the backup volume: %w", err)
	}
	if _, err := provisioner.PruneEtcdSnapshots(ctx, store, folder, volumeBackupKeep); err != nil {
		return &taken, fmt.Errorf("deleting older snapshots on the backup volume: %w", err)
	}
	return &taken, nil
}

func storeFile(ctx context.Context, store provisioner.ObjectStore, folder string, taken *provisioner.EtcdSnapshot, file string, to age.Recipient) error {
	f, err := os.Open(file) //nolint:gosec // the appliance's own file
	if err != nil {
		return fmt.Errorf("reading the snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	return provisioner.StoreEtcdSnapshot(ctx, store, folder, taken, f, to) //nolint:wrapcheck // the caller names the store
}

func (a *Appliance) copyCap() int64 {
	if a.CopyCap > 0 {
		return a.CopyCap
	}
	return ApplianceCopyCap
}

// copyOnAppliance keeps the snapshot, encrypted, as the appliance's one copy, within its cap. A
// snapshot over the cap leaves the copy before it in place.
func (a *Appliance) copyOnAppliance(ctx context.Context, keys *volumeBackupKeys, file string, taken provisioner.EtcdSnapshot) *ApplianceCopy {
	c := &ApplianceCopy{Cap: a.copyCap()}
	d := a.applianceCopy()
	to, err := age.ParseX25519Recipient(keys.Recipient)
	if err == nil {
		if info, serr := os.Stat(file); serr == nil && info.Size() > c.Cap {
			err = fmt.Errorf("snapshot %s takes %d MiB, more than the appliance's copy may hold (%d MiB): the backup volume keeps it alone, and the appliance keeps its last copy that fit", taken.Name, info.Size()>>20, c.Cap>>20)
		}
	}
	if err == nil {
		err = storeFile(ctx, d, provisioner.ManagementEtcdFolder(""), &taken, file, to)
	}
	if err == nil {
		sealed, _ := base64.StdEncoding.DecodeString(keys.Sealed)
		_, err = d.Put(ctx, volumeIdentityKey(keys.Recipient), bytes.NewReader(sealed), nil)
	}
	if err == nil {
		_, err = provisioner.PruneEtcdSnapshots(ctx, d, provisioner.ManagementEtcdFolder(""), 1)
		c.Snapshot = taken.Name
	}
	c.Bytes, _ = d.Size(ctx)
	if err != nil {
		c.Error = err.Error()
	}
	return c
}

// snapshotToFile streams a snapshot of the management cluster's etcd into a file of the
// appliance's, from which each place the backups go reads it.
func (a *Appliance) snapshotToFile(ctx context.Context, nodes []string) (string, error) {
	talosconfig, err := os.ReadFile(a.etcdBackupFile("talosconfig"))
	if err != nil {
		return "", fmt.Errorf("the snapshots' talosconfig: %w", err)
	}
	stream, err := a.Talos.Snapshot(ctx, talosconfig, nodes)
	if err != nil {
		return "", fmt.Errorf("reaching etcd: %w", err)
	}
	defer func() { _ = stream.Close() }()
	f, err := os.CreateTemp(a.Dir, "etcd-snapshot-*.db")
	if err != nil {
		return "", fmt.Errorf("making room for the snapshot: %w", err)
	}
	_, err = io.Copy(f, &ctxReader{ctx: ctx, r: stream})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("reading the snapshot from etcd: %w", err)
	}
	return f.Name(), nil
}

type ctxReader struct {
	ctx context.Context //nolint:containedctx // bounds one copy
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err //nolint:wrapcheck // the context's own
	}
	return c.r.Read(p) //nolint:wrapcheck // io.Reader's own errors, io.EOF among them
}

// backUpToVolume takes a snapshot onto the volume and the appliance's copy, and records how it went.
func (a *Appliance) backUpToVolume(ctx context.Context, keys *volumeBackupKeys, file string, taken provisioner.EtcdSnapshot) (*provisioner.EtcdSnapshot, error) {
	stored, err := a.storeOnVolume(ctx, keys, file, taken)
	rec := a.volumeBackupRecord()
	rec.Copy = a.copyOnAppliance(ctx, keys, file, taken)
	export, exportErr := a.exportToVolume(ctx, keys, taken.Name)
	if exportErr == nil {
		rec.Export = export
	}
	err = errors.Join(err, exportErr)
	if err != nil {
		now := a.now()
		rec.Error, rec.Failed = err.Error(), &now
	} else {
		rec.Last, rec.Error, rec.Failed = stored, "", nil
	}
	if werr := writeJSONFile(a.etcdBackupFile("volume-status.json"), rec); werr != nil {
		err = errors.Join(err, werr)
	}
	return stored, err
}

// backupVolumeAddress is the backup volume's VM in the module.
const backupVolumeAddress = "proxmox_virtual_environment_vm.backups"

// releaseBackupVolume prepares the backup volume for a destroy of the module. Kept, its VM leaves
// OpenTofu's state, so that the destroy passes it by. Removed, its protection is lifted, so that
// the destroy deletes it with its disk.
func (a *Appliance) releaseBackupVolume(ctx context.Context, j *Job, env []string, remove bool) error {
	if remove {
		j.Add("Lifting the backup volume's protection, so that it is removed too")
		return a.tofu(ctx, j, env, jsonMessage, "apply", "-auto-approve", "-input=false", "-json",
			"-target="+backupVolumeAddress, "-var=backup_volume={protection=false}")
	}
	held := false
	err := a.tofu(ctx, j, env, func(line string) string {
		held = held || strings.TrimSpace(line) == backupVolumeAddress
		return ""
	}, "state", "list", backupVolumeAddress)
	if err != nil || !held {
		return err
	}
	j.Add("Keeping the backup volume and its VM " + backupVolumeAddress + " on Proxmox VE")
	return a.tofu(ctx, j, env, plainLine, "state", "rm", backupVolumeAddress)
}

// volumeBackupFailed records why a snapshot for the volume could not be taken at all.
func (a *Appliance) volumeBackupFailed(err error) {
	rec := a.volumeBackupRecord()
	now := a.now()
	rec.Error, rec.Failed = err.Error(), &now
	if werr := writeJSONFile(a.etcdBackupFile("volume-status.json"), rec); werr != nil && a.Log != nil {
		a.Log.Error("recording a failed backup", "error", werr)
	}
}
