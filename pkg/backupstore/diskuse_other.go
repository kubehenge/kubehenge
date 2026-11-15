//go:build !unix

package backupstore

import "errors"

// DiskUse is not known here. The appliance and the store run only on Linux.
func DiskUse(string) (uint64, uint64, error) {
	return 0, 0, errors.New("the free space is not known on this system")
}
