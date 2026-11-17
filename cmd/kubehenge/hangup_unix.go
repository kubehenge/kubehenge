//go:build unix

package main

import (
	"os"
	"syscall"
)

// hangUp is what a dropped SSH session sends.
var hangUp os.Signal = syscall.SIGHUP
