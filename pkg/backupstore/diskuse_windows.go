//go:build windows

package backupstore

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// DiskUse is how many bytes are taken on the volume holding path, and how many the caller may
// still write there.
func DiskUse(path string) (uint64, uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, fmt.Errorf("the free space at %s: %w", path, err)
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, 0, fmt.Errorf("the free space at %s: %w", path, err)
	}
	return total - free, avail, nil
}
