package appliance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"filippo.io/age"
)

// The state's key opens OpenTofu's encrypted state of the management cluster and the snapshots'
// keys. The appliance makes it at the first setup and keeps it beside the state, readable by its
// own user alone and in its own backups, so that the signed-in user's Proxmox VE privileges are the
// only check on the actions that open the state. An administrator may require a passphrase on top:
// the key is then kept only sealed with that passphrase, and every such action asks for it. Sealing
// the key, rather than encrypting the state again, leaves the snapshots' keys sealed as they are.

// ErrNeedsPassphrase is an action on a state that opens only with a passphrase, given none.
var ErrNeedsPassphrase = errors.New("the management cluster's state opens only with its passphrase: give it")

// stateKeyBytes is the key's randomness, 256 bits.
const stateKeyBytes = 32

func (a *Appliance) stateKeyPath() string { return filepath.Join(a.workdir(), "state.key") }

func (a *Appliance) sealedStateKeyPath() string { return filepath.Join(a.workdir(), "state.key.age") }

// PassphraseRequired tells whether an administrator chose to require a passphrase to open the
// management cluster's state.
func (a *Appliance) PassphraseRequired() bool {
	_, err := os.Stat(a.sealedStateKeyPath())
	return err == nil
}

// PassphraseNeeded tells whether the actions that open the state take a passphrase: one is
// required, or the state was made before the appliance kept its key and no action has opened it
// since.
func (a *Appliance) PassphraseNeeded() bool {
	if a.PassphraseRequired() {
		return true
	}
	_, err := os.Stat(a.stateKeyPath())
	return err != nil && a.HasState()
}

// statePassphrase is what opens the state for an action given passphrase: the kept key, the key
// the passphrase unseals where one is required, or, for a state made before the appliance kept
// its key, the passphrase itself, which is kept as the key once it opens the state. Before any
// state, it makes the key.
func (a *Appliance) statePassphrase(ctx context.Context, passphrase string) (string, error) {
	sealed, err := os.ReadFile(a.sealedStateKeyPath())
	switch {
	case err == nil:
		key, openErr := openStateKey(sealed, passphrase)
		if openErr != nil {
			return "", openErr
		}
		// Finishes a switch to a required passphrase that stopped before it deleted the kept key.
		return key, removeFile(a.stateKeyPath())
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("reading the state's sealed key: %w", err)
	}
	key, err := os.ReadFile(a.stateKeyPath())
	switch {
	case err == nil:
		return string(key), nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("reading the state's key: %w", err)
	case !a.HasState():
		return a.makeStateKey()
	case passphrase == "":
		return "", ErrNeedsPassphrase
	}
	if err := ValidPassphrase(passphrase); err != nil {
		return "", err
	}
	// A state whose first run failed has no outputs to try the passphrase on. It stays asked for
	// until a run gets through.
	if _, err := a.output(ctx, a.stateEnv(passphrase), "api_endpoint"); err == nil {
		if err := a.keepKeyOpen(passphrase); err != nil {
			return "", err
		}
	}
	return passphrase, nil
}

func (a *Appliance) makeStateKey() (string, error) {
	raw := make([]byte, stateKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("making the state's key: %w", err)
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
		return "", fmt.Errorf("preparing the management cluster's folder: %w", err)
	}
	return key, a.keepKeyOpen(key)
}

// keepKeyOpen keeps key in the open, where automatic upgrades then find it too, so they keep no
// copy of their own.
func (a *Appliance) keepKeyOpen(key string) error {
	if err := writeFileAtomic(a.stateKeyPath(), []byte(key)); err != nil {
		return fmt.Errorf("keeping the state's key: %w", err)
	}
	return a.rekeepUnattended("")
}

// RequirePassphrase seals the state's key with next and deletes the kept key, so that every action
// that opens the state asks for next. current opens a state made before the appliance kept its key.
// Automatic upgrades, when on, keep next to apply with.
func (a *Appliance) RequirePassphrase(ctx context.Context, current, next string) error {
	if a.PassphraseRequired() {
		return errors.New("a passphrase is required already: switch it off first to set another")
	}
	if err := ValidPassphrase(next); err != nil {
		return err
	}
	key, err := a.statePassphrase(ctx, current)
	if err != nil {
		return err
	}
	sealed, err := a.sealWith(next, []byte(key))
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.sealedStateKeyPath(), sealed); err != nil {
		return fmt.Errorf("keeping the state's sealed key: %w", err)
	}
	if err := a.rekeepUnattended(next); err != nil {
		return err
	}
	return removeFile(a.stateKeyPath())
}

// KeepStateKey stops requiring a passphrase: the key passphrase unseals is kept again.
func (a *Appliance) KeepStateKey(ctx context.Context, passphrase string) error {
	if !a.PassphraseRequired() {
		return nil
	}
	key, err := a.statePassphrase(ctx, passphrase)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.stateKeyPath(), []byte(key)); err != nil {
		return fmt.Errorf("keeping the state's key: %w", err)
	}
	if err := removeFile(a.sealedStateKeyPath()); err != nil {
		return err
	}
	return a.rekeepUnattended("")
}

// rekeepUnattended keeps what automatic upgrades keep again with passphrase, empty where the key is
// kept. Nothing kept, or nothing that still opens, stays as it is.
func (a *Appliance) rekeepUnattended(passphrase string) error {
	u, err := a.unattended()
	if err != nil || u.Passphrase == passphrase {
		return nil //nolint:nilerr // nothing to keep again
	}
	u.Passphrase = passphrase
	return a.KeepUnattended(*u)
}

func openStateKey(sealed []byte, passphrase string) (string, error) {
	if passphrase == "" {
		return "", ErrNeedsPassphrase
	}
	with, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return "", ErrPassphrase
	}
	r, err := age.Decrypt(bytes.NewReader(sealed), with)
	if err != nil {
		return "", ErrPassphrase
	}
	key, err := io.ReadAll(r)
	if err != nil {
		return "", ErrPassphrase
	}
	return string(key), nil
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting %s: %w", path, err)
	}
	return nil
}

// stateEnv is OpenTofu's environment for reading the state with passphrase, outside any run.
func (a *Appliance) stateEnv(passphrase string) []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + a.Dir, "TF_IN_AUTOMATION=1", "TF_ENCRYPTION=" + encryption(passphrase)}
}

// stateCredential is Credential with the state's key.
func (a *Appliance) stateCredential(ctx context.Context, key, name string) ([]byte, error) {
	if name != "kubeconfig" && name != talosconfigName {
		return nil, fmt.Errorf("no credential %q", name)
	}
	out, err := a.stateOutput(ctx, key, name)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}
