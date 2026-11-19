package installer

import (
	"strings"
	"testing"
)

// The plain output prints the plan before it changes anything, a line as each step starts, and the
// line of a finished step.
func TestPlainPrintsThePlanAndEachStepStart(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	out, err := plainOn(t, h, Config{IP: "dhcp", PVEEntry: "no"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	plan, run, ok := strings.Cut(out, "...  ")
	if !ok || !strings.Contains(plan, "Installing:") || !strings.Contains(plan, "IPv4") || !strings.Contains(plan, "DHCP") {
		t.Fatalf("no plan before the first step:\n%s", out)
	}
	if !strings.Contains(run, "\nok   ") || strings.Count(out, "...  ") < 5 {
		t.Fatalf("steps do not show as they start:\n%s", out)
	}
}

// A failed step is printed with its cause, and the log is named.
func TestPlainFailureNamesTheCauseBesideTheStep(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.fail("pct start 100", 1)
	out, err := plainOn(t, h, Config{IP: "dhcp", PVEEntry: "no"})
	if err == nil {
		t.Fatalf("a container that did not start installed:\n%s", out)
	}
	if !strings.Contains(out, "fail Start container: container 100 did not start") || !strings.Contains(out, "kubehenge: log ") {
		t.Fatalf("the failed step has no cause or the log is not named:\n%s", out)
	}
	if h.exec.count("pct destroy 100") != 1 {
		t.Fatalf("the container was not removed: %v", h.exec.calls)
	}
}

// --keep-failed leaves the container and says how to look into it.
func TestPlainKeepFailedKeepsTheContainer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.fail("pct start 100", 1)
	out, err := plainOn(t, h, Config{IP: "dhcp", PVEEntry: "no", KeepFailed: true})
	if err == nil {
		t.Fatal("the install did not fail")
	}
	if h.exec.count("pct destroy") != 0 || !strings.Contains(out, "Inspect it with: pct enter 100") || !strings.Contains(out, "the container is kept") {
		t.Fatalf("the container was not kept:\n%s\n%v", out, h.exec.calls)
	}
}

// --dry-run prints the plan and changes nothing.
func TestPlainDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	out, err := plainOn(t, h, Config{IP: "dhcp", PVEEntry: "no", DryRun: true})
	if err != nil || !strings.Contains(out, "Dry run, nothing was changed") || !strings.Contains(out, "DHCP") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if h.exec.changed() != 0 || h.hits != 0 {
		t.Fatalf("the node was changed: %v, %d downloads", h.exec.calls, h.hits)
	}
}
