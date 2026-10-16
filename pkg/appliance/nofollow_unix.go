//go:build unix

package appliance

import "syscall"

// noFollow makes opening a symbolic link fail.
const noFollow = syscall.O_NOFOLLOW
