//go:build !unix

package main

import "os"

// hangUp is nil where no session sends a hang-up. The installer runs only on Linux.
var hangUp os.Signal
