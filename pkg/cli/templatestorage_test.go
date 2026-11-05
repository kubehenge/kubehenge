package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

func TestDefaultTemplateStorageIsTheSharedOneWithTheMostRoom(t *testing.T) {
	t.Parallel()
	list := []storageResource{
		{Storage: "local-lvm", Node: "pve1", Content: "images,rootdir", Status: "available", MaxDisk: 900, Disk: 0},
		{Storage: "ceph-small", Node: "pve1", Content: "images", Shared: 1, Status: "available", MaxDisk: 100, Disk: 10},
		{Storage: "ceph-big", Node: "pve1", Content: "images", Shared: 1, Status: "available", MaxDisk: 500, Disk: 100},
		{Storage: "nfs-iso", Node: "pve1", Content: "iso", Shared: 1, Status: "available", MaxDisk: 9000},
		{Storage: "ceph-down", Node: "pve1", Content: "images", Shared: 1, Status: "unknown", MaxDisk: 9000},
		{Storage: "ceph-elsewhere", Node: "pve2", Content: "images", Shared: 1, Status: "available", MaxDisk: 9000},
	}
	got, err := defaultTemplateStorage(list, []string{"pve1"})
	if err != nil || got != "ceph-big" {
		t.Errorf("defaultTemplateStorage = %q, %v; want ceph-big", got, err)
	}
	_, err = defaultTemplateStorage(list[:1], []string{"pve1"})
	if err == nil || !strings.Contains(err.Error(), "--storage") {
		t.Errorf("with only node-local storage: %v; want a request to name --storage", err)
	}
}

func TestResolveStorageSaysWhenTheStorageIsNotShared(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []storageResource{
			{Storage: "local-lvm", Node: "pve1", Content: "images", Status: "available"},
			{Storage: "ceph", Node: "pve1", Content: "images", Shared: 1, Status: "available", MaxDisk: 10},
		}})
	}))
	t.Cleanup(srv.Close)
	c := proxmox.NewClient(srv.URL + "/api2/json")

	chosen, note, err := resolveStorage(t.Context(), c, "local-lvm", []string{"pve1"})
	if err != nil || chosen != "local-lvm" || !strings.Contains(note, "storage local-lvm is not shared") || !strings.Contains(note, "only on node pve1") {
		t.Errorf("resolveStorage(local-lvm) = %q, %q, %v; want a note that clones start on pve1 only", chosen, note, err)
	}
	chosen, note, err = resolveStorage(t.Context(), c, "", []string{"pve1"})
	if err != nil || chosen != "ceph" || note != "" {
		t.Errorf("resolveStorage() = %q, %q, %v; want the shared ceph and no note", chosen, note, err)
	}
}

func TestSplitNodes(t *testing.T) {
	t.Parallel()
	if got := splitNodes(" pve1, pve2 ,,pve3"); strings.Join(got, "|") != "pve1|pve2|pve3" {
		t.Errorf("splitNodes = %q", got)
	}
	if got := splitNodes(" , "); len(got) != 0 {
		t.Errorf("splitNodes of separators = %q", got)
	}
}
