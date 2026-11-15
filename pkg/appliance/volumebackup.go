package appliance

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// volumeTalosconfig takes the volume's snapshots. It is apart from the bucket's, which goes when
// the bucket's backups are switched off.
const volumeTalosconfig = "volume-talosconfig"

// volumeBackupKeys is what the appliance keeps to back up onto the volume with nobody signed in:
// the age recipient the snapshots are encrypted to, its identity sealed with the passphrase, and
// the control-plane nodes.
type volumeBackupKeys struct {
	Recipient string    `json:"recipient"`
	Sealed    string    `json:"sealed"`
	Nodes     []string  `json:"nodes"`
	Since     time.Time `json:"since"`
	// SharesHost is the storage chart's word that one Proxmox VE host's loss takes the volume.
	SharesHost bool `json:"sharesHost,omitempty"`
}

// sharesHost annotates the store's Deployment, from the module: "true" where the volume sits on a
// node's own storage or the Proxmox VE cluster has one node, so that losing a host loses it.
const sharesHost = "kubehenge.dev/shares-host"

// VolumeBackupStatus is how the backups on the management cluster's volume go.
type VolumeBackupStatus struct {
	// Claim is the backup volume's PersistentVolumeClaim, namespace/name.
	Claim  string                    `json:"claim"`
	Last   *provisioner.EtcdSnapshot `json:"last,omitempty"`
	Next   *time.Time                `json:"next,omitempty"`
	Error  string                    `json:"error,omitempty"`
	Failed *time.Time                `json:"failed,omitempty"`
	// Copy is the appliance's own copy of the latest snapshot.
	Copy *BackupCopy `json:"copy,omitempty"`
	// Export is the last export of Kubehenge's objects taken with a snapshot.
	Export *ExportRecord `json:"export,omitempty"`
	// SharesHost is set where one Proxmox VE host's loss can take the volume, and the appliance's
	// copy with it.
	SharesHost bool `json:"sharesHost,omitempty"`
}

// BackupCopy is the copy of the latest backups the appliance keeps on its own disk.
type BackupCopy struct {
	Snapshot string `json:"snapshot,omitempty"`
	Bytes    int64  `json:"bytes"`
	Cap      int64  `json:"cap"`
	Error    string `json:"error,omitempty"`
}

type volumeBackupRecord struct {
	Last   *provisioner.EtcdSnapshot `json:"last,omitempty"`
	Error  string                    `json:"error,omitempty"`
	Failed *time.Time                `json:"failed,omitempty"`
	Copy   *BackupCopy               `json:"copy,omitempty"`
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
// sealed with the passphrase the first time, and the credential the snapshots are taken with. talos
// is the management cluster's talosconfig and admin its admin kubeconfig.
func (a *Appliance) setUpVolumeBackups(ctx context.Context, talos, admin []byte, passphrase string) error {
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
	talosconfig, err := a.etcdBackupCredential(ctx, talos, &nodes)
	if err != nil {
		return err
	}
	keys.Nodes = nodes.Nodes
	keys.SharesHost = a.volumeSharesHost(ctx, admin, keys.SharesHost)
	if err := os.MkdirAll(a.etcdBackupDir(), privateDir); err != nil {
		return fmt.Errorf("keeping the backups' settings: %w", err)
	}
	if err := writeFileAtomic(a.etcdBackupFile(volumeTalosconfig), talosconfig); err != nil {
		return err
	}
	return writeJSONFile(a.etcdBackupFile("volume.json"), keys)
}

// volumeSharesHost reads the store's sharesHost annotation. A cluster that does not answer keeps
// what was read before, since the warning is advice and never holds an apply up.
func (a *Appliance) volumeSharesHost(ctx context.Context, admin []byte, before bool) bool {
	kube, err := a.managementClient(admin)
	if err == nil {
		var d *appsv1.Deployment
		if d, err = kube.AppsV1().Deployments(backupstore.Namespace).Get(ctx, backupstore.Service, metav1.GetOptions{}); err == nil {
			return d.Annotations[sharesHost] == "true"
		}
	}
	if a.Log != nil {
		a.Log.Warn("reading whether the backup volume shares its host", "error", err)
	}
	return before
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
		SharesHost: keys.SharesHost,
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
	api, err := a.interfaceAPI()
	if err != nil {
		return nil, err
	}
	if err = a.wakeStore(ctx, api.kube); err != nil {
		return nil, err
	}
	secret, err := api.kube.CoreV1().Secrets(backupstore.Namespace).Get(ctx, backupstore.TokenSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("the backup volume's token: %w", err)
	}
	return &backupstore.Client{Base: backupstore.ProxyBase(api.host), Token: string(secret.Data[backupstore.TokenKey]), HTTP: api.http}, nil
}

// interfaceAPI is the management cluster's API as the interface reaches it.
type interfaceAPI struct {
	host string
	kube kubernetes.Interface
	http *http.Client
}

func (a *Appliance) interfaceAPI() (*interfaceAPI, error) {
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
	client, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("a client for the management cluster: %w", err)
	}
	return &interfaceAPI{host: cfg.Host, kube: kube, http: client}, nil
}

