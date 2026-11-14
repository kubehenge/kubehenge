package ui

import "golang.org/x/sys/windows"

// errRefused is the error of a connection the host refused. Windows reports its own code, which
// syscall.ECONNREFUSED does not match.
const errRefused = windows.WSAECONNREFUSED
