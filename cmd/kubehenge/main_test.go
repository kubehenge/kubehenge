package main

import (
	"os"
	"slices"
	"syscall"
	"testing"
)

// A dropped SSH session sends SIGHUP. The installer must see it to remove its half-made container.
func TestShutdownSignalsIncludeHangUp(t *testing.T) {
	t.Parallel()
	signals := shutdownSignals()
	for _, want := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !slices.Contains(signals, want) {
			t.Errorf("shutdown signals %v lack %v", signals, want)
		}
	}
}