// SnapshotVolume is the backup volume as the interface reaches it for the clusters' etcd
// snapshots, through the API server. Opening it asks the appliance to wake it, under a request of
// the interface's own.
func (a *Appliance) SnapshotVolume() (provisioner.BackupVolume, error) { //nolint:ireturn // the controller's and the interface's volume are one interface
	api, err := a.interfaceAPI()
	if err != nil {
		return nil, err
	}
	return &backupstore.Volume{Kube: api.kube, HTTP: api.http, Base: backupstore.ProxyBase(api.host), Annotation: backupstore.InterfaceWakeAnnotation}, nil
}

// idleReplicas annotates the store's Deployment with the replicas it keeps between backups. The
// storage chart sets it: 0 on a node's own storage, whose disk then stays detached, so that
// Proxmox VE can migrate the node VMs and a node can go into maintenance.
const idleReplicas = "kubehenge.dev/idle-replicas"

// storeReadyPoll is how often a wake looks for the store's pod to be ready.
var storeReadyPoll = 2 * time.Second

// wakeStore starts the backup volume's store when it sleeps and waits, within ctx, until its pod is
// ready, which is once the plugin has attached the disk to that pod's node. Only a store on a node's
// own storage sleeps, and its pod must land on a node VM on that node, so the nodes' zone labels are
// set first. The store stays asleep only when no management VM runs on the node holding the disk.
func (a *Appliance) wakeStore(ctx context.Context, kube kubernetes.Interface) error {
	deps := kube.AppsV1().Deployments(backupstore.Namespace)
	d, err := deps.Get(ctx, backupstore.Service, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("the backup volume's store: %w", err)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas == 0 {
		unknown, zerr := a.followZones(ctx, kube, d.Annotations[volumeZone])
		if zerr != nil {
			return zerr
		}
		if len(unknown) > 0 && a.Log != nil {
			a.Log.Warn("Proxmox VE lists no VM for these management nodes, whose zone labels stay as they are", "nodes", unknown)
		}
		if _, err = deps.Patch(ctx, backupstore.Service, types.MergePatchType, []byte(`{"spec":{"replicas":1}}`), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("starting the backup volume's store: %w", err)
		}
	}
	for d.Status.ReadyReplicas < 1 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("the backup volume's store did not start: %w", ctx.Err())
		case <-time.After(storeReadyPoll):
		}
		if d, err = deps.Get(ctx, backupstore.Service, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("the backup volume's store: %w", err)
		}
	}
	return nil
}

// restStore puts the backup volume's store back to the replicas its chart keeps between backups,
// which detaches a node-local disk again. A store the controller asked for stays awake.
func restStore(ctx context.Context, kube kubernetes.Interface, now time.Time) error {
	deps := kube.AppsV1().Deployments(backupstore.Namespace)
	d, err := deps.Get(ctx, backupstore.Service, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("the backup volume's store: %w", err)
	}
	idle, ok := d.Annotations[idleReplicas]
	if !ok || (d.Spec.Replicas != nil && strconv.Itoa(int(*d.Spec.Replicas)) == idle) || backupstore.WakeRequested(d, now) {
		return nil
	}
	if _, err := strconv.Atoi(idle); err != nil {
		return fmt.Errorf("the backup volume's store: %s %q is no number", idleReplicas, idle)
	}
	patch := []byte(`{"spec":{"replicas":` + idle + `}}`)
	if _, err := deps.Patch(ctx, backupstore.Service, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("putting the backup volume's store back to sleep: %w", err)
	}
	return nil
}

