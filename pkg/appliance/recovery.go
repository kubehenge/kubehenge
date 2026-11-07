package appliance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// EtcdBackupWarningSnooze is how long an administrator can silence the warning that the management
// cluster's etcd is not backed up.
const EtcdBackupWarningSnooze = 30 * 24 * time.Hour

// exportVersion is the layout of the export bundle.
const exportVersion = 1

// The ways an export or an import is refused.
var (
	// ErrExportPassphrase is an export the passphrase does not open.
	ErrExportPassphrase = errors.New("the passphrase does not open the export")
	// ErrExportDamaged is a file that is no export, or an export that was changed.
	ErrExportDamaged = errors.New("the file is no export of this appliance's state, or it was changed")
	// ErrHasState refuses an import over a state the appliance holds.
	ErrHasState = errors.New("this appliance has a management cluster already; import into a new appliance")
	// ErrNoWarning is a dismissal asked for while nothing is warned of.
	ErrNoWarning = errors.New("the management cluster's etcd is backed up, or there is no management cluster")
)

// EtcdBackupWarning stands while the management cluster runs without etcd backups. Every cluster's
// backup keys, CAs and kubeconfigs live only in that etcd.
type EtcdBackupWarning struct {
	// Dismissal is set while an administrator has silenced the warning.
	Dismissal *WarningDismissal `json:"dismissal,omitempty"`
}

// WarningDismissal records who silenced the warning, when, and until when.
type WarningDismissal struct {
	By    string    `json:"by"`
	At    time.Time `json:"at"`
	Until time.Time `json:"until"`
}

// Shown tells whether the warning is to be shown and told.
func (w *EtcdBackupWarning) Shown() bool { return w != nil && w.Dismissal == nil }

func (a *Appliance) dismissalFile() string { return a.etcdBackupFile("warning-dismissed.json") }

// etcdBackupWarning is the warning that stands now, nil when the backups are on or there is no
// management cluster.
func (a *Appliance) etcdBackupWarning(st *EtcdBackupStatus) *EtcdBackupWarning {
	if st.Settings != nil || !a.Ready() {
		return nil
	}
	w := &EtcdBackupWarning{}
	if d, err := readJSONFile[WarningDismissal](a.dismissalFile()); err == nil && a.now().Before(d.Until) {
		w.Dismissal = d
	}
	return w
}

// DismissEtcdBackupWarning silences the warning for EtcdBackupWarningSnooze and records who did it.
func (a *Appliance) DismissEtcdBackupWarning(by string) error {
	if a.etcdBackupWarning(&EtcdBackupStatus{Settings: a.EtcdBackups().Settings}) == nil {
		return ErrNoWarning
	}
	if err := os.MkdirAll(a.etcdBackupDir(), privateDir); err != nil {
		return fmt.Errorf("recording the dismissal: %w", err)
	}
	now := a.now()
	return writeJSONFile(a.dismissalFile(), WarningDismissal{By: by, At: now, Until: now.Add(EtcdBackupWarningSnooze)})
}

// exportBundle is what an export holds: what a new appliance needs to take over the management
// cluster and restore its etcd. The state is still encrypted with the state passphrase, so the
// bundle's own passphrase and the state passphrase together open it.
type exportBundle struct {
	Version int       `json:"version"`
	Created time.Time `json:"created"`
	// Talosconfig is the management cluster's admin talosconfig, which reaches its nodes with
	// Kubernetes down.
	Talosconfig string `json:"talosconfig"`
	// State is OpenTofu's encrypted state of the management cluster.
	State []byte `json:"state"`
	// Settings are the last applied settings.
	Settings json.RawMessage `json:"settings"`
	Backups  exportBackups   `json:"backups"`
}

// exportBackups are the etcd backups' settings, the target and its access key.
type exportBackups struct {
	Settings EtcdBackupSettings `json:"settings"`
	Keys     etcdBackupKeys     `json:"keys"`
	Target   BackupTarget       `json:"target"`
	// SpecKey is the recovery key that opens the Secrets in the stored cluster specs.
	SpecKey string `json:"specKey,omitempty"`
}

// exportRecord is what was last exported, to tell a later change of the settings.
type exportRecord struct {
	At          time.Time `json:"at"`
	Fingerprint string    `json:"fingerprint"`
}

