package installer

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A new run's log leaves the newest ten logs and removes the older ones.
func TestRunLogsKeepTheNewestTen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Date(2026, 11, 19, 13, 41, 0, 0, time.UTC)
	names := make([]string, 0, 12)
	for i := range 12 {
		f, err := openLog(dir, at.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, f.Name())
		_ = f.Close()
	}
	left, _ := filepath.Glob(filepath.Join(dir, LogPrefix+"*.log"))
	if len(left) != keptLogs || left[0] != names[2] || left[len(left)-1] != names[11] {
		t.Fatalf("want the newest %d of %v, got %v", keptLogs, names, left)
	}
}

// Two runs started in the same second write two logs, each named after the time.
func TestTwoRunsInOneSecondGetTwoLogs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Date(2026, 11, 19, 13, 41, 28, 0, time.UTC)
	first, err := openLog(dir, at)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := openLog(dir, at)
	if err != nil {
		t.Fatalf("the second run in the same second: %v", err)
	}
	defer func() { _ = second.Close() }()
	if first.Name() == second.Name() || !strings.HasPrefix(filepath.Base(second.Name()), LogPrefix+"20261119-134128-") {
		t.Errorf("logs %s and %s", first.Name(), second.Name())
	}
}

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

// A failed plain run prints the command that runs it again with the same answers. A kept
// container holds its ID, so that command names none.
func TestPlainFailurePrintsTheCommandToRepeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.fail("pct start 100", 1)
	out, _ := plainOn(t, h, Config{IP: "dhcp", DNS: "192.0.2.53", Hostname: "ct1", PVEEntry: "no", Plain: true})
	want := "kubehenge: to run it again with the same answers: sh install.sh --ctid 100 --storage local-lvm --template-storage local-lvm --bridge vmbr0 --ip dhcp --dns 192.0.2.53 --hostname ct1 --proxmox-url https://pve1.example.com:8006 --tofu " + shellWord(h.tofu) + " --no-pve-entry --plain\n"
	if !strings.Contains(out, want) {
		t.Fatalf("want %q in:\n%s", want, out)
	}
	if got := shellWord("/root/it's here.pem"); got != `'/root/it'\''s here.pem'` {
		t.Errorf("a path with a quote and a space came out as %s", got)
	}
	h = newHarness(t, opts{dhcp: true})
	h.exec.fail("pct start 100", 1)
	out, _ = plainOn(t, h, Config{IP: "dhcp", PVEEntry: "no", KeepFailed: true})
	if !strings.Contains(out, "same answers: sh install.sh --storage") || !strings.Contains(out, " --keep-failed\n") {
		t.Fatalf("the command after a kept container:\n%s", out)
	}
}

// A removal Proxmox VE refuses is said with the command that removes the container by hand, and
// the repeat command leaves out the ID the container still holds.
func TestPlainRefusedRemovalNamesTheContainer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.fail("pct start 100", 1)
	h.exec.failWith("pct destroy 100", "unable to destroy")
	out, _ := plainOn(t, h, Config{IP: "dhcp", PVEEntry: "no"})
	if !strings.Contains(out, "kubehenge: container 100 could not be removed. Remove it with: pct destroy 100 --purge\n") {
		t.Fatalf("the refused removal is not said:\n%s", out)
	}
	if !strings.Contains(out, "same answers: sh install.sh --storage") {
		t.Fatalf("the repeat command names the taken ID:\n%s", out)
	}
}

// A plain run refused before it changes anything prints the command that runs it again, once
// answers were given.
func TestPlainRefusalPrintsTheCommandToRepeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	out, err := plainOn(t, h, Config{DNS: "192.0.2.53", PVEEntry: "no"})
	if err == nil || h.exec.count("pct create") != 0 {
		t.Fatalf("a node with no DHCP sign was not refused: %v", err)
	}
	if !strings.Contains(out, "kubehenge: to run it again with the same answers: sh install.sh ") || !strings.Contains(out, "--dns 192.0.2.53") {
		t.Fatalf("no repeat command after the refusal:\n%s", out)
	}
}

// A stop before any answer prints no repeat command, since it would repeat nothing.
func TestPlainRepeatNeedsAnAnswer(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	printRepeat(&out, &Config{}, nil)
	if out.Len() != 0 {
		t.Fatalf("a stop without answers printed %q", out.String())
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
	if err != nil || !strings.Contains(out, "Dry run, nothing changed") || !strings.Contains(out, "DHCP") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if h.exec.changed() != 0 || h.hits != 0 {
		t.Fatalf("the node was changed: %v, %d downloads", h.exec.calls, h.hits)
	}
}
