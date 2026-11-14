package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// ciShort is ci with its VMs gone and no node with the memory for them, while the Proxmox VE nodes
// run two major versions.
func ciShort(d *demoCluster) {
	d.AvoidSkewedNode = false
	d.LackingMemory = "No Proxmox VE node this cluster may use has 16 GiB of free memory for its largest machine. The best, pve2, has 11 GiB"
	d.VMsMissing = true
	d.ProxmoxVersions = "8=pve1,pve3;9=pve2"
	d.NodeMTUs = "pve1=1500,pve2=9000"
	d.HARulesPending = "The Proxmox VE nodes run different major versions. The cluster's HA rules are written once they run one."
}

// dataQuorumLost is data with two of its three control-plane VMs stopped.
func dataQuorumLost(d *demoCluster) {
	d.AvoidSkewedNode = false
	d.QuorumLost = true
}

// platformDrainHeld is platform with a Talos upgrade a disruption budget holds, a machine's
// replacement waiting for a node in maintenance and a worker whose clock is not synced.
func platformDrainHeld(d *demoCluster) {
	d.AvoidSkewedNode = false
	d.Held = true
	d.Unsynced = true
	d.TalosHold = &demoTalosHold{To: "v1.14.5", SinceMinutes: 25, Pod: "data/orders-db-0", Budget: "orders-db"}
}

// demoProblems are the demo as it ships and with each problem it can show, by name.
var demoProblems = map[string]map[string]func(*demoCluster){
	"as shipped":         nil,
	"ci short":           {"ci": ciShort},
	"data quorum lost":   {"data": dataQuorumLost},
	"platform held":      {"platform": platformDrainHeld},
	"all three together": {"ci": ciShort, "data": dataQuorumLost, "platform": platformDrainHeld},
}

// demoFleetEdited is demo-fleet.json with the clusters named changed as edits say.
func demoFleetEdited(t *testing.T, edits map[string]func(*demoCluster)) []byte {
	t.Helper()
	var data demoData
	if err := json.Unmarshal(demoFleetJSON, &data); err != nil {
		t.Fatal(err)
	}
	for name, edit := range edits {
		found := false
		for i := range data.Clusters {
			if data.Clusters[i].Name == name {
				edit(&data.Clusters[i])
				found = true
			}
		}
		if !found {
			t.Fatalf("the demo has no cluster %s", name)
		}
	}
	raw, err := json.Marshal(&data)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// newDemoWith is newDemo over the demo's fleet with the clusters named changed as edits say.
func newDemoWith(t *testing.T, edits map[string]func(*demoCluster)) *demoBrowser {
	t.Helper()
	s := &Server{Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Kubehenge</title>")}}}
	cleanup, err := demoOver(s, func() time.Time { return demoAt }, demoFleetEdited(t, edits))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return &demoBrowser{t: t, handler: s.Handler()}
}

// demoVerdicts are the fleet's verdicts by cluster. No cluster lists an offline node, as none of
// the demo's is: the node that fails forwarded calls is online in Proxmox VE and runs its VMs.
func demoVerdicts(t *testing.T, b *demoBrowser) map[string]string {
	t.Helper()
	var health []fleetHealthView
	decodeDemo(t, b.do("GET", "/api/fleet/health", ""), &health)
	verdicts := map[string]string{}
	for _, h := range health {
		verdicts[h.Name] = h.Verdict
		for i := range h.Attention {
			if what := h.Attention[i].What; strings.Contains(what, "offline") {
				t.Errorf("%s lists %q, but no Proxmox VE node of the demo is offline", h.Name, what)
			}
		}
	}
	return verdicts
}

// The fleet edited is the shipped one where no problem is set, and each problem the demo can show
// still takes its cluster out of healthy.
func TestDemoShowsEachProblemOnItsCluster(t *testing.T) {
	t.Parallel()
	for name, edits := range demoProblems {
		if len(edits) != 1 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newDemoWith(t, edits)
			b.signIn()
			verdicts := demoVerdicts(t, b)
			for cluster := range edits {
				if v := verdicts[cluster]; v == verdictHealthy || v == verdictChanging {
					t.Errorf("%s is %s with its problem", cluster, v)
				}
			}
		})
	}
}

func TestDemoShowsAClusterShortOfMemory(t *testing.T) {
	t.Parallel()
	b := newDemoWith(t, map[string]func(*demoCluster){"ci": ciShort})
	b.signIn()
	var clusters []clusterView
	decodeDemo(t, b.do("GET", "/api/clusters", ""), &clusters)
	lacking := false
	for i := range clusters {
		for _, cond := range clusters[i].Conditions {
			lacking = lacking || clusters[i].Name == "ci" && cond.Type == provisioner.NodesLackMemoryCondition && cond.Status == "True"
		}
	}
	if !lacking {
		t.Error("the demo's ci cluster does not show NodesLackMemory")
	}
}
