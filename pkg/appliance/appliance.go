// Package appliance is what Kubehenge's appliance keeps and runs beside the interface: the
// management cluster's OpenTofu configuration and encrypted state, the runs of the
// management-cluster module that create and upgrade it on a signed-in administrator's Proxmox VE
// session, and the interface's own kubeconfig.
package appliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// File modes. What the appliance keeps is its user's alone. The software it installs is
// world-readable: the interface runs as its own user and reads the module and charts, and
// systemd's units are read by everyone.
const (
	privateDir       = 0o700
	privateFile      = 0o600
	publicDir        = 0o755
	publicFile       = 0o644
	publicExecutable = 0o755
	anyExecute       = 0o111
)

// Appliance is the appliance's state directory and the tools it runs.
type Appliance struct {
	// Dir holds everything the appliance keeps. Only its user may read it.
	Dir string
	// KeyDir holds the keys that seal what automatic upgrades of the management cluster keep and the
	// interface's kept sessions, on a volume the container's backups leave out. It is empty where
	// there is none.
	KeyDir string
	// Module is the management-cluster module, as released with this binary.
	Module string
	// Tofu is the OpenTofu binary.
	Tofu string
	// ProxmoxURL and ProxmoxCA (PEM) are the Proxmox VE API the module signs in to, and
	// ProxmoxCAFile the file ProxmoxCA was read from, for the appliance's own calls.
	// ProxmoxInsecure has the module and the management cluster skip verifying its certificate.
	ProxmoxURL      string
	ProxmoxCA       string
	ProxmoxCAFile   string
	ProxmoxInsecure bool
	// Release is the appliance's own release, which carries the module. Every apply records it, so
	// that the interface can tell when the management cluster runs what an older one applied.
	Release string
	Log     *slog.Logger
	// InterfaceKubeconfig makes the interface's kubeconfig from the management cluster's admin
	// kubeconfig and API endpoint, once an apply has succeeded, with its token in tokenFile.
	InterfaceKubeconfig func(ctx context.Context, admin []byte, endpoint, tokenFile string) ([]byte, error)
	// ManagementClient reaches the management cluster with its admin kubeconfig, for the lease each
	// apply claims. Nil makes a client of the kubeconfig.
	ManagementClient func(admin []byte) (kubernetes.Interface, error)
	// NextID reads the datacenter's next-id option as the ticket's user, for the range the
	// management VMs' IDs are drawn from. Nil keeps the default range.
	NextID func(ctx context.Context, t *pve.Ticket) (pve.NextID, error)
	// Talos reaches the management cluster's nodes for its etcd backups and its first etcd start.
	// OpenStore opens the backups' target, provisioner.OpenTarget when nil.
	Talos     TalosAccess
	OpenStore provisioner.OpenStore
	// OpenVolume reaches the store on the management cluster's backup volume, through the
	// interface's kubeconfig when nil.
	OpenVolume func(ctx context.Context) (provisioner.ObjectStore, error)
	// ExportObjects reads Kubehenge's own objects from the management cluster as a YAML stream, for
	// the export taken with each snapshot onto the backup volume.
	ExportObjects func(ctx context.Context) ([]byte, error)
	// CopyCap bounds the appliance's copy of the latest backups, ApplianceCopyCap when 0.
	CopyCap int64
	// waitVolume bounds a recovery's wait for the backup volume's store, volumeWait when 0.
	waitVolume time.Duration
	// stallPause is the wait before an apply runs again after etcd timed out a request,
	// etcdStallPause when 0.
	stallPause time.Duration
	// etcdPoll is how often watchFirstEtcd looks at the nodes, firstEtcdPoll when 0.
	etcdPoll time.Duration
	// vmHosts reads with token the VMs it may see, with the Proxmox VE node each runs on, by their
	// SMBIOS UUID. Nil asks the Proxmox VE API.
	vmHosts backupstore.VMReader
	// protect sets or lifts a management VM's protection with the ticket. Nil asks the Proxmox VE
	// API.
	protect func(ctx context.Context, t *pve.Ticket, vm managementVM, value protection) error
	Now     func() time.Time
	// KeyWorkFactor is the scrypt work factor the passphrase seals the snapshots' key with, age's
	// own when 0. Opening the key reads it from the sealed key.
	KeyWorkFactor int
	// Addresses lists the addresses the appliance reaches the workload clusters from, which their
	// firewalls accept. Nil reads the container's own.
	Addresses func() ([]string, error)

	mu          sync.Mutex
	job         *Job
	templateJob *Job
	// snapshot is the etcd snapshot being taken, nil while none is.
	snapshot *snapshotRun
	stopping bool
}

