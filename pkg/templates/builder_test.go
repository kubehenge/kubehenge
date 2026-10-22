package templates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/luthermonson/go-proxmox"
)

const testPool = "kubehenge-templates"

// fakeCluster serves the parts of the Proxmox VE API the template lookups and Delete read. Its
// resource view answers with each of views in turn and then keeps to the last, the way Proxmox
// VE's lagging view can disagree from one request to the next; configs holds each VM's
// configuration by VM ID, which is always current, and a VM without one does not exist.
type fakeCluster struct {
	t       *testing.T
	mu      sync.Mutex
	views   [][]map[string]any
	configs map[int]map[string]any
	served  []time.Time // when each answer of the resource view was given
	deleted []int
}

var (
	qemuPath = regexp.MustCompile(`^/nodes/[^/]+/qemu/(\d+)(/config|/status/current)?$`)
	taskPath = regexp.MustCompile(`^/nodes/[^/]+/tasks/[^/]+/status$`)
)

func (f *fakeCluster) servedAt() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.served)
}

func (f *fakeCluster) deletedVMs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deleted)
}

func (f *fakeCluster) client() *proxmox.Client {
	f.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	return proxmox.NewClient(srv.URL + "/api2/json")
}

// memoryClient reaches the fake without a network, for tests run in a synctest bubble, where time
// moves only while every goroutine waits: a socket would hold that up.
func (f *fakeCluster) memoryClient() *proxmox.Client {
	return proxmox.NewClient("http://fake/api2/json", proxmox.WithHTTPClient(&http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		f.serve(w, r)
		return w.Result(), nil
	})}))
}

type roundTrip func(*http.Request) (*http.Response, error)

func (rt roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func (f *fakeCluster) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	var data any
	switch m := qemuPath.FindStringSubmatch(path); {
	case path == "/cluster/status":
		data = []any{}
	case path == "/cluster/resources":
		data = f.views[min(len(f.served), len(f.views)-1)]
		f.served = append(f.served, time.Now())
	case path == "/nodes/pve1/status":
		data = map[string]any{}
	case taskPath.MatchString(path):
		data = map[string]any{"status": "stopped", "exitstatus": "OK"}
	case m != nil:
		vmid, err := strconv.Atoi(m[1])
		if err != nil {
			f.t.Error(err)
		}
		c, ok := f.configs[vmid]
		if !ok {
			f.missing(w, vmid)
			return
		}
		switch {
		case r.Method == http.MethodDelete && m[2] == "":
			delete(f.configs, vmid)
			f.deleted = append(f.deleted, vmid)
			data = fmt.Sprintf("UPID:pve1:00001234:00005678:6700AB12:qmdestroy:%d:root@pam:", vmid)
		case m[2] == "/config":
			data = c
		case m[2] == "/status/current":
			data = map[string]any{"vmid": vmid, "name": c["name"], "status": "stopped"}
		default:
			f.t.Errorf("unexpected %s %s", r.Method, path)
		}
	default:
		f.t.Errorf("unexpected %s %s", r.Method, path)
		http.NotFound(w, r)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		f.t.Error(err)
	}
}

// missing answers as Proxmox VE does for a VM without a configuration: a 500 whose status line,
// not its body, says why.
func (f *fakeCluster) missing(w http.ResponseWriter, vmid int) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		f.t.Error(err)
		return
	}
	defer func() { _ = conn.Close() }()
	_, err = fmt.Fprintf(buf, "HTTP/1.1 500 Configuration file 'nodes/pve1/qemu-server/%d.conf' does not exist\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", vmid)
	if err == nil {
		err = buf.Flush()
	}
	if err != nil {
		f.t.Error(err)
	}
}

// clusterVMs serves a resource view that always shows vms, each with a configuration that agrees.
func clusterVMs(t *testing.T, vms ...map[string]any) *proxmox.Client {
	t.Helper()
	return (&fakeCluster{t: t, views: [][]map[string]any{vms}, configs: configsOf(vms...)}).client()
}