// sleepVolume puts the store back once a backup or a recovery is done with it. Tests that stand in
// for the store have none.
func (a *Appliance) sleepVolume(ctx context.Context) error {
	if a.OpenVolume != nil {
		return nil
	}
	kube, err := a.interfaceClient()
	if err != nil {
		return err
	}
	return restStore(ctx, kube, time.Now())
}

// volumeWakeTimeout bounds a wake the controller asked for.
const volumeWakeTimeout = 5 * time.Minute

// ServeVolumeRequests wakes the backup volume's store while the controller asks for it, to store
// the etcd snapshots of clusters without a bucket target, and puts it back to sleep once no request
// is left. A backup or a recovery of the appliance's own keeps the store as it is. kube nil reaches
// the management cluster as the interface.
func (a *Appliance) ServeVolumeRequests(ctx context.Context, kube kubernetes.Interface, now time.Time) error {
	if st := a.EtcdBackups(); st.Taking || a.Idle() != nil || a.VolumeBackups() == nil {
		return nil
	}
	if kube == nil {
		var err error
		if kube, err = a.interfaceClient(); err != nil {
			return err
		}
	}
	d, err := kube.AppsV1().Deployments(backupstore.Namespace).Get(ctx, backupstore.Service, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("the backup volume's store: %w", err)
	}
	if !backupstore.WakeRequested(d, now) {
		return restStore(ctx, kube, now)
	}
	if d.Status.ReadyReplicas > 0 {
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, volumeWakeTimeout)
	defer cancel()
	return a.wakeStore(wctx, kube)
}