// ErrPassphrase is a state the passphrase does not open.
var ErrPassphrase = errors.New("the passphrase does not open the management cluster's state")

// KubeconfigPath is the interface's kubeconfig for the management cluster.
func (a *Appliance) KubeconfigPath() string { return filepath.Join(a.Dir, "interface.kubeconfig") }

// Ready tells whether the management cluster exists and the interface can reach it.
func (a *Appliance) Ready() bool {
	_, err := os.Stat(a.KubeconfigPath())
	return err == nil
}

// HasState tells whether OpenTofu's state from an earlier run is on the appliance, such as after a
// first run that failed part of the way. Only that run's passphrase opens it.
func (a *Appliance) HasState() bool {
	_, err := os.Stat(filepath.Join(a.workdir(), "terraform.tfstate"))
	return err == nil
}

// SavedSettings returns the settings of the last apply, or nil.
func (a *Appliance) SavedSettings() *Settings {
	raw, err := os.ReadFile(filepath.Join(a.workdir(), "settings.json"))
	if err != nil {
		return nil
	}
	var s Settings
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

// Current returns the last run of the module, or nil.
func (a *Appliance) Current() *JobView {
	a.mu.Lock()
	j := a.job
	a.mu.Unlock()
	return j.view()
}

// Apply starts creating or updating the management cluster with the module, on the user's ticket,
// and returns at once. ready is called with the interface's kubeconfig once it succeeds, and the
// run fails with its error, since the interface cannot use the cluster then. The ticket has to
// last the whole run, so the caller passes a freshly renewed one.
func (a *Appliance) Apply(settings *Settings, passphrase string, ticket *pve.Ticket, ready func(ctx context.Context, kubeconfig []byte) error) error {
	// The run outlives the call, so it works on a copy the caller cannot change under it.
	s := *settings
	was := a.SavedSettings()
	if err := s.ValidateAgainst(was); err != nil {
		return err
	}
	if was != nil && a.Ready() {
		if err := s.KeepsFixed(was); err != nil {
			return err
		}
	}
	if err := ValidPassphrase(passphrase); err != nil {
		return err
	}
	return a.start(ticket, "apply", func(ctx context.Context, j *Job) error {
		kubeconfig, err := a.run(ctx, j, &s, passphrase, ticket)
		if err != nil {
			return err
		}
		if err := ready(ctx, kubeconfig); err != nil {
			return fmt.Errorf("the management cluster is up, but the interface cannot use it: %w", err)
		}
		return nil
	})
}

// Check validates settings as Apply would, against the running management cluster's too, without
// starting a run.
func (a *Appliance) Check(settings *Settings) error {
	was := a.SavedSettings()
	if err := settings.ValidateAgainst(was); err != nil {
		return err
	}
	if was != nil && a.Ready() {
		return settings.KeepsFixed(was)
	}
	return nil
}

// ReleasesHA tells whether an apply of next switches Proxmox VE HA off, or finishes switching it
// off after a run that did not get through. The HA token goes with it, so what the
// controller left in HA has to be removed with the user's session once the run is through, and
// HAReleased called then.
func (a *Appliance) ReleasesHA(next *Settings) bool {
	if next.ProxmoxHA {
		return false
	}
	if was := a.SavedSettings(); was != nil && was.ProxmoxHA {
		return true
	}
	_, err := os.Stat(a.haReleasePath())
	return err == nil
}

// HAReleased records that nothing of Kubehenge's is left in Proxmox VE HA.
func (a *Appliance) HAReleased() error {
	if err := os.Remove(a.haReleasePath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("recording that Proxmox VE HA is off: %w", err)
	}
	return nil
}

// BuildTemplate starts building a VM template on the user's ticket, as a job of its own kind that
// holds the same lock as runs of the module: an upgrade's restart would cut it off, and removing
// the management cluster deletes the templates. It returns at once. request is what the build was
// asked for, kept with it so that it can be started again.
func (a *Appliance) BuildTemplate(ticket *pve.Ticket, request any, run func(ctx context.Context, j *Job) error) error {
	return a.startIn(&a.templateJob, sessionRunner(ticket), "template", request, run)
}

// TemplateJob returns the last template build, or nil.
func (a *Appliance) TemplateJob() *JobView {
	a.mu.Lock()
	j := a.templateJob
	a.mu.Unlock()
	return j.view()
}

// Busy tells whether a run of the module or a template build is going on.
func (a *Appliance) Busy() bool { return a.Idle() != nil }

// Idle is nil while no job runs, else ErrBusy for a run of the module or ErrTemplateBusy for a
// template build.
func (a *Appliance) Idle() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running()
}

