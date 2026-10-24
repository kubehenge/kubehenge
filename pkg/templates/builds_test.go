package templates

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildTagsAndLines(t *testing.T) {
	t.Parallel()
	line, _ := rke2(t)
	at := time.Date(2026, 10, 18, 1, 2, 3, 0, time.FixedZone("CEST", 2*60*60))
	build, err := line.Build(at)
	if err != nil {
		t.Fatal(err)
	}
	if build.Name != "kubehenge-rke2-almalinux-10-build-202610172302" || build.LineName != line.Name || !slices.Contains(build.Tags, "build-202610172302") {
		t.Fatalf("build %+v", build)
	}
	got, built := BuildOf(build.Tags)
	if !built || !got.Equal(at.Truncate(time.Minute)) || !slices.Equal(LineOf(build.Tags), line.Tags) || LineKey(build.Tags) != strings.Join(line.Tags, ";") {
		t.Errorf("build of %v: %s, %v", build.Tags, got, built)
	}
	if _, err = build.Build(at); err == nil {
		t.Error("a build was built again")
	}
	talos, err := Plan(&Request{Distribution: Talos})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := talos.Build(at); err == nil || talos.Refreshed() {
		t.Error("a Talos template has builds")
	}
}

func buildVM(t *testing.T, line *Template, vmid int, at time.Time) map[string]any {
	t.Helper()
	b, err := line.Build(at)
	if err != nil {
		t.Fatal(err)
	}
	return template(vmid, b.Name, strings.Join(b.Tags, ";"), testPool)
}

func TestEnsureKeepsTheNewestBuild(t *testing.T) {
	t.Parallel()
	line, _ := rke2(t)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	older, newer := buildVM(t, &line, 101, day(1)), buildVM(t, &line, 102, day(8))
	kubeadm, err := Plan(&Request{Distribution: Kubeadm, KubernetesVersion: "v1.36.5"})
	if err != nil {
		t.Fatal(err)
	}
	other := buildVM(t, &kubeadm, 103, day(15))
	b := &Builder{Client: clusterVMs(t, older, newer, other), Pool: testPool}
	builds, err := b.Builds(t.Context(), &line)
	if err != nil || len(builds) != 2 || builds[0].Resource.VMID != 102 || !builds[1].At.Equal(day(1)) {
		t.Fatalf("builds %v, %v", builds, err)
	}
	if id, err := b.Ensure(t.Context(), &line, day(20)); err != nil || id != 102 {
		t.Errorf("Ensure gave %d, %v; want the newest build, 102", id, err)
	}
}