func (a *Appliance) exportFile() string { return a.etcdBackupFile("export.json") }

// exportFingerprint identifies the settings an export carries and an import restores: the target,
// its access key, the schedule's target and the snapshots' recipient.
func exportFingerprint(b *exportBackups) string {
	raw, err := json.Marshal(struct {
		Schedule        string       `json:"schedule"`
		AccessKeyID     string       `json:"accessKeyId"`
		SecretAccessKey string       `json:"secretAccessKey"`
		Recipient       string       `json:"recipient"`
		Target          BackupTarget `json:"target"`
		SpecKey         string       `json:"specKey"`
		SpecRecipients  []string     `json:"specRecipients"`
	}{b.Settings.Target, b.Keys.AccessKeyID, b.Keys.SecretAccessKey, b.Keys.Recipient, b.Target, b.SpecKey, b.Settings.SpecRecipients})
	if err != nil {
		// Strings and plain structs always marshal. Distinct input still gives a distinct value.
		raw = fmt.Appendf(nil, "%+v", b)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// currentBackups reads the backups' settings as an export would carry them, without opening the
// target.
func (a *Appliance) currentBackups() (*exportBackups, error) {
	s, err := readJSONFile[EtcdBackupSettings](a.etcdBackupFile("settings.json"))
	if err != nil {
		return nil, err
	}
	keys, err := readJSONFile[etcdBackupKeys](a.etcdBackupFile("keys.json"))
	if err != nil {
		return nil, err
	}
	saved := a.SavedSettings()
	if saved == nil {
		return nil, errors.New("there are no saved settings")
	}
	b := &exportBackups{Settings: *s, Keys: *keys, Target: targetOf(saved.BackupTargets, s.Target)}
	if raw, err := os.ReadFile(a.specKeyFile()); err == nil {
		b.SpecKey = strings.TrimSpace(string(raw))
	}
	return b, nil
}

// exportDue tells whether the backups work and an export of the settings now was not taken yet.
// It is judged after a backup succeeded, since an export without a snapshot restores nothing. Once
// the recovery key exists it is due at once, since the stored Secrets open only with that key and
// an appliance lost before an export locks them for good.
func (a *Appliance) exportDue(st *EtcdBackupStatus) (bool, *time.Time) {
	var at *time.Time
	rec, _ := readJSONFile[exportRecord](a.exportFile())
	if rec != nil {
		at = &rec.At
	}
	if st.Settings == nil {
		return false, at
	}
	now, err := a.currentBackups()
	if err != nil {
		return false, at
	}
	stale := rec == nil || rec.Fingerprint != exportFingerprint(now)
	if now.SpecKey != "" {
		return stale, at
	}
	return st.Last != nil && stale, at
}

// ExportState makes the export bundle, encrypted with exportPassphrase. The state passphrase proves
// the caller may read the state.
func (a *Appliance) ExportState(ctx context.Context, statePassphrase, exportPassphrase string) ([]byte, error) {
	if err := ValidPassphrase(exportPassphrase); err != nil {
		return nil, fmt.Errorf("export %w", err)
	}
	if s, _ := readJSONFile[EtcdBackupSettings](a.etcdBackupFile("settings.json")); s == nil {
		return nil, ErrNoEtcdBackups
	}
	// The export carries the recovery key, so that a new appliance opens the stored specs.
	if _, err := a.ensureSpecIdentity(); err != nil {
		return nil, err
	}
	backups, err := a.currentBackups()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoEtcdBackups
	}
	if err != nil {
		return nil, err
	}
	talosconfig, err := a.Credential(ctx, statePassphrase, "talosconfig")
	if err != nil {
		return nil, err
	}
	state, err := os.ReadFile(filepath.Join(a.workdir(), "terraform.tfstate"))
	if err != nil {
		return nil, fmt.Errorf("reading the state: %w", err)
	}
	settings, err := os.ReadFile(filepath.Join(a.workdir(), "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("reading the settings: %w", err)
	}
	plain, err := json.Marshal(exportBundle{
		Version: exportVersion, Created: a.now().UTC(), Talosconfig: string(talosconfig), State: state, Settings: settings, Backups: *backups,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding the export: %w", err)
	}
	sealed, err := a.sealWith(exportPassphrase, plain)
	if err != nil {
		return nil, err
	}
	rec := exportRecord{At: a.now().UTC(), Fingerprint: exportFingerprint(backups)}
	if err := writeJSONFile(a.exportFile(), rec); err != nil {
		return nil, err
	}
	return sealed, nil
}

func (a *Appliance) sealWith(passphrase string, plain []byte) ([]byte, error) {
	to, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("encrypting the export: %w", err)
	}
	if a.KeyWorkFactor > 0 {
		to.SetWorkFactor(a.KeyWorkFactor)
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, to)
	if err == nil {
		_, err = w.Write(plain)
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("encrypting the export: %w", err)
	}
	return out.Bytes(), nil
}

func openBundle(sealed []byte, passphrase string) (*exportBundle, error) {
	with, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("opening the export: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(sealed), with)
	var none *age.NoIdentityMatchError
	switch {
	case errors.As(err, &none):
		return nil, ErrExportPassphrase
	case err != nil:
		return nil, ErrExportDamaged
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, ErrExportDamaged
	}
	var b exportBundle
	if err := json.Unmarshal(plain, &b); err != nil || b.Version != exportVersion || len(b.State) == 0 || b.Backups.Keys.Recipient == "" {
		return nil, ErrExportDamaged
	}
	return &b, nil
}

// ImportState puts an export's state, settings and backup access into a new appliance, so that it
// reaches the management cluster and restores its etcd from the snapshots in the target.
func (a *Appliance) ImportState(ctx context.Context, sealed []byte, exportPassphrase string) error {
	if a.HasState() || a.Ready() {
		return ErrHasState
	}
	if err := a.Idle(); err != nil {
		return err
	}
	b, err := openBundle(sealed, exportPassphrase)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
		return fmt.Errorf("preparing the management cluster's folder: %w", err)
	}
	if err := os.MkdirAll(a.etcdBackupDir(), privateDir); err != nil {
		return fmt.Errorf("preparing the backups' folder: %w", err)
	}
	// The settings go first: a restore reads the target from them.
	files := []struct {
		path string
		data []byte
	}{
		{filepath.Join(a.workdir(), "settings.json"), b.Settings},
		{filepath.Join(a.workdir(), "terraform.tfstate"), b.State},
	}
	for _, f := range files {
		if err := writeFileAtomic(f.path, f.data); err != nil {
			return err
		}
	}
	keys := b.Backups.Keys
	if err := writeJSONFile(a.etcdBackupFile("keys.json"), keys); err != nil {
		return err
	}
	if err := writeJSONFile(a.etcdBackupFile("settings.json"), b.Backups.Settings); err != nil {
		return err
	}
	if err := a.importSpecKey(b.Backups.SpecKey); err != nil {
		return err
	}
	if err := writeJSONFile(a.exportFile(), exportRecord{At: b.Created, Fingerprint: exportFingerprint(&b.Backups)}); err != nil {
		return err
	}
	// The snapshots' talosconfig is made again when the nodes answer. Until then a restore, which
	// reads the admin talosconfig from the state, works without it.
	if a.Talos != nil {
		if talosconfig, err := a.etcdBackupCredential(ctx, []byte(b.Talosconfig), &keys); err == nil {
			if err := writeFileAtomic(a.etcdBackupFile("talosconfig"), talosconfig); err != nil {
				return err
			}
			return writeJSONFile(a.etcdBackupFile("keys.json"), keys)
		}
	}
	return nil
}

// importSpecKey keeps the recovery key an export carried. An export made before there was one
// carries none.
func (a *Appliance) importSpecKey(key string) error {
	if key == "" {
		return nil
	}
	return writeFileAtomic(a.specKeyFile(), []byte(key))
}

// recreateTargets are the module's addresses an apply creates the VMs and configures the nodes
// with, and nothing after: the cluster's bootstrap and health, which cannot finish on nodes whose
// etcd is empty. Each node's module waits for the one before, so naming the last names them all.
func recreateTargets(nodes int) []string {
	return []string{"-target=proxmox_virtual_environment_vm.node", fmt.Sprintf("-target=module.talos_node_%d", max(nodes, 1)-1)}
}

// RecreateRequest is what a recreation of the management cluster was started with.
type RecreateRequest struct {
	Snapshot string `json:"snapshot"`
}

// RecreateManagement makes the management cluster's VMs again and restores its etcd from a
// snapshot into them. It checks the snapshot first, then applies the module for the VMs and the
// nodes' configuration only, restores etcd, and applies the module whole so that the cluster is
// bootstrapped as recorded, healthy and reachable. A whole apply first would wait out the health
// read on nodes with no etcd, and a restore first has no nodes to reach. ready is called with the
// interface's kubeconfig as for Apply.
func (a *Appliance) RecreateManagement(ctx context.Context, name, passphrase string, ticket *pve.Ticket, ready func(ctx context.Context, kubeconfig []byte) error) error {
	if !provisioner.ValidSnapshotName(name) {
		return fmt.Errorf("%q is no snapshot's name", name)
	}
	if a.Talos == nil {
		return errors.New("this appliance does not reach Talos nodes")
	}
	settings := a.SavedSettings()
	if settings == nil || !a.HasState() {
		return errors.New("there is no management cluster state to recreate it from: import an export first")
	}
	b, err := a.backupSetup()
	if err != nil {
		return err
	}
	admin, err := a.Credential(ctx, passphrase, "talosconfig")
	if err != nil {
		return err
	}
	nodes, err := provisioner.TalosEndpoints(admin)
	if err != nil {
		return fmt.Errorf("the management cluster's talosconfig: %w", err)
	}
	folder := provisioner.ManagementEtcdFolder(b.target.Prefix)
	r := &recreation{
		name: name, passphrase: passphrase, ticket: ticket, settings: settings, setup: b, folder: folder, admin: admin, nodes: nodes,
		targets: recreateTargets(len(settings.Nodes)), ready: ready,
	}
	//nolint:contextcheck // the run outlives the request that starts it
	return a.startIn(&a.job, sessionRunner(ticket), "recreate", RecreateRequest{Snapshot: name}, func(ctx context.Context, j *Job) error {
		return a.recreate(ctx, j, r)
	})
}

// recreation is everything a recreation of the management cluster was started with.
type recreation struct {
	name, passphrase, folder string
	ticket                   *pve.Ticket
	settings                 *Settings
	setup                    *backupSetup
	admin                    []byte
	nodes, targets           []string
	ready                    func(ctx context.Context, kubeconfig []byte) error
}

func (a *Appliance) recreate(ctx context.Context, j *Job, r *recreation) error {
	j.Add("Checking the snapshot " + r.name)
	file, err := a.fetchEtcdSnapshot(ctx, r.setup, r.folder, r.name, r.passphrase)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file) }()
	if err = a.applyVMs(ctx, j, r); err != nil {
		return err
	}
	open := func() (io.ReadCloser, error) { return os.Open(file) } //nolint:gosec // the appliance's own file
	j.Add("Restoring the management cluster's etcd from " + r.name)
	if err = a.Talos.Restore(ctx, r.admin, r.nodes, open, j.Add); err != nil {
		return fmt.Errorf("restoring etcd: %w", err)
	}
	j.Add("Applying the management cluster whole")
	kubeconfig, err := a.run(ctx, j, r.settings, r.passphrase, ticketAuth(r.ticket))
	if err != nil {
		return err
	}
	if err = r.ready(ctx, kubeconfig); err != nil {
		return fmt.Errorf("the management cluster is up, but the interface cannot use it: %w", err)
	}
	return nil
}

// applyVMs applies the module for the VMs and the nodes' configuration only.
func (a *Appliance) applyVMs(ctx context.Context, j *Job, r *recreation) error {
	if err := a.writeConfig(r.settings); err != nil {
		return err
	}
	env, err := a.env(r.passphrase, ticketAuth(r.ticket))
	if err != nil {
		return err
	}
	if err = a.init(ctx, j, env); err != nil {
		return err
	}
	j.Add("Making the VMs and configuring the nodes")
	return a.tofu(ctx, j, env, jsonMessage, append([]string{"apply", "-auto-approve", "-input=false", "-json"}, r.targets...)...)
}
