package appliance

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// Applied is what the last successful apply ran: the appliance's release and the versions its
// module pins.
type Applied struct {
	Release  string            `json:"release"`
	Versions map[string]string `json:"versions,omitempty"`
	Time     time.Time         `json:"time"`
	// CredentialsExpire is when the first of the admin kubeconfig's and talosconfig's client
	// certificates the apply left in the state runs out. Talos's provider renews them on an apply in
	// their last month, and nothing else does: the state opens only with the passphrase.
	CredentialsExpire time.Time `json:"credentialsExpire,omitzero"`
}

func (a *Appliance) appliedPath() string { return filepath.Join(a.workdir(), "applied.json") }

// LastApplied returns what the last successful apply ran, or nil before any.
func (a *Appliance) LastApplied() *Applied {
	raw, err := os.ReadFile(a.appliedPath())
	if err != nil {
		return nil
	}
	var ap Applied
	if json.Unmarshal(raw, &ap) != nil {
		return nil
	}
	return &ap
}

func (a *Appliance) recordApplied(credentialsExpire time.Time) error {
	raw, err := json.MarshalIndent(Applied{Release: a.Release, Versions: a.ModuleVersions(), Time: time.Now(), CredentialsExpire: credentialsExpire}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding what the apply ran: %w", err)
	}
	return writeFileAtomic(a.appliedPath(), raw)
}

// ModuleVersions are the versions the module released with the appliance pins, by the keys of
// its tested set (versions.tf), with the charts beneath the providers (cert_manager,
// capi_operator) and Kubehenge's own chart (kubehenge).
func (a *Appliance) ModuleVersions() map[string]string {
	out := map[string]string{}
	if raw, err := os.ReadFile(filepath.Join(a.Module, "versions.tf")); err == nil {
		block := ""
		for line := range strings.Lines(string(raw)) {
			if m := localBlock.FindStringSubmatch(line); m != nil {
				block = m[1]
				continue
			}
			if strings.TrimRight(line, "\r\n") == "  }" {
				block = ""
				continue
			}
			m := blockEntry.FindStringSubmatch(line)
			switch {
			case m == nil:
			case block == "tested_versions":
				out[m[1]] = m[2]
			case (block == "cert_manager" || block == "capi_operator") && m[1] == "chart":
				version, _, _ := strings.Cut(m[2], "@")
				out[block] = version
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(a.Module, "..", "charts", "kubehenge", "Chart.yaml")); err == nil {
		if m := chartVersion.FindSubmatch(raw); m != nil {
			out["kubehenge"] = string(m[1])
		}
	}
	return out
}

// The module's versions.tf is held to tofu fmt, so its locals sit at two spaces and their entries
// at four. The images nested deeper are not versions of their own.
var (
	localBlock   = regexp.MustCompile(`^ {2}(\w+) = \{\s*$`)
	blockEntry   = regexp.MustCompile(`^ {4}(\w+)\s*=\s*"([^"]+)"`)
	chartVersion = regexp.MustCompile(`(?m)^version:\s*"?([^"\s]+)"?\s*$`)
)

// Unattended is what the appliance keeps, only when an administrator chose so, to apply the
// management cluster with nobody signed in: the state passphrase, which no derived key can stand in
// for (OpenTofu derives a new key for every state it writes), and an API token of a Proxmox VE user
// of its own, as user@realm!name=secret. Both are kept sealed.
type Unattended struct {
	Passphrase string
	Token      string
	By         string
	Since      time.Time
}

// ErrNotUnattended is an automatic apply asked for while the appliance keeps nothing to run it.
var ErrNotUnattended = errors.New("automatic upgrades of the management cluster are off: the appliance keeps nothing to apply it with")

// ErrUnattendedKey is what automatic upgrades keep without the key that opens it, as after the
// appliance was restored from a backup, which leaves the key out.
var ErrUnattendedKey = errors.New("the key that opens what automatic upgrades keep is gone, as after a restore from a backup: switch automatic upgrades of the management cluster off and on again")

// unattendedPath sits in the appliance's own directory, which only its user may read. It holds the
// passphrase and the token sealed with the key at unattendedKeyPath.
func (a *Appliance) unattendedPath() string { return filepath.Join(a.Dir, "unattended.json") }

// unattendedKeyPath is on a volume of its own that Proxmox VE's backups of the container leave
// out, so that a backup, which holds the state, never holds what opens it as well.
func (a *Appliance) unattendedKeyPath() string { return filepath.Join(a.KeyDir, "unattended.key") }

// unattendedFile is what the appliance keeps on disk: who switched automatic upgrades on, when, and
// the Proxmox VE user the runs act as, in the open. The passphrase and token are sealed.
type unattendedFile struct {
	By     string    `json:"by"`
	Since  time.Time `json:"since"`
	User   string    `json:"user"`
	Sealed []byte    `json:"sealed"`
}

// unattendedSecrets is what is sealed.
type unattendedSecrets struct {
	Passphrase string `json:"passphrase"`
	Token      string `json:"token"`
}

// unattendedSealing binds a sealed value to its use, so that no other sealed value opens as it.
var unattendedSealing = []byte("kubehenge unattended upgrades")

// sealKeySize is AES-256's key size.
const sealKeySize = 32

// KeepUnattended keeps what an unattended apply needs, sealed with a new key. The caller has
// checked the passphrase against the state (CheckPassphrase) and made the token.
func (a *Appliance) KeepUnattended(u Unattended) error {
	if err := ValidPassphrase(u.Passphrase); err != nil {
		return err
	}
	id, secret, ok := strings.Cut(u.Token, "=")
	if !ok || !strings.Contains(id, "!") || secret == "" {
		return errors.New("the Proxmox VE API token must be user@realm!name=secret")
	}
	if a.KeyDir == "" {
		return errors.New("this appliance has no volume to keep the key of automatic upgrades on: upgrade it with install.sh --upgrade")
	}
	key := make([]byte, sealKeySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("making the key of automatic upgrades: %w", err)
	}
	plain, err := json.Marshal(unattendedSecrets{Passphrase: u.Passphrase, Token: u.Token})
	if err != nil {
		return fmt.Errorf("encoding what automatic upgrades keep: %w", err)
	}
	sealed, err := seal(key, plain)
	if err != nil {
		return err
	}
	user, _, _ := strings.Cut(id, "!")
	raw, err := json.Marshal(unattendedFile{By: u.By, Since: u.Since, User: user, Sealed: sealed})
	if err != nil {
		return fmt.Errorf("encoding what automatic upgrades keep: %w", err)
	}
	if err := writeFileAtomic(a.unattendedKeyPath(), key); err != nil {
		return fmt.Errorf("keeping the key of automatic upgrades: %w", err)
	}
	return writeFileAtomic(a.unattendedPath(), raw)
}

// ForgetUnattended deletes what KeepUnattended kept, and its key. Nothing kept is no error.
func (a *Appliance) ForgetUnattended() error {
	paths := []string{a.unattendedPath()}
	if a.KeyDir != "" {
		paths = append(paths, a.unattendedKeyPath())
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("deleting what automatic upgrades kept: %w", err)
		}
	}
	return nil
}

func seal(key, plain []byte) ([]byte, error) {
	gcm, err := sealCipher(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("sealing what automatic upgrades keep: %w", err)
	}
	return gcm.Seal(nonce, nonce, plain, unattendedSealing), nil
}

func unseal(key, sealed []byte) ([]byte, error) {
	gcm, err := sealCipher(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, ErrUnattendedKey
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], unattendedSealing)
	if err != nil {
		return nil, ErrUnattendedKey
	}
	return plain, nil
}

func sealCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != sealKeySize {
		return nil, ErrUnattendedKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("the key of automatic upgrades: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("the key of automatic upgrades: %w", err)
	}
	return gcm, nil
}

// UnattendedInfo is what may be shown of what KeepUnattended kept: who chose it and when, and the
// Proxmox VE user the runs act as. It never shows the secrets.
type UnattendedInfo struct {
	By    string
	Since time.Time
	User  string
}

// UnattendedOn returns what may be shown of what the appliance keeps for unattended applies, or
// nil while it keeps nothing.
func (a *Appliance) UnattendedOn() *UnattendedInfo {
	f, err := a.unattendedFile()
	if err != nil {
		return nil
	}
	return &UnattendedInfo{By: f.By, Since: f.Since, User: f.User}
}

func (a *Appliance) unattendedFile() (*unattendedFile, error) {
	raw, err := os.ReadFile(a.unattendedPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotUnattended
	}
	if err != nil {
		return nil, fmt.Errorf("reading what automatic upgrades keep: %w", err)
	}
	var f unattendedFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("reading what automatic upgrades keep: %w", err)
	}
	return &f, nil
}

// unattended opens what KeepUnattended kept.
func (a *Appliance) unattended() (*Unattended, error) {
	f, err := a.unattendedFile()
	if err != nil {
		return nil, err
	}
	if a.KeyDir == "" {
		return nil, ErrUnattendedKey
	}
	key, err := os.ReadFile(a.unattendedKeyPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrUnattendedKey
	}
	if err != nil {
		return nil, fmt.Errorf("reading the key of automatic upgrades: %w", err)
	}
	plain, err := unseal(key, f.Sealed)
	if err != nil {
		return nil, err
	}
	var s unattendedSecrets
	if err := json.Unmarshal(plain, &s); err != nil {
		return nil, fmt.Errorf("reading what automatic upgrades keep: %w", err)
	}
	return &Unattended{Passphrase: s.Passphrase, Token: s.Token, By: f.By, Since: f.Since}, nil
}

// ApplyUnattended starts applying the saved settings again with what KeepUnattended kept, as an
// automatic run, and returns at once. It fails with ErrNotUnattended while nothing is kept. What
// keeps the run from going ahead once it has started fails it, so that its record says why. ready
// is called as for Apply.
func (a *Appliance) ApplyUnattended(ready func(ctx context.Context, kubeconfig []byte) error) error {
	u, err := a.unattended()
	if err != nil {
		return err
	}
	user, _, _ := strings.Cut(u.Token, "!")
	// As long as a run on an administrator's session may take.
	by := runner{user: user, automatic: true, until: time.Now().Add(pve.TicketLifetime)}
	return a.startIn(&a.job, by, "apply", nil, func(ctx context.Context, j *Job) error {
		s := a.SavedSettings()
		if s == nil || !a.Ready() {
			return errors.New("there is no management cluster to upgrade")
		}
		if err := s.Validate(); err != nil {
			return err
		}
		if a.ReleasesHA(s) {
			return errors.New("switching Proxmox VE HA off has not got through yet, and only an administrator's session can finish it: apply again from the Administration page")
		}
		from := "an earlier release"
		if last := a.LastApplied(); last != nil && last.Release != "" {
			from = last.Release
		}
		j.Add(fmt.Sprintf("Upgrading the management cluster automatically, from what %s applied to what %s carries", from, a.Release))
		kubeconfig, err := a.run(ctx, j, s, u.Passphrase, tokenAuth(u.Token))
		if err != nil {
			return err
		}
		if err := ready(ctx, kubeconfig); err != nil {
			return fmt.Errorf("the management cluster is up, but the interface cannot use it: %w", err)
		}
		return nil
	})
}
