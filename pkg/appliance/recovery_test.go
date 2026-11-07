package appliance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const exportPassphrase = "an export passphrase of its own"

func backedUp(t *testing.T) (*Appliance, *memoryStore, *time.Time) {
	t.Helper()
	store := newMemoryStore()
	a, _ := backupAppliance(t, store)
	at := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return at }
	return a, store, &at
}

func configure(t *testing.T, a *Appliance, at time.Time) {
	t.Helper()
	s := EtcdBackupSettings{Target: "minio", EveryHours: 6, Keep: 3, By: "root@pam", Since: at}
	if err := a.ConfigureEtcdBackups(context.Background(), s, "AKID", "secret", passphrase); err != nil {
		t.Fatal(err)
	}
}

// freshAppliance is a new appliance on the same bucket, with no management cluster of its own.
func freshAppliance(t *testing.T, store *memoryStore) (*Appliance, *fakeManagementTalos) {
	t.Helper()
	fresh, talos := backupAppliance(t, store)
	for _, name := range []string{"management/terraform.tfstate", "management/settings.json", "interface.kubeconfig"} {
		if err := os.Remove(filepath.Join(fresh.Dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return fresh, talos
}

func TestEtcdBackupWarningStandsUntilDismissedForThirtyDays(t *testing.T) {
	t.Parallel()
	a, _, now := backedUp(t)
	st := a.EtcdBackups()
	if !st.Warning.Shown() {
		t.Fatalf("backups are off, yet no warning stands: %+v", st.Warning)
	}
	if err := a.DismissEtcdBackupWarning("alice@pve"); err != nil {
		t.Fatal(err)
	}
	st = a.EtcdBackups()
	d := st.Warning.Dismissal
	if st.Warning.Shown() || d == nil || d.By != "alice@pve" || !d.At.Equal(*now) || !d.Until.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("dismissal %+v", d)
	}
	*now = now.Add(29 * 24 * time.Hour)
	if a.EtcdBackups().Warning.Shown() {
		t.Fatal("the warning came back before the 30 days ended")
	}
	*now = now.Add(2 * 24 * time.Hour)
	if !a.EtcdBackups().Warning.Shown() {
		t.Fatal("the warning stayed silent past the 30 days")
	}
	configure(t, a, *now)
	if a.EtcdBackups().Warning != nil {
		t.Fatal("the warning stands with backups on")
	}
	if err := a.DismissEtcdBackupWarning("alice@pve"); !errors.Is(err, ErrNoWarning) {
		t.Fatalf("dismissed a warning that does not stand: %v", err)
	}
}

func TestEtcdBackupWarningIsOnlyForARunningManagementCluster(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if a.EtcdBackups().Warning != nil {
		t.Fatal("warned before any management cluster exists")
	}
	if err := a.DismissEtcdBackupWarning("alice@pve"); !errors.Is(err, ErrNoWarning) {
		t.Fatalf("dismissed with no management cluster: %v", err)
	}
}

func TestExportIsDueUntilTakenAndAfterEachChange(t *testing.T) {
	t.Parallel()
	a, _, now := backedUp(t)
	ctx := context.Background()
	if _, err := a.ExportState(ctx, passphrase, exportPassphrase); !errors.Is(err, ErrNoEtcdBackups) {
		t.Fatalf("export without backups: %v", err)
	}
	configure(t, a, *now)
	if a.EtcdBackups().ExportDue {
		t.Fatal("an export is due before any snapshot")
	}
	if _, err := a.TakeEtcdSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if st := a.EtcdBackups(); !st.ExportDue || st.ExportedAt != nil {
		t.Fatalf("after a snapshot with no export: %+v", st)
	}
	if _, err := a.ExportState(ctx, "wrong passphrase, long enough", exportPassphrase); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong state passphrase: %v", err)
	}
	if _, err := a.ExportState(ctx, passphrase, "short"); err == nil {
		t.Fatal("exported under a short passphrase")
	}
	bundle, err := a.ExportState(ctx, passphrase, exportPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"AKID", "secret", "endpoints"} {
		if bytes.Contains(bundle, []byte(secret)) {
			t.Fatalf("the export holds %q in the clear", secret)
		}
	}
	if st := a.EtcdBackups(); st.ExportDue || st.ExportedAt == nil {
		t.Fatalf("after an export: %+v", st)
	}
	keys, err := readJSONFile[etcdBackupKeys](a.etcdBackupFile("keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	keys.SecretAccessKey = "another secret"
	if err = writeJSONFile(a.etcdBackupFile("keys.json"), keys); err != nil {
		t.Fatal(err)
	}
	if !a.EtcdBackups().ExportDue {
		t.Fatal("a changed access key leaves the export current")
	}
}

func TestImportRestoresWhatTheExportCarried(t *testing.T) {
	t.Parallel()
	a, store, now := backedUp(t)
	ctx := context.Background()
	configure(t, a, *now)
	if _, err := a.TakeEtcdSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	bundle, err := a.ExportState(ctx, passphrase, exportPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	fresh, talos := freshAppliance(t, store)
	if err = fresh.ImportState(ctx, bundle, "not the export passphrase"); !errors.Is(err, ErrExportPassphrase) {
		t.Fatalf("wrong export passphrase: %v", err)
	}
	if fresh.HasState() || fresh.SavedSettings() != nil {
		t.Fatal("a refused import left files behind")
	}
	if err = fresh.ImportState(ctx, bundle, exportPassphrase); err != nil {
		t.Fatal(err)
	}
	if !fresh.HasState() || fresh.SavedSettings() == nil || fresh.EtcdBackups().Settings == nil || fresh.EtcdBackups().ExportDue {
		t.Fatalf("imported state %+v", fresh.EtcdBackups())
	}
	list, err := fresh.ListEtcdSnapshots(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("snapshots after the import: %v, %v", list, err)
	}
	if err = fresh.RestoreEtcd(ctx, list[0].Name, passphrase, ticket()); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, fresh); j.State != "succeeded" || !bytes.Equal(talos.restored, etcdDatabase()) {
		t.Fatalf("restore on the imported appliance: %+v", j)
	}
	if err = fresh.ImportState(ctx, bundle, exportPassphrase); !errors.Is(err, ErrHasState) {
		t.Fatalf("import over a state: %v", err)
	}
}

func TestImportRefusesATamperedOrForeignFile(t *testing.T) {
	t.Parallel()
	a, store, now := backedUp(t)
	ctx := context.Background()
	configure(t, a, *now)
	bundle, err := a.ExportState(ctx, passphrase, exportPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	for name, mangled := range map[string][]byte{
		"empty":   nil,
		"garbage": []byte("this is not an export"),
		"cut":     bundle[:len(bundle)/2],
		"body":    flip(bundle, len(bundle)-20),
		"header":  flip(bundle, 100),
	} {
		fresh, _ := freshAppliance(t, store)
		if err := fresh.ImportState(ctx, mangled, exportPassphrase); !errors.Is(err, ErrExportDamaged) {
			t.Errorf("%s: %v", name, err)
		}
		if fresh.HasState() {
			t.Errorf("%s: the refused import left a state", name)
		}
	}
}

func flip(b []byte, at int) []byte {
	out := slices.Clone(b)
	out[at] ^= 0xff
	return out
}

// tracedTalos records, when it restores, which applies had run: the order is what is tested.
type tracedTalos struct {
	fakeManagementTalos

	tofuLog string
	applies []string
}

func (f *tracedTalos) Restore(ctx context.Context, admin []byte, nodes []string, open func() (io.ReadCloser, error), log func(string)) error {
	f.applies = appliesIn(f.tofuLog)
	return f.fakeManagementTalos.Restore(ctx, admin, nodes, open, log)
}

// appliesIn lists the applies the fake OpenTofu was asked for, in order.
func appliesIn(log string) []string {
	raw, _ := os.ReadFile(filepath.Clean(log))
	var out []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, `{"args":["apply"`) {
			out = append(out, line)
		}
	}
	return out
}

func TestRecreateMakesTheVMsBeforeTheRestoreAndAppliesWholeAfter(t *testing.T) {
	t.Parallel()
	a, _, now := backedUp(t)
	ctx := context.Background()
	configure(t, a, *now)
	taken, err := a.TakeEtcdSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	traced := &tracedTalos{tofuLog: filepath.Join(a.Dir, "fake-tofu.log")}
	a.Talos = traced
	var kubeconfig []byte
	ready := func(_ context.Context, k []byte) error { kubeconfig = k; return nil }

	if err = a.RecreateManagement(ctx, "../identity.age", passphrase, ticket(), ready); err == nil {
		t.Fatal("recreated from something that is no snapshot")
	}
	if err = a.RecreateManagement(ctx, taken.Name, "wrong passphrase, long enough", ticket(), ready); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if err = a.RecreateManagement(ctx, taken.Name, passphrase, ticket(), ready); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	if j.State != "succeeded" || len(kubeconfig) == 0 {
		t.Fatalf("recreate %+v, kubeconfig %q", j, kubeconfig)
	}
	if len(traced.applies) != 1 || !strings.Contains(traced.applies[0], "-target=proxmox_virtual_environment_vm.node") ||
		!strings.Contains(traced.applies[0], "-target=module.talos_node_0") {
		t.Fatalf("before the restore, applies were %q: only the VMs and nodes belong there", traced.applies)
	}
	all := appliesIn(traced.tofuLog)
	if len(all) != 2 || strings.Contains(all[1], "-target") {
		t.Fatalf("applies in all: %q, want the targeted one, then a whole one", all)
	}
}
