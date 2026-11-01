package pve

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var bob = &Ticket{Username: "bob@pve", Ticket: "PVE:bob@pve:1", CSRF: "csrf-bob"}

func replyData(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Error(err)
	}
}

// refuseInStatusLine answers as Proxmox VE does, with its message in the status line, which
// net/http cannot write.
func refuseInStatusLine(t *testing.T, w http.ResponseWriter, line string) {
	t.Helper()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "HTTP/1.1 "+line+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

// signedIn is the ticket a request carries, nil when none.
func signedIn(r *http.Request) *Ticket {
	c, err := r.Cookie("PVEAuthCookie")
	if err != nil {
		return nil
	}
	for _, t := range []*Ticket{alice, bob} {
		if c.Value == t.Ticket {
			return t
		}
	}
	return nil
}

func serveAPI(t *testing.T, mux *http.ServeMux) *Accounts {
	t.Helper()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAccounts(Config{URL: srv.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// clusterResources is /cluster/resources as Proxmox VE filters it per user: alice holds
// Datastore.Audit and Sys.Audit everywhere, bob only on node one.
func clusterResources(t *testing.T) *Accounts {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		user := signedIn(r)
		if user == nil {
			http.Error(w, "no ticket", http.StatusUnauthorized)
			return
		}
		var all []map[string]any
		switch r.URL.Query().Get("type") {
		case "storage":
			all = []map[string]any{
				{"id": "storage/one/local-lvm", "storage": "local-lvm", "node": "one", "shared": 0, "content": "rootdir,images", "disk": 300, "maxdisk": 1000, "status": "available"},
				{"id": "storage/two/ceph", "storage": "ceph", "node": "two", "shared": 1, "content": "images", "disk": 10, "maxdisk": 50, "status": "unknown"},
			}
		case "node":
			all = []map[string]any{
				{"id": "node/one", "node": "one", "status": "online", "mem": 4096, "maxmem": 16384},
				{"id": "node/two", "node": "two", "status": "offline"},
			}
		default:
			t.Errorf("resources of type %q", r.URL.Query().Get("type"))
		}
		visible := []map[string]any{}
		for _, res := range all {
			if user == alice || res["node"] == "one" {
				visible = append(visible, res)
			}
		}
		replyData(t, w, visible)
	})
	return serveAPI(t, mux)
}

func TestStoragesShowSpaceOnlyWhereAvailable(t *testing.T) {
	t.Parallel()
	a := clusterResources(t)
	storages, err := a.Storages(context.Background(), alice)
	if err != nil {
		t.Fatal(err)
	}
	want := []Storage{
		{Storage: "local-lvm", Node: "one", Content: []string{"rootdir", "images"}, Avail: 700, Total: 1000},
		{Storage: "ceph", Node: "two", Shared: true, Content: []string{"images"}},
	}
	if !reflect.DeepEqual(storages, want) {
		t.Errorf("storages %+v, want %+v", storages, want)
	}
}

func TestListingsAreThoseTheUsersTicketMaySee(t *testing.T) {
	t.Parallel()
	a := clusterResources(t)
	ctx := context.Background()
	storages, err := a.Storages(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(storages) != 1 || storages[0].Node != "one" {
		t.Errorf("bob's storages %+v", storages)
	}
	nodes, err := a.Nodes(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Node{{Node: "one", Online: true, Memory: 4096, MaxMemory: 16384}}; !reflect.DeepEqual(nodes, want) {
		t.Errorf("bob's nodes %+v, want %+v", nodes, want)
	}
	if _, err := a.Nodes(ctx, &Ticket{Ticket: "PVE:expired"}); !errors.Is(err, ErrAuthentication) {
		t.Errorf("an expired ticket: %v", err)
	}
}

func TestNodesTellOnlineFromOffline(t *testing.T) {
	t.Parallel()
	nodes, err := clusterResources(t).Nodes(context.Background(), alice)
	if err != nil {
		t.Fatal(err)
	}
	// A node that is down reports no memory.
	want := []Node{{Node: "one", Online: true, Memory: 4096, MaxMemory: 16384}, {Node: "two"}}
	if !reflect.DeepEqual(nodes, want) {
		t.Errorf("nodes %+v, want %+v", nodes, want)
	}
}

func TestBridgesOfANodeNeedSysAuditOnIt(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/nodes/{node}/network", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "any_bridge" {
			t.Errorf("network asked for with %s", r.URL.RawQuery)
		}
		if r.PathValue("node") != "one" || signedIn(r) != alice {
			http.Error(w, "Permission check failed (/nodes/"+r.PathValue("node")+", Sys.Audit)", http.StatusForbidden)
			return
		}
		replyData(t, w, []map[string]any{
			{"iface": "vmbr0", "type": "bridge", "bridge_vlan_aware": 1, "comments": "  uplink\n"},
			{"iface": "vmbr1", "type": "OVSBridge"},
		})
	})
	a := serveAPI(t, mux)
	ctx := context.Background()
	bridges, err := a.Bridges(ctx, alice, "one")
	if err != nil {
		t.Fatal(err)
	}
	want := []Bridge{{Bridge: "vmbr0", Node: "one", VLANAware: true, Comment: "uplink"}, {Bridge: "vmbr1", Node: "one"}}
	if !reflect.DeepEqual(bridges, want) {
		t.Errorf("bridges %+v, want %+v", bridges, want)
	}
	if _, err := a.Bridges(ctx, bob, "one"); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob on node one: %v", err)
	}
	if _, err := a.Bridges(ctx, alice, "two/../one"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a node name with a path in it reached another node: %v", err)
	}
}

