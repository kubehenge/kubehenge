package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

// fakeHolders stands in for Proxmox VE with a cluster's pool of holder VMs, and the cluster's
// volume owner, VM 4242, which is never a holder.
type fakeHolders struct {
	mu sync.Mutex
	// vms are the pool's VMs by VMID: their node, name and configuration.
	vms map[int]*fakeVM
	// unlisted VMs exist but are not in the pool's listing yet.
	unlisted map[int]bool
	next     int
	// created are the VMs created, as "<vmid>@<node>"; deleted those destroyed, with their query.
	created, deleted []string
}

type fakeVM struct {
	node, name string
	config     map[string]any
}

func newFakeHolders(n Names) *fakeHolders {
	return &fakeHolders{
		vms:      map[int]*fakeVM{4242: {node: "pve1", name: n.VolumeOwner, config: map[string]any{"unused0": "local:vm-4242-disk-0"}}},
		unlisted: map[int]bool{},
		next:     200,
	}
}

func (f *fakeHolders) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
	path := strings.Split(strings.TrimPrefix(r.URL.Path, "/api2/json/"), "/")
	body := url.Values{}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		var fields map[string]any
		if json.Unmarshal(raw, &fields) == nil {
			for k, v := range fields {
				body.Set(k, fmt.Sprint(v))
			}
		}
	}
	switch {
	case path[0] == "pools":
		var members []map[string]any
		for vmid, vm := range f.vms {
			if !f.unlisted[vmid] {
				members = append(members, map[string]any{"type": "qemu", "vmid": vmid, "name": vm.name, "node": vm.node})
			}
		}
		reply([]any{map[string]any{"members": members}})
	case r.URL.Path == "/api2/json/cluster/nextid":
		reply(strconv.Itoa(f.next))
	case len(path) == 3 && path[2] == "qemu" && r.Method == http.MethodPost:
		vmid, _ := strconv.Atoi(body.Get("vmid"))
		f.vms[vmid] = &fakeVM{node: path[1], name: body.Get("name"), config: map[string]any{"name": body.Get("name")}}
		f.unlisted[vmid] = true
		f.created = append(f.created, fmt.Sprintf("%d@%s", vmid, path[1]))
		f.next++
		reply("UPID:create")
	case len(path) >= 4 && path[2] == "tasks":
		reply(map[string]any{"status": "stopped", "exitstatus": "OK"})
	case len(path) >= 4 && path[2] == "qemu":
		vmid, _ := strconv.Atoi(path[3])
		vm, ok := f.vms[vmid]
		if !ok || vm.node != path[1] {
			http.Error(w, fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", path[1], vmid), http.StatusInternalServerError)
			return
		}
		switch {
		case r.Method == http.MethodDelete && len(path) == 4:
			delete(f.vms, vmid)
			f.deleted = append(f.deleted, fmt.Sprintf("%d %s", vmid, r.URL.RawQuery))
			reply("UPID:destroy")
		case r.Method == http.MethodPut:
			for k := range body {
				vm.config[k] = body.Get(k)
			}
			reply(nil)
		default:
			reply(vm.config)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeHolders) holder(vmid int, node string, unused int, n Names) {
	config := map[string]any{"name": n.VolumeHolders()}
	for i := range unused {
		config["unused"+strconv.Itoa(i)] = fmt.Sprintf("local:vm-%d-disk-%d", vmid, i)
	}
	f.vms[vmid] = &fakeVM{node: node, name: n.VolumeHolders(), config: config}
}

func holdersProxmox(t *testing.T, f *fakeHolders) *Proxmox {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}
}

func TestEnsureVolumeHoldersGivesEachNodeRoom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeHolders(webNames)
	f.holder(100, "pve1", 3, webNames)
	f.holder(101, "pve2", holderFull, webNames)
	p := holdersProxmox(t, f)

	if err := p.EnsureVolumeHolders(ctx, webNames, []string{"pve1", "pve2", "pve3"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"200@pve2", "201@pve3"}; !slices.Equal(f.created, want) {
		t.Errorf("created %v, want a holder where none had room: %v", f.created, want)
	}
	for _, vmid := range []int{200, 201} {
		vm := f.vms[vmid]
		if vm.name != webNames.VolumeHolders() || vm.config["tags"] != holderTags || !strings.Contains(fmt.Sprint(vm.config["description"]), webNames.Pool) {
			t.Errorf("holder %d: %s %v", vmid, vm.name, vm.config)
		}
	}

	// Proxmox VE has not listed the new holders yet. They count all the same.
	if err := p.EnsureVolumeHolders(ctx, webNames, []string{"pve1", "pve2", "pve3"}); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 2 {
		t.Errorf("created %v; the holders not yet listed were made again", f.created)
	}
}

func TestRemoveEmptyVolumeHoldersLeavesThoseWithDisks(t *testing.T) {
	t.Parallel()
	f := newFakeHolders(webNames)
	f.holder(100, "pve1", 0, webNames)
	f.holder(101, "pve2", 1, webNames)
	f.holder(102, "pve2", 0, webNames)
	f.vms[102].config["scsi0"] = "local:vm-102-disk-0,size=1G"
	p := holdersProxmox(t, f)

	if err := p.RemoveEmptyVolumeHolders(context.Background(), webNames); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || !strings.HasPrefix(f.deleted[0], "100 ") {
		t.Errorf("deleted %v, want only the empty holder 100", f.deleted)
	}
	if _, ok := f.vms[4242]; !ok {
		t.Error("the volume owner went with the holders")
	}
}

func TestRemoveVolumeHoldersDestroysTheirCopies(t *testing.T) {
	t.Parallel()
	f := newFakeHolders(webNames)
	f.holder(100, "pve1", 2, webNames)
	f.holder(101, "pve2", 0, webNames)
	p := holdersProxmox(t, f)

	if err := p.removeVolumeHolders(context.Background(), webNames); err != nil {
		t.Fatal(err)
	}
	slices.Sort(f.deleted)
	want := []string{"100 destroy-unreferenced-disks=1&purge=1", "101 destroy-unreferenced-disks=1&purge=1"}
	if !slices.Equal(f.deleted, want) {
		t.Errorf("deleted %v, want %v", f.deleted, want)
	}
	if _, ok := f.vms[4242]; !ok {
		t.Error("the volume owner went with the holders")
	}
}

func TestHoldsDisks(t *testing.T) {
	t.Parallel()
	for config, want := range map[string]bool{
		`{"name":"x","tags":"a"}`:          false,
		`{"unused0":"local:vm-1-disk-0"}`:  true,
		`{"scsi12":"local:vm-1-disk-0"}`:   true,
		`{"efidisk0":"local:vm-1-disk-0"}`: true,
		`{"scsihw":"virtio-scsi-pci"}`:     false,
		`{"sata":"x"}`:                     false,
	} {
		var c map[string]any
		if err := json.Unmarshal([]byte(config), &c); err != nil {
			t.Fatal(err)
		}
		if got := holdsDisks(c); got != want {
			t.Errorf("%s: %t, want %t", config, got, want)
		}
	}
}
