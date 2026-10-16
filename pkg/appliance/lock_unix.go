//go:build unix

package appliance

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// tryLock locks the directory exclusively without waiting. A directory needs no file made for the
// lock, so the service's user and root share it whoever comes first.
func tryLock(dir string) (func(), error) {
	f, err := os.Open(dir) //nolint:gosec // the appliance's own state directory
	if err != nil {
		return nil, fmt.Errorf("locking jobs: %w", err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, fmt.Errorf("locking jobs in %s: %w", dir, err)
	}
	// Closing the descriptor releases the lock, as the process ending does.
	return func() { _ = f.Close() }, nil
}