func TestAllowedNodesOfAnHARule(t *testing.T) {
	t.Parallel()
	nodes := []string{"one", "two", "three"}
	for _, c := range []struct {
		name string
		rule HARule
		want []string
	}{
		{"positive", HARule{Nodes: []string{"two", "one"}}, []string{"one", "two"}},
		{"negative", HARule{Nodes: []string{"three"}, Negative: true}, []string{"one", "two"}},
		{"positive on nodes that are gone", HARule{Nodes: []string{"four"}}, nil},
		{"negative on every node", HARule{Nodes: nodes, Negative: true}, nil},
		{"negative on none", HARule{Nodes: []string{}, Negative: true}, nodes},
	} {
		if got := c.rule.Allowed(nodes); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestZoneOfABridge(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/cluster/sdn/vnets/{vnet}", func(w http.ResponseWriter, r *http.Request) {
		switch vnet := r.PathValue("vnet"); vnet {
		case "tenants":
			replyData(t, w, map[string]any{"vnet": vnet, "zone": "evpn1", "type": "vnet"})
		case "zoneless":
			replyData(t, w, map[string]any{"vnet": vnet})
		case "hidden":
			http.Error(w, "Permission check failed (/sdn/zones, SDN.Audit)", http.StatusForbidden)
		case "broken":
			refuseInStatusLine(t, w, "500 cfs-lock 'file-sdn_cfg' error: got lock request timeout")
		default:
			refuseInStatusLine(t, w, "500 sdn vnet object ID '"+vnet+"' does not exist")
		}
	})
	a := serveAPI(t, mux)
	ctx := context.Background()
	for bridge, want := range map[string]string{"tenants": "evpn1", "vmbr0": "localnetwork"} {
		if zone, err := a.Zone(ctx, alice, bridge); err != nil || zone != want {
			t.Errorf("%s: %q, %v, want %q", bridge, zone, err, want)
		}
	}
	if _, err := a.Zone(ctx, alice, "zoneless"); err == nil || !strings.Contains(err.Error(), "names no zone for VNet zoneless") {
		t.Errorf("a VNet without a zone: %v", err)
	}
	if _, err := a.Zone(ctx, alice, "hidden"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a VNet the user may not see: %v", err)
	}
	// Only "does not exist" means a plain bridge. Any other failure is no zone at all.
	if zone, err := a.Zone(ctx, alice, "broken"); err == nil {
		t.Errorf("a failed lookup gave zone %q", zone)
	}
}

