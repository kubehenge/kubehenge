package appliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// The bounds and defaults of the management cluster's etcd backups.
const (
	DefaultEtcdBackupHours = 6
	DefaultEtcdBackupKeep  = 28
	maxEtcdBackupHours     = 168
	maxEtcdBackupKeep      = 500
	// etcdBackupRetry is how soon a snapshot that failed is tried again.
	etcdBackupRetry = 15 * time.Minute
	// etcdBackupCredentialTTL is how long the talosconfig the snapshots are taken with lasts. Every
	// apply and every save of the settings makes a new one.
	etcdBackupCredentialTTL = 365 * 24 * time.Hour
	// etcdBackupTimeout bounds one snapshot, taken and stored.
	etcdBackupTimeout = 30 * time.Minute
)

// ErrNoEtcdBackups is a snapshot or restore asked for while backups are not set up.
var ErrNoEtcdBackups = errors.New("backups of the management cluster's etcd are not set up")

// ErrEtcdSnapshotTaking refuses a snapshot while another is being taken.
var ErrEtcdSnapshotTaking = errors.New("a snapshot is being taken already")

// ErrEtcdRecovering refuses a snapshot while etcd is being restored. It would store the cluster
// half made, as the newest backup the restore then reads.
var ErrEtcdRecovering = errors.New("the management cluster is being recovered. Take a snapshot once that has finished")

// ErrEtcdRemoving refuses a snapshot while the management cluster is being removed. It would
// write the removed cluster's backup back after the removal cleared it.
var ErrEtcdRemoving = errors.New("the management cluster is being removed")

// snapshotRefusals are the runs no snapshot may start beside, each with its refusal. Each stops a
// snapshot under way before it goes on (stopEtcdSnapshot).
var snapshotRefusals = map[string]error{
	"recover": ErrEtcdRecovering, "recreate": ErrEtcdRecovering, "etcd-restore": ErrEtcdRecovering,
	"remove": ErrEtcdRemoving,
}

// snapshotRun is an etcd snapshot under way. Its owner closes done once it has returned, after
// its last write to the appliance's disk.
type snapshotRun struct {
	done chan struct{}
	// cancel ends the snapshot's context, once the snapshot has made it. cancelled asks for that
	// before then.
	cancel    context.CancelCauseFunc
	cancelled bool
}

// errSnapshotStopped is the cause a stopped snapshot's context carries. Such a snapshot did not
// fail: the run that stopped it decides what happens to the backups, so its error is recorded
// nowhere and the backups' status keeps the last real result.
var errSnapshotStopped = errors.New("the etcd snapshot was stopped for a recovery or a removal")

// snapshotStopped tells whether a snapshot's context ended because a run stopped it.
func snapshotStopped(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errSnapshotStopped)
}

// stopEtcdSnapshot cancels the snapshot under way, if any, and waits for it to return. A run that
// restores or removes the management cluster calls it first, once it holds the job, so that no
// snapshot can start after it either (claimEtcdSnapshot).
func (a *Appliance) stopEtcdSnapshot(ctx context.Context, j *Job) error {
	a.mu.Lock()
	run := a.snapshot
	if run != nil {
		run.cancelled = true
		if run.cancel != nil {
			run.cancel(errSnapshotStopped)
		}
	}
	a.mu.Unlock()
	if run == nil {
		return nil
	}
	j.Add("Stopping the etcd snapshot under way")
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the etcd snapshot under way did not stop: %w", ctx.Err())
	}
}

// TalosAccess reaches the management cluster's control-plane nodes for its etcd backups, and for
// its first etcd start. provisioner.ManagementTalos is the real one.
type TalosAccess interface {
	BackupConfig(ctx context.Context, admin []byte, address string, ttl time.Duration) ([]byte, error)
	Snapshot(ctx context.Context, talosconfig []byte, nodes []string) (io.ReadCloser, error)
	Restore(ctx context.Context, admin []byte, nodes []string, open func() (io.ReadCloser, error), log func(string)) error
	StartFirstEtcd(ctx context.Context, admin []byte, nodes []string, starts map[string]provisioner.EtcdStart, record func() error, log func(string))
}

