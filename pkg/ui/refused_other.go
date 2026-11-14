//go:build !windows

package ui

import "syscall"

// errRefused is the error of a connection the host refused.
const errRefused = syscall.ECONNREFUSED
