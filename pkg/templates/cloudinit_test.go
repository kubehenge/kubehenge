package templates

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/luthermonson/go-proxmox"
)

const (
	uploadTask = "UPID:pve1:00001234:00005678:6700AB12:imgcopy::root@pam:"
	configTask = "UPID:pve1:00001235:00005679:6700AB13:qmconfig:101:root@pam:"
)

// seedNode is a Proxmox VE node whose upload task runs for a few polls, as on a busy node.
type seedNode struct {
	t        *testing.T
	mu       sync.Mutex
	uploaded []byte
	// parts are the upload form's part names in the order sent, which Proxmox VE's proxy holds
	// to: content, checksum-algorithm, checksum, then the file.
	parts    []string
	content  string
	filename string
	polls    int
	// attached is the drive the VM was given, and done whether the upload had ended by then.
	attached string
	done     bool
}

func (n *seedNode) serve(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var data any
	switch path := strings.TrimPrefix(r.URL.Path, "/api2/json"); path {
	case "/nodes/pve1/status", "/nodes/pve1/storage/local/status":
		data = map[string]any{}
	case "/nodes/pve1/qemu/101/status/current":
		data = map[string]any{"vmid": 101, "status": "stopped"}
	case "/nodes/pve1/qemu/101/config":
		if r.Method == http.MethodPost {
			var change map[string]string
			if err := json.NewDecoder(r.Body).Decode(&change); err != nil {
				n.t.Error(err)
			}
			n.attached, n.done = change["ide2"], n.polls >= 3
			data = configTask
			break
		}
		data = map[string]any{}
	case "/nodes/pve1/storage/local/upload":
		data = n.upload(r)
	// go-proxmox takes every field of a task from its status, as Proxmox VE sends them all.
	case "/nodes/pve1/tasks/" + uploadTask + "/status":
		n.polls++
		data = map[string]any{"upid": uploadTask, "node": "pve1", "status": "running"}
		if n.polls >= 3 {
			data = map[string]any{"upid": uploadTask, "node": "pve1", "status": "stopped", "exitstatus": "OK"}
		}
	case "/nodes/pve1/tasks/" + configTask + "/status":
		data = map[string]any{"upid": configTask, "node": "pve1", "status": "stopped", "exitstatus": "OK"}
	default:
		n.t.Errorf("unexpected %s %s", r.Method, path)
		http.NotFound(w, r)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		n.t.Error(err)
	}
}

func (n *seedNode) upload(r *http.Request) string {
	mr, err := r.MultipartReader()
	if err != nil {
		n.t.Error(err)
		return ""
	}
	n.parts = nil
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return uploadTask
		}
		if err != nil {
			n.t.Error(err)
			return ""
		}
		body, err := io.ReadAll(io.LimitReader(p, 1<<20))
		if err != nil {
			n.t.Error(err)
		}
		n.parts = append(n.parts, p.FormName())
		switch p.FormName() {
		case "filename":
			n.filename, n.uploaded = p.FileName(), body
		case "content":
			n.content = string(body)
		}
	}
}

func TestAttachSeedWaitsForTheUpload(t *testing.T) {
	t.Parallel()
	n := &seedNode{t: t}
	srv := httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(srv.Close)
	c := proxmox.NewClient(srv.URL + "/api2/json")
	node, err := c.Node(t.Context(), "pve1")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := node.VirtualMachine(t.Context(), 101)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"user-data": "#cloud-config\n", "meta-data": "instance-id: a\n", "network-config": "version: 2\n"}
	if err = attachSeed(t.Context(), node, "local", vm, "ide2", files); err != nil {
		t.Fatal(err)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.attached != "local:iso/user-data-101.iso,media=cdrom" || !n.done {
		t.Errorf("attached %q, upload ended first: %v", n.attached, n.done)
	}
	if n.filename != "user-data-101.iso" || n.content != "iso" || !slices.Equal(n.parts, []string{"content", "filename"}) {
		t.Errorf("uploaded %s as %q in parts %v", n.filename, n.content, n.parts)
	}
	iso := filepath.Join(t.TempDir(), "seed.iso")
	if err = os.WriteFile(iso, n.uploaded, 0o600); err != nil {
		t.Fatal(err)
	}
	disk, err := file.OpenFromPath(iso, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	fs, err := iso9660.Read(disk, 0, 0, isoBlockSize)
	if err != nil {
		t.Fatal(err)
	}
	// The volume descriptor's field is padded, which Label returns as it is.
	if strings.TrimRight(fs.Label(), "\x00 ") != seedLabel {
		t.Errorf("label %q", fs.Label())
	}
	for name, want := range files {
		f, err := fs.OpenFile("/"+name, os.O_RDONLY)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := io.ReadAll(f)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
}