// EtcdBackupSettings is how the management cluster's etcd is backed up: into which of the backup
// targets, how often, and how many snapshots are kept.
type EtcdBackupSettings struct {
	Target     string    `json:"target"`
	EveryHours int       `json:"everyHours"`
	Keep       int       `json:"keep"`
	By         string    `json:"by,omitempty"`
	Since      time.Time `json:"since"`
	// SpecRecipients are age public keys, besides the appliance's own, that each cluster spec's
	// Secrets are sealed to. A holder of the matching private key opens a spec without the appliance.
	SpecRecipients []string `json:"specRecipients,omitempty"`
}

// maxSpecRecipients bounds the extra recipients of a spec's Secrets.
const maxSpecRecipients = 8

// Validate checks the settings against the targets there are.
func (s *EtcdBackupSettings) Validate(targets []BackupTarget) error {
	var errs []error
	if !slices.ContainsFunc(targets, func(t BackupTarget) bool { return t.Name == s.Target }) {
		errs = append(errs, fmt.Errorf("target: %q is not one of the backup targets", s.Target))
	}
	if s.EveryHours < 1 || s.EveryHours > maxEtcdBackupHours {
		errs = append(errs, fmt.Errorf("every: 1 to %d hours", maxEtcdBackupHours))
	}
	if s.Keep < 1 || s.Keep > maxEtcdBackupKeep {
		errs = append(errs, fmt.Errorf("keep: 1 to %d snapshots", maxEtcdBackupKeep))
	}
	if len(s.SpecRecipients) > maxSpecRecipients {
		errs = append(errs, fmt.Errorf("specRecipients: at most %d keys", maxSpecRecipients))
	}
	for i, key := range s.SpecRecipients {
		if _, err := age.ParseX25519Recipient(key); err != nil {
			errs = append(errs, fmt.Errorf("specRecipients: %q is not an age public key (age1...)", key))
		} else if slices.Contains(s.SpecRecipients[:i], key) {
			errs = append(errs, fmt.Errorf("specRecipients: %s is given twice", key))
		}
	}
	return errors.Join(errs...)
}

// EtcdBackupStatus is what the interface shows of the backups.
type EtcdBackupStatus struct {
	Settings *EtcdBackupSettings       `json:"settings,omitempty"`
	Last     *provisioner.EtcdSnapshot `json:"last,omitempty"`
	Next     *time.Time                `json:"next,omitempty"`
	Error    string                    `json:"error,omitempty"`
	Failed   *time.Time                `json:"failed,omitempty"`
	// Taking is set while a snapshot is being taken.
	Taking bool `json:"taking,omitempty"`
	// Warning stands while the backups are off.
	Warning *EtcdBackupWarning `json:"warning,omitempty"`
	// ExportDue is set when no export of the appliance's state was taken since the settings it
	// carries last changed, after a snapshot succeeded.
	ExportDue  bool       `json:"exportDue,omitempty"`
	ExportedAt *time.Time `json:"exportedAt,omitempty"`
	// SpecKey is set once the appliance holds the recovery key that opens the stored specs' Secrets.
	// Only an export carries it.
	SpecKey bool `json:"specKey,omitempty"`
	// Volume is how the backups on the management cluster's own volume go, which need no settings.
	Volume *VolumeBackupStatus `json:"volume,omitempty"`
	// Where sums up where the backups are, when the last ran and why one failed.
	Where *BackupSummary `json:"summary,omitempty"`
}

// etcdBackupKeys is what the appliance keeps to take snapshots with nobody signed in: the target's
// access key, the age recipient snapshots are encrypted to, and the control-plane nodes.
type etcdBackupKeys struct {
	AccessKeyID     string   `json:"accessKeyID"`
	SecretAccessKey string   `json:"secretAccessKey"`
	Recipient       string   `json:"recipient"`
	Nodes           []string `json:"nodes"`
}

// etcdBackupRecord is how the last snapshot went.
type etcdBackupRecord struct {
	Last   *provisioner.EtcdSnapshot `json:"last,omitempty"`
	Error  string                    `json:"error,omitempty"`
	Failed *time.Time                `json:"failed,omitempty"`
}

