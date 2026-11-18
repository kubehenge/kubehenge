package appliance

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// sessionsSealing binds the kept sessions to their use, so that no other sealed value opens as them.
var sessionsSealing = []byte("kubehenge sessions")

// sessionsPath sits in the appliance's own directory, which only its user may read. It holds the
// interface's kept sessions sealed with the key at sessionsKeyPath.
func (a *Appliance) sessionsPath() string { return filepath.Join(a.Dir, "sessions.sealed") }

// sessionsKeyPath is on the key volume, which backups leave out, so that a backup never holds the
// sessions and what opens them together.
func (a *Appliance) sessionsKeyPath() string { return filepath.Join(a.KeyDir, "sessions.key") }

// KeepSessions replaces the kept sessions with plain, sealed with the sessions' key. The key is made
// on first use and whenever it is gone, as after a restore from a backup.
func (a *Appliance) KeepSessions(plain []byte) error {
	if a.KeyDir == "" {
		return errors.New("this appliance has no volume to keep the key of sessions on")
	}
	key, err := os.ReadFile(a.sessionsKeyPath())
	if errors.Is(err, fs.ErrNotExist) || err == nil && len(key) != sealKeySize {
		key = make([]byte, sealKeySize)
		if _, err = rand.Read(key); err == nil {
			err = writeFileAtomic(a.sessionsKeyPath(), key)
		}
	}
	if err != nil {
		return fmt.Errorf("the key of sessions: %w", err)
	}
	sealed, err := seal(key, plain, sessionsSealing)
	if err != nil {
		return err
	}
	return writeFileAtomic(a.sessionsPath(), sealed)
}

// KeptSessions opens what KeepSessions kept. Nothing kept is nil.
func (a *Appliance) KeptSessions() ([]byte, error) {
	sealed, err := os.ReadFile(a.sessionsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the kept sessions: %w", err)
	}
	key, err := os.ReadFile(a.sessionsKeyPath())
	if err != nil {
		return nil, fmt.Errorf("the key of sessions: %w", err)
	}
	plain, err := unseal(key, sealed, sessionsSealing)
	if err != nil {
		return nil, errors.New("the kept sessions do not open with their key")
	}
	return plain, nil
}
