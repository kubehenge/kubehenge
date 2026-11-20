package acme

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

// The files of the appliance's directory that hold ACME's setting, its progress and its account.
// All start with appliance.ACMEFilePrefix, so that whoever puts the installer's certificate back
// can remove them with one pattern.
const (
	settingFile = appliance.ACMEFilePrefix + "setting.json"
	// StateFile holds the State, which has no secret, so the installer reads it to follow an order.
	StateFile   = appliance.ACMEFilePrefix + "state.json"
	accountFile = appliance.ACMEFilePrefix + "account.json"
)

var files = []string{settingFile, StateFile, accountFile}

// store keeps the files. The setting and the account hold secrets, so each is written with mode
// 0600 and replaced whole.
type store interface {
	read(name string) ([]byte, error)
	write(name string, data []byte) error
	remove(name string) error
}

type dirStore struct{ dir string }

func (s dirStore) read(name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, name)) //nolint:gosec // one of the package's own file names
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return data, nil
}

// write puts data in a temporary file beside the target, syncs it and renames it over the target,
// so that a crash leaves the old file or the new one.
func (s dirStore) write(name string, data []byte) (err error) {
	f, err := os.CreateTemp(s.dir, name+".*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing %s: %w", name, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", name, err)
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("replacing %s: %w", name, err)
	}
	return nil
}

func (s dirStore) remove(name string) error {
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", name, err)
	}
	return nil
}

// memoryStore keeps the files in memory, for the demo.
type memoryStore struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (s *memoryStore) read(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[name]
	if !ok {
		return nil, fmt.Errorf("reading %s: %w", name, fs.ErrNotExist)
	}
	return append([]byte(nil), data...), nil
}

func (s *memoryStore) write(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[name] = append([]byte(nil), data...)
	return nil
}

func (s *memoryStore) remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, name)
	return nil
}

// leftovers removes the temporary files a write cut off by a crash left, which may hold secrets.
func (s dirStore) leftovers() {
	for _, name := range files {
		left, _ := filepath.Glob(filepath.Join(s.dir, name+".*"))
		for _, f := range left {
			_ = os.Remove(f)
		}
	}
}
