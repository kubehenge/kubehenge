package appliance

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestTofuIsInterruptedBeforeTheDeadline holds OpenTofu to a run that ends tofuGrace after it
// starts: interrupted at once, it writes its state and stops, long before the kill.
func TestTofuIsInterruptedBeforeTheDeadline(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot send a process an interrupt")
	}
	a := &Appliance{Dir: t.TempDir(), Tofu: fakeTofu}
	if err := os.MkdirAll(a.workdir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.workdir(), "terraform.tfvars.json"), []byte(`{"cluster_name":"slow"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(tofuGrace+100*time.Millisecond))
	defer cancel()
	j := &Job{}
	started := time.Now()
	err := a.tofu(ctx, j, nil, func(line string) string { return line }, "apply")
	if err == nil || !strings.Contains(err.Error(), "interrupted before the Proxmox VE sign-in") {
		t.Errorf("tofu = %v, want the run named as interrupted", err)
	}
	if _, statErr := os.Stat(filepath.Join(a.Dir, "state-written")); statErr != nil {
		t.Errorf("tofu was not interrupted, so it wrote no state: %v", statErr)
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Errorf("tofu took %s, as if it had been left to the kill", took)
	}
	if log := j.Log(); len(log) == 0 || !strings.Contains(log[0], "Interrupt received") {
		t.Errorf("job log = %q, want tofu's own last lines", log)
	}
}
