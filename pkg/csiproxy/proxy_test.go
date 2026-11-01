package csiproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	token   = "PVEAPIToken=kubehenge.team-a.web@pve!cluster=s3cret" //nolint:gosec // test data
	pool    = "kubehenge/team-a.web"
	holders = "web-e148d768-copies"
	owner   = 4242
)

// fakePVE is the Proxmox VE API as a cluster's token sees it: the cluster's volume owner VM 4242,
// its holder VMs 300 on pve1 and 301 on pve2, a node VM 101, and VM 500 of another pool. Storage
// local-zfs is a block storage, local a directory. Both are each node's own. It copies volumes only
// by import-from into a VM's disk, and refuses the storage's own copy and delete, as it does a
// token without root@pam or Datastore.Allocate.
type fakePVE struct {
	mu sync.Mutex
	// volumes are those on each node's storage, "<node>/<storage>".
	volumes map[string][]string
	configs map[int]map[string]string
	nodes   map[int]string
	// tasks are the import tasks by UPID, and whether each has ended.
	tasks map[string]bool
	// hold keeps import tasks running until released. failImport has them fail.
	hold       chan struct{}
	failImport bool
	// calls are those that reached it, as "METHOD path?params".
	calls []string
	// auth are the Authorization headers the calls carried.
	auth []string
}

func newFakePVE() *fakePVE {
	return &fakePVE{
		volumes: map[string][]string{
			"pve1/local-zfs": {"local-zfs:vm-4242-pvc-1", "local-zfs:vm-101-disk-0", "local-zfs:vm-500-disk-0"},
			"pve1/local":     {"local:4242/vm-4242-pvc-3.raw"},
			"pve2/local-zfs": {"local-zfs:vm-4242-pvc-2"},
		},
		configs: map[int]map[string]string{
			owner: {"name": "web-e148d768-volumes"},
			300:   {"name": holders},
			301:   {"name": holders},
			302:   {"name": holders},
			101:   {"name": "web-cp-1", "scsi0": "local-zfs:vm-101-disk-0,size=10G"},
			500:   {"name": "other", "scsi0": "local-zfs:vm-500-disk-0,size=10G"},
		},
		nodes: map[int]string{owner: "pve1", 300: "pve1", 301: "pve2", 302: "pve1", 101: "pve1", 500: "pve1"},
		tasks: map[string]bool{},
	}
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	call := r.Method + " " + path
	if len(r.Form) > 0 {
		call += "?" + r.Form.Encode()
	}
	f.calls = append(f.calls, call)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	seg := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case path == "/storage":
		writeData(w, []map[string]any{{"storage": "local", "type": "dir"}, {"storage": "local-zfs", "type": "zfspool", "sparse": 1}})
	case path == "/cluster/resources":
		// Proxmox VE names a VM's pool only to Pool.Audit on it, which the cluster's token lacks.
		// VM 302 is in a pool whose Pool.Audit it holds.
		list := make([]map[string]any, 0, len(f.nodes))
		for vmid, node := range f.nodes {
			vm := map[string]any{"vmid": vmid, "node": node, "name": f.configs[vmid]["name"], "type": "qemu"}
			if vmid == 302 {
				vm["pool"] = "other"
			}
			list = append(list, vm)
		}
		writeData(w, list)
	case path == "/cluster/nextid":
		writeData(w, "102")
	case len(seg) == 5 && seg[2] == "storage" && seg[4] == "status":
		writeData(w, map[string]any{"total": 100, "used": 10, "avail": 90})
	case len(seg) == 5 && seg[2] == "storage" && seg[4] == "content":
		f.content(w, r, seg[1], seg[3])
	case len(seg) >= 6 && seg[2] == "storage" && seg[4] == "content":
		volume := strings.Join(seg[5:], "/")
		if r.Method == http.MethodGet {
			for _, v := range f.volumes[seg[1]+"/"+seg[3]] {
				if v == volume || v == seg[3]+":"+volume {
					writeData(w, map[string]any{"volid": v, "size": 1 << 30})
					return
				}
			}
			writeErrors(w, http.StatusInternalServerError, map[string]string{"volume": "no such volume"})
			return
		}
		writeErrors(w, http.StatusForbidden, map[string]string{"permission": "Permission check failed (Datastore.Allocate)"})
	case len(seg) == 5 && seg[2] == "tasks" && seg[4] == "status":
		if ended, ok := f.tasks[seg[3]]; ok && !ended {
			writeData(w, map[string]any{"status": "running"})
			return
		}
		exit := "OK"
		if f.failImport {
			exit = "import failed"
		}
		writeData(w, map[string]any{"status": "stopped", "exitstatus": exit})
	case len(seg) >= 5 && seg[2] == "qemu":
		vmid, _ := strconv.Atoi(seg[3])
		f.vm(w, r, seg[1], vmid, seg[4])
	default:
		writeErrors(w, http.StatusNotImplemented, map[string]string{"fake": "not modelled: " + call})
	}
}

