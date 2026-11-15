//go:build !unix && !windows

package backupstore

import "errors"

// DiskUse is not known here. The appliance, the store and the snapshot receiver run on Linux.
func DiskUse(string) (uint64, uint64, error) {
	return 0, 0, errors.New("the free space is not known on this system")
}
