//go:build integration && !windows

package provisioner

// envtest stops its API server and etcd itself here, with a signal to their process group.
const endsChildrenItself = false

func ownChildren() error { return nil }

func endChildren() error { return nil }
