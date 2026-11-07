package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
		if got := maintenanceOf(ctx, refused, &last); got != nil {
			t.Fatalf("maintenance %v", got)
		}
	}
	if len(lines) != 0 {
		t.Fatalf("a refusal logged %v", lines)
	}
	broken := refusingProxmox{err: errors.New("connection reset")}
	for range 3 {
		maintenanceOf(ctx, broken, &last)
	}
	if len(lines) != 1 {
		t.Fatalf("a failure that goes on logged %d times: %v", len(lines), lines)
	}
	maintenanceOf(ctx, refusingProxmox{err: errors.New("timeout")}, &last)
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