func (f *fakePVE) content(w http.ResponseWriter, r *http.Request, node, storage string) {
	key := node + "/" + storage
	switch r.Method {
	case http.MethodPost:
		name := r.Form.Get("filename")
		if storage == "local" {
			name = r.Form.Get("vmid") + "/" + name
		}
		f.volumes[key] = append(f.volumes[key], storage+":"+name)
		writeData(w, storage+":"+name)
	default:
		list := make([]map[string]any, 0, len(f.volumes[key]))
		for _, v := range f.volumes[key] {
			list = append(list, map[string]any{"volid": v, "size": 1 << 30, "format": "raw"})
		}
		writeData(w, list)
	}
}

func (f *fakePVE) vm(w http.ResponseWriter, r *http.Request, node string, vmid int, what string) {
	cfg, ok := f.configs[vmid]
	if !ok || f.nodes[vmid] != node {
		writeErrors(w, http.StatusInternalServerError, map[string]string{"vm": fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", node, vmid)})
		return
	}
	switch {
	case what == "config" && r.Method == http.MethodGet, what == "pending":
		out := map[string]any{}
		for k, v := range cfg {
			out[k] = v
		}
		writeData(w, out)
	case what == "config" && r.Method == http.MethodPost && strings.Contains(r.Form.Encode(), "import-from"):
		f.importInto(w, r, node, vmid)
	case what == "config":
		for key := range strings.SplitSeq(r.Form.Get("delete"), ",") {
			if key != "" {
				f.deleteKey(vmid, node, key)
			}
		}
		for k := range r.Form {
			if k != "delete" {
				cfg[k] = r.Form.Get(k)
			}
		}
		writeData(w, nil)
	case what == "unlink":
		delete(cfg, r.Form.Get("idlist"))
		writeData(w, nil)
	default:
		writeData(w, nil)
	}
}

// importInto makes the new volume vm-<vmid>-disk-<k> on the source's storage, attached at the
// form's slot, as Proxmox VE does.
func (f *fakePVE) importInto(w http.ResponseWriter, r *http.Request, node string, vmid int) {
	for slot := range r.Form {
		spec := r.Form.Get(slot)
		_, rest, _ := strings.Cut(spec, "import-from=")
		source, _, _ := strings.Cut(rest, ",")
		storage, _, _ := strings.Cut(source, ":")
		key := node + "/" + storage
		if !slices.Contains(f.volumes[key], source) {
			writeErrors(w, http.StatusInternalServerError, map[string]string{"import-from": "no such volume " + source})
			return
		}
		pattern := "%[1]s:vm-%[2]d-disk-%[3]d"
		if storage == "local" {
			pattern = "%[1]s:%[2]d/vm-%[2]d-disk-%[3]d.raw"
		}
		var name string
		for k := 0; ; k++ {
			if name = fmt.Sprintf(pattern, storage, vmid, k); !slices.Contains(f.volumes[key], name) {
				break
			}
		}
		f.volumes[key] = append(f.volumes[key], name)
		f.configs[vmid][slot] = name + ",size=1G"
		upid := fmt.Sprintf("UPID:%s:import:%d", node, len(f.tasks))
		f.tasks[upid] = f.hold == nil
		if f.hold != nil {
			go func() {
				<-f.hold
				f.mu.Lock()
				f.tasks[upid] = true
				f.mu.Unlock()
			}()
		}
		writeData(w, upid)
		return
	}
}

// deleteKey deletes a disk of a VM: an attached one becomes unused, as the VM owns it, and an
// unused one is deleted with its volume.
func (f *fakePVE) deleteKey(vmid int, node, key string) {
	cfg := f.configs[vmid]
	volid, _, _ := strings.Cut(cfg[key], ",")
	delete(cfg, key)
	if !strings.HasPrefix(key, "unused") {
		for i := 0; ; i++ {
			if _, used := cfg["unused"+strconv.Itoa(i)]; !used {
				cfg["unused"+strconv.Itoa(i)] = volid
				return
			}
		}
	}
	storage, _, _ := strings.Cut(volid, ":")
	f.volumes[node+"/"+storage] = slices.DeleteFunc(f.volumes[node+"/"+storage], func(v string) bool { return v == volid })
}

func (f *fakePVE) seen(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakePVE) has(key, volid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.volumes[key], volid)
}

func (f *fakePVE) configOf(vmid int) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.configs[vmid])
}

// lockedBuffer collects the proxy's log, written from the copies' goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	srv  *httptest.Server
	pve  *fakePVE
	kube client.Client
	log  *lockedBuffer
	p    *Proxy
}

