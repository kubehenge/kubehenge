package main

import (
	"os"
	"slices"
	"syscall"
	"testing"
)

// A dropped SSH session sends SIGHUP. The installer must see it to remove its half-made container,
// unless the process started with SIGHUP ignored (nohup), which must survive the drop.
func TestShutdownSignalsAndHangUp(t *testing.T) {
	t.Parallel()
	catches := func(ignored bool) []os.Signal {
		return shutdownSignals(func(s os.Signal) bool { return ignored && s == syscall.SIGHUP })
	}
	for _, want := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !slices.Contains(catches(false), want) {
			t.Errorf("shutdown signals %v lack %v", catches(false), want)
		}
	}
	if slices.Contains(catches(true), os.Signal(syscall.SIGHUP)) {
		t.Errorf("a process started with SIGHUP ignored catches it: %v", catches(true))
	}
	if !slices.Contains(catches(true), os.Signal(syscall.SIGTERM)) {
		t.Errorf("SIGTERM dropped with SIGHUP ignored: %v", catches(true))
	}
}
