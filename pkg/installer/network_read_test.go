package installer

import (
	"context"
	"testing"
)

// A container configuration that cannot be read records no network and stops nothing.
func TestInstallGoesOnWhenTheContainersConfigCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.fail("pct config 100", 255)
	out, err := plainOn(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", DNS: "192.0.2.53", PVEEntry: "no"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, ok := h.exec.pushed["/var/lib/kubehenge/network"]; ok {
		t.Errorf("a configuration that could not be read wrote %q", got)
	}
}

func TestUpgradeGoesOnWhenTheContainersConfigCannotBeRead(t *testing.T) {
	t.Parallel()
	h, r, out := upgradeHarness(t)
	h.exec.fail("pct config 100", 255)
	if _, err := r.Upgrade(context.Background(), out); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, ok := h.exec.pushed["/var/lib/kubehenge/network"]; ok {
		t.Errorf("a configuration that could not be read wrote %q", got)
	}
}
