//go:build integration && !windows

package integration

// EndsChildrenItself tells whether the package ends the API server and etcd itself; here envtest
// does, with a signal to their process group.
const EndsChildrenItself = false

// OwnChildren has nothing to do here.
func OwnChildren() error { return nil }

// EndChildren has nothing to do here.
func EndChildren() error { return nil }