func newHarness(t *testing.T, pve *fakePVE, objs ...client.Object) *harness {
	t.Helper()
	upstream := httptest.NewServer(pve)
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithObjects(objs...).Build()
	log := &lockedBuffer{}
	p := &Proxy{
		Upstream: u, Transport: http.DefaultTransport, Client: kube, Log: slog.New(slog.NewTextHandler(log, nil)),
		Owner: owner, Pool: pool, Holders: holders, Copies: true, Poll: 5 * time.Millisecond,
		Pod: types.NamespacedName{Namespace: "kube-system", Name: "proxmox-csi-plugin-controller-0"},
	}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return &harness{srv: srv, pve: pve, kube: kube, log: log, p: p}
}

// answer is a Proxmox VE answer as the proxy gives it.
type answer struct {
	Data   json.RawMessage   `json:"data"`
	Errors map[string]string `json:"errors"`
}

// call calls the proxy as the plugin's client does: parameters in the query, whatever the method.
func (h *harness) call(t *testing.T, method, path string, params url.Values) (int, answer) {
	t.Helper()
	u := h.srv.URL + "/api2/json" + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(t.Context(), method, u, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", token)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out answer
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s %s answered %d %s", method, path, resp.StatusCode, body)
	}
	return resp.StatusCode, out
}

// copyVolume asks for a copy as the plugin does.
func (h *harness) copyVolume(t *testing.T, node, storage, source, target string, extra ...string) (int, answer) {
	t.Helper()
	params := url.Values{"target": {target}}
	for i := 0; i+1 < len(extra); i += 2 {
		params.Set(extra[i], extra[i+1])
	}
	return h.call(t, http.MethodPost, "/nodes/"+node+"/storage/"+storage+"/content/"+source, params)
}

func (h *harness) records(t *testing.T) map[string]Copy {
	t.Helper()
	list := &corev1.ConfigMapList{}
	if err := h.kube.List(context.Background(), list, client.InNamespace(Namespace)); err != nil {
		t.Fatal(err)
	}
	out := map[string]Copy{}
	for i := range list.Items {
		c, err := parseCopy(&list.Items[i])
		if err != nil {
			t.Fatal(err)
		}
		out[c.PluginID()] = c
	}
	return out
}

func (h *harness) events(t *testing.T) []corev1.Event {
	t.Helper()
	list := &corev1.EventList{}
	if err := h.kube.List(context.Background(), list, client.InNamespace("kube-system")); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// doneRecord is the record of a copy made before.
func doneRecord(storage, volume, copyID string, holder int) *corev1.ConfigMap {
	c := Copy{
		Name: recordName(storage, volume), Storage: storage, Volume: volume, Source: storage + ":vm-4242-pvc-1",
		Node: "pve1", Holder: holder, Slot: "scsi0", State: stateDone, Real: copyID,
	}
	return c.configMap()
}

func TestCopyIsMadeWithTheClustersToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, newFakePVE())

	status, a := h.copyVolume(t, "pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1")
	if status != http.StatusOK || string(a.Data) != "null" {
		t.Fatalf("copy answered %d %s %v", status, a.Data, a.Errors)
	}
	imports := h.pve.seen("POST /nodes/pve1/qemu/300/config")
	if len(imports) != 1 || !strings.Contains(imports[0], url.Values{"scsi0": {"local-zfs:0,import-from=local-zfs:vm-4242-pvc-1,format=raw"}}.Encode()) {
		t.Errorf("imports %v", imports)
	}
	cfg := h.pve.configOf(300)
	if cfg["unused0"] != "local-zfs:vm-300-disk-0" || cfg["scsi0"] != "" {
		t.Errorf("holder 300 is %v, want the copy as its unused disk", cfg)
	}
	rec := h.records(t)["local-zfs:vm-4242-snap-1"]
	if rec.State != stateDone || rec.Real != "local-zfs:vm-300-disk-0" || rec.Holder != 300 || rec.Source != "local-zfs:vm-4242-pvc-1" {
		t.Errorf("record %+v", rec)
	}
	for i, auth := range h.pve.auth {
		if auth != token {
			t.Errorf("call %s went with %q, not the plugin's token", h.pve.calls[i], auth)
		}
	}
	if !strings.Contains(h.log.String(), "copied a volume") {
		t.Errorf("no log of the copy:\n%s", h.log)
	}

	// The plugin finds its copy under the name it asked for.
	_, list := h.call(t, http.MethodGet, "/nodes/pve1/storage/local-zfs/content", nil)
	if !strings.Contains(string(list.Data), `"local-zfs:vm-4242-snap-1"`) || strings.Contains(string(list.Data), "vm-300-disk-0") {
		t.Errorf("content %s", list.Data)
	}

	// A retried call finds the copy made and makes no second one.
	if status, _ := h.copyVolume(t, "pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1"); status != http.StatusOK {
		t.Errorf("retried copy answered %d", status)
	}
	if n := len(h.pve.seen("POST /nodes/pve1/qemu/300/config")); n != 1 {
		t.Errorf("%d imports, want the first alone", n)
	}
}

