package templates

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// onNode is vm as the resource view shows a VM on another node, which is down when status is
// "unknown".
func onNode(vm map[string]any, node, status string) map[string]any {
	out := maps.Clone(vm)
	out["node"] = node
	if status != "" {
		out["status"] = status
	}
	return out
}

func inventoryOf(t *testing.T, f *fakeCluster) Inventory {
	t.Helper()
	inv, err := (&Builder{Client: f.client(), Pool: testPool}).Inventory(t.Context())
	if err != nil {
		t.Fatalf("Inventory failed over a template on a down node: %v", err)
	}
	return inv
}

// A template on a down node is reported, and the template on a node that is up is still listed.
func TestInventoryReportsATemplateOnADownNodeAndListsTheOthers(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	up := template(101, tmpl.Name, tags, testPool)
	down := onNode(template(102, "kubehenge-talos", "kubehenge;talos;talos-v1.14.4", testPool), "pve2", "unknown")
	f := &fakeCluster{t: t, views: [][]map[string]any{{up, down}}, configs: configsOf(up)}
	f.configs[101]["scsi0"] = "ceph:base-101-disk-0,size=30G"

	inv := inventoryOf(t, f)
	if len(inv.Templates) != 1 || inv.Templates[0].VMID != 101 || !slices.Equal(inv.Storages, []string{"ceph"}) {
		t.Errorf("Inventory listed %v on %v; want template 101 on ceph", inv.Templates, inv.Storages)
	}
	if len(inv.Down) != 1 || inv.Down[0].VMID != 102 || inv.Down[0].Node != "pve2" {
		t.Errorf("Inventory.Down = %v; want VM 102 on pve2", inv.Down)
	}
	b := &Builder{Client: f.client(), Pool: testPool}
	if list, err := b.List(t.Context()); err != nil || len(list) != 1 {
		t.Errorf("List gave %v, %v; want template 101 alone", list, err)
	}
}

// With every template's node down there is nothing to list, and nothing fails.
func TestInventoryOfOnlyDownNodes(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	first := onNode(template(101, tmpl.Name, tags, testPool), "pve1", "unknown")
	second := onNode(template(102, "kubehenge-talos", "kubehenge;talos;talos-v1.14.4", testPool), "pve2", "unknown")
	inv := inventoryOf(t, &fakeCluster{t: t, views: [][]map[string]any{{first, second}}, configs: map[int]map[string]any{}})
	if len(inv.Templates) != 0 || len(inv.Storages) != 0 || len(inv.Down) != 2 {
		t.Errorf("Inventory = %+v; want two VMs down and nothing listed", inv)
	}
}

// A template whose site is cached is still reported down once the view says its node is.
func TestInventoryReportsACachedTemplateOnADownNode(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	up := template(101, tmpl.Name, tags, testPool)
	f := &fakeCluster{t: t, views: [][]map[string]any{{up}, {onNode(up, "pve1", "unknown")}}, configs: configsOf(up)}
	cache := &SiteCache{}
	b := &Builder{Client: f.client(), Pool: testPool, SiteCache: cache}

	if inv, err := b.Inventory(t.Context()); err != nil || len(inv.Templates) != 1 || cache.Len() != 1 {
		t.Fatalf("first Inventory = %+v, %v; want template 101 listed and cached", inv, err)
	}
	inv, err := b.Inventory(t.Context())
	if err != nil || len(inv.Templates) != 0 || len(inv.Down) != 1 || inv.Down[0].VMID != 101 {
		t.Errorf("Inventory = %+v, %v; want 101 down and nothing listed", inv, err)
	}
	if cache.Len() != 1 {
		t.Errorf("the cache holds %d templates, want 101 kept for when its node is back", cache.Len())
	}
}

// A cached template whose node went down while the view still shows it online is reported down,
// without a configuration read, and is listed again once its node answers.
func TestInventoryNoticesACachedTemplatesNodeTheViewStillShowsOnline(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	first := onNode(template(101, tmpl.Name, tags, testPool), "pve2", "")
	second := onNode(template(102, "kubehenge-talos", "kubehenge;talos;talos-v1.14.4", testPool), "pve2", "")
	other := onNode(template(103, "kubehenge-k3s", "k3s;kubehenge", testPool), "pve1", "")
	f := &fakeCluster{t: t, views: [][]map[string]any{{first, second, other}}, configs: configsOf(first, second, other)}
	b := &Builder{Client: f.client(), Pool: testPool, SiteCache: &SiteCache{}}
	if inv, err := b.Inventory(t.Context()); err != nil || len(inv.Templates) != 3 {
		t.Fatalf("first Inventory = %+v, %v; want three templates listed and cached", inv, err)
	}

	f.mu.Lock()
	f.unreachable = map[int]bool{101: true, 102: true}
	f.mu.Unlock()
	inv, err := b.Inventory(t.Context())
	if err != nil || len(inv.Templates) != 1 || inv.Templates[0].VMID != 103 || len(inv.Down) != 2 {
		t.Errorf("Inventory = %+v, %v; want 103 listed and 101 and 102 down", inv, err)
	}
	if f.configReads() != 3 {
		t.Errorf("%d configuration reads, want the 3 of the first listing alone", f.configReads())
	}

	f.mu.Lock()
	f.unreachable = nil
	f.mu.Unlock()
	if inv, err = b.Inventory(t.Context()); err != nil || len(inv.Templates) != 3 || len(inv.Down) != 0 {
		t.Errorf("Inventory once pve2 answers = %+v, %v; want all three listed", inv, err)
	}
}

