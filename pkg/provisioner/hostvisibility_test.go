package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// haManagerStatusFixture is what `GET /cluster/ha/status/manager_status` returns, built from Proxmox VE's
// source (pve-ha-manager, master, fetched 2026-11-07):
//   - src/PVE/API2/HA/Status.pm, endpoint `manager_status`: `$data = { manager_status => $status }`,
//     then `$data->{quorum}` and `$data->{lrm_status}->{$node}`, with `check => ['perm', '/', ['Sys.Audit']]`.
//   - src/PVE/HA/Manager.pm, `flush_master_status`: `$ms->{node_status} = $ns->{status}`.
//   - src/PVE/HA/NodeStatus.pm, `$valid_node_states`: online, maintenance, unknown, fence, gone.
const haManagerStatusFixture = `{
  "manager_status": {
    "master_node": "pve1",
    "node_status": {"pve1": "online", "pve2": "maintenance", "pve3": "unknown", "pve4": "fence", "pve5": "gone"},
    "service_status": {},
    "timestamp": 1790000000
  },
  "quorum": {"node": "pve1", "quorate": 1},
  "lrm_status": {"pve1": {"mode": "active", "state": "wait_for_agent_lock", "results": {}, "timestamp": 1790000000}}
}`

func TestHAMaintenanceIsOnlyTheMaintenanceState(t *testing.T) {
	t.Parallel()
	got, err := haMaintenance(t.Context(), fakeProxmox{haStatusPath: haManagerStatusFixture})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["pve2"] {
		t.Errorf("maintenance = %v, want only pve2: unknown, fence and gone are failures, not maintenance", got)
	}
	// A cluster that never used HA has an empty status file.
	got, err = haMaintenance(t.Context(), fakeProxmox{haStatusPath: `{"manager_status":{},"quorum":{"node":"pve1","quorate":1}}`})
	if err != nil || len(got) != 0 {
		t.Errorf("without HA: %v, %v", got, err)
	}
}

// Only `gone` proves a host will not return. `PVE::HA::NodeStatus::update` sets it for a node that
// was `unknown` and is missing from the cluster's member list. `unknown` and `fence` are failures a
// rebooting or fenced host recovers from: `fence_node` moves a fenced node back to `unknown`.
func TestHAGoneIsTheOnlyPermanentState(t *testing.T) {
	t.Parallel()
	maintenance, gone, err := haNodeSets(t.Context(), fakeProxmox{haStatusPath: haManagerStatusFixture})
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || !gone["pve5"] {
		t.Errorf("gone = %v, want only pve5: online, maintenance, unknown and fence are not removals", gone)
	}
	if len(maintenance) != 1 || !maintenance["pve2"] {
		t.Errorf("maintenance = %v", maintenance)
	}
}

// haStates is the HA manager's status with each node in the state given.
func (f *vmFixture) haStates(states map[string]string) {
	parts := make([]string, 0, len(states))
	for node, state := range states {
		parts = append(parts, fmt.Sprintf("%q:%q", node, state))
	}
	f.pve[haStatusPath] = `{"manager_status":{"node_status":{` + strings.Join(parts, ",") + `}}}`
}

// members is the cluster's own node list, as /cluster/config/nodes returns it.
func (f *vmFixture) members(names ...string) {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf(`{"node":%q}`, n))
	}
	f.pve[clusterNodesPath] = "[" + strings.Join(parts, ",") + "]"
}

func TestAHostTheHAManagerHasGoneEndsTheWaitAtOnce(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.nodeNotReady()
	f.nodes([]string{"pve2"})
	f.look(0, RemediationHeldCondition)
	wantHeld(t, f.heldMachines(), heldSecond)
	// pve2 left the cluster: it is still in the resource list as offline, and the HA manager has it as gone.
	f.haStates(map[string]string{"pve1": "online", "pve2": "gone", "pve3": "online", "pve4": "online"})
	if cond := f.look(time.Minute, RemediationHeldCondition); cond != nil {
		t.Fatalf("a gone host still holds: %+v", cond)
	}
	wantHeld(t, f.heldMachines())
	if _, since := f.machine(heldSecond).Annotations[HoldSinceAnnotation]; since {
		t.Error("the record of the outage stays")
	}
}

func TestAFencedOrUnknownHostIsStillWaitedFor(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"fence", "unknown"} {
		f := newVMFixture(t)
		f.nodeNotReady()
		f.nodes([]string{"pve2"})
		f.haStates(map[string]string{"pve1": "online", "pve2": state, "pve3": "online", "pve4": "online"})
		f.members("pve1", "pve2", "pve3", "pve4")
		if cond := f.look(0, RemediationHeldCondition); cond == nil || cond.Reason != HeldNodeOffline {
			t.Fatalf("%s: condition %+v", state, cond)
		}
		wantHeld(t, f.heldMachines(), heldSecond)
	}
}