func configsOf(vms ...map[string]any) map[int]map[string]any {
	configs := map[int]map[string]any{}
	for _, vm := range vms {
		vmid, ok := vm["vmid"].(int)
		if !ok {
			panic(fmt.Sprintf("VM %v has no integer vmid", vm))
		}
		configs[vmid] = map[string]any{"digest": "0123abcd", "name": vm["name"], "tags": vm["tags"], "template": vm["template"]}
	}
	return configs
}

func template(vmid int, name, tags, pool string) map[string]any {
	vm := map[string]any{"vmid": vmid, "node": "pve1", "name": name, "tags": tags, "template": 1, "type": "qemu"}
	if pool != "" {
		vm["pool"] = pool
	}
	return vm
}

// shownAs is vm as a lagging resource view may show it, with another name and template flag.
func shownAs(vm map[string]any, name string, template int) map[string]any {
	out := maps.Clone(vm)
	out["name"] = name
	out["template"] = template
	return out
}

func rke2(t *testing.T) (Template, string) {
	t.Helper()
	tmpl, err := Plan(Request{Distribution: RKE2})
	if err != nil {
		t.Fatal(err)
	}
	return tmpl, strings.Join(tmpl.Tags, ";")
}

func TestFindLooksOnlyInTheBuildersPool(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	elsewhere := template(101, tmpl.Name, tags, "")
	inPool := template(201, tmpl.Name, tags, testPool)
	other := template(202, "kubehenge-talos", "kubehenge;talos;talos-v1.14.4", testPool)
	ctx := t.Context()

	b := &Builder{Client: clusterVMs(t, elsewhere, other), Pool: testPool}
	if _, err := b.Find(ctx, &tmpl); !errors.Is(err, ErrNoTemplate) {
		t.Errorf("with the template only outside the pool, Find gave %v, want ErrNoTemplate", err)
	}
	if list, listErr := b.List(ctx); listErr != nil || len(list) != 1 || list[0].VMID != 202 {
		t.Errorf("List gave %v, %v; want the pool's template 202 alone", list, listErr)
	}

	b = &Builder{Client: clusterVMs(t, elsewhere, inPool, other), Pool: testPool}
	found, err := b.Find(ctx, &tmpl)
	if err != nil || found.VMID != 201 {
		t.Errorf("Find gave %v, %v; want the pool's 201", found, err)
	}

	b = &Builder{Client: clusterVMs(t, elsewhere, inPool, other)}
	if _, err := b.Find(ctx, &tmpl); err == nil || !strings.Contains(err.Error(), "2 templates carry") {
		t.Errorf("without a pool, Find gave %v, want both templates refused as ambiguous", err)
	}
	if list, err := b.List(ctx); err != nil || len(list) != 3 {
		t.Errorf("without a pool, List gave %d templates, %v; want all 3", len(list), err)
	}
}

func TestInventoryNamesTheStoragesOfThePoolsTemplates(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	first := template(201, tmpl.Name, tags, testPool)
	second := template(202, "kubehenge-talos", "kubehenge;talos;talos-v1.14.4", testPool)
	elsewhere := template(101, tmpl.Name, tags, "")
	configs := configsOf(first, second, elsewhere)
	configs[201]["scsi0"] = "ceph:base-201-disk-0,discard=on,size=30G"
	configs[201]["ide2"] = "none,media=cdrom"
	configs[201]["efidisk0"] = "nvme:base-201-disk-1,efitype=4m,size=4M"
	configs[202]["virtio0"] = "ceph:base-202-disk-0,size=10G"
	configs[202]["ide0"] = "local-lvm:base-202-cloudinit,media=cdrom"
	configs[101]["scsi0"] = "elsewhere:base-101-disk-0,size=30G"
	vms := []map[string]any{first, second, elsewhere}
	b := &Builder{Client: (&fakeCluster{t: t, views: [][]map[string]any{vms}, configs: configs}).client(), Pool: testPool}

	inv, err := b.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Templates) != 2 || !slices.Equal(inv.Storages, []string{"ceph", "local-lvm", "nvme"}) {
		t.Errorf("Inventory gave %d templates on %v; want 2 on ceph, local-lvm and nvme", len(inv.Templates), inv.Storages)
	}
}

