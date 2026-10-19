//go:build unix

package appliance

import (
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
)

// A job may start the moment the one before it has ended, however many children other goroutines
// are forking then.
func TestUnlockWhileChildrenFork(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stop atomic.Bool
	var forks sync.WaitGroup
	for range 4 {
		forks.Go(func() {
			for !stop.Load() {
				_ = exec.CommandContext(t.Context(), "true").Run()
			}
		})
	}
	defer forks.Wait()
	defer stop.Store(true)
	for i := range 2000 {
		unlock, err := tryLock(dir)
		if err != nil {
			t.Fatalf("round %d, first lock: %v", i, err)
		}
		unlock()
		again, err := tryLock(dir)
		if err != nil {
			t.Fatalf("round %d, lock right after the unlock: %v", i, err)
		}
		again()
	}
}
