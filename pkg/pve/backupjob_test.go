package pve

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"testing"
)

// The interface adds the job the installer adds: to Proxmox Backup Server before a shared and a
// local storage, once, and only where no job includes the guest.
func TestAddApplianceBackup(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var added []url.Values
	jobs := []map[string]any{{"id": "all", "all": 1, "node": "one", "exclude": "151"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		replyData(t, w, []map[string]any{{"vmid": 150, "node": "one"}, {"vmid": 151, "node": "one"}, {"vmid": 152, "node": "two"}})
	})
	mux.HandleFunc("GET /api2/json/cluster/backup", func(w http.ResponseWriter, _ *http.Request) { replyData(t, w, jobs) })
	mux.HandleFunc("GET /api2/json/nodes/{node}/storage", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("content") != "backup" {
			t.Errorf("storages asked for with %s", r.URL.RawQuery)
		}
		if r.PathValue("node") == "two" {
			replyData(t, w, []map[string]any{})
			return
		}
		replyData(t, w, []map[string]any{
			{"storage": "local", "type": "dir", "active": 1, "avail": 900},
			{"storage": "nas", "type": "nfs", "shared": 1, "active": 1, "avail": 500},
			{"storage": "pbs", "type": "pbs", "shared": 1, "active": 1, "avail": 100},
		})
	})
	mux.HandleFunc("POST /api2/json/cluster/backup", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		added = append(added, r.PostForm)
		mu.Unlock()
		replyData(t, w, nil)
	})
	a, ctx := serveAPI(t, mux), context.Background()
	if storage, err := a.AddApplianceBackup(ctx, alice, 150); err != nil || storage != "" || len(added) != 0 {
		t.Errorf("a guest the all job includes: %q, %v, %v", storage, err, added)
	}
	if _, err := a.AddApplianceBackup(ctx, alice, 152); !errors.Is(err, ErrNoBackupStorage) {
		t.Errorf("a node without backup storage: %v", err)
	}
	storage, err := a.AddApplianceBackup(ctx, alice, 151)
	if err != nil || storage != "pbs" || len(added) != 1 {
		t.Fatalf("an excluded guest: %q, %v, %v", storage, err, added)
	}
	want := url.Values{
		"vmid": {"151"}, "storage": {"pbs"}, "schedule": {"02:30"}, "mode": {"snapshot"},
		"prune-backups": {"keep-daily=7,keep-weekly=4"}, "comment": {"Kubehenge appliance"}, "enabled": {"1"},
	}
	if added[0].Encode() != want.Encode() {
		t.Errorf("job = %s, want %s", added[0].Encode(), want.Encode())
	}
}