// BackupsOnRemove is what a removal does with the backup volume and the appliance's copy.
type BackupsOnRemove int

const (
	// KeepBackups keeps the backup volume on Proxmox VE and the appliance's copy.
	KeepBackups BackupsOnRemove = iota
	// DeleteBackups deletes the backup volume with its disk, and the appliance's copy.
	DeleteBackups
)

// Remove destroys the management cluster and everything the module made on Proxmox VE, on the
// user's ticket, and returns the appliance to its first run. before runs first: what the module
// does not own but its pools still hold, the templates, has to go before the pools can. gone is
// called once the management cluster is gone. backups says whether the backup volume stays, out of
// OpenTofu's state, or goes too.
func (a *Appliance) Remove(passphrase string, backups BackupsOnRemove, ticket *pve.Ticket, before func(ctx context.Context, j *Job) error, gone func()) error {
	if err := ValidPassphrase(passphrase); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(a.workdir(), "terraform.tfstate")); err != nil {
		return errors.New("there is no management cluster to remove")
	}
	release, forget := a.keepBackupVolume, a.forgetRemoved
	if backups == DeleteBackups {
		release, forget = a.liftBackupVolume, a.forgetRemovedWithCopy
	}
	return a.start(ticket, "remove", func(ctx context.Context, j *Job) error {
		env, err := a.removalEnv(ctx, j, passphrase, ticket)
		if err != nil {
			return err
		}
		if err := before(ctx, j); err != nil {
			return err
		}
		if err := a.init(ctx, j, env); err != nil {
			return err
		}
		if err := a.stopBackupStore(ctx, j, env, ticket); err != nil {
			return err
		}
		if err := release(ctx, j, env); err != nil {
			return err
		}
		j.Add("Destroying the management cluster")
		if err := a.applyPlanned(ctx, j, env, ticket, "-destroy"); err != nil {
			return err
		}
		if err := forget(); err != nil {
			return err
		}
		gone()
		return nil
	})
}

// talosconfigName names the management cluster's talosconfig: the module's output, and the file
// the bucket's snapshots are taken with.
const talosconfigName = "talosconfig"

// Credential reads the management cluster's admin kubeconfig or talosconfig out of the encrypted
// state, for repairs the interface cannot make. A wrong passphrase cannot decrypt it.
func (a *Appliance) Credential(ctx context.Context, passphrase, name string) ([]byte, error) {
	if name != "kubeconfig" && name != talosconfigName {
		return nil, fmt.Errorf("no credential %q", name)
	}
	out, err := a.stateOutput(ctx, passphrase, name)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// CheckPassphrase tells whether the passphrase opens the management cluster's state, ErrPassphrase
// when it does not.
func (a *Appliance) CheckPassphrase(ctx context.Context, passphrase string) error {
	_, err := a.stateOutput(ctx, passphrase, "api_endpoint")
	return err
}

// LoadLast restores the last jobs' records after a restart, so their outcomes are still shown. A
// job recorded as running was cut off by the restart.
func (a *Appliance) LoadLast() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, slot := range []**Job{&a.job, &a.templateJob} {
		raw, err := os.ReadFile(a.recordFile(slot))
		if err != nil {
			continue
		}
		var v JobView
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		if v.State == StateRunning {
			v.State, v.Error = StateFailed, "the appliance stopped during the run"
		}
		*slot = &Job{User: v.User, Automatic: v.Automatic, Kind: v.Kind, Started: v.Started, Finished: v.Finished, State: v.State, Error: v.Error, Stale: v.Stale, Request: v.Request, log: v.Log}
	}
}

