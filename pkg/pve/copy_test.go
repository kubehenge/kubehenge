package pve

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

var root = &Ticket{Username: "root@pam", Ticket: "PVE:root@pam:1", CSRF: "csrf"}

// fakeStorageAPI is Proxmox VE's storage content and task API, recording each copy as
// "path form", with the raw path as Proxmox VE's router reads it.
type fakeStorageAPI struct {
	mu     sync.Mutex
	copies []string
	status map[string]any
}

func (f *fakeStorageAPI) accounts(t *testing.T) *Accounts {
	t.Helper()
	reply := func(w http.ResponseWriter, data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "PVEAuthCookie="+root.Ticket {
			http.Error(w, "no ticket", http.StatusUnauthorized)
			return
		}
		path := strings.TrimPrefix(r.URL.EscapedPath(), "/api2/json")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/nodes/pve1/storage/local/content/"):
			if r.Header.Get("Csrfpreventiontoken") != root.CSRF {
				http.Error(w, "no CSRF token", http.StatusUnauthorized)
				return
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			f.copies = append(f.copies, path+" "+r.PostForm.Encode())
			reply(w, "UPID:pve1:0001:copy")
		case r.Method == http.MethodGet && path == "/nodes/pve1/storage/local/content":
			reply(w, []map[string]any{{"volid": "local:4242/vm-4242-disk-0.raw"}, {"volid": "local:iso/x.iso"}})
		case r.Method == http.MethodGet && path == "/nodes/pve1/tasks/UPID:pve1:0001:copy/status":
			reply(w, f.status)
		default:
			http.Error(w, "refused", http.StatusInternalServerError)
		}
	}))
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

func TestStartCopyKeepsTheVolumesSlashes(t *testing.T) {
	t.Parallel()
	f := &fakeStorageAPI{}
	a := f.accounts(t)
	upid, err := a.StartCopy(t.Context(), root, "pve1", "local", "4242/vm-4242-disk-0.raw", "vm-4242-snapshot-1.raw", "pve2")
	if err != nil || upid != "UPID:pve1:0001:copy" {
		t.Fatalf("StartCopy = %q, %v", upid, err)
	}
	want := []string{"/nodes/pve1/storage/local/content/4242/vm-4242-disk-0.raw target=vm-4242-snapshot-1.raw&target_node=pve2"}
	if !slices.Equal(f.copies, want) {
		t.Errorf("copies %q, want %q", f.copies, want)
	}
	if _, err := a.StartCopy(t.Context(), root, "pve1", "local", "vm-4242-disk-0", "vm-4242-snapshot-1", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.copies[1]; got != "/nodes/pve1/storage/local/content/vm-4242-disk-0 target=vm-4242-snapshot-1" {
		t.Errorf("a copy on the same node %q", got)
	}
}

func TestTaskExit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status map[string]any
		want   string
	}{
		"running":   {map[string]any{"status": "running"}, ""},
		"succeeded": {map[string]any{"status": "stopped", "exitstatus": "OK"}, "OK"},
		"failed":    {map[string]any{"status": "stopped", "exitstatus": "storage full"}, "storage full"},
		"no status": {map[string]any{"status": "stopped"}, "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &fakeStorageAPI{status: tc.status}
			got, err := f.accounts(t).TaskExit(t.Context(), root, "pve1", "UPID:pve1:0001:copy")
			if err != nil || got != tc.want {
				t.Errorf("TaskExit = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestStorageVolumes(t *testing.T) {
	t.Parallel()
	got, err := (&fakeStorageAPI{}).accounts(t).StorageVolumes(t.Context(), root, "pve1", "local")
	if want := []string{"local:4242/vm-4242-disk-0.raw", "local:iso/x.iso"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("StorageVolumes = %v, %v; want %v", got, err, want)
	}
	if _, err := (&fakeStorageAPI{}).accounts(t).StorageVolumes(t.Context(), &Ticket{Ticket: "other"}, "pve1", "local"); err == nil {
		t.Error("a refused ticket read as an empty storage")
	}
}