func (a *Appliance) etcdBackupDir() string { return filepath.Join(a.Dir, "etcd-backup") }

func (a *Appliance) etcdBackupFile(name string) string { return filepath.Join(a.etcdBackupDir(), name) }

func readJSONFile[T any](path string) (*T, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err //nolint:wrapcheck // callers tell a missing file from others
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return &v, nil
}

func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	return writeFileAtomic(path, raw)
}

// EtcdBackups returns the backups' settings and how the last snapshot went.
func (a *Appliance) EtcdBackups() *EtcdBackupStatus {
	st := &EtcdBackupStatus{}
	st.Settings, _ = readJSONFile[EtcdBackupSettings](a.etcdBackupFile("settings.json"))
	if rec, err := readJSONFile[etcdBackupRecord](a.etcdBackupFile("status.json")); err == nil {
		st.Last, st.Error, st.Failed = rec.Last, rec.Error, rec.Failed
	}
	if st.Settings != nil {
		next := nextEtcdBackup(st)
		st.Next = &next
	}
	a.mu.Lock()
	st.Taking = a.snapshot != nil
	a.mu.Unlock()
	st.Volume = a.VolumeBackups()
	st.Warning = a.etcdBackupWarning(st)
	st.ExportDue, st.ExportedAt = a.exportDue(st)
	_, keyErr := os.Stat(a.specKeyFile())
	st.SpecKey = keyErr == nil
	if sum := st.Summary(); len(sum.Where) > 0 {
		st.Where = sum
	}
	return st
}

