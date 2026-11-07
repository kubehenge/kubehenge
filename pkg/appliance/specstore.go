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
	"strings"

	"filippo.io/age"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// Why a spec's Secrets cannot be opened.
var (
	// ErrNoSpecKey means the appliance holds no recovery key for the Secrets in specs.
	ErrNoSpecKey = errors.New("this appliance holds no recovery key for the Secrets in specs")
	// ErrWrongSpecKey means the Secret was sealed with another appliance's recovery key.
	ErrWrongSpecKey = errors.New("the Secret was sealed with another recovery key")
)

// StoreClusterSpec writes a cluster's exported spec into the target the management cluster's etcd
// is backed up to. Only the appliance holds that target's access key, which is why it writes the
// specs and the controller does not. It answers ErrNoEtcdBackups where there is no target.
func (a *Appliance) StoreClusterSpec(ctx context.Context, namespace, name string, spec []byte) error {
	b, err := a.backupSetup()
	if err != nil {
		return err
	}
	if _, err := b.store.Put(ctx, provisioner.ClusterSpecKey(b.target.Prefix, namespace, name), bytes.NewReader(spec), map[string]string{"content-type": "application/yaml"}); err != nil {
		return fmt.Errorf("storing the spec of %s/%s in target %s: %w", namespace, name, b.target.Name, err)
	}
	return nil
}

func (a *Appliance) specKeyFile() string { return a.etcdBackupFile("spec-recovery.key") }

// specIdentity reads the recovery key, which never leaves the appliance except in an export.
func (a *Appliance) specIdentity() (*age.X25519Identity, error) {
	raw, err := os.ReadFile(a.specKeyFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoSpecKey
	}
	if err != nil {
		return nil, fmt.Errorf("reading the recovery key: %w", err)
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("reading the recovery key: %w", err)
	}
	return id, nil
}

// ensureSpecIdentity reads the recovery key and makes it where there is none.
func (a *Appliance) ensureSpecIdentity() (*age.X25519Identity, error) {
	id, err := a.specIdentity()
	if !errors.Is(err, ErrNoSpecKey) {
		return id, err
	}
	id, err = age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("making the recovery key: %w", err)
	}
	if err = os.MkdirAll(a.etcdBackupDir(), privateDir); err != nil {
		return nil, fmt.Errorf("preparing the backups' folder: %w", err)
	}
	if err = writeFileAtomic(a.specKeyFile(), []byte(id.String())); err != nil {
		return nil, err
	}
	return id, nil
}

// SealSpecSecret seals a Secret's data for a spec with the recovery key's public half, making the
// key on first use. Only a holder of the key opens it.
func (a *Appliance) SealSpecSecret(plain []byte) (string, error) {
	id, err := a.ensureSpecIdentity()
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, id.Recipient())
	if err != nil {
		return "", fmt.Errorf("sealing a Secret: %w", err)
	}
	if _, err = w.Write(plain); err != nil {
		return "", fmt.Errorf("sealing a Secret: %w", err)
	}
	if err = w.Close(); err != nil {
		return "", fmt.Errorf("sealing a Secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// OpenSpecSecret opens what SealSpecSecret sealed. It answers ErrNoSpecKey where the appliance has
// no key and ErrWrongSpecKey where the key is not the one that sealed it.
func (a *Appliance) OpenSpecSecret(sealed string) ([]byte, error) {
	id, err := a.specIdentity()
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, fmt.Errorf("the sealed Secret is damaged: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(raw), id)
	if _, wrong := errors.AsType[*age.NoIdentityMatchError](err); wrong {
		return nil, ErrWrongSpecKey
	}
	if err != nil {
		return nil, fmt.Errorf("the sealed Secret is damaged: %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("the sealed Secret is damaged: %w", err)
	}
	return plain, nil
}