// storeOnVolume stores the snapshot in file on the volume, encrypted, with the sealed key beside
// it, and keeps the newest volumeBackupKeep. The appliance then keeps a copy of it alone.
func (a *Appliance) storeOnVolume(ctx context.Context, keys *volumeBackupKeys, file string, taken *provisioner.EtcdSnapshot) (*provisioner.EtcdSnapshot, error) {
	// Each place fills in its own record of the snapshot.
	snap := *taken
	to, err := age.ParseX25519Recipient(keys.Recipient)
	if err != nil {
		return nil, fmt.Errorf("the snapshots' key: %w", err)
	}
	store, err := a.openVolume(ctx)
	if err != nil {
		return nil, err
	}
	key := volumeIdentityKey(keys.Recipient)
	if _, err = store.Stat(ctx, key); errors.Is(err, provisioner.ErrNoObject) {
		sealed, derr := base64.StdEncoding.DecodeString(keys.Sealed)
		if derr != nil {
			return nil, fmt.Errorf("the snapshots' sealed key: %w", derr)
		}
		if _, err = store.Put(ctx, key, bytes.NewReader(sealed), nil); err != nil {
			return nil, fmt.Errorf("storing the snapshots' key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("the backup volume: %w", err)
	}
	folder := provisioner.ManagementEtcdFolder("")
	if err = storeFile(ctx, store, folder, &snap, file, to); err != nil {
		return nil, fmt.Errorf("storing the snapshot on the backup volume: %w", err)
	}
	if _, err = provisioner.PruneEtcdSnapshots(ctx, store, folder, volumeBackupKeep); err != nil {
		return &snap, fmt.Errorf("deleting older snapshots on the backup volume: %w", err)
	}
	return &snap, nil
}

func storeFile(ctx context.Context, store provisioner.ObjectStore, folder string, taken *provisioner.EtcdSnapshot, file string, to age.Recipient) error {
	f, err := os.Open(file) //nolint:gosec // the appliance's own file
	if err != nil {
		return fmt.Errorf("reading the snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	return provisioner.StoreEtcdSnapshot(ctx, store, folder, taken, f, to) //nolint:wrapcheck // the caller names the store
}

// copyOwnerKey names, in the appliance's copy, the recipient of the management cluster the copy is
// of. Each setup makes a key of its own, so a copy left by a removed cluster names another.
const copyOwnerKey = "management/cluster"

// maxRecipient bounds what is read of the owner's recipient, an age X25519 recipient of 62 bytes.
const maxRecipient = 256

// ErrForeignCopy refuses the appliance's copy of a management cluster other than this one.
var ErrForeignCopy = errors.New("the appliance's copy holds the backups of a management cluster removed since, not of this one. It is not restored onto this cluster")

// checkCopyOwner tells whether the appliance's copy is of the management cluster whose snapshots
// are encrypted to recipient.
func (a *Appliance) checkCopyOwner(ctx context.Context, recipient string) error {
	r, err := a.applianceCopy().Get(ctx, copyOwnerKey)
	if err != nil {
		if errors.Is(err, provisioner.ErrNoObject) || errors.Is(err, fs.ErrNotExist) {
			return ErrForeignCopy
		}
		return fmt.Errorf("the appliance's copy: %w", err)
	}
	defer func() { _ = r.Close() }()
	owner, err := io.ReadAll(io.LimitReader(r, maxRecipient))
	if err != nil {
		return fmt.Errorf("the appliance's copy: %w", err)
	}
	if string(owner) != recipient {
		return ErrForeignCopy
	}
	return nil
}

func (a *Appliance) copyCap() int64 {
	if a.CopyCap > 0 {
		return a.CopyCap
	}
	return ApplianceCopyCap
}

// copyOnAppliance keeps the snapshot, encrypted, with its export and key, as the appliance's one
// copy, within its cap. A copy over the cap, or one that fails, leaves the copy before it, prev, in
// place.
func (a *Appliance) copyOnAppliance(ctx context.Context, keys *volumeBackupKeys, file string, taken *provisioner.EtcdSnapshot, export []byte, prev *BackupCopy) *BackupCopy {
	c := &BackupCopy{Cap: a.copyCap()}
	err := a.replaceCopy(ctx, keys, file, taken, export, c.Cap)
	switch {
	case err == nil:
		c.Snapshot = taken.Name
	case prev != nil:
		c.Snapshot = prev.Snapshot
	}
	c.Bytes, _ = a.applianceCopy().Size(ctx)
	if err != nil {
		c.Error = err.Error()
	}
	return c
}

// The appliance's copy is made whole beside the one in use, then swapped in, so that its size is
// known before it replaces the last that fit, and a crash leaves one whole copy (sweepCopy).
const (
	copyStaging  = ".new"
	copyReplaced = ".old"
)

func (a *Appliance) replaceCopy(ctx context.Context, keys *volumeBackupKeys, file string, taken *provisioner.EtcdSnapshot, export []byte, capBytes int64) error {
	// The copy fills in its own record of the snapshot.
	snap := *taken
	to, err := age.ParseX25519Recipient(keys.Recipient)
	if err != nil {
		return fmt.Errorf("the snapshots' key: %w", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(keys.Sealed)
	if err != nil {
		return fmt.Errorf("the snapshots' sealed key: %w", err)
	}
	live := a.applianceCopy().Root
	staged := backupstore.Dir{Root: live + copyStaging}
	if err = os.RemoveAll(staged.Root); err != nil {
		return fmt.Errorf("clearing room for the appliance's copy: %w", err)
	}
	defer func() { _ = os.RemoveAll(staged.Root) }()
	err = storeFile(ctx, staged, provisioner.ManagementEtcdFolder(""), &snap, file, to)
	if err == nil {
		_, err = staged.Put(ctx, volumeIdentityKey(keys.Recipient), bytes.NewReader(sealed), nil)
	}
	if err == nil {
		_, err = staged.Put(ctx, copyOwnerKey, strings.NewReader(keys.Recipient), nil)
	}
	if err == nil && export != nil {
		_, err = staged.Put(ctx, exportKey(taken.Name), bytes.NewReader(export), nil)
	}
	if err != nil {
		return fmt.Errorf("the appliance's copy: %w", err)
	}
	size, err := staged.Size(ctx)
	if err != nil {
		return fmt.Errorf("the appliance's copy: %w", err)
	}
	if size > capBytes {
		return fmt.Errorf("the appliance's copy of snapshot %s with its export would take %s, more than its cap of %s. The backup volume keeps it alone, and the appliance keeps its last copy that fit", taken.Name, mebibytes(size), mebibytes(capBytes))
	}
	old := live + copyReplaced
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("the appliance's copy: %w", err)
	}
	if err := os.Rename(live, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the appliance's copy: %w", err)
	}
	if err := os.Rename(staged.Root, live); err != nil {
		_ = os.Rename(old, live)
		return fmt.Errorf("the appliance's copy: %w", err)
	}
	_ = os.RemoveAll(old)
	return nil
}

// sweepCopy finishes or undoes a swap of the appliance's copy a crash cut short.
func (a *Appliance) sweepCopy() error {
	live := a.applianceCopy().Root
	if _, err := os.Stat(live); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(live+copyReplaced, live); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("restoring the appliance's copy: %w", err)
		}
	}
	return errors.Join(os.RemoveAll(live+copyStaging), os.RemoveAll(live+copyReplaced))
}

// mebibytes writes a size in MiB, rounded up, so that a size just over a cap never reads as it.
func mebibytes(n int64) string {
	if n < mib {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%d MiB", (n+mib-1)/mib)
}

// CleanUp clears what a run cut short by a crash or a power loss left on the appliance's disk. It
// runs at start, before any run or backup.
func (a *Appliance) CleanUp() error {
	return errors.Join(a.sweepCopy(), os.RemoveAll(a.plainDir()))
}

// plainDir holds the unencrypted etcd snapshots a backup or a restore works on, each removed once
// it is done, and the folder whole at the next start after a crash.
func (a *Appliance) plainDir() string {
	return filepath.Join(a.Dir, "plain")
}

func (a *Appliance) plainFile(pattern string) (*os.File, error) {
	if err := os.MkdirAll(a.plainDir(), privateDir); err != nil {
		return nil, err //nolint:wrapcheck // the caller says what it made room for
	}
	return os.CreateTemp(a.plainDir(), pattern) //nolint:wrapcheck // the caller says what it made room for
}

// snapshotToFile streams a snapshot of the management cluster's etcd, taken with the talosconfig
// in credential, into a file of the appliance's, from which each place the backups go reads it.
func (a *Appliance) snapshotToFile(ctx context.Context, credential string, nodes []string) (string, error) {
	talosconfig, err := os.ReadFile(a.etcdBackupFile(credential))
	if err != nil {
		return "", fmt.Errorf("the snapshots' talosconfig: %w", err)
	}
	stream, err := a.Talos.Snapshot(ctx, talosconfig, nodes)
	if err != nil {
		return "", fmt.Errorf("reaching etcd: %w", err)
	}
	defer func() { _ = stream.Close() }()
	f, err := a.plainFile("etcd-snapshot-*.db")
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
func (a *Appliance) backUpToVolume(ctx context.Context, keys *volumeBackupKeys, file string, taken *provisioner.EtcdSnapshot) (*provisioner.EtcdSnapshot, error) {
	stored, err := a.storeOnVolume(ctx, keys, file, taken)
	rec := a.volumeBackupRecord()
	export, sealedExport, exportErr := a.exportToVolume(ctx, keys, taken.Name)
	err = errors.Join(err, a.sleepVolume(ctx))
	if exportErr == nil {
		rec.Export = export
	}
	// A copy cut short by a stop keeps the record of the last one, which is still in place.
	if c := a.copyOnAppliance(ctx, keys, file, taken, sealedExport, rec.Copy); c.Error == "" || !snapshotStopped(ctx) {
		rec.Copy = c
	}
	err = errors.Join(err, exportErr)
	switch {
	case err == nil:
		rec.Last, rec.Error, rec.Failed = stored, "", nil
	case !snapshotStopped(ctx):
		now := a.now()
		rec.Error, rec.Failed = err.Error(), &now
	}
	if werr := writeJSONFile(a.etcdBackupFile("volume-status.json"), rec); werr != nil {
		err = errors.Join(err, werr)
	}
	return stored, err
}

// BackupSummary is where the management backups are, when the last was taken and why the last one
// failed, over the backup volume, the appliance's copy and a target.
type BackupSummary struct {
	Where []string   `json:"where"`
	Last  *time.Time `json:"last,omitempty"`
	Error string     `json:"error,omitempty"`
	// SameHost warns that every backup can be lost with one Proxmox VE host: the volume shares it
	// and no bucket target is set.
	SameHost bool `json:"sameHost,omitempty"`
}

// Summary sums the backups' status up.
func (st *EtcdBackupStatus) Summary() *BackupSummary {
	s := &BackupSummary{}
	var errs []string
	newer := func(t time.Time) {
		if s.Last == nil || t.After(*s.Last) {
			s.Last = &t
		}
	}
	if v := st.Volume; v != nil {
		s.Where = append(s.Where, "backup volume "+v.Claim)
		if v.Copy != nil && v.Copy.Snapshot != "" {
			s.Where = append(s.Where, "appliance copy")
		}
		if v.Last != nil {
			newer(v.Last.Time)
		}
		if v.Error != "" {
			errs = append(errs, "backup volume: "+v.Error)
		}
		if v.Copy != nil && v.Copy.Error != "" {
			errs = append(errs, "appliance copy: "+v.Copy.Error)
		}
	}
	if st.Settings != nil {
		s.Where = append(s.Where, "target "+st.Settings.Target)
		if st.Last != nil {
			newer(st.Last.Time)
		}
		if st.Error != "" {
			errs = append(errs, "target "+st.Settings.Target+": "+st.Error)
		}
	}
	s.Error = strings.Join(errs, "; ")
	s.SameHost = st.Volume != nil && st.Volume.SharesHost && st.Settings == nil
	return s
}

// copyNearCap is the share of its cap, in per cent, from which the appliance's copy counts as near
// it.
const copyNearCap = 80

// copyRoom is how many times the copy's size the next snapshot needs free on the disk: its plain
// file, and its new copy beside the old one.
const copyRoom = 2

// CopyTight returns the appliance's copy of the latest backups when the next may not fit: the copy
// is near its cap, or the disk's free bytes hold less than the next snapshot needs. Nil while there
// is room.
func (a *Appliance) CopyTight(free uint64) *BackupCopy {
	st := a.VolumeBackups()
	if st == nil || st.Copy == nil {
		return nil
	}
	c := *st.Copy
	c.Cap = a.copyCap()
	if c.Bytes*100 >= c.Cap*copyNearCap || free < uint64(copyRoom*c.Bytes) { //nolint:gosec // a size is never negative
		return &c
	}
	return nil
}

// moduleAddress prefixes an address of the module's with the root config's call of it.
const moduleAddress = "module.management."

// backupVolumeAddress is the backup volume's VM, as the root config's state names it.
const backupVolumeAddress = moduleAddress + "proxmox_virtual_environment_vm.backups"

// liftBackupVolume lifts the backup volume's protection before a destroy of the module, so that
// the destroy deletes it with its disk.
func (a *Appliance) liftBackupVolume(ctx context.Context, j *Job, env []string) error {
	// A targeted apply makes the backups VM when the state lacks it, as a removal of a cluster whose
	// setup stopped before that VM must not.
	held, err := a.backupVolumeHeld(ctx, j, env)
	if err != nil || !held {
		return err
	}
	j.Add("Lifting the backup volume's protection, so that it is removed too")
	return a.tofu(ctx, j, env, jsonMessage, "apply", "-auto-approve", "-input=false", "-json",
		"-target="+backupVolumeAddress, "-var=backup_volume_protection=false")
}

// keepBackupVolume takes the backup volume's VM out of OpenTofu's state before a destroy of the
// module, so that the destroy passes it by.
func (a *Appliance) keepBackupVolume(ctx context.Context, j *Job, env []string) error {
	held, err := a.backupVolumeHeld(ctx, j, env)
	if err != nil || !held {
		return err
	}
	j.Add("Keeping the backup volume and its VM " + backupVolumeAddress + " on Proxmox VE")
	return a.tofu(ctx, j, env, plainLine, "state", "rm", backupVolumeAddress)
}

// backupVolumeHeld reports whether OpenTofu's state holds the backup volume's VM. It reads the whole
// list, since OpenTofu fails on an address the state does not hold, as with a cluster set up before
// the backup volume existed or one whose setup stopped short of it.
func (a *Appliance) backupVolumeHeld(ctx context.Context, j *Job, env []string) (bool, error) {
	held := false
	err := a.tofu(ctx, j, env, func(line string) string {
		held = held || strings.TrimSpace(line) == backupVolumeAddress
		return ""
	}, "state", "list")
	return held, err
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
