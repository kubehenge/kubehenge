package appliance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A service stop, as for a migration or a backup in stop mode, interrupts a running apply so that
// OpenTofu writes its state, waits for its outcome to be recorded, and refuses new runs.
func TestStopInterruptsTheRunUnderWay(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot send a process an interrupt")
	}
	a, _ := newAppliance(t)
	if err := os.MkdirAll(a.workdir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.workdir(), "terraform.tfvars.json"), []byte(`{"cluster_name":"slow"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	applying := make(chan struct{})
	var once sync.Once
	by := runner{user: "root@pam", until: time.Now().Add(time.Hour)}
	err := a.startIn(&a.job, by, "apply", nil, func(ctx context.Context, j *Job) error {
		return a.tofu(ctx, j, nil, func(line string) string {
			if strings.Contains(line, "Applying.") {
				once.Do(func() { close(applying) })
			}
			return line
		}, "apply")
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	select {
	case <-applying:
	case <-ctx.Done():
		t.Fatal("tofu never started applying")
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	got := a.Current()
	if got == nil || got.State != StateFailed || !strings.Contains(got.Error, "interrupted as the appliance's service stopped") {
		t.Errorf("the run once stopped = %+v", got)
	}
	if _, statErr := os.Stat(filepath.Join(a.Dir, "state-written")); statErr != nil {
		t.Errorf("tofu was not interrupted, so it wrote no state: %v", statErr)
	}
	err = a.startIn(&a.job, by, "apply", nil, func(context.Context, *Job) error { return nil })
	if !errors.Is(err, ErrStopping) {
		t.Errorf("a run started while stopping: %v", err)
	}
}

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
	if log := j.Log(); len(log) == 0 || !strings.Contains(log[len(log)-1], "Interrupt received") {
		t.Errorf("job log = %q, want tofu's own last lines", log)
	}
}
