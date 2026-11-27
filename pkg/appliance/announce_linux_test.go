//go:build linux

package appliance

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSenderErrorOfATentativeAddressIsNotReady(t *testing.T) {
	t.Parallel()
	if err := senderError(announceV6, unix.EADDRNOTAVAIL); !errors.Is(err, errAddressNotReady) || !errors.Is(err, unix.EADDRNOTAVAIL) {
		t.Errorf("tentative: %v, want not ready", err)
	}
	if err := senderError(announceV6, unix.EPERM); errors.Is(err, errAddressNotReady) {
		t.Errorf("no permission: %v, want a real failure", err)
	}
}