const destroyTask = "UPID:one:0001E240:00A1B2C3:6720F000:qmdestroy:100:alice@pve:"

// taskAPI destroys VMs: 100 in a task that runs for a while and ends well, 101 in one that fails,
// 102 not at all for want of VM.Allocate, and 103 in one that never ends.
type taskAPI struct {
	mu      sync.Mutex
	deletes []string
	polls   int
}

func (f *taskAPI) accounts(t *testing.T) *Accounts {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api2/json/nodes/one/qemu/{vmid}", func(w http.ResponseWriter, r *http.Request) {
		if signedIn(r) != alice || r.Header.Get("Csrfpreventiontoken") != alice.CSRF {
			http.Error(w, "no ticket or CSRF token", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.deletes = append(f.deletes, r.PathValue("vmid")+"?"+r.URL.RawQuery)
		f.mu.Unlock()
		if r.PathValue("vmid") == "102" {
			http.Error(w, "Permission check failed (/vms/102, VM.Allocate)", http.StatusForbidden)
			return
		}
		replyData(t, w, strings.Replace(destroyTask, ":100:", ":"+r.PathValue("vmid")+":", 1))
	})
	mux.HandleFunc("GET /api2/json/nodes/one/tasks/{upid}/status", func(w http.ResponseWriter, r *http.Request) {
		if signedIn(r) != alice {
			http.Error(w, "no ticket", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.polls++
		polls := f.polls
		f.mu.Unlock()
		upid := r.PathValue("upid")
		switch {
		case strings.Contains(upid, ":100:") && polls >= 2:
			replyData(t, w, map[string]any{"upid": upid, "status": "stopped", "exitstatus": "OK"})
		case strings.Contains(upid, ":101:"):
			replyData(t, w, map[string]any{"upid": upid, "status": "stopped", "exitstatus": "lvremove 'pve/vm-101-disk-0' error: Logical volume in use"})
		default:
			replyData(t, w, map[string]any{"upid": upid, "status": "running"})
		}
	})
	return serveAPI(t, mux)
}

func TestDeleteVMWaitsForItsTaskToEnd(t *testing.T) {
	t.Parallel()
	f := &taskAPI{}
	if err := f.accounts(t).DeleteVM(context.Background(), alice, "one", 100); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []string{"100?purge=1&destroy-unreferenced-disks=1"}; !reflect.DeepEqual(f.deletes, want) {
		t.Errorf("deletes %v, want %v", f.deletes, want)
	}
	if f.polls != 2 {
		t.Errorf("%d task polls, want 2", f.polls)
	}
}

func TestDeleteVMFailsWithTheTasksExitStatus(t *testing.T) {
	t.Parallel()
	err := (&taskAPI{}).accounts(t).DeleteVM(context.Background(), alice, "one", 101)
	if err == nil || !strings.Contains(err.Error(), "Logical volume in use") || !strings.Contains(err.Error(), ":qmdestroy:101:") {
		t.Errorf("got %v", err)
	}
}

func TestDeleteVMRefusedStartsNoWait(t *testing.T) {
	t.Parallel()
	f := &taskAPI{}
	if err := f.accounts(t).DeleteVM(context.Background(), alice, "one", 102); !errors.Is(err, ErrForbidden) {
		t.Errorf("got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.polls != 0 {
		t.Errorf("%d task polls after a refused delete", f.polls)
	}
}

func TestDeleteVMStopsWaitingWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := (&taskAPI{}).accounts(t)
	// Cancelled once the first status has been read, so the wait before the next poll sees it.
	a.http.Transport = closeHook{next: a.http.Transport, matches: func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/status") }, after: cancel}
	err := a.DeleteVM(ctx, alice, "one", 103)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "waiting for proxmox VE task") {
		t.Errorf("got %v", err)
	}
}
