package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// versionedPVE answers the reads of OnlineNodes and NodeMTUs, with the version each node reports. A
// node in broken answers its version with an error.
type versionedPVE struct {
	mu       sync.Mutex
	versions map[string]string
	broken   map[string]bool
	asked    map[string]int
	// vmNets are the net0 settings of the VMs of pool "p", by node, and bridges each node's bridge MTU.
	vmNets  map[string]string
	bridges map[string]int
}

func (f *versionedPVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	var data any
	switch {
	case path == "/nodes":
		list := make([]map[string]any, 0, len(f.versions))
		for n := range f.versions {
			list = append(list, map[string]any{"node": n, "status": "online"})
		}
		data = list
	case path == "/cluster/resources" && r.URL.Query().Get("type") == "storage":
		data = []any{}
	case path == "/cluster/resources":
		vms := make([]map[string]any, 0, len(f.vmNets))
		id := 100
		for n := range f.vmNets {
			id++
			vms = append(vms, map[string]any{"vmid": id, "node": n, "pool": "p", "type": "qemu", "template": 0})
		}
		data = vms
	case strings.HasSuffix(path, "/version"):
		node := strings.Split(path, "/")[2]
		f.asked[node]++
		if f.broken[node] {
			http.Error(w, `{"message":"no"}`, http.StatusInternalServerError)
			return
		}
		data = map[string]string{"version": f.versions[node], "release": "x"}
	case strings.HasSuffix(path, "/status"):
		data = map[string]any{"cpuinfo": map[string]string{"flags": ""}}
	case strings.HasSuffix(path, "/config"):
		data = map[string]string{"net0": f.vmNets[strings.Split(path, "/")[2]]}
	case strings.Contains(path, "/network"):
		data = []map[string]any{{"iface": "vmbr0", "mtu": f.bridges[strings.Split(path, "/")[2]]}}
	default:
		http.Error(w, `{"message":"unexpected `+path+`"}`, http.StatusNotFound)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		panic(err)
	}
}

func newVersionedPVE(t *testing.T, versions map[string]string) (*versionedPVE, *Proxmox) {
	t.Helper()
	f := &versionedPVE{versions: versions, broken: map[string]bool{}, asked: map[string]int{}, vmNets: map[string]string{}, bridges: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}
}

func TestVersionCacheReadsEachNodeOnceAMinute(t *testing.T) {
	t.Parallel()
	f, p := newVersionedPVE(t, map[string]string{"pve1": "9.0.3"})
	now := time.Unix(1_000, 0)
	p.versions.now = func() time.Time { return now }
	for range 3 {
		if v, major := p.versions.read(t.Context(), p.Client, "pve1"); v != "9.0.3" || major != 9 {
			t.Fatalf("read %q, %d", v, major)
		}
	}
	if f.asked["pve1"] != 1 {
		t.Errorf("asked %d times within a minute, want 1", f.asked["pve1"])
	}
	now = now.Add(versionFresh + time.Second)
	f.mu.Lock()
	f.versions["pve1"] = "9.1.1"
	f.mu.Unlock()
	if v, _ := p.versions.read(t.Context(), p.Client, "pve1"); v != "9.1.1" || f.asked["pve1"] != 2 {
		t.Errorf("after a minute: %q after %d reads, want 9.1.1 after 2", v, f.asked["pve1"])
	}
}

func TestVersionCacheKeepsTheLastAnswerWhenAReadFails(t *testing.T) {
	t.Parallel()
	f, p := newVersionedPVE(t, map[string]string{"pve1": "8.4.1", "pve2": "9.0.3"})
	now := time.Unix(1_000, 0)
	p.versions.now = func() time.Time { return now }
	f.broken["pve2"] = true
	if _, major := p.versions.read(t.Context(), p.Client, "pve2"); major != 0 {
		t.Errorf("a node never read has major %d, want 0", major)
	}
	f.mu.Lock()
	f.broken["pve2"] = false
	f.mu.Unlock()
	p.versions.read(t.Context(), p.Client, "pve2")
	now = now.Add(2 * versionFresh)
	f.mu.Lock()
	f.broken["pve2"] = true
	f.mu.Unlock()
	if _, major := p.versions.read(t.Context(), p.Client, "pve2"); major != 9 {
		t.Errorf("after a failed read the node has major %d, want the last one, 9", major)
	}
}