func TestCopyOfACopyOnADirectoryStorage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, newFakePVE())
	if status, a := h.copyVolume(t, "pve1", "local", "4242/vm-4242-pvc-3.raw", "4242/vm-4242-snap-3.raw"); status != http.StatusOK {
		t.Fatalf("copy answered %d %v", status, a.Errors)
	}
	// Restoring the snapshot copies the copy, by its real name.
	if status, a := h.copyVolume(t, "pve1", "local", "4242/vm-4242-snap-3.raw", "4242/vm-4242-pvc-4.raw"); status != http.StatusOK {
		t.Fatalf("restore answered %d %v", status, a.Errors)
	}
	imports := h.pve.seen("POST /nodes/pve1/qemu/300/config")
	if len(imports) != 2 || !strings.Contains(imports[1], url.QueryEscape("import-from=local:300/vm-300-disk-0.raw")) {
		t.Errorf("imports %v", imports)
	}
	recs := h.records(t)
	if recs["local:4242/vm-4242-pvc-4.raw"].Real != "local:300/vm-300-disk-1.raw" {
		t.Errorf("records %+v", recs)
	}
}

func TestCopiesOfOtherVolumesAreRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		node, storage, source, target string
		extra                         []string
		copiesOff                     bool
		want                          string
	}{
		"another VM's volume":        {"pve1", "local-zfs", "vm-500-disk-0", "vm-4242-snap-1", nil, false, "is not one of the cluster's own"},
		"a namesake in another pool": {"pve1", "local-zfs", "vm-302-disk-0", "vm-4242-snap-1", nil, false, "is not one of the cluster's own"},
		"a node VM's disk":           {"pve1", "local-zfs", "vm-101-disk-0", "vm-4242-snap-1", nil, false, "is not one of the cluster's own"},
		"a target of another":        {"pve1", "local-zfs", "vm-4242-pvc-1", "vm-500-disk-9", nil, false, "is not named after the cluster's volume owner VM 4242"},
		"a holder's name":            {"pve1", "local-zfs", "vm-4242-pvc-1", "vm-300-disk-9", nil, false, "is not named after"},
		"another node":               {"pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1", []string{"target_node", "pve2"}, false, "stays on its source's node"},
		"a target there":             {"pve1", "local-zfs", "vm-4242-pvc-1", "vm-101-disk-0", nil, false, "is not named after"},
		"copies switched off":        {"pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1", nil, true, "switched off"},
	} {
		h := newHarness(t, newFakePVE())
		h.p.Copies = !tc.copiesOff
		status, a := h.copyVolume(t, tc.node, tc.storage, tc.source, tc.target, tc.extra...)
		if status != http.StatusForbidden || !strings.Contains(a.Errors["copy"], tc.want) {
			t.Errorf("%s: answered %d %v, want %q", name, status, a.Errors, tc.want)
		}
		if n := len(h.pve.seen("POST /nodes/pve1/qemu/")); n != 0 {
			t.Errorf("%s: %d imports", name, n)
		}
		if recs := h.records(t); len(recs) != 0 {
			t.Errorf("%s: records %v", name, recs)
		}
		events := h.events(t)
		if len(events) != 1 || events[0].Reason != "VolumeCopyFailed" || events[0].InvolvedObject.Name != "proxmox-csi-plugin-controller-0" ||
			!strings.Contains(events[0].Message, tc.want) {
			t.Errorf("%s: events %+v", name, events)
		}
	}
}

func TestCopyOntoAnExistingVolumeIsRefused(t *testing.T) {
	t.Parallel()
	pve := newFakePVE()
	pve.volumes["pve1/local-zfs"] = append(pve.volumes["pve1/local-zfs"], "local-zfs:vm-4242-snap-1")
	h := newHarness(t, pve)
	if status, a := h.copyVolume(t, "pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1"); status != http.StatusForbidden || !strings.Contains(a.Errors["copy"], "is there already") {
		t.Errorf("answered %d %v", status, a.Errors)
	}
}

func TestCopyWaitsForAHolderWithRoom(t *testing.T) {
	t.Parallel()
	pve := newFakePVE()
	for i := range maxUnused - 1 {
		pve.configs[300]["unused"+strconv.Itoa(i)] = fmt.Sprintf("local-zfs:vm-300-disk-%d", i)
	}
	h := newHarness(t, pve)
	status, a := h.copyVolume(t, "pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1")
	if status != http.StatusInternalServerError || !strings.Contains(a.Errors["copy"], "no holder VM on node pve1 has room") {
		t.Errorf("answered %d %v", status, a.Errors)
	}
	if recs := h.records(t); len(recs) != 0 {
		t.Errorf("records %v", recs)
	}
}

