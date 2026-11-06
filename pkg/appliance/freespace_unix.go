//go:build unix

package appliance

import (
	"fmt"
	"syscall"
)

// DiskUse is how many bytes are taken on the file system holding path, and how many an
// unprivileged process may still write there. Their sum leaves out the blocks kept for root, as df
// does.
func DiskUse(path string) (used, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("the free space at %s: %w", path, err)
	}
	//nolint:gosec,unconvert // the field types differ between systems, and none is negative
	size, blocks, unused, avail := uint64(st.Bsize), uint64(st.Blocks), uint64(st.Bfree), uint64(st.Bavail)
	return (blocks - unused) * size, avail * size, nil
}