func TestAHostMissingFromTheClustersMembersEndsTheWait(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.nodeNotReady()
	f.nodes([]string{"pve2"})
	f.members("pve1", "pve2", "pve3", "pve4")
	f.look(0, RemediationHeldCondition)
	wantHeld(t, f.heldMachines(), heldSecond)
	// The HA manager has not yet moved pve2 to gone, but `pvecm delnode` took it out of the nodelist.
	f.members("pve1", "pve3", "pve4")
	if cond := f.look(time.Minute, RemediationHeldCondition); cond != nil {
		t.Fatalf("a removed host still holds: %+v", cond)
	}
	wantHeld(t, f.heldMachines())
}

func TestWithoutAMembersListNoHostCountsAsRemoved(t *testing.T) {
	t.Parallel()
	for name, members := range map[string]func(*vmFixture){
		"empty":      func(f *vmFixture) { f.members() },
		"unreadable": func(f *vmFixture) { delete(f.pve, clusterNodesPath) },
	} {
		f := newVMFixture(t)
		f.nodeNotReady()
		f.nodes([]string{"pve2"})
		members(f)
		if cond := f.look(0, RemediationHeldCondition); cond == nil {
			t.Fatalf("%s membership: the offline host is not held", name)
		}
		wantHeld(t, f.heldMachines(), heldSecond)
	}
}

func TestAnAdministratorsSkipSurvivesAGoneHost(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	m := f.machine(heldSecond)
	m.Annotations = map[string]string{clusterv1.MachineSkipRemediationAnnotation: ""}
	if err := f.kube.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	f.nodes([]string{"pve2"})
	f.haStates(map[string]string{"pve1": "online", "pve2": "gone", "pve3": "online", "pve4": "online"})
	f.look(0, RemediationHeldCondition)
	if _, skip := f.machine(heldSecond).Annotations[clusterv1.MachineSkipRemediationAnnotation]; !skip {
		t.Error("Kubehenge removed an administrator's own skip")
	}
}

func TestVMWatchReadsMaintenanceFromTheHAManagersStatus(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	f.allReady()
	f.nodes(nil)
	f.pve[haStatusPath] = haManagerStatusFixture
	cond := f.look(0, RemediationHeldCondition)
	if cond == nil || cond.Reason != HeldNodeMaintenance {
		t.Fatalf("condition %+v", cond)
	}
	wantHeld(t, f.heldMachines(), heldSecond)
}

// refusingProxmox answers the HA status with a refusal, as a token without Sys.Audit on / gets.
type refusingProxmox struct{ err error }

func (r refusingProxmox) Get(context.Context, string, any) error { return r.err }

func TestHAStatusFailuresAreLoggedOnceAndRefusalsNever(t *testing.T) {
	t.Parallel()
	var lines []string
	log := funcr.New(func(prefix, args string) { lines = append(lines, prefix+args) }, funcr.Options{})
	ctx := logr.NewContext(t.Context(), log)
	var last string
	refused := refusingProxmox{err: proxmox.ErrNotAuthorized}
	for range 3 {
		if maintenance, gone := hostFactsOf(ctx, refused, nil, &last); maintenance != nil || gone != nil {
			t.Fatalf("maintenance %v, gone %v", maintenance, gone)
		}
	}
	if len(lines) != 0 {
		t.Fatalf("a refusal logged %v", lines)
	}
	broken := refusingProxmox{err: errors.New("connection reset")}
	for range 3 {
		hostFactsOf(ctx, broken, nil, &last)
	}
	if len(lines) != 1 {
		t.Fatalf("a failure that goes on logged %d times: %v", len(lines), lines)
	}
	hostFactsOf(ctx, refusingProxmox{err: errors.New("timeout")}, nil, &last)
	if len(lines) != 2 {
		t.Errorf("a different failure logged %d lines in all", len(lines))
	}
}

func TestProxmoxReachSaysWhenTheTokenCannotSeeHostMaintenance(t *testing.T) {
	t.Parallel()
	var forbid atomic.Bool
	forbid.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api2/json/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"version": "9.0.3"}})
		case forbid.Load():
			http.Error(w, "Permission check failed (/, Sys.Audit)", http.StatusForbidden)
		default:
			_, _ = w.Write([]byte(`{"data":` + haManagerStatusFixture + `}`))
		}
	}))
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ProxmoxReach{Reader: kube, Client: kube, Namespace: "kubehenge-system", Proxmox: proxmox.NewClient(srv.URL + "/api2/json"), Interval: time.Minute}
	recorded := func() Reachability {
		t.Helper()
		if err := r.publish(t.Context()); err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{}
		if err := kube.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: ProxmoxReachName}, cm); err != nil {
			t.Fatal(err)
		}
		got, err := ParseReachability(cm)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := recorded(); !got.OK || got.Blind != "Sys.Audit on /" {
		t.Fatalf("a refused token recorded as %+v", got)
	}
	forbid.Store(false)
	if got := recorded(); !got.OK || got.Blind != "" {
		t.Fatalf("a token that may read recorded as %+v", got)
	}
}
