package installer

import (
	"context"
	"strings"
	"testing"
)

// jobFor is the backup job the installer adds for container 100 on the storage.
func jobFor(storage string) string {
	return "pvesh create /cluster/backup --comment Kubehenge appliance --enabled 1 --mode snapshot " +
		"--prune-backups keep-daily=7,keep-weekly=4 --schedule 02:30 --storage " + storage + " --vmid 100"
}

// A fresh install backs the appliance up daily, to Proxmox Backup Server before a local storage.
func TestInstallAddsABackupJobToProxmoxBackupServer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.on("pvesh get /nodes/pve1/storage --content backup --enabled 1", `[
		{"storage":"local","type":"dir","active":1,"avail":900000000},
		{"storage":"nas","type":"nfs","shared":1,"active":1,"avail":500000000},
		{"storage":"pbs","type":"pbs","shared":1,"active":1,"avail":1000},
		{"storage":"pbs-off","type":"pbs","active":0,"avail":9000000000}]`)
	out, err := plainOn(t, h, Config{PVEEntry: "no"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.exec.last("pvesh create /cluster/backup") != jobFor("pbs") || !strings.Contains(out, "ok   Backup job: daily to pbs") {
		t.Errorf("job %q\n%s", h.exec.last("pvesh create /cluster/backup"), out)
	}
}

// A job that includes the container already is left as it is. One of all guests that excludes it
// does not include it.
func TestInstallKeepsAJobThatIncludesTheAppliance(t *testing.T) {
	t.Parallel()
	for jobs, want := range map[string]int{
		`[{"id":"a","vmid":"90,100"}]`:                                     0,
		`[{"id":"a","all":1}]`:                                             0,
		`[{"id":"a","pool":"infra"}]`:                                      0,
		`[{"id":"a","all":1,"exclude":"100"}]`:                             1,
		`[{"id":"a","vmid":"100","enabled":0}]`:                            1,
		`[{"id":"a","all":1,"node":"pve2"},{"id":"b","pool":"elsewhere"}]`: 1,
	} {
		h := newHarness(t, opts{dhcp: true})
		h.exec.on("pvesh get /cluster/backup", jobs)
		h.exec.on("pvesh get /cluster/resources", `[{"type":"lxc","vmid":100,"node":"pve1","pool":"infra"}]`)
		h.exec.on("pvesh get /cluster/resources --type vm --output-format json", `[{"type":"lxc","vmid":100,"node":"pve1","pool":"infra"}]`)
		h.exec.on("pvesh get /cluster/nextid", "100\n")
		out, err := plainOn(t, h, Config{CTID: "100", PVEEntry: "no"})
		if err != nil {
			t.Fatalf("%s: %v\n%s", jobs, err, out)
		}
		if got := h.exec.count("pvesh create /cluster/backup"); got != want {
			t.Errorf("%s: %d jobs added, want %d\n%s", jobs, got, want, out)
		}
	}
}

// Without a storage that takes backups the install says what to do and goes on.
func TestInstallWithoutABackupStorageSaysSo(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.on("pvesh get /nodes/pve1/storage", "[]")
	out, err := plainOn(t, h, Config{PVEEntry: "no"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "No storage on node pve1 takes backups, so no backup job includes container 100. " +
		"Allow Backup content on one under Datacenter, Storage, Edit, then run: sh install.sh --upgrade"
	if !strings.Contains(out, want) || h.exec.count("pvesh create /cluster/backup") != 0 || h.exec.count("pct set 100 --protection 1") != 1 {
		t.Errorf("no backup storage:\n%s", out)
	}
}

func TestNoBackupJobAddsNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	out, err := plainOn(t, h, Config{PVEEntry: "no", NoBackupJob: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.exec.count("pvesh create /cluster/backup") != 0 || strings.Contains(out, "Backup job") {
		t.Errorf("--no-backup-job added one:\n%s", out)
	}
	h, r, buf := upgradeHarness(t)
	r.S.Cfg.NoBackupJob = true
	if _, err = r.Upgrade(context.Background(), buf); err != nil || h.exec.count("pvesh create /cluster/backup") != 0 {
		t.Errorf("an upgrade with --no-backup-job: %v\n%s", err, buf)
	}
}

// An upgrade adds the job an appliance installed without one lacks, and leaves one that is there.
func TestUpgradeAddsAMissingBackupJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, r, out := upgradeHarness(t)
	if _, err := r.Upgrade(ctx, out); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.exec.last("pvesh create /cluster/backup") != jobFor("local") || !strings.Contains(out.String(), "ok   Backup job daily to local") {
		t.Errorf("job %q\n%s", h.exec.last("pvesh create /cluster/backup"), out)
	}
	h, r, out = upgradeHarness(t)
	h.exec.on("pvesh get /cluster/backup", `[{"id":"kubehenge","vmid":"100","comment":"Kubehenge appliance"}]`)
	if _, err := r.Upgrade(ctx, out); err != nil || h.exec.count("pvesh create /cluster/backup") != 0 || strings.Contains(out.String(), "Backup job") {
		t.Errorf("an upgrade beside a job: %v\n%s", err, out)
	}
}