func TestOnlineNodesCarryTheirVersions(t *testing.T) {
	t.Parallel()
	f, p := newVersionedPVE(t, map[string]string{"pve1": "8.4.1", "pve2": "9.0.3", "pve3": "9.0.3"})
	f.broken["pve3"] = true
	online, err := p.OnlineNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, n := range online {
		got[n.Name] = n.Major
	}
	if got["pve1"] != 8 || got["pve2"] != 9 || got["pve3"] != 0 || len(online) != 3 {
		t.Errorf("majors %v, want pve1 8, pve2 9, pve3 unknown and still online", got)
	}
}

func nodesOf(majors map[string]int) []Node {
	out := make([]Node, 0, len(majors))
	for name, major := range majors {
		out = append(out, Node{Name: name, CPULevel: 3, Major: major})
	}
	slices.SortFunc(out, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func TestVersionNoteNamesTheNodesOfEachVersion(t *testing.T) {
	t.Parallel()
	for note, majors := range map[string]map[string]int{
		"none":                    {"pve1": 9, "pve2": 9},
		"8=pve2,pve4;9=pve1,pve3": {"pve1": 9, "pve2": 8, "pve3": 9, "pve4": 8},
		"8=pve3;9=pve2;10=pve1":   {"pve1": 10, "pve2": 9, "pve3": 8},
		"8=pve3;9=pve1":           {"pve1": 9, "pve2": 0, "pve3": 8},
		"":                        {"pve1": 0, "pve2": 0},
	} {
		if note == "none" {
			note = ""
		}
		if got := versionNote(nodesOf(majors)); got != note {
			t.Errorf("note %q for %v, want %q", got, majors, note)
		}
	}
}

// A VM goes to the oldest nodes while one has room, to any placeable node when none has, and is
// never refused for the versions alone.
func TestPlacementPrefersTheOldestMajorVersionWhileItHasRoom(t *testing.T) {
	t.Parallel()
	const need = 4096
	free := func(majors map[string]int, freeMiB map[string]int64) []Node {
		online := nodesOf(majors)
		for i := range online {
			online[i].FreeMiB, online[i].MemoryKnown = freeMiB[online[i].Name], true
		}
		return online
	}
	for _, tc := range []struct {
		name      string
		online    []Node
		want      []string
		short     bool
		wantNewer bool
	}{
		{"one version", free(map[string]int{"pve1": 9, "pve2": 9}, map[string]int64{"pve1": 9000, "pve2": 9000}), []string{"pve1", "pve2"}, false, false},
		{"one version, no room", free(map[string]int{"pve1": 9, "pve2": 9}, map[string]int64{"pve1": 1000, "pve2": 1000}), []string{"pve1", "pve2"}, true, false},
		{"the oldest nodes have room", free(map[string]int{"pve1": 8, "pve2": 9, "pve3": 8}, map[string]int64{"pve1": 9000, "pve2": 90000, "pve3": 9000}), []string{"pve1", "pve3"}, false, false},
		{"the oldest nodes are full and a newer one is free", free(map[string]int{"pve1": 8, "pve2": 9}, map[string]int64{"pve1": 1000, "pve2": 90000}), []string{"pve2"}, false, true},
		{"three versions fall back to the nodes with room", free(map[string]int{"pve1": 8, "pve2": 9, "pve3": 10}, map[string]int64{"pve1": 1000, "pve2": 1000, "pve3": 90000}), []string{"pve3"}, false, true},
		{"every node is full", free(map[string]int{"pve1": 8, "pve2": 9}, map[string]int64{"pve1": 1000, "pve2": 1000}), []string{"pve1", "pve2"}, true, false},
		{"a node whose version cannot be read counts as the oldest", free(map[string]int{"pve1": 9, "pve2": 0, "pve3": 8}, map[string]int64{"pve1": 90000, "pve2": 9000, "pve3": 9000}), []string{"pve2", "pve3"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hosts := make([]string, len(tc.online))
			for i, n := range tc.online {
				hosts[i] = n.Name
			}
			fit := func(list []string) ([]string, *roomOn) {
				room := withRoom(tc.online, list, list, need)
				if room.short(list) {
					return list, &room
				}
				return room.Nodes, nil
			}
			got, short, newer := preferOldest(tc.online, hosts, fit)
			if !slices.Equal(got, tc.want) || (short != nil) != tc.short || newer != tc.wantNewer {
				t.Errorf("preferOldest = %v, short %v, newer %v, want %v, short %v, newer %v", got, short != nil, newer, tc.want, tc.short, tc.wantNewer)
			}
		})
	}
}

func TestPlacerSaysOnceThatItFellBackToANewerNode(t *testing.T) {
	t.Parallel()
	web := cluster()
	rec := events.NewFakeRecorder(10)
	p := &Placer{Events: rec}
	online := nodesOf(map[string]int{"pve1": 8, "pve2": 9})
	p.markNewer(web, online, false)
	p.markNewer(web, online, true)
	p.markNewer(web, online, true)
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "8: pve1, 9: pve2") || !strings.Contains(e, "cannot move back to an older node") {
			t.Errorf("event %q does not name the versions and the one-way move", e)
		}
	default:
		t.Fatal("no event")
	}
	select {
	case e := <-rec.Events:
		t.Errorf("a second look repeats the event: %q", e)
	default:
	}
	p.markNewer(web, online, false)
	p.markNewer(web, online, true)
	if len(rec.Events) != 1 {
		t.Errorf("%d events after the placement fell back again, want 1", len(rec.Events))
	}
}