func TestDiskStorage(t *testing.T) {
	t.Parallel()
	for disk, want := range map[string]string{
		"ceph:base-201-disk-0,size=30G":     "ceph",
		"local:iso/debian.iso,media=cdrom":  "local",
		"none,media=cdrom":                  "",
		"/dev/disk/by-id/ata-disk,backup=0": "",
		"":                                  "",
	} {
		if got := diskStorage(disk); got != want {
			t.Errorf("diskStorage(%q) = %q, want %q", disk, got, want)
		}
	}
}

// The resource view shows a template converted a moment ago as no template at all, and under a
// name it no longer has; its configuration says what it is.
func TestLookupsTakeATemplateFromItsConfiguration(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	converted := template(101, tmpl.Name, tags, testPool)
	f := &fakeCluster{t: t, views: [][]map[string]any{{shownAs(converted, "", 0)}}, configs: configsOf(converted)}
	b := &Builder{Client: f.client(), Pool: testPool}
	ctx := t.Context()

	found, err := b.Find(ctx, &tmpl)
	if err != nil || found.VMID != 101 || found.Name != tmpl.Name || found.Template != 1 {
		t.Errorf("Find gave %v, %v; want template 101 named %s", found, err, tmpl.Name)
	}
	if list, listErr := b.List(ctx); listErr != nil || len(list) != 1 || list[0].Name != tmpl.Name {
		t.Errorf("List gave %v, %v; want template 101 named %s", list, listErr, tmpl.Name)
	}
	if err = b.checkNotInProgress(ctx, &tmpl); err != nil {
		t.Errorf("checkNotInProgress refused the finished template: %v", err)
	}
	id, err := b.Delete(ctx, &tmpl)
	if err != nil || id != 101 {
		t.Errorf("Delete gave %d, %v; want 101 removed", id, err)
	}
	if deleted := f.deletedVMs(); !slices.Equal(deleted, []int{101}) {
		t.Errorf("Delete removed VMs %v, want 101", deleted)
	}
}

// After a VM ID is reused, the view shows the new VM with the removed template's name and flag
// until the node's next status round.
func TestLookupsSeeThroughAReusedVMID(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	building := template(101, tmpl.Name, "kubehenge;kubehenge-building", testPool)
	building["template"] = 0
	converting := template(102, tmpl.Name, tags, testPool)
	converting["template"] = 0
	view := []map[string]any{shownAs(building, "kubehenge-talos-v1.14.4", 1), shownAs(converting, "kubehenge-talos-v1.14.3", 1)}
	b := &Builder{Client: (&fakeCluster{t: t, views: [][]map[string]any{view}, configs: configsOf(building, converting)}).client(), Pool: testPool}
	ctx := t.Context()

	if found, err := b.Find(ctx, &tmpl); !errors.Is(err, ErrNoTemplate) {
		t.Errorf("Find gave %v, %v; want ErrNoTemplate for VMs not yet templates", found, err)
	}
	if list, err := b.List(ctx); err != nil || len(list) != 0 {
		t.Errorf("List gave %v, %v; want nothing", list, err)
	}
	if err := b.checkNotInProgress(ctx, &tmpl); err == nil || !strings.Contains(err.Error(), "already named") {
		t.Errorf("checkNotInProgress gave %v; want the build in progress refused", err)
	}
}

// A VM removed between listing and reading its configuration is no template and no build.
func TestLookupsSkipAVMRemovedSinceTheListing(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	gone := template(101, tmpl.Name, tags, testPool)
	other := template(202, "kubehenge-talos", "kubehenge;talos;talos-v1.14.4", testPool)
	view := []map[string]any{gone, shownAs(gone, tmpl.Name, 0), other}
	view[1]["vmid"] = 102
	b := &Builder{Client: (&fakeCluster{t: t, views: [][]map[string]any{view}, configs: configsOf(other)}).client(), Pool: testPool}
	ctx := t.Context()

	if found, err := b.Find(ctx, &tmpl); !errors.Is(err, ErrNoTemplate) {
		t.Errorf("Find gave %v, %v; want ErrNoTemplate", found, err)
	}
	if list, err := b.List(ctx); err != nil || len(list) != 1 || list[0].VMID != 202 {
		t.Errorf("List gave %v, %v; want 202 alone", list, err)
	}
	if err := b.checkNotInProgress(ctx, &tmpl); err != nil {
		t.Errorf("checkNotInProgress refused over removed VMs: %v", err)
	}
	if id, err := b.Delete(ctx, &tmpl); err != nil || id != 0 {
		t.Errorf("Delete gave %d, %v; want nothing to remove", id, err)
	}
}