// The view still showed the node online when the node went down: only the 595 on the VM's
// configuration tells.
func TestInventoryTakesA595OnAConfigurationForADownNode(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	up := template(101, tmpl.Name, tags, testPool)
	racing := onNode(template(102, "kubehenge-talos", "kubehenge;talos;talos-v1.14.4", testPool), "pve2", "")
	f := &fakeCluster{t: t, views: [][]map[string]any{{up, racing}}, configs: configsOf(up, racing), unreachable: map[int]bool{102: true}}

	inv := inventoryOf(t, f)
	if len(inv.Templates) != 1 || len(inv.Down) != 1 || inv.Down[0].VMID != 102 {
		t.Errorf("Inventory = %+v; want 101 listed and 102 down", inv)
	}
}

// Find stays strict: a candidate on a down node, skipped, would have Create build a second template with
// the same tags.
func TestFindRefusesWhileACandidateIsOnADownNode(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	down := onNode(template(101, tmpl.Name, tags, testPool), "pve2", "unknown")
	b := &Builder{Client: (&fakeCluster{t: t, views: [][]map[string]any{{down}}, configs: map[int]map[string]any{}}).client(), Pool: testPool}

	_, err := b.Find(t.Context(), &tmpl)
	nodeDown, ok := errors.AsType[*NodeDownError](err)
	if !ok || nodeDown.Node != "pve2" || !strings.Contains(err.Error(), "which is down") {
		t.Errorf("Find gave %v; want a NodeDownError for pve2", err)
	}
	if _, err = b.Create(t.Context(), &tmpl); !errors.As(err, new(*NodeDownError)) {
		t.Errorf("Create gave %v; want it to stop at the template on the down node", err)
	}
}

// The build VM on a down node cannot be told from the line's newest build, so Builds stops where a
// build made meanwhile would take its place. Another line is not held up.
func TestBuildsStopAtABuildOnADownNode(t *testing.T) {
	t.Parallel()
	line, _ := rke2(t)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	older := buildVM(t, &line, 101, day(1))
	newer := onNode(buildVM(t, &line, 102, day(8)), "pve2", "unknown")
	kubeadm, err := Plan(&Request{Distribution: Kubeadm, KubernetesVersion: "v1.36.5"})
	if err != nil {
		t.Fatal(err)
	}
	other := buildVM(t, &kubeadm, 103, day(15))
	b := &Builder{Client: (&fakeCluster{t: t, views: [][]map[string]any{{older, newer, other}}, configs: configsOf(older, other)}).client(), Pool: testPool}

	if _, err = b.Builds(t.Context(), &line); !errors.As(err, new(*NodeDownError)) {
		t.Errorf("Builds gave %v; want a NodeDownError", err)
	}
	if _, err = b.Ensure(t.Context(), &line, day(20)); !errors.As(err, new(*NodeDownError)) {
		t.Errorf("Ensure gave %v; want it to make no second build", err)
	}
	if builds, err := b.Builds(t.Context(), &kubeadm); err != nil || len(builds) != 1 || builds[0].Resource.VMID != 103 {
		t.Errorf("Builds of another line gave %v, %v; want build 103", builds, err)
	}
}

// A VM left by a build on a down node is neither removed nor in the way of a new build.
func TestLeftoversAndBuildChecksSkipAVMOnADownNode(t *testing.T) {
	t.Parallel()
	line, _ := rke2(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	building := template(101, line.Name+"-build-x", "kubehenge;kubehenge-building", testPool)
	building["template"] = 0
	down := onNode(building, "pve2", "unknown")
	racing := onNode(building, "pve3", "")
	f := &fakeCluster{t: t, views: [][]map[string]any{{down, racing}}, configs: map[int]map[string]any{}, unreachable: map[int]bool{101: true}}
	b := &Builder{Client: f.client(), Pool: testPool}

	if removed, err := b.RemoveLeftovers(t.Context(), &line, time.Hour, now); err != nil || len(removed) != 0 {
		t.Errorf("RemoveLeftovers gave %v, %v; want nothing removed and no failure", removed, err)
	}
	if err := b.checkNotInProgress(t.Context(), &line); err != nil {
		t.Errorf("checkNotInProgress failed over VMs on down nodes: %v", err)
	}
}
