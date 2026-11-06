package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

const (
	largeNodes    = 20
	largeVNets    = 30
	largeStorages = 15
)

// largeCluster is Proxmox VE with 20 nodes, 30 cluster-wide VNets and 15 storages, and a user with
// the given grants.
func largeCluster(t *testing.T, grants, viaPool map[string][]string) (*harness, *browser) {
	t.Helper()
	h := newHarness(t)
	for i := range largeVNets {
		h.accounts.bridgeList = append(h.accounts.bridgeList, pve.Bridge{Bridge: fmt.Sprintf("v%02d", i), Zone: "z", ZoneType: "simple"})
	}
	for i := range largeStorages {
		h.accounts.storageList = append(h.accounts.storageList, pve.Storage{Storage: fmt.Sprintf("s%02d", i), Node: "pve1", Content: []string{"images"}})
	}
	h.accounts.passwords["big@pve"] = "secret"
	h.accounts.grants["big@pve"] = grants
	h.accounts.viaPool = map[string]map[string][]string{"big@pve": viaPool}
	b := h.browser()
	b.signIn("big@pve")
	return h, b
}

func (h *harness) privilegeAsks() int {
	h.accounts.mu.Lock()
	defer h.accounts.mu.Unlock()
	return h.accounts.privilegeCalls
}

func (b *browser) usableBridges(node string) map[string]bool {
	b.h.t.Helper()
	rec := b.do("GET", "/api/proxmox/nodes/"+node+"/bridges", "")
	expect(b.h.t, rec, http.StatusOK)
	var views []bridgeView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil || len(views) != largeVNets {
		b.h.t.Fatalf("bridges: %v %s", err, rec.Body)
	}
	usable := map[string]bool{}
	for i := range views {
		if views[i].Usable {
			usable[views[i].Bridge.Bridge] = true
		}
	}
	return usable
}

func (b *browser) usableStorages() map[string]bool {
	b.h.t.Helper()
	rec := b.do("GET", "/api/proxmox/storages", "")
	expect(b.h.t, rec, http.StatusOK)
	var views []storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil || len(views) != largeStorages {
		b.h.t.Fatalf("storages: %v %s", err, rec.Body)
	}
	usable := map[string]bool{}
	for i := range views {
		if views[i].Usable {
			usable[views[i].Storage.Storage] = true
		}
	}
	return usable
}

func sameSet(got map[string]bool, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

// A user with grants on the VNets' own paths costs one question to Proxmox VE for a list, not one
// per VNet or storage.
func TestListsAskOnceForListedPaths(t *testing.T) {
	t.Parallel()
	grants := map[string][]string{}
	for _, v := range []string{"v00", "v01", "v02"} {
		grants["/sdn/zones/z/"+v] = []string{"SDN.Use"}
	}
	grants["/storage/s00"] = []string{"Datastore.AllocateSpace"}
	h, b := largeCluster(t, grants, nil)

	if got := b.usableBridges("pve1"); !sameSet(got, "v00", "v01", "v02") {
		t.Fatalf("usable VNets = %v", got)
	}
	bridgeAsks := h.privilegeAsks()
	t.Logf("privilege questions for one bridge list of %d VNets: %d", largeVNets, bridgeAsks)
	if bridgeAsks > 2 {
		t.Fatalf("a bridge list asked %d times", bridgeAsks)
	}

	h2, b2 := largeCluster(t, grants, nil)
	if got := b2.usableStorages(); !sameSet(got, "s00") {
		t.Fatalf("usable storages = %v", got)
	}
	storageAsks := h2.privilegeAsks()
	t.Logf("privilege questions for one storage list of %d storages: %d", largeStorages, storageAsks)
	if storageAsks > 2 {
		t.Fatalf("a storage list asked %d times", storageAsks)
	}
}

// Privileges that come only through a pool, only on one VNet's path, or not at all give the lists
// they always did.
func TestListsKeepTheirAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		grants   map[string][]string
		viaPool  map[string][]string
		vnets    []string
		storages []string
	}{{
		name: "through a pool",
		viaPool: map[string][]string{
			"/sdn/zones/z/v03": {"SDN.Use"}, "/storage/s03": {"Datastore.AllocateSpace"},
		},
		vnets: []string{"v03"}, storages: []string{"s03"},
	}, {
		name:   "one VNet path",
		grants: map[string][]string{"/sdn/zones/z/v07": {"SDN.Use"}},
		vnets:  []string{"v07"},
	}, {
		name:   "the wrong privilege",
		grants: map[string][]string{"/sdn/zones/z/v07": {"SDN.Audit"}, "/storage/s01": {"Datastore.Audit"}},
	}, {
		name: "nothing",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, b := largeCluster(t, tc.grants, tc.viaPool)
			if got := b.usableBridges("pve1"); !sameSet(got, tc.vnets...) {
				t.Fatalf("usable VNets = %v, want %v", got, tc.vnets)
			}
			if got := b.usableStorages(); !sameSet(got, tc.storages...) {
				t.Fatalf("usable storages = %v, want %v", got, tc.storages)
			}
			t.Logf("privilege questions for both lists: %d", h.privilegeAsks())
		})
	}
}

// A privilege inherited from a path above the VNets and storages is not in the one answer, so each
// is asked for, and the answer is the same as when every path was.
func TestListsAskWhereTheAnswerCannotTell(t *testing.T) {
	t.Parallel()
	h, b := largeCluster(t, map[string][]string{
		"/sdn/zones/z": {"SDN.Use"}, "/storage": {"Datastore.AllocateSpace"},
	}, nil)
	if got := b.usableBridges("pve1"); len(got) != largeVNets {
		t.Fatalf("usable VNets = %v", got)
	}
	if got := b.usableStorages(); len(got) != largeStorages {
		t.Fatalf("usable storages = %v", got)
	}
	t.Logf("privilege questions for both lists: %d", h.privilegeAsks())
}

// The form asks for the bridges of every node at once. Each VNet is asked about once for them all.
func TestBridgeListsOfAllNodesShareTheirQuestions(t *testing.T) {
	t.Parallel()
	h, b := largeCluster(t, map[string][]string{"/": {"SDN.Use"}}, nil)
	var wg sync.WaitGroup
	for i := range largeNodes {
		wg.Go(func() {
			if got := b.usableBridges(fmt.Sprintf("pve%d", i+1)); len(got) != largeVNets {
				t.Errorf("node %d: usable VNets = %v", i+1, got)
			}
		})
	}
	wg.Wait()
	asks := h.privilegeAsks()
	t.Logf("privilege questions for %d nodes' bridge lists of %d VNets: %d", largeNodes, largeVNets, asks)
	if asks > largeVNets+1 {
		t.Fatalf("%d questions for %d VNets", asks, largeVNets)
	}
}
