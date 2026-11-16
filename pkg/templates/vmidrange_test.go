package templates

import (
	"strings"
	"testing"
)

// createIn runs createFresh on a builder with the range, against a cluster that lists the VMs of
// views and holds the configurations of configs.
func createIn(t *testing.T, first, last int, f *fakeCluster) (int, error) {
	t.Helper()
	tmpl, _ := rke2(t)
	client := f.client()
	ctx := t.Context()
	cluster, err := client.Cluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	node, err := client.Node(ctx, "pve1")
	if err != nil {
		t.Fatal(err)
	}
	b := &Builder{Client: client, Storage: "local-lvm", Pool: testPool, FirstVMID: first, LastVMID: last}
	return b.createFresh(ctx, cluster, node, &tmpl, "local:import/image.qcow2", DefaultHardware(DefaultCPUType), "https://example.test/image.qcow2", nil)
}

func TestCreateFreshTakesTheLowestFreeVMIDOfItsRange(t *testing.T) {
	t.Parallel()
	_, tags := rke2(t)
	listed := []map[string]any{template(10000, "a", tags, testPool), template(10001, "b", tags, testPool), template(5, "outside", tags, testPool)}
	// next-id would offer 100, below the range.
	f := &fakeCluster{t: t, views: [][]map[string]any{listed}, configs: configsOf(listed...), nextIDs: []int{100}}
	if vmid, err := createIn(t, 10000, 10005, f); err != nil || vmid != 10002 {
		t.Fatalf("createFresh gave %d, %v; want VM 10002", vmid, err)
	}
	if len(f.nextIDs) != 1 {
		t.Error("the range's builder asked Proxmox VE for its next ID")
	}
}

// A VM the token cannot list is found by asking Proxmox VE whether its ID is free, however many
// there are at the start of the range.
func TestCreateFreshSkipsEveryHiddenVMOfItsRange(t *testing.T) {
	t.Parallel()
	_, tags := rke2(t)
	var hidden []map[string]any
	for id := 10000; id < 10006; id++ {
		hidden = append(hidden, template(id, "hidden", tags, ""))
	}
	f := &fakeCluster{t: t, views: [][]map[string]any{{}}, configs: configsOf(hidden...)}
	if vmid, err := createIn(t, 10000, 10099, f); err != nil || vmid != 10006 {
		t.Fatalf("createFresh gave %d, %v; want VM 10006", vmid, err)
	}
	if len(f.created) != 1 {
		t.Errorf("%d creates for one template", len(f.created))
	}
}

// Proxmox VE does not reserve a VM ID it calls free, so a client that takes it between the check and
// the create makes the create fail, and the build asks for another ID.
func TestCreateFreshAsksAgainWhenAnotherClientTakesTheIDItWasOffered(t *testing.T) {
	t.Parallel()
	_, tags := rke2(t)
	f := &fakeCluster{
		t: t, views: [][]map[string]any{{}}, configs: configsOf(template(10000, "other", tags, "")),
		late: map[int]bool{10000: true},
	}
	if vmid, err := createIn(t, 10000, 10005, f); err != nil || vmid != 10001 {
		t.Fatalf("createFresh gave %d, %v; want VM 10001", vmid, err)
	}
}

func TestCreateFreshFailsWhenItsRangeIsFullNeverTakingAnotherID(t *testing.T) {
	t.Parallel()
	_, tags := rke2(t)
	listed := []map[string]any{template(10000, "a", tags, testPool), template(10001, "b", tags, testPool)}
	f := &fakeCluster{t: t, views: [][]map[string]any{listed}, configs: configsOf(listed...), nextIDs: []int{100}}
	_, err := createIn(t, 10000, 10001, f)
	if err == nil || !strings.Contains(err.Error(), "no free VM ID in 10000-10001. Widen the site's VM ID range") {
		t.Fatalf("createFresh on a full range: %v", err)
	}
	if len(f.created) != 0 {
		t.Errorf("created %v on a full range", f.created)
	}
}