// Stop refuses new jobs and interrupts the running ones with ErrServiceStopped, which lets
// OpenTofu write its state. It waits until they have recorded their outcome, or ctx ends.
func (a *Appliance) Stop(ctx context.Context) error {
	a.mu.Lock()
	a.stopping = true
	var running []*Job
	for _, j := range []*Job{a.job, a.templateJob} {
		if j.running() && j.stop != nil {
			j.stop(ErrServiceStopped)
			running = append(running, j)
		}
	}
	a.mu.Unlock()
	for _, j := range running {
		a.Log.Info("waiting for the run under way to end", "kind", j.Kind)
		select {
		case <-j.done:
		case <-ctx.Done():
			return fmt.Errorf("waiting for the %s run to end: %w", j.Kind, ctx.Err())
		}
	}
	return nil
}

// removalEnv is OpenTofu's environment for a removal, once nothing stands in the removal's way.
func (a *Appliance) removalEnv(ctx context.Context, j *Job, passphrase string, ticket *pve.Ticket) ([]string, error) {
	env, err := a.env(passphrase, ticketAuth(ticket))
	if err != nil {
		return nil, err
	}
	// A wrong passphrase must stop the removal before anything is deleted.
	if _, err := a.output(ctx, env, "api_endpoint"); err != nil {
		return nil, ErrPassphrase
	}
	if err := a.checkLease(ctx, env); err != nil {
		return nil, err
	}
	// Last, so that a refused removal leaves the backup under way alone. What that snapshot writes
	// must be on disk before the removal clears it.
	return env, a.stopEtcdSnapshot(ctx, j)
}

// forgetRemoved clears what the appliance kept of a management cluster just destroyed. The state
// now records nothing. It goes, so that the next setup starts with a passphrase of its own. The
// settings stay to fill that setup in. The etcd snapshots stay in their target and on a kept
// backup volume, and the keys and the talosconfigs that took them go, so that nothing is scheduled
// for a cluster that is gone and the next setup makes a key of its own. The appliance's copy of the
// latest backups stays for the removed cluster alone (forgetRemovedWithCopy deletes it).
func (a *Appliance) forgetRemoved() error {
	for _, name := range []string{
		a.KubeconfigPath(), a.TokenPath(), filepath.Join(a.workdir(), "terraform.tfstate"), filepath.Join(a.workdir(), "terraform.tfstate.backup"), a.appliedPath(), a.etcdStartsPath(),
		a.etcdBackupFile("volume.json"), a.etcdBackupFile("volume-status.json"), a.etcdBackupFile(volumeTalosconfig),
	} {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clearing the removed cluster's state: %w", err)
		}
	}
	return a.StopEtcdBackups()
}

// forgetRemovedWithCopy is forgetRemoved where the backup volume went too, which takes the
// appliance's copy of the latest backups with it.
func (a *Appliance) forgetRemovedWithCopy() error {
	if err := os.RemoveAll(a.applianceCopy().Root); err != nil {
		return fmt.Errorf("deleting the appliance's copy of the backups: %w", err)
	}
	return a.forgetRemoved()
}

// stateOutput reads one output of the encrypted state with the passphrase, outside any run.
func (a *Appliance) stateOutput(ctx context.Context, passphrase, name string) (string, error) {
	if err := ValidPassphrase(passphrase); err != nil {
		return "", err
	}
	a.mu.Lock()
	running := a.job.running()
	a.mu.Unlock()
	if running {
		return "", ErrBusy
	}
	if _, err := os.Stat(filepath.Join(a.workdir(), "terraform.tfstate")); err != nil {
		return "", errors.New("there is no management cluster state yet")
	}
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + a.Dir, "TF_IN_AUTOMATION=1", "TF_ENCRYPTION=" + encryption(passphrase)}
	out, err := a.output(ctx, env, name)
	if err != nil {
		return "", ErrPassphrase
	}
	return out, nil
}

func (a *Appliance) workdir() string { return filepath.Join(a.Dir, "management") }

// haReleasePath marks a run that switched Proxmox VE HA off and has not got through yet.
func (a *Appliance) haReleasePath() string { return filepath.Join(a.workdir(), "ha-release") }

