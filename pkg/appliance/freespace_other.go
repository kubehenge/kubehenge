//go:build !unix

package appliance

import "errors"

// DiskUse is not known here. The appliance runs only in its Linux container.
func DiskUse(string) (used, free uint64, err error) {
	return 0, 0, errors.New("the free space is not known on this system")
}
