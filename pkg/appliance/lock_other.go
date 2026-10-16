//go:build !unix

package appliance

import "sync"

var locked sync.Map

// tryLock locks within this process only; the appliance and its updater run only on Linux, so this
// serves the tests elsewhere.
func tryLock(dir string) (func(), error) {
	if _, held := locked.LoadOrStore(dir, true); held {
		return nil, errLocked
	}
	return func() { locked.Delete(dir) }, nil
}