// Successive answers of the view showed a new template, then not, then again. Each Create must
// find the one template rather than build a second.
func TestCreateFindsATemplateTheViewFlaps(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	converted := template(101, tmpl.Name, tags, testPool)
	views := [][]map[string]any{{converted}, {shownAs(converted, tmpl.Name, 0)}, {converted}}
	// Sources that can reach nothing, so a Create that went on to build fails rather than
	// downloads.
	b := &Builder{
		Client:  (&fakeCluster{t: t, views: views, configs: configsOf(converted)}).client(),
		Sources: &Sources{HTTP: &http.Client{}},
		Pool:    testPool,
	}
	for range views {
		if id, err := b.Create(t.Context(), &tmpl); err != nil || id != 101 {
			t.Fatalf("Create gave %d, %v; want the existing template 101", id, err)
		}
	}
}

func TestWaitSelectableWaitsForTheViewToSettle(t *testing.T) {
	t.Parallel()
	tmpl, tags := rke2(t)
	converted := template(101, tmpl.Name, tags, testPool)
	stale := shownAs(converted, tmpl.Name, 0)
	// Long against the poll, so that only a view showing the template throughout can last it. Both
	// run on synctest's clock: on the real one, a test machine stalling for the settle window
	// between two answers would rightly count as a view that showed the template throughout.
	const settle = 200 * time.Millisecond
	newBuilder := func(f *fakeCluster) *Builder {
		return &Builder{Client: f.memoryClient(), Pool: testPool, selectableSettle: settle, selectablePoll: time.Millisecond}
	}

	t.Run("drops out once", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			f := &fakeCluster{t: t, views: [][]map[string]any{{stale}, {converted}, {stale}, {converted}}, configs: configsOf(converted)}
			if err := newBuilder(f).waitSelectable(t.Context(), &tmpl, 101); err != nil {
				t.Fatal(err)
			}
			const lastDrop = 2
			served := f.servedAt()
			if len(served) <= lastDrop+1 {
				t.Fatalf("waitSelectable read the view %d times, returning before it showed the template again", len(served))
			}
			if shown := time.Since(served[lastDrop+1]); shown < settle {
				t.Errorf("waitSelectable returned %s after the template showed again, before the %s settle window", shown, settle)
			}
		})
	})

	t.Run("keeps flapping", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			// More answers than the polls that fit in the context's time, each third without the
			// template.
			views := make([][]map[string]any, 2000)
			for i := range views {
				views[i] = []map[string]any{converted}
				if i%3 == 2 {
					views[i] = []map[string]any{stale}
				}
			}
			f := &fakeCluster{t: t, views: views, configs: configsOf(converted)}
			ctx, cancel := context.WithTimeout(t.Context(), 3*settle)
			defer cancel()
			if err := newBuilder(f).waitSelectable(ctx, &tmpl, 101); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("waitSelectable gave %v over a flapping view; want it still waiting", err)
			}
		})
	})

	t.Run("a second template", func(t *testing.T) {
		t.Parallel()
		second := template(102, tmpl.Name, tags, testPool)
		f := &fakeCluster{t: t, views: [][]map[string]any{{converted, second}}, configs: configsOf(converted, second)}
		if err := newBuilder(f).waitSelectable(t.Context(), &tmpl, 101); err == nil || !strings.Contains(err.Error(), "2 templates carry") {
			t.Errorf("waitSelectable gave %v; want the two templates refused", err)
		}
	})
}