func TestFailedImportLeavesNoRecord(t *testing.T) {
	t.Parallel()
	pve := newFakePVE()
	pve.failImport = true
	h := newHarness(t, pve)
	if status, a := h.copyVolume(t, "pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1"); status != http.StatusInternalServerError || !strings.Contains(a.Errors["copy"], "import failed") {
		t.Errorf("answered %d %v", status, a.Errors)
	}
	if recs := h.records(t); len(recs) != 0 {
		t.Errorf("records %v", recs)
	}
	if events := h.events(t); len(events) != 1 || events[0].Reason != "VolumeCopyFailed" {
		t.Errorf("events %+v", events)
	}
}

// A proxy that stopped half-way finds its copy again from the record it wrote first, and never
// imports the volume twice.
func TestCopyRecoversFromARestart(t *testing.T) {
	t.Parallel()
	intent := func(task string) *corev1.ConfigMap {
		c := Copy{
			Name: recordName("local-zfs", "vm-4242-snap-1"), Storage: "local-zfs", Volume: "vm-4242-snap-1", Source: "local-zfs:vm-4242-pvc-1",
			Node: "pve1", Holder: 300, Slot: "scsi0", State: stateCopying, Task: task,
		}
		return c.configMap()
	}
	for name, tc := range map[string]struct {
		record *corev1.ConfigMap
		// imported has the import happened before the restart.
		imported bool
		imports  int
	}{
		"before the import":                   {intent(""), false, 1},
		"after the import, before its task":   {intent(""), true, 0},
		"after the import's task was written": {intent("UPID:pve1:import:0"), true, 0},
	} {
		pve := newFakePVE()
		if tc.imported {
			pve.volumes["pve1/local-zfs"] = append(pve.volumes["pve1/local-zfs"], "local-zfs:vm-300-disk-0")
			pve.configs[300]["scsi0"] = "local-zfs:vm-300-disk-0,size=1G"
		}
		h := newHarness(t, pve, tc.record)
		if status, a := h.copyVolume(t, "pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1"); status != http.StatusOK {
			t.Errorf("%s: answered %d %v", name, status, a.Errors)
			continue
		}
		if n := len(pve.seen("POST /nodes/pve1/qemu/300/config")); n != tc.imports {
			t.Errorf("%s: %d imports, want %d", name, n, tc.imports)
		}
		if rec := h.records(t)["local-zfs:vm-4242-snap-1"]; rec.State != stateDone || rec.Real != "local-zfs:vm-300-disk-0" {
			t.Errorf("%s: record %+v", name, rec)
		}
		if cfg := pve.configOf(300); cfg["unused0"] != "local-zfs:vm-300-disk-0" || len(cfg) != 2 {
			t.Errorf("%s: holder %v", name, cfg)
		}
	}
}

