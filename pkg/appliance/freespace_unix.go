//go:build unix

package appliance

import (
	"fmt"
	"syscall"
)

// freeSpace is how many bytes an unprivileged process may still write on the file system holding
// path.
func freeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("the free space at %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec,unconvert // the field types differ between systems, and neither is negative
}