func TestNewBuildKeepsTheCPUTypeOfTheNewest(t *testing.T) {
	t.Parallel()
	line, _ := rke2(t)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	older, newer := buildVM(t, &line, 101, day(1)), buildVM(t, &line, 102, day(8))
	configs := configsOf(older, newer)
	configs[101]["cpu"] = "x86-64-v3"
	configs[102]["cpu"] = "host"
	b := &Builder{Client: (&fakeCluster{t: t, views: [][]map[string]any{{older, newer}}, configs: configs}).client(), Pool: testPool}
	next, err := line.Build(day(15))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		builder string
		t       *Template
		want    string
	}{
		{"a new build", "", &next, "host"},
		{"a line's first", "", &line, DefaultCPUType},
		{"one the builder is told", "x86-64-v4", &next, "x86-64-v4"},
	} {
		b.CPUType = c.builder
		if got, err := b.cpuType(t.Context(), c.t); err != nil || got != c.want {
			t.Errorf("%s: CPU type %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	delete(configs[102], "cpu")
	b.CPUType = ""
	if got, err := b.cpuType(t.Context(), &next); err != nil || got != "kvm64" {
		t.Errorf("after a build without a CPU type: %q, %v; want Proxmox VE's default", got, err)
	}
}

func TestRemoveLeftoversLeavesABuildUnderWay(t *testing.T) {
	t.Parallel()
	line, _ := rke2(t)
	now := time.Date(2026, 10, 18, 6, 0, 0, 0, time.UTC)
	vm := func(vmid int, at time.Time, made time.Duration) map[string]any {
		b, err := line.Build(at)
		if err != nil {
			t.Fatal(err)
		}
		v := template(vmid, b.Name, "kubehenge;kubehenge-building", testPool)
		v["template"] = 0
		v["meta"] = fmt.Sprintf("creation-qemu=10.0.2,ctime=%d", now.Add(-made).Unix())
		return v
	}
	stale, underWay := vm(101, now.Add(-5*time.Hour), 5*time.Hour), vm(102, now.Add(-time.Hour), time.Hour)
	finished := buildVM(t, &line, 103, now.Add(-24*time.Hour))
	finished["meta"] = fmt.Sprintf("ctime=%d", now.Add(-24*time.Hour).Unix())
	// Left by a run that stopped between creating the VM and tagging it.
	untagged := vm(104, now.Add(-4*time.Hour), 4*time.Hour)
	untagged["tags"] = ""
	configs := configsOf(stale, underWay, finished, untagged)
	for _, v := range []map[string]any{stale, underWay, finished, untagged} {
		vmid, ok := v["vmid"].(int)
		if !ok {
			t.Fatalf("VM %v has no VM ID", v)
		}
		configs[vmid]["meta"] = v["meta"]
	}
	f := &fakeCluster{t: t, views: [][]map[string]any{{stale, underWay, finished, untagged}}, configs: configs}
	b := &Builder{Client: f.client(), Pool: testPool}
	removed, err := b.RemoveLeftovers(t.Context(), &line, 3*time.Hour, now)
	if err != nil || !slices.Equal(removed, []int{101, 104}) || !slices.Equal(f.deletedVMs(), []int{101, 104}) {
		t.Errorf("removed %v (deleted %v), %v; want the stale VMs 101 and 104 alone", removed, f.deletedVMs(), err)
	}
}

// Proxmox VE refuses tags on a new VM to a builder whose rights come from the template pool alone,
// so the VM is created in the pool without them and tagged as a build under way once it is there.
func TestANewVMIsTaggedOnceInThePool(t *testing.T) {
	t.Parallel()
	line, _ := rke2(t)
	build, err := line.Build(time.Date(2026, 10, 18, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCluster{t: t, views: [][]map[string]any{{}}, configs: map[int]map[string]any{}}
	b := &Builder{Client: f.client(), Pool: testPool, Storage: "local-lvm"}
	ctx := t.Context()
	node, err := b.Client.Node(ctx, "pve1")
	if err != nil {
		t.Fatal(err)
	}
	recipe := &Recipe{Image: &NodeImageBoot{}, DiskGiB: 12, BuilderDiskGiB: 20, MemoryMiB: 4096}
	if err = b.createVM(ctx, node, 104, &build, "local:import/almalinux.qcow2", DefaultCPUType, recipe); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	created := slices.Clone(f.created)
	f.mu.Unlock()
	if len(created) != 1 || created[0]["pool"] != testPool || created[0]["name"] != build.Name {
		t.Fatalf("created %v; want one VM named %s in %s", created, build.Name, testPool)
	}
	// In image mode the template's own disk starts empty, and the family's image boots beside it.
	if created[0]["scsi0"] != "local-lvm:12,discard=on,ssd=1" || created[0]["scsi1"] != "local-lvm:0,import-from=local:import/almalinux.qcow2,discard=on,ssd=1" ||
		created[0]["boot"] != "order=scsi1" || fmt.Sprint(created[0]["memory"]) != "4096" {
		t.Errorf("created %v", created[0])
	}
	if tags, given := created[0]["tags"]; given {
		t.Errorf("the VM was created with the tags %v", tags)
	}
	vm, err := node.VirtualMachine(ctx, 104)
	if err != nil {
		t.Fatal(err)
	}
	if err = markBuilding(ctx, vm); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	tags := f.configs[104]["tags"]
	f.mu.Unlock()
	if tags != "kubehenge;kubehenge-building" {
		t.Errorf("tags %v; want those of a build under way", tags)
	}
}

// A VM in the template pool that is not tagged yet may be a build another run has just created.
func TestCheckNotInProgressSeesABuildNotTaggedYet(t *testing.T) {
	t.Parallel()
	line, _ := rke2(t)
	build, err := line.Build(time.Date(2026, 10, 18, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	created := template(104, build.Name, "", testPool)
	created["template"] = 0
	b := &Builder{Client: clusterVMs(t, created), Pool: testPool}
	if err := b.checkNotInProgress(t.Context(), &line); err == nil || !strings.Contains(err.Error(), "already named") {
		t.Errorf("checkNotInProgress gave %v; want the build just created refused", err)
	}
}