// A copy under way is not there for the plugin until it is done: its volume has a name of Proxmox
// VE's choosing until then, and a second call for it waits on the first.
func TestCopyUnderWayIsHidden(t *testing.T) {
	t.Parallel()
	pve := newFakePVE()
	pve.hold = make(chan struct{})
	h := newHarness(t, pve)
	first := make(chan int)
	go func() {
		status, _ := h.copyVolume(t, "pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1")
		first <- status
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(pve.seen("POST /nodes/pve1/qemu/300/config")) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	_, list := h.call(t, http.MethodGet, "/nodes/pve1/storage/local-zfs/content", nil)
	if strings.Contains(string(list.Data), "vm-4242-snap-1") {
		t.Errorf("the copy under way is listed: %s", list.Data)
	}
	if status, _ := h.call(t, http.MethodDelete, "/nodes/pve1/storage/local-zfs/content/vm-4242-snap-1", nil); status != http.StatusConflict {
		t.Errorf("deleting the copy under way answered %d", status)
	}
	second := make(chan int)
	go func() {
		status, _ := h.copyVolume(t, "pve1", "local-zfs", "vm-4242-pvc-1", "vm-4242-snap-1")
		second <- status
	}()
	close(pve.hold)
	if a, b := <-first, <-second; a != http.StatusOK || b != http.StatusOK {
		t.Errorf("answered %d and %d", a, b)
	}
	if n := len(pve.seen("POST /nodes/pve1/qemu/300/config")); n != 1 {
		t.Errorf("%d imports", n)
	}
}

func TestCopiesOntoOneHolderGoOneAtATime(t *testing.T) {
	t.Parallel()
	h := newHarness(t, newFakePVE())
	var wg sync.WaitGroup
	statuses := make([]int, 4)
	for i := range statuses {
		wg.Go(func() {
			statuses[i], _ = h.copyVolume(t, "pve1", "local-zfs", "vm-4242-pvc-1", fmt.Sprintf("vm-4242-snap-%d", i))
		})
	}
	wg.Wait()
	reals := map[string]bool{}
	for i, s := range statuses {
		if s != http.StatusOK {
			t.Errorf("copy %d answered %d", i, s)
		}
		reals[h.records(t)[fmt.Sprintf("local-zfs:vm-4242-snap-%d", i)].Real] = true
	}
	if len(reals) != len(statuses) {
		t.Errorf("copies %v", reals)
	}
	if cfg := h.pve.configOf(300); len(cfg) != 1+len(statuses) {
		t.Errorf("holder %v", cfg)
	}
}

func TestDeletingACopyDeletesItThroughItsHolder(t *testing.T) {
	t.Parallel()
	pve := newFakePVE()
	pve.volumes["pve1/local-zfs"] = append(pve.volumes["pve1/local-zfs"], "local-zfs:vm-300-disk-0", "local-zfs:vm-300-disk-1")
	pve.configs[300]["unused0"] = "local-zfs:vm-300-disk-0"
	pve.configs[300]["unused1"] = "local-zfs:vm-300-disk-1"
	h := newHarness(t, pve,
		doneRecord("local-zfs", "vm-4242-snap-1", "local-zfs:vm-300-disk-0", 300),
		doneRecord("local-zfs", "vm-4242-snap-10", "local-zfs:vm-300-disk-1", 300))

	if status, a := h.call(t, http.MethodDelete, "/nodes/pve1/storage/local-zfs/content/vm-4242-snap-1", nil); status != http.StatusOK {
		t.Fatalf("answered %d %v", status, a.Errors)
	}
	if pve.has("pve1/local-zfs", "local-zfs:vm-300-disk-0") || !pve.has("pve1/local-zfs", "local-zfs:vm-300-disk-1") {
		t.Errorf("volumes %v", pve.volumes["pve1/local-zfs"])
	}
	if cfg := pve.configOf(300); cfg["unused0"] != "" || cfg["unused1"] == "" {
		t.Errorf("holder %v", cfg)
	}
	recs := h.records(t)
	if _, ok := recs["local-zfs:vm-4242-snap-1"]; ok || len(recs) != 1 {
		t.Errorf("records %v", recs)
	}
	if !strings.Contains(h.log.String(), "deleted a volume copy") {
		t.Errorf("no log of the deletion:\n%s", h.log)
	}

	// A volume that is no copy goes on to Proxmox VE, which refuses it to the token.
	if status, _ := h.call(t, http.MethodDelete, "/nodes/pve1/storage/local-zfs/content/vm-4242-pvc-1", nil); status != http.StatusForbidden {
		t.Errorf("deleting a volume that is no copy answered %d", status)
	}
	if got := pve.seen("DELETE "); len(got) != 1 || got[0] != "DELETE /nodes/pve1/storage/local-zfs/content/vm-4242-pvc-1" {
		t.Errorf("deletions that reached Proxmox VE %v", got)
	}
}

// A record that names a volume of no holder of the cluster's translates nothing and deletes
// nothing, whoever wrote it.
func TestForgedRecordIsIgnored(t *testing.T) {
	t.Parallel()
	pve := newFakePVE()
	pve.configs[500]["unused0"] = "local-zfs:vm-500-disk-0"
	h := newHarness(t, pve, doneRecord("local-zfs", "vm-4242-snap-1", "local-zfs:vm-500-disk-0", 500))

	if status, _ := h.call(t, http.MethodDelete, "/nodes/pve1/storage/local-zfs/content/vm-4242-snap-1", nil); status != http.StatusForbidden {
		t.Errorf("deleting through a forged record answered %d", status)
	}
	if !pve.has("pve1/local-zfs", "local-zfs:vm-500-disk-0") {
		t.Error("another pool's volume was deleted")
	}
	h.call(t, http.MethodPost, "/nodes/pve1/qemu/101/config", url.Values{"scsi1": {"local-zfs:vm-4242-snap-1,backup=0"}})
	if got := pve.configOf(101)["scsi1"]; got != "local-zfs:vm-4242-snap-1,backup=0" {
		t.Errorf("a forged record turned the plugin's volume into %q", got)
	}
}

func TestStorageConfigurationIsReadFromTheList(t *testing.T) {
	t.Parallel()
	h := newHarness(t, newFakePVE())
	status, a := h.call(t, http.MethodGet, "/storage/local-zfs", nil)
	if status != http.StatusOK || !strings.Contains(string(a.Data), `"zfspool"`) {
		t.Errorf("answered %d %s", status, a.Data)
	}
	if got := h.pve.seen("GET /storage"); len(got) != 1 || got[0] != "GET /storage" {
		t.Errorf("calls %v", got)
	}
}

// Every call the CSI plugin makes, with the copies' names it may carry: the plugin's names go to
// Proxmox VE as the real ones and come back as the plugin's, and calls that name no copy go on as
// they came. Creating a volume posts to the storage's content without a volume, which is no copy.
func TestEveryCallOfThePluginTranslatesTheCopies(t *testing.T) {
	t.Parallel()
	const (
		blockCopy = "local-zfs:vm-4242-snap-1"
		blockReal = "local-zfs:vm-300-disk-0"
		fileCopy  = "local:4242/vm-4242-snap-2.raw"
		fileReal  = "local:300/vm-300-disk-1.raw"
	)
	newMapped := func() *harness {
		pve := newFakePVE()
		pve.volumes["pve1/local-zfs"] = append(pve.volumes["pve1/local-zfs"], blockReal, "local-zfs:vm-300-disk-01")
		pve.volumes["pve1/local"] = append(pve.volumes["pve1/local"], fileReal)
		pve.configs[300]["unused0"] = blockReal
		pve.configs[300]["unused1"] = fileReal
		pve.configs[101]["scsi1"] = blockReal + ",backup=0,iothread=1,size=1G,wwn=0x5056432d49443031"
		return newHarness(t, pve,
			doneRecord("local-zfs", "vm-4242-snap-1", blockReal, 300),
			doneRecord("local", "4242/vm-4242-snap-2.raw", fileReal, 300))
	}
	for _, tc := range []struct {
		name, method, path string
		params             url.Values
		// upstream is the call Proxmox VE must get. answer is what the plugin's answer holds, and
		// hidden what it must not.
		upstream, answer, hidden string
	}{
		{name: "storages", method: "GET", path: "/storage", upstream: "GET /storage", answer: "local-zfs"},
		{name: "resources", method: "GET", path: "/cluster/resources", params: url.Values{"type": {"vm"}}, upstream: "GET /cluster/resources?type=vm", answer: holders},
		{name: "storage status", method: "GET", path: "/nodes/pve1/storage/local-zfs/status", upstream: "GET /nodes/pve1/storage/local-zfs/status", answer: "avail"},
		{name: "block content", method: "GET", path: "/nodes/pve1/storage/local-zfs/content", upstream: "GET /nodes/pve1/storage/local-zfs/content", answer: `"` + blockCopy + `"`, hidden: `"` + blockReal + `"`},
		{name: "file content", method: "GET", path: "/nodes/pve1/storage/local/content", upstream: "GET /nodes/pve1/storage/local/content", answer: fileCopy, hidden: fileReal},
		{name: "a copy's entry", method: "GET", path: "/nodes/pve1/storage/local-zfs/content/vm-4242-snap-1", upstream: "GET /nodes/pve1/storage/local-zfs/content/vm-300-disk-0", answer: blockCopy, hidden: blockReal},
		{name: "a file copy's entry", method: "GET", path: "/nodes/pve1/storage/local/content/" + fileCopy, upstream: "GET /nodes/pve1/storage/local/content/" + fileReal, answer: fileCopy, hidden: fileReal},
		{
			name: "creating a volume", method: "POST", path: "/nodes/pve1/storage/local-zfs/content", params: url.Values{"filename": {"vm-4242-pvc-9"}, "size": {"1G"}, "vmid": {"4242"}},
			upstream: "POST /nodes/pve1/storage/local-zfs/content?filename=vm-4242-pvc-9&size=1G&vmid=4242", answer: "local-zfs:vm-4242-pvc-9",
		},
		{name: "a node VM's configuration", method: "GET", path: "/nodes/pve1/qemu/101/config", upstream: "GET /nodes/pve1/qemu/101/config", answer: blockCopy + ",backup=0", hidden: blockReal},
		{name: "its pending configuration", method: "GET", path: "/nodes/pve1/qemu/101/pending", upstream: "GET /nodes/pve1/qemu/101/pending", answer: blockCopy, hidden: blockReal},
		{
			name: "attaching a copy", method: "POST", path: "/nodes/pve1/qemu/101/config", params: url.Values{"scsi2": {blockCopy + ",backup=0,iothread=1,wwn=0x5056432d49443032"}},
			upstream: "POST /nodes/pve1/qemu/101/config?" + url.Values{"scsi2": {blockReal + ",backup=0,iothread=1,wwn=0x5056432d49443032"}}.Encode(),
		},
		{
			name: "attaching a file copy", method: "POST", path: "/nodes/pve1/qemu/101/config", params: url.Values{"scsi3": {fileCopy + ",backup=0"}},
			upstream: "POST /nodes/pve1/qemu/101/config?" + url.Values{"scsi3": {fileReal + ",backup=0"}}.Encode(),
		},
		{
			name: "attaching a volume that only starts like a copy", method: "POST", path: "/nodes/pve1/qemu/101/config", params: url.Values{"scsi4": {"local-zfs:vm-4242-snap-10,backup=0"}},
			upstream: "POST /nodes/pve1/qemu/101/config?" + url.Values{"scsi4": {"local-zfs:vm-4242-snap-10,backup=0"}}.Encode(),
		},
		{name: "detaching", method: "PUT", path: "/nodes/pve1/qemu/101/unlink", params: url.Values{"idlist": {"scsi1"}}, upstream: "PUT /nodes/pve1/qemu/101/unlink?idlist=scsi1"},
		{name: "resizing", method: "PUT", path: "/nodes/pve1/qemu/101/resize", params: url.Values{"disk": {"scsi1"}, "size": {"2G"}}, upstream: "PUT /nodes/pve1/qemu/101/resize?disk=scsi1&size=2G"},
		{name: "a task", method: "GET", path: "/nodes/pve1/tasks/UPID:pve1:x/status", upstream: "GET /nodes/pve1/tasks/UPID:pve1:x/status", answer: "stopped"},
		{name: "the next VMID", method: "GET", path: "/cluster/nextid", params: url.Values{"vmid": {"102"}}, upstream: "GET /cluster/nextid?vmid=102", answer: "102"},
		{name: "a VM's status", method: "GET", path: "/nodes/pve1/qemu/101/status/current", upstream: "GET /nodes/pve1/qemu/101/status/current"},
		{name: "deleting a volume that is no copy", method: "DELETE", path: "/nodes/pve1/storage/local-zfs/content/vm-4242-pvc-1", upstream: "DELETE /nodes/pve1/storage/local-zfs/content/vm-4242-pvc-1"},
	} {
		h := newMapped()
		h.call(t, tc.method, tc.path, tc.params)
		got := h.pve.seen(tc.method + " ")
		if !slices.Contains(got, tc.upstream) {
			t.Errorf("%s: Proxmox VE got %v, want %q", tc.name, got, tc.upstream)
		}
		for _, c := range h.pve.calls {
			if strings.HasPrefix(c, "POST /nodes/pve1/storage/") && strings.Contains(c, "/content/") {
				t.Errorf("%s: a copy reached Proxmox VE: %s", tc.name, c)
			}
		}
		if tc.answer == "" && tc.hidden == "" {
			continue
		}
		_, a := h.call(t, tc.method, tc.path, tc.params)
		if raw := string(a.Data); !strings.Contains(raw, tc.answer) || tc.hidden != "" && strings.Contains(raw, tc.hidden) {
			t.Errorf("%s: answered %s, want %q and not %q", tc.name, raw, tc.answer, tc.hidden)
		}
	}
}

// A form body, as other clients than the plugin's send, is translated as the query is.
func TestFormBodiesAreTranslated(t *testing.T) {
	t.Parallel()
	pve := newFakePVE()
	pve.configs[300]["unused0"] = "local-zfs:vm-300-disk-0"
	h := newHarness(t, pve, doneRecord("local-zfs", "vm-4242-snap-1", "local-zfs:vm-300-disk-0", 300))
	body := url.Values{"scsi1": {"local-zfs:vm-4242-snap-1,backup=0"}}.Encode()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/api2/json/nodes/pve1/qemu/101/config", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := pve.configOf(101)["scsi1"]; got != "local-zfs:vm-300-disk-0,backup=0" {
		t.Errorf("attached %q", got)
	}
}