func nextEtcdBackup(st *EtcdBackupStatus) time.Time {
	next := st.Settings.Since
	if st.Last != nil {
		next = st.Last.Time.Add(time.Duration(st.Settings.EveryHours) * time.Hour)
	}
	if st.Failed != nil && (st.Last == nil || st.Failed.After(st.Last.Time)) {
		next = later(next, st.Failed.Add(etcdBackupRetry))
	}
	return next
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// EtcdBackupDue tells whether a scheduled snapshot is due at now, for the target or the volume.
func (a *Appliance) EtcdBackupDue(now time.Time) bool {
	st := a.EtcdBackups()
	due := (st.Settings != nil && !now.Before(*st.Next)) || a.volumeBackupDue(now)
	return due && !st.Taking && a.Idle() == nil
}

// ConfigureEtcdBackups sets the backups up, or changes them, with the passphrase, which opens the
// state's admin talosconfig and encrypts the key the snapshots are encrypted to. Empty access keys
// keep the ones stored. The target gets that key, encrypted, beside the snapshots. A target that
// holds one already keeps it, when the passphrase opens it.
func (a *Appliance) ConfigureEtcdBackups(ctx context.Context, s *EtcdBackupSettings, accessKeyID, secretAccessKey, passphrase string) error {
	saved := a.SavedSettings()
	if saved == nil {
		return errors.New("there is no management cluster yet")
	}
	if err := s.Validate(saved.BackupTargets); err != nil {
		return err
	}
	old, _ := readJSONFile[etcdBackupKeys](a.etcdBackupFile("keys.json"))
	if accessKeyID == "" && secretAccessKey == "" && old != nil {
		accessKeyID, secretAccessKey = old.AccessKeyID, old.SecretAccessKey
	}
	if accessKeyID == "" || secretAccessKey == "" {
		return errors.New("access key: both its ID and its secret are needed")
	}
	passphrase, err := a.statePassphrase(ctx, passphrase)
	if err != nil {
		return err
	}
	admin, err := a.credential(ctx, passphrase, talosconfigName)
	if err != nil {
		return err
	}
	keys := &etcdBackupKeys{AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey}
	talosconfig, err := a.etcdBackupCredential(ctx, admin, keys)
	if err != nil {
		return err
	}
	target := targetOf(saved.BackupTargets, s.Target)
	store, err := a.openEtcdStore(&target, keys)
	if err != nil {
		return err
	}
	identity, err := etcdIdentity(ctx, store, target.Prefix, passphrase, a.KeyWorkFactor)
	if err != nil {
		return err
	}
	keys.Recipient = identity.Recipient().String()
	if err := os.MkdirAll(a.etcdBackupDir(), privateDir); err != nil {
		return fmt.Errorf("keeping the backups' settings: %w", err)
	}
	if err := writeFileAtomic(a.etcdBackupFile(talosconfigName), talosconfig); err != nil {
		return err
	}
	if err := writeJSONFile(a.etcdBackupFile("keys.json"), keys); err != nil {
		return err
	}
	return writeJSONFile(a.etcdBackupFile("settings.json"), s)
}

// etcdBackupCredential reads the control-plane nodes from the admin talosconfig into keys, and has
// the first that answers make a talosconfig that may take etcd snapshots and nothing else.
func (a *Appliance) etcdBackupCredential(ctx context.Context, admin []byte, keys *etcdBackupKeys) ([]byte, error) {
	if a.Talos == nil {
		return nil, errors.New("this appliance does not reach Talos nodes")
	}
	nodes, err := provisioner.TalosEndpoints(admin)
	if err != nil {
		return nil, fmt.Errorf("the management cluster's talosconfig: %w", err)
	}
	keys.Nodes = nodes
	var errs []error
	for _, node := range nodes {
		raw, err := a.Talos.BackupConfig(ctx, admin, node, etcdBackupCredentialTTL)
		if err == nil {
			return raw, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// renewEtcdBackupCredential makes the snapshots' talosconfig again after an apply, which may have
// changed the nodes, when backups are set up.
func (a *Appliance) renewEtcdBackupCredential(ctx context.Context, admin []byte) error {
	keys, err := readJSONFile[etcdBackupKeys](a.etcdBackupFile("keys.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	talosconfig, err := a.etcdBackupCredential(ctx, admin, keys)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.etcdBackupFile(talosconfigName), talosconfig); err != nil {
		return err
	}
	return writeJSONFile(a.etcdBackupFile("keys.json"), keys)
}

// EtcdBackupKeysStored tells whether an access key for the backups is kept.
func (a *Appliance) EtcdBackupKeysStored() bool {
	_, err := os.Stat(a.etcdBackupFile("keys.json"))
	return err == nil
}

// StopEtcdBackups switches the backups off and forgets the access key. The snapshots stay.
func (a *Appliance) StopEtcdBackups() error {
	var errs []error
	for _, name := range []string{"settings.json", "keys.json", talosconfigName, "status.json"} {
		if err := os.Remove(a.etcdBackupFile(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func targetOf(targets []BackupTarget, name string) BackupTarget {
	i := slices.IndexFunc(targets, func(t BackupTarget) bool { return t.Name == name })
	if i < 0 {
		return BackupTarget{Name: name}
	}
	return targets[i]
}

func (a *Appliance) openEtcdStore(t *BackupTarget, keys *etcdBackupKeys) (provisioner.ObjectStore, error) { //nolint:ireturn // the store OpenStore makes
	open := a.OpenStore
	if open == nil {
		open = provisioner.OpenTarget
	}
	checksums := !t.NoChecksums
	ca := t.CACertificate
	if ca == "" && t.Endpoint != "" {
		ca = a.siteCA()
	}
	store, err := open(&provisioner.BackupTarget{
		Name: t.Name, Endpoint: t.Endpoint, Bucket: t.Bucket, Region: t.Region, Prefix: t.Prefix,
		CACertificate: ca, Checksums: &checksums,
	}, map[string][]byte{"accessKeyID": []byte(keys.AccessKeyID), "secretAccessKey": []byte(keys.SecretAccessKey)})
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", t.Name, err)
	}
	return store, nil
}

// etcdIdentity opens the identity the target keeps for the management cluster's snapshots with the
// passphrase, or makes one and stores it there, encrypted with the passphrase at scrypt's work
// factor, age's own when 0.
func etcdIdentity(ctx context.Context, store provisioner.ObjectStore, prefix, passphrase string, workFactor int) (*age.X25519Identity, error) {
	key := provisioner.ManagementEtcdIdentity(prefix)
	r, err := store.Get(ctx, key)
	switch {
	case err == nil:
		defer func() { _ = r.Close() }()
		return openIdentity(r, passphrase)
	case !errors.Is(err, provisioner.ErrNoObject):
		return nil, fmt.Errorf("reading the snapshots' key: %w", err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("making the snapshots' key: %w", err)
	}
	sealed, err := sealIdentity(identity, passphrase, workFactor)
	if err != nil {
		return nil, err
	}
	if _, err := store.Put(ctx, key, bytes.NewReader(sealed), nil); err != nil {
		return nil, fmt.Errorf("storing the snapshots' key: %w", err)
	}
	return identity, nil
}

// sealIdentity encrypts identity with the passphrase at scrypt's work factor, age's own when 0.
func sealIdentity(identity *age.X25519Identity, passphrase string, workFactor int) ([]byte, error) {
	to, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("encrypting the snapshots' key: %w", err)
	}
	if workFactor > 0 {
		to.SetWorkFactor(workFactor)
	}
	var sealed bytes.Buffer
	w, err := age.Encrypt(&sealed, to)
	if err == nil {
		_, err = io.WriteString(w, identity.String()+"\n")
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("encrypting the snapshots' key: %w", err)
	}
	return sealed.Bytes(), nil
}

func openIdentity(r io.Reader, passphrase string) (*age.X25519Identity, error) {
	with, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("opening the snapshots' key: %w", err)
	}
	plain, err := age.Decrypt(r, with)
	if err != nil {
		return nil, errors.New("the passphrase does not open the target's key for the management cluster's snapshots: give the passphrase they were taken with")
	}
	raw, err := io.ReadAll(plain)
	if err != nil {
		return nil, fmt.Errorf("reading the snapshots' key: %w", err)
	}
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("reading the snapshots' key: %w", err)
	}
	return identity, nil
}

// backupSetup is everything a snapshot or restore reads of the backups' settings.
type backupSetup struct {
	settings *EtcdBackupSettings
	keys     *etcdBackupKeys
	target   BackupTarget
	store    provisioner.ObjectStore
}

func (a *Appliance) backupSetup() (*backupSetup, error) {
	s, err := readJSONFile[EtcdBackupSettings](a.etcdBackupFile("settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoEtcdBackups
	}
	if err != nil {
		return nil, err
	}
	keys, err := readJSONFile[etcdBackupKeys](a.etcdBackupFile("keys.json"))
	if err != nil {
		return nil, fmt.Errorf("the backups' keys: %w", err)
	}
	saved := a.SavedSettings()
	if saved == nil || !slices.ContainsFunc(saved.BackupTargets, func(t BackupTarget) bool { return t.Name == s.Target }) {
		return nil, fmt.Errorf("backup target %s is gone from the settings", s.Target)
	}
	target := targetOf(saved.BackupTargets, s.Target)
	store, err := a.openEtcdStore(&target, keys)
	if err != nil {
		return nil, err
	}
	return &backupSetup{settings: s, keys: keys, target: target, store: store}, nil
}

// TakeEtcdSnapshot takes a snapshot of the management cluster's etcd now, stores it encrypted in
// the target, deletes all but the newest the settings keep, and records how it went.
func (a *Appliance) TakeEtcdSnapshot(ctx context.Context) (*provisioner.EtcdSnapshot, error) {
	if err := a.claimEtcdSnapshot(); err != nil {
		return nil, err
	}
	return a.snapshotEtcd(ctx, nil)
}

// TakeScheduledEtcdSnapshot takes the snapshot the schedules have due at now, into the places
// whose own schedule has it due, so that each keeps the spacing it was set to. It returns
// ErrNoSnapshotDue when none is due.
func (a *Appliance) TakeScheduledEtcdSnapshot(ctx context.Context, now time.Time) (*provisioner.EtcdSnapshot, error) {
	if err := a.claimEtcdSnapshot(); err != nil {
		return nil, err
	}
	return a.snapshotEtcd(ctx, &now)
}

// StartEtcdSnapshot takes a snapshot as TakeEtcdSnapshot does, in the background, and returns
// once the backups' status shows it being taken.
func (a *Appliance) StartEtcdSnapshot(ctx context.Context) error {
	if err := a.claimEtcdSnapshot(); err != nil {
		return err
	}
	go func() {
		taken, err := a.snapshotEtcd(context.WithoutCancel(ctx), nil)
		switch {
		case a.Log == nil:
		case err != nil:
			a.Log.Error("management cluster snapshot", "error", err)
		default:
			a.Log.Info("management cluster snapshot taken", "snapshot", taken.Name)
		}
	}()
	return nil
}

// claimEtcdSnapshot marks a snapshot as being taken, unless one is or the backups are off.
func (a *Appliance) claimEtcdSnapshot() error {
	if a.EtcdBackups().Settings == nil && a.volumeBackupKeys() == nil {
		return ErrNoEtcdBackups
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.job.running() {
		if refusal := snapshotRefusals[a.job.Kind]; refusal != nil {
			return refusal
		}
	}
	if a.snapshot != nil {
		return ErrEtcdSnapshotTaking
	}
	a.snapshot = &snapshotRun{done: make(chan struct{})}
	return nil
}

// ErrNoSnapshotDue is TakeScheduledEtcdSnapshot's answer when no place has a snapshot due.
var ErrNoSnapshotDue = errors.New("no management snapshot is due")

// snapshotPlaces are the places one snapshot goes.
type snapshotPlaces struct {
	// vol is the backup volume's keys, nil where the volume takes none.
	vol *volumeBackupKeys
	// b and berr are the bucket target's setup, and bucket whether it takes the snapshot.
	b      *backupSetup
	berr   error
	bucket bool
}

// placesFor picks every place backups go, or with due set, those whose schedule has one due then.
func (a *Appliance) placesFor(due *time.Time) (snapshotPlaces, error) {
	p := snapshotPlaces{vol: a.volumeBackupKeys()}
	p.b, p.berr = a.backupSetup()
	p.bucket = !errors.Is(p.berr, ErrNoEtcdBackups)
	if due != nil {
		if !a.volumeBackupDue(*due) {
			p.vol = nil
		}
		if st := a.EtcdBackups(); st.Settings == nil || st.Next == nil || due.Before(*st.Next) {
			p.bucket = false
		}
		if p.vol == nil && !p.bucket {
			return p, ErrNoSnapshotDue
		}
	}
	if p.vol == nil && !p.bucket {
		return p, p.berr
	}
	return p, nil
}

// snapshotEtcd takes a snapshot into every place backups go, or with due set, into those whose
// schedule has one due then.
func (a *Appliance) snapshotEtcd(ctx context.Context, due *time.Time) (*provisioner.EtcdSnapshot, error) {
	ctx, finish := a.runSnapshot(ctx)
	defer finish()
	p, err := a.placesFor(due)
	if err != nil {
		return nil, err
	}
	vol, b, berr, bucket := p.vol, p.b, p.berr, p.bucket
	failed := func(err error) {
		if snapshotStopped(ctx) {
			return
		}
		if vol != nil {
			a.volumeBackupFailed(err)
		}
		if bucket {
			a.etcdBackupFailed(err)
		}
	}
	var nodes []string
	credential := talosconfigName
	switch {
	case vol != nil:
		nodes, credential = vol.Nodes, volumeTalosconfig
	case berr != nil:
		failed(berr)
		return nil, berr
	default:
		nodes = b.keys.Nodes
	}
	file, err := a.snapshotToFile(ctx, credential, nodes)
	if err != nil {
		failed(err)
		return nil, err
	}
	defer func() { _ = os.Remove(file) }()
	version := "unknown"
	if applied := a.LastApplied(); applied != nil && applied.Versions["kubernetes"] != "" {
		version = applied.Versions["kubernetes"]
	}
	taken := &provisioner.EtcdSnapshot{Name: provisioner.EtcdSnapshotName(a.now(), version), KubernetesVersion: version, Distribution: "talos"}
	var stored *provisioner.EtcdSnapshot
	var errs []error
	if vol != nil {
		stored, err = a.backUpToVolume(ctx, vol, file, taken)
		errs = append(errs, err)
	}
	if bucket {
		inBucket, err := a.backUpToTarget(ctx, b, berr, file, taken)
		if stored == nil {
			stored = inBucket
		}
		errs = append(errs, err)
	}
	return stored, errors.Join(errs...)
}

// runSnapshot bounds the claimed snapshot run by its timeout and hands stopEtcdSnapshot its cancel,
// which a stop asked for before the run began takes effect at once. finish frees the claim and
// tells a waiting stop that nothing more is written.
func (a *Appliance) runSnapshot(ctx context.Context) (context.Context, func()) {
	ctx, timeout := context.WithTimeout(ctx, etcdBackupTimeout)
	ctx, cancel := context.WithCancelCause(ctx)
	a.mu.Lock()
	run := a.snapshot
	run.cancel = cancel
	if run.cancelled {
		cancel(errSnapshotStopped)
	}
	a.mu.Unlock()
	return ctx, func() {
		cancel(nil)
		timeout()
		a.mu.Lock()
		a.snapshot = nil
		a.mu.Unlock()
		close(run.done)
	}
}

// backUpToTarget stores the snapshot in file in the target and records how it went.
func (a *Appliance) backUpToTarget(ctx context.Context, b *backupSetup, berr error, file string, taken *provisioner.EtcdSnapshot) (*provisioner.EtcdSnapshot, error) {
	if berr != nil {
		a.etcdBackupFailed(berr)
		return nil, berr
	}
	stored, err := storeEtcdSnapshot(ctx, b, file, taken)
	if err != nil {
		if !snapshotStopped(ctx) {
			a.etcdBackupFailed(err)
		}
		return stored, err
	}
	rec := a.etcdBackupRecord()
	rec.Last, rec.Error, rec.Failed = stored, "", nil
	return stored, writeJSONFile(a.etcdBackupFile("status.json"), rec)
}

func (a *Appliance) etcdBackupRecord() *etcdBackupRecord {
	rec, _ := readJSONFile[etcdBackupRecord](a.etcdBackupFile("status.json"))
	if rec == nil {
		rec = &etcdBackupRecord{}
	}
	return rec
}

// etcdBackupFailed records why the backups failed, for the interface, which shows it until the
// next snapshot is taken.
func (a *Appliance) etcdBackupFailed(err error) {
	rec := a.etcdBackupRecord()
	now := a.now()
	rec.Error, rec.Failed = err.Error(), &now
	if werr := writeJSONFile(a.etcdBackupFile("status.json"), rec); werr != nil && a.Log != nil {
		a.Log.Error("recording a failed etcd backup", "error", werr)
	}
}

func storeEtcdSnapshot(ctx context.Context, b *backupSetup, file string, taken *provisioner.EtcdSnapshot) (*provisioner.EtcdSnapshot, error) {
	// The bucket fills in its own record of the snapshot.
	snap := *taken
	to, err := age.ParseX25519Recipient(b.keys.Recipient)
	if err != nil {
		return nil, fmt.Errorf("the snapshots' key: %w", err)
	}
	folder := provisioner.ManagementEtcdFolder(b.target.Prefix)
	if err = storeFile(ctx, b.store, folder, &snap, file, to); err != nil {
		return nil, fmt.Errorf("storing the snapshot: %w", err)
	}
	if _, err = provisioner.PruneEtcdSnapshots(ctx, b.store, folder, b.settings.Keep); err != nil {
		return &snap, fmt.Errorf("deleting older snapshots: %w", err)
	}
	return &snap, nil
}

// ListEtcdSnapshots lists the management cluster's snapshots in the target, newest first.
func (a *Appliance) ListEtcdSnapshots(ctx context.Context) ([]provisioner.EtcdSnapshot, error) {
	b, err := a.backupSetup()
	if err != nil {
		return nil, err
	}
	list, err := provisioner.ListEtcdSnapshots(ctx, b.store, provisioner.ManagementEtcdFolder(b.target.Prefix))
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", b.target.Name, err)
	}
	return list, nil
}

// EtcdRestoreRequest is what a restore of the management cluster's etcd was started with.
type EtcdRestoreRequest struct {
	Snapshot string `json:"snapshot"`
}

// RestoreEtcd restores the management cluster's etcd from a snapshot, on the user's ticket, as a
// run of its own in the background. The passphrase opens the admin talosconfig and the snapshots'
// key. Neither needs the management cluster's API, which may be down.
func (a *Appliance) RestoreEtcd(ctx context.Context, name, passphrase string, ticket *pve.Ticket) error {
	if !provisioner.ValidSnapshotName(name) {
		return fmt.Errorf("%q is no snapshot's name", name)
	}
	if a.Talos == nil {
		return errors.New("this appliance does not reach Talos nodes")
	}
	passphrase, err := a.statePassphrase(ctx, passphrase)
	if err != nil {
		return err
	}
	acc, err := a.etcdRestoreAccess(ctx, passphrase)
	if err != nil {
		return err
	}
	//nolint:contextcheck // the run outlives the request that starts it
	return a.startIn(&a.job, sessionRunner(ticket), "etcd-restore", EtcdRestoreRequest{Snapshot: name}, func(ctx context.Context, j *Job) error {
		if err := a.stopEtcdSnapshot(ctx, j); err != nil {
			return err
		}
		j.Add("Restoring the management cluster's etcd from " + name)
		file, err := a.fetchEtcdSnapshot(ctx, acc.setup, acc.folder, name, passphrase)
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(file) }()
		j.Add("The snapshot matches its checksum")
		open := func() (io.ReadCloser, error) { return os.Open(file) } //nolint:gosec // the appliance's own file
		if err := a.Talos.Restore(ctx, acc.admin, acc.nodes, open, j.Add); err != nil {
			return fmt.Errorf("restoring etcd: %w", err)
		}
		j.Add("etcd is restored and runs on every control-plane node that answered")
		return nil
	})
}

// etcdRestoreAccess is what a restore of the management cluster's etcd needs to read the snapshots
// and reach the control-plane nodes.
type etcdRestoreAccess struct {
	setup  *backupSetup
	admin  []byte
	nodes  []string
	folder string
}

func (a *Appliance) etcdRestoreAccess(ctx context.Context, passphrase string) (*etcdRestoreAccess, error) {
	b, err := a.backupSetup()
	if err != nil {
		return nil, err
	}
	admin, err := a.credential(ctx, passphrase, talosconfigName)
	if err != nil {
		return nil, err
	}
	nodes, err := provisioner.TalosEndpoints(admin)
	if err != nil {
		return nil, fmt.Errorf("the management cluster's talosconfig: %w", err)
	}
	return &etcdRestoreAccess{setup: b, admin: admin, nodes: nodes, folder: provisioner.ManagementEtcdFolder(b.target.Prefix)}, nil
}

// fetchEtcdSnapshot decrypts a snapshot into a file of the appliance's and checks it against the
// checksum stored with it, before anything is wiped.
func (a *Appliance) fetchEtcdSnapshot(ctx context.Context, b *backupSetup, folder, name, passphrase string) (string, error) {
	r, err := b.store.Get(ctx, provisioner.ManagementEtcdIdentity(b.target.Prefix))
	if err != nil {
		return "", fmt.Errorf("reading the snapshots' key: %w", err)
	}
	identity, err := openIdentity(r, passphrase)
	_ = r.Close()
	if err != nil {
		return "", err
	}
	info, err := provisioner.ReadEtcdSnapshot(ctx, b.store, folder, name)
	if err != nil {
		return "", fmt.Errorf("snapshot %s: %w", name, err)
	}
	want := info.SHA256
	sealed, err := b.store.Get(ctx, folder+"/"+name)
	if err != nil {
		return "", fmt.Errorf("reading snapshot %s: %w", name, err)
	}
	defer func() { _ = sealed.Close() }()
	f, err := a.plainFile("etcd-restore-*.db")
	if err != nil {
		return "", fmt.Errorf("making room for the snapshot: %w", err)
	}
	sum, _, err := provisioner.DecryptEtcdSnapshot(sealed, identity.String(), f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && want != "" && sum != want {
		err = fmt.Errorf("snapshot %s does not match its checksum", name)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func (a *Appliance) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