func TestVersionNoteRoundTrips(t *testing.T) {
	t.Parallel()
	groups := ParseVersionNote("9=pve2;8=pve1,pve3;junk")
	if len(groups) != 2 || groups[0].Major != 8 || !slices.Equal(groups[0].Nodes, []string{"pve1", "pve3"}) || groups[1].Major != 9 {
		t.Errorf("groups %+v", groups)
	}
	if ParseVersionNote("") != nil {
		t.Error("an empty note has groups")
	}
}

func TestPlacerRecordsMixedVersionsAndNamesTheNodesLeftOut(t *testing.T) {
	t.Parallel()
	web := cluster()
	_, c := newReconciler(t, nil, web)
	rec := events.NewFakeRecorder(10)
	online := nodesOf(map[string]int{"pve1": 8, "pve2": 9})
	p := &Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: &versionedNodes{online: online}, Events: rec}
	get := func() *clusterv1.Cluster {
		got := &clusterv1.Cluster{}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	p.placeAll(t.Context())
	got := get()
	if v := got.Annotations[OnlineNodesAnnotation]; v != "pve1" {
		t.Errorf("online nodes %q, want only pve1", v)
	}
	if v := got.Annotations[ProxmoxVersionsAnnotation]; v != "8=pve1;9=pve2" {
		t.Errorf("versions %q", v)
	}
	p.placeAll(t.Context())
	p.Nodes = &versionedNodes{online: nodesOf(map[string]int{"pve1": 9, "pve2": 9})}
	p.placeAll(t.Context())
	got = get()
	if _, ok := got.Annotations[ProxmoxVersionsAnnotation]; ok || got.Annotations[OnlineNodesAnnotation] != "pve1,pve2" {
		t.Errorf("after the upgrade: %v", got.Annotations)
	}
}

type versionedNodes struct{ online []Node }

func (v *versionedNodes) OnlineNodes(context.Context) ([]Node, error) {
	return v.online, nil
}

func TestMTUNote(t *testing.T) {
	t.Parallel()
	if got := mtuNote(map[string]int{"pve1": 1500, "pve2": 1500}); got != "" {
		t.Errorf("equal sizes noted as %q", got)
	}
	got := mtuNote(map[string]int{"pve2": 9000, "pve1": 1500})
	if got != "pve1=1500,pve2=9000" {
		t.Errorf("note %q", got)
	}
	if parsed := ParseNodeMTUs(got); len(parsed) != 2 || parsed[1] != (NodeMTU{Node: "pve2", MTU: 9000}) {
		t.Errorf("parsed %+v", parsed)
	}
}

func TestNodeMTUsFollowTheNodesVersion(t *testing.T) {
	t.Parallel()
	f, p := newVersionedPVE(t, map[string]string{"pve1": "8.4.1", "pve2": "9.0.3", "pve3": "9.0.3"})
	f.vmNets = map[string]string{
		"pve1": "virtio=AA:BB,bridge=vmbr0",
		"pve2": "virtio=AA:BC,bridge=vmbr0",
		"pve3": "virtio=AA:BD,bridge=vmbr0,mtu=1400",
	}
	f.bridges = map[string]int{"pve1": 9000, "pve2": 9000, "pve3": 9000}
	online, err := p.OnlineNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.NodeMTUs(t.Context(), "p", online)
	if err != nil {
		t.Fatal(err)
	}
	if got["pve1"] != 1500 || got["pve2"] != 9000 || got["pve3"] != 1400 {
		t.Errorf("sizes %v, want pve1 1500 (unset on 8), pve2 9000 (the bridge on 9), pve3 1400 (set)", got)
	}
}
