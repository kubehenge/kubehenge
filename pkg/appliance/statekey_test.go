package appliance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const hardened = "a passphrase only the admin knows"

// setUp applies a first time with no passphrase and writes the state the real tofu would.
func setUp(t *testing.T) *Appliance {
	t.Helper()
	a, _ := newAppliance(t)
	if err := a.Apply(settings("pve1"), "", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("setup without a passphrase: %s: %s", j.State, j.Error)
	}
	if err := os.WriteFile(filepath.Join(a.Dir, "management", "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return a
}

// requirePassphrase makes the test's passphrase required, so that a wrong one is refused.
func requirePassphrase(t *testing.T, a *Appliance) {
	t.Helper()
	if err := a.RequirePassphrase(t.Context(), passphrase, passphrase); err != nil {
		t.Fatal(err)
	}
}

func credentialWith(a *Appliance, passphrase string) error {
	_, err := a.Credential(context.Background(), passphrase, "kubeconfig")
	return err
}

func TestSetupKeepsTheStateKey(t *testing.T) {
	t.Parallel()
	a := setUp(t)
	kept, err := os.Stat(a.stateKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && kept.Mode().Perm() != 0o600 {
		t.Fatalf("the key kept with mode %v", kept.Mode())
	}
	if a.PassphraseNeeded() || a.PassphraseRequired() {
		t.Fatal("a passphrase asked for on a fresh appliance")
	}
	if err := credentialWith(a, ""); err != nil {
		t.Fatalf("a credential without a passphrase: %v", err)
	}
}

func TestRequiredPassphrase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := setUp(t)
	if err := a.RequirePassphrase(ctx, "", "short"); err == nil {
		t.Fatal("required a passphrase too short to be one")
	}
	if err := a.RequirePassphrase(ctx, "", hardened); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.stateKeyPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the key is still kept: %v", err)
	}
	if !a.PassphraseRequired() || !a.PassphraseNeeded() {
		t.Fatal("not shown as required")
	}
	if err := credentialWith(a, ""); !errors.Is(err, ErrNeedsPassphrase) {
		t.Fatalf("no passphrase: %v", err)
	}
	if err := credentialWith(a, "the wrong passphrase entirely"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("a wrong passphrase: %v", err)
	}
	if err := credentialWith(a, hardened); err != nil {
		t.Fatalf("the passphrase: %v", err)
	}
	if err := a.RequirePassphrase(ctx, hardened, "another passphrase entirely"); err == nil {
		t.Fatal("required a second passphrase over the first")
	}
	if err := a.KeepStateKey(ctx, "the wrong passphrase entirely"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("switched off with a wrong passphrase: %v", err)
	}
	if err := a.KeepStateKey(ctx, hardened); err != nil {
		t.Fatal(err)
	}
	if a.PassphraseRequired() || a.PassphraseNeeded() {
		t.Fatal("still required after switching off")
	}
	if err := credentialWith(a, ""); err != nil {
		t.Fatalf("the kept key after switching off: %v", err)
	}
}

// An appliance set up with a passphrase keeps it as the key once an action opens the state with it.
func TestAPassphraseStateKeepsItsKey(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if err := os.MkdirAll(a.workdir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.workdir(), "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !a.PassphraseNeeded() {
		t.Fatal("no passphrase asked for a state made with one")
	}
	if err := credentialWith(a, ""); !errors.Is(err, ErrNeedsPassphrase) {
		t.Fatalf("no passphrase: %v", err)
	}
	if err := credentialWith(a, "the wrong passphrase entirely"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("a wrong passphrase: %v", err)
	}
	if _, err := os.Stat(a.stateKeyPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a wrong passphrase was kept: %v", err)
	}
	if err := credentialWith(a, passphrase); err != nil {
		t.Fatal(err)
	}
	if kept, err := os.ReadFile(a.stateKeyPath()); err != nil || string(kept) != passphrase {
		t.Fatalf("kept %q, %v", kept, err)
	}
	if a.PassphraseNeeded() || credentialWith(a, "") != nil {
		t.Fatal("the kept key does not open the state")
	}
}

// What automatic upgrades keep holds the passphrase only while one is required.
func TestUnattendedKeepsThePassphraseOnlyWhenRequired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := setUp(t)
	kept := func() string {
		t.Helper()
		u, err := a.unattended()
		if err != nil {
			t.Fatal(err)
		}
		return u.Passphrase
	}
	if err := a.KeepUnattended(Unattended{Passphrase: "anything at all here", Token: upgraderAPI}); err != nil {
		t.Fatal(err)
	}
	if p := kept(); p != "" {
		t.Fatalf("kept %q beside the key", p)
	}
	if err := a.RequirePassphrase(ctx, "", hardened); err != nil {
		t.Fatal(err)
	}
	if p := kept(); p != hardened {
		t.Fatalf("kept %q once required", p)
	}
	if err := a.KeepStateKey(ctx, hardened); err != nil {
		t.Fatal(err)
	}
	if p := kept(); p != "" {
		t.Fatalf("kept %q after switching off", p)
	}
}