// running is Idle with a.mu held.
func (a *Appliance) running() error {
	switch {
	case a.stopping:
		return ErrStopping
	case a.job.running():
		return ErrBusy
	case a.templateJob.running():
		return ErrTemplateBusy
	}
	return nil
}

// start runs one run of the module in the background, on the user's ticket.
func (a *Appliance) start(ticket *pve.Ticket, what string, run func(ctx context.Context, j *Job) error) error {
	return a.startIn(&a.job, sessionRunner(ticket), what, nil, run)
}

// runner is whom a job acts as, and until when it may run.
type runner struct {
	user      string
	automatic bool
	until     time.Time
}

// sessionRunner runs a job on a signed-in user's ticket, which it cannot outlive.
func sessionRunner(t *pve.Ticket) runner {
	return runner{user: t.Username, until: t.Issued.Add(pve.TicketLifetime)}
}

// startIn runs one job in the background and keeps it in slot. Only one job of any kind runs at a
// time. The job outlives the request that started it, but not the time its runner allows. It is
// recorded on disk when it starts and when it ends, so a restart in between shows it as cut off.
// request, which may be nil, is kept with it.
func (a *Appliance) startIn(slot **Job, by runner, what string, request any, run func(ctx context.Context, j *Job) error) error {
	var asked json.RawMessage
	if request != nil {
		raw, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("recording what the run was asked: %w", err)
		}
		asked = raw
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.running(); err != nil {
		return err
	}
	unlock, err := tryLock(a.Dir)
	switch {
	case errors.Is(err, errLocked):
		return ErrUpgrading
	case err != nil:
		return err
	}
	j := &Job{User: by.user, Automatic: by.automatic, Kind: what, Started: time.Now(), State: StateRunning, Request: asked, done: make(chan struct{})}
	*slot = j
	a.saveLog(slot, &JobView{User: j.User, Automatic: j.Automatic, Kind: what, Started: j.Started, State: StateRunning, Request: asked, Log: []string{}})
	ctx, cancel := context.WithDeadline(context.Background(), by.until)
	ctx, j.stop = context.WithCancelCause(ctx)
	go func() {
		defer close(j.done)
		defer cancel()
		defer j.stop(nil)
		err := run(ctx, j)
		state, errText := StateSucceeded, ""
		if err != nil {
			state, errText = StateFailed, err.Error()
		}
		var stale *StaleStateError
		errors.As(err, &stale)
		// The record is on disk before the outcome shows, so a restart right after still has it.
		// From then on an upgrade may restart the service, and a new job may start once the
		// outcome shows.
		a.saveLog(slot, &JobView{User: j.User, Automatic: j.Automatic, Kind: what, Started: j.Started, Finished: time.Now(), State: state, Error: errText, Stale: stale, Request: j.Request, Log: j.Log()})
		unlock()
		j.mu.Lock()
		j.Finished, j.State, j.Error, j.Stale = time.Now(), state, errText, stale
		j.mu.Unlock()
		if err != nil {
			a.Log.Error(what+" failed", "user", j.User, "error", err)
			return
		}
		a.Log.Info(what+" succeeded", "user", j.User)
	}()
	return nil
}

// recordFile is where the last job of a slot is recorded: runs of the module and template builds
// apart, as each is shown in its own place.
func (a *Appliance) recordFile(slot **Job) string {
	if slot == &a.templateJob {
		return filepath.Join(a.Dir, "last-template.json")
	}
	return filepath.Join(a.workdir(), "last-apply.json")
}

func (a *Appliance) saveLog(slot **Job, v *JobView) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(a.recordFile(slot)), privateDir)
	}
	if err == nil {
		err = writeFileAtomic(a.recordFile(slot), raw)
	}
	if err != nil {
		a.Log.Error("saving the job's record", "kind", v.Kind, "error", err)
	}
}

// writeFileAtomic replaces a file so that a crash, or a backup snapshot taken at any moment,
// sees either the old content or the new.
func writeFileAtomic(path string, data []byte) error {
	return writeAtomicFrom(path, bytes.NewReader(data), privateFile)
}

// writeAtomicFrom is writeFileAtomic for content read from r, which it never holds whole, with the
// given mode.
func writeAtomicFrom(path string, r io.Reader, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	err = tmp.Chmod(mode)
	if err == nil {
		_, err = io.Copy(tmp, r)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
