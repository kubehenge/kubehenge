//go:build !unix

package appliance

import "errors"

// freeSpace is not known here. The updater runs only in the appliance's Linux container.
func freeSpace(string) (uint64, error) {
	return 0, errors.New("the free space is not known on this system")
}