func TestReplaceTakesOnlyWholeVolumeIDs(t *testing.T) {
	t.Parallel()
	m := map[string]string{
		"local-zfs:vm-101-disk-1":       "local-zfs:vm-300-disk-0",
		"local-zfs:vm-101-disk-10":      "local-zfs:vm-300-disk-7",
		"local:101/vm-101-disk-1.raw":   "local:300/vm-300-disk-1.raw",
		"local:101/vm-101-disk-1.qcow2": "local:300/vm-300-disk-2.qcow2",
	}
	for in, want := range map[string]string{
		"local-zfs:vm-101-disk-1":                 "local-zfs:vm-300-disk-0",
		"local-zfs:vm-101-disk-10":                "local-zfs:vm-300-disk-7",
		"local-zfs:vm-101-disk-100":               "local-zfs:vm-101-disk-100",
		"local-zfs:vm-101-disk-1,size=1G":         "local-zfs:vm-300-disk-0,size=1G",
		"other-local-zfs:vm-101-disk-1":           "other-local-zfs:vm-101-disk-1",
		"xlocal-zfs:vm-101-disk-1":                "xlocal-zfs:vm-101-disk-1",
		"local:101/vm-101-disk-1.raw":             "local:300/vm-300-disk-1.raw",
		"local:101/vm-101-disk-1.raw2":            "local:101/vm-101-disk-1.raw2",
		"local:101/vm-101-disk-1.qcow2,backup=0":  "local:300/vm-300-disk-2.qcow2,backup=0",
		"a local-zfs:vm-101-disk-1 b":             "a local-zfs:vm-300-disk-0 b",
		"local-zfs:vm-101-disk-1;local-zfs:other": "local-zfs:vm-300-disk-0;local-zfs:other",
		"no volume at all":                        "no volume at all",
	} {
		if got := replace(in, m); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
