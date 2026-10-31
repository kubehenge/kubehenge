package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/luthermonson/go-proxmox"
)

// The CPU flags of a node that reaches x86-64-v2 and of one that reaches x86-64-v3.
const (
	v2Flags = "fpu cx16 lahf_lm popcnt sse4_1 sse4_2 ssse3"
	v3Flags = v2Flags + " avx avx2 bmi1 bmi2 f16c fma abm movbe xsave"
)

// guestResult is how a command run through the guest agent ends.
type guestResult struct {
	code int
	out  string
}

// bakeHost is one Proxmox VE node, pve1, in memory, as far as making a template takes it: its
// storages and their volumes, its VMs with their state and guest agents, and the tasks it runs. It
// writes every call that changes something into changes, in the order made, as a short step.
type bakeHost struct {
	t  *testing.T
	mu sync.Mutex

	vms     map[int]map[string]any // configuration by VM ID
	running map[int]bool
	volumes []string // the volume IDs of every storage
	nextID  int
	flags   string

	// failing is the type of the task that fails, such as resize or qmtemplate.
	failing string
	// agentDown keeps the guest agent from ever answering; keepsRunning ignores a shutdown.
	agentDown    bool
	keepsRunning bool
	// noISO leaves the node without a storage for ISO images.
	noISO bool
	// exit is how each guest command ends, by its words joined with spaces; others exit 0.
	exit map[string]guestResult

	changes []string
	uploads [][]byte // each cloud-init ISO uploaded, in turn
	pids    []string // the command of each guest process, by pid - 1
}

func newBakeHost(t *testing.T) *bakeHost {
	t.Helper()
	return &bakeHost{t: t, vms: map[int]map[string]any{}, running: map[int]bool{}, nextID: 101, flags: v2Flags, exit: map[string]guestResult{}}
}

var (
	vmPath      = regexp.MustCompile(`^/nodes/pve1/qemu/(\d+)(/.*)?$`)
	storagePath = regexp.MustCompile(`^/nodes/pve1/storage/([^/]+)(/.*)?$`)
	taskStatus  = regexp.MustCompile(`^/nodes/pve1/tasks/([^/]+)/status$`)
	taskLog     = regexp.MustCompile(`^/nodes/pve1/tasks/([^/]+)/log$`)
)

// client reaches the host without a network, so that the tests can run in a synctest bubble.
func (h *bakeHost) client() *proxmox.Client {
	return proxmox.NewClient("http://pve1.test/api2/json", proxmox.WithHTTPClient(&http.Client{Transport: roundTrip(h.roundTrip)}))
}

// roundTrip answers as Proxmox VE does: the data under "data", and a refusal as a 500 whose status
// line says why, which is all go-proxmox keeps of it.
func (h *bakeHost) roundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	data, refusal := h.answer(r)
	h.mu.Unlock()
	resp := &http.Response{Request: r, Header: http.Header{}, ProtoMajor: 1, ProtoMinor: 1, StatusCode: http.StatusOK, Status: "200 OK"}
	if refusal != "" {
		resp.StatusCode, resp.Status = http.StatusInternalServerError, "500 "+refusal
		resp.Body = io.NopCloser(strings.NewReader(""))
		return resp, nil
	}
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return nil, fmt.Errorf("encoding the answer: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func (h *bakeHost) answer(r *http.Request) (any, string) {
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	switch m, s, task, log := vmPath.FindStringSubmatch(path), storagePath.FindStringSubmatch(path), taskStatus.FindStringSubmatch(path), taskLog.FindStringSubmatch(path); {
	case path == "/cluster/status":
		return []any{}, ""
	case path == "/cluster/nextid":
		id := h.nextID
		h.nextID++
		return strconv.Itoa(id), ""
	case path == "/cluster/resources":
		return h.resources(), ""
	case path == "/nodes/pve1/status":
		return map[string]any{"cpuinfo": map[string]any{"flags": h.flags}}, ""
	case path == "/nodes/pve1/storage":
		content := "iso,import"
		if h.noISO {
			content = "import"
		}
		return []any{map[string]any{"storage": "local", "content": content, "enabled": 1}}, ""
	case path == "/nodes/pve1/qemu" && r.Method == http.MethodPost:
		return h.create(r), ""
	case task != nil:
		return h.taskStatus(task[1]), ""
	case log != nil:
		return []any{map[string]any{"n": 1, "t": fmt.Sprint("TASK ERROR: ", h.taskStatus(log[1])["exitstatus"])}}, ""
	case m != nil:
		vmid, err := strconv.Atoi(m[1])
		if err != nil {
			h.t.Error(err)
		}
		return h.vm(r, vmid, m[2])
	case s != nil:
		return h.storage(r, s[1], s[2])
	}
	h.t.Errorf("unexpected %s %s", r.Method, path)
	return nil, "not implemented"
}

func (h *bakeHost) resources() []map[string]any {
	out := make([]map[string]any, 0, len(h.vms))
	for _, vmid := range slices.Sorted(maps.Keys(h.vms)) {
		c := h.vms[vmid]
		vm := map[string]any{"vmid": vmid, "node": "pve1", "name": c["name"], "tags": c["tags"], "type": "qemu", "template": 0}
		if c["template"] == 1 {
			vm["template"] = 1
		}
		if pool, ok := c["pool"]; ok {
			vm["pool"] = pool
		}
		out = append(out, vm)
	}
	return out
}

func (h *bakeHost) create(r *http.Request) string {
	params := h.body(r)
	id, ok := params["vmid"].(float64)
	if !ok {
		h.t.Errorf("a VM created without a VM ID: %v", params)
	}
	vmid := int(id)
	// Proxmox VE gives a new network device a MAC address of its own.
	if net, ok := params["net0"].(string); ok {
		model, rest, _ := strings.Cut(net, ",")
		params["net0"] = model + "=BC:24:11:0A:0B:0C," + rest
	}
	delete(params, "vmid")
	h.vms[vmid] = params
	h.changes = append(h.changes, fmt.Sprintf("create %d", vmid))
	return upid("qmcreate", vmid)
}

func (h *bakeHost) vm(r *http.Request, vmid int, rest string) (any, string) {
	c, ok := h.vms[vmid]
	if !ok {
		return nil, fmt.Sprintf("Configuration file 'nodes/pve1/qemu-server/%d.conf' does not exist", vmid)
	}
	step := func(change, task string) (any, string) {
		h.changes = append(h.changes, change)
		return upid(task, vmid), ""
	}
	switch {
	case strings.HasPrefix(rest, "/agent/"):
		return h.agent(r, vmid, rest)
	case strings.HasPrefix(rest, "/status/"):
		return h.power(r, vmid, rest, step)
	case r.Method == http.MethodGet && rest == "/config":
		return c, ""
	case r.Method == http.MethodPost && rest == "/config":
		change := h.body(r)
		if deleted, ok := change["delete"].(string); ok {
			for key := range strings.SplitSeq(deleted, ",") {
				delete(c, key)
			}
		}
		words := make([]string, 0, len(change))
		for _, key := range slices.Sorted(maps.Keys(change)) {
			words = append(words, fmt.Sprintf("%s=%v", key, change[key]))
			if key != "delete" {
				c[key] = change[key]
			}
		}
		return step("config "+strings.Join(words, " "), "qmconfig")
	case r.Method == http.MethodPut && rest == "/resize":
		change := h.body(r)
		return step(fmt.Sprintf("resize %v %v", change["disk"], change["size"]), "resize")
	case r.Method == http.MethodPut && rest == "/unlink":
		change := h.body(r)
		delete(c, fmt.Sprint(change["idlist"]))
		return step(fmt.Sprintf("unlink %v", change["idlist"]), "unlink")
	case r.Method == http.MethodPost && rest == "/template":
		if h.failing != "qmtemplate" {
			c["template"] = 1
		}
		return step("template", "qmtemplate")
	case r.Method == http.MethodDelete && rest == "":
		delete(h.vms, vmid)
		delete(h.running, vmid)
		return step(fmt.Sprintf("delete %d", vmid), "qmdestroy")
	}
	h.t.Errorf("unexpected %s on VM %d%s", r.Method, vmid, rest)
	return nil, "not implemented"
}

// power reads and changes whether a VM runs.
func (h *bakeHost) power(r *http.Request, vmid int, rest string, step func(change, task string) (any, string)) (any, string) {
	switch {
	case r.Method == http.MethodGet && rest == "/status/current":
		status := "stopped"
		if h.running[vmid] {
			status = "running"
		}
		return map[string]any{"vmid": vmid, "name": h.vms[vmid]["name"], "status": status}, ""
	case r.Method == http.MethodPost && rest == "/status/start":
		h.running[vmid] = true
		return step("start", "qmstart")
	case r.Method == http.MethodPost && rest == "/status/shutdown":
		h.running[vmid] = h.running[vmid] && h.keepsRunning
		return step("shutdown", "qmshutdown")
	case r.Method == http.MethodPost && rest == "/status/stop":
		h.running[vmid] = false
		return step("stop", "qmstop")
	}
	h.t.Errorf("unexpected %s on VM %d%s", r.Method, vmid, rest)
	return nil, "not implemented"
}

// agent answers for a VM's guest agent, which answers only while the VM runs.
func (h *bakeHost) agent(r *http.Request, vmid int, rest string) (any, string) {
	switch rest {
	case "/agent/get-osinfo":
		if !h.running[vmid] || h.agentDown {
			return nil, "QEMU guest agent is not running"
		}
		return map[string]any{"result": map[string]any{"id": "almalinux"}}, ""
	case "/agent/exec":
		var command struct {
			Command []string `json:"command"`
		}
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			h.t.Error(err)
		}
		words := strings.Join(command.Command, " ")
		h.pids = append(h.pids, words)
		h.changes = append(h.changes, "run "+words)
		return map[string]any{"pid": len(h.pids)}, ""
	case "/agent/exec-status":
		pid, err := strconv.Atoi(r.URL.Query().Get("pid"))
		if err != nil || pid < 1 || pid > len(h.pids) {
			h.t.Errorf("the status of pid %q", r.URL.Query().Get("pid"))
			return nil, "no such process"
		}
		result := h.exit[h.pids[pid-1]]
		return map[string]any{"exited": 1, "exitcode": result.code, "out-data": result.out}, ""
	}
	h.t.Errorf("unexpected %s on VM %d%s", r.Method, vmid, rest)
	return nil, "not implemented"
}

func (h *bakeHost) storage(r *http.Request, name, rest string) (any, string) {
	switch {
	case rest == "/status":
		return map[string]any{"storage": name}, ""
	case rest == "/content" && r.Method == http.MethodGet:
		var content []map[string]any
		for _, v := range h.volumes {
			if strings.HasPrefix(v, name+":") {
				content = append(content, map[string]any{"volid": v})
			}
		}
		return content, ""
	case strings.HasPrefix(rest, "/content/") && r.Method == http.MethodDelete:
		volume := strings.TrimPrefix(rest, "/content/")
		h.volumes = slices.DeleteFunc(h.volumes, func(v string) bool { return v == volume })
		h.changes = append(h.changes, "remove "+volume)
		return upid("imgdel", 0), ""
	case rest == "/download-url":
		params := h.body(r)
		volume := fmt.Sprintf("%s:%v/%v", name, params["content"], params["filename"])
		h.volumes = append(h.volumes, volume)
		step := "download " + volume
		if sum, ok := params["checksum"]; ok {
			step += fmt.Sprintf(" %v:%v", params["checksum-algorithm"], sum)
		}
		h.changes = append(h.changes, step)
		return upid("download", 0), ""
	case rest == "/upload":
		return h.upload(r, name), ""
	}
	h.t.Errorf("unexpected %s on storage %s%s", r.Method, name, rest)
	return nil, "not implemented"
}

func (h *bakeHost) upload(r *http.Request, storage string) string {
	mr, err := r.MultipartReader()
	if err != nil {
		h.t.Error(err)
		return ""
	}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return upid("imgcopy", 0)
		}
		if err != nil {
			h.t.Error(err)
			return ""
		}
		body, err := io.ReadAll(p)
		if err != nil {
			h.t.Error(err)
		}
		if p.FormName() == "filename" {
			volume := storage + ":iso/" + p.FileName()
			h.volumes = append(h.volumes, volume)
			h.uploads = append(h.uploads, body)
			h.changes = append(h.changes, "upload "+volume)
		}
	}
}

func upid(task string, vmid int) string {
	return fmt.Sprintf("UPID:pve1:00001234:00005678:6700AB12:%s:%d:root@pam:", task, vmid)
}

// taskStatus ends every task at once, the failing type's with Proxmox VE's own word for it.
func (h *bakeHost) taskStatus(upid string) map[string]any {
	parts := strings.Split(upid, ":")
	if len(parts) > 5 && parts[5] == h.failing {
		return map[string]any{"upid": upid, "node": "pve1", "status": "stopped", "exitstatus": parts[5] + " went wrong"}
	}
	return map[string]any{"upid": upid, "node": "pve1", "status": "stopped", "exitstatus": "OK"}
}

func (h *bakeHost) body(r *http.Request) map[string]any {
	params := map[string]any{}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		h.t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
	}
	return params
}

func (h *bakeHost) steps() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.changes)
}

// forget clears the steps so far, so that a test reads only those of the step it drives.
func (h *bakeHost) forget() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes = nil
}

// imageRecipe is a bake in image mode that reboots into what it installed.
func imageRecipe() *Recipe {
	return &Recipe{
		UserData:     "#cloud-config\n# bake\n",
		Checks:       [][]string{{cloudInit, "status", "--wait"}},
		Reboot:       true,
		RebootChecks: [][]string{{"test", "-e", "/run/rebooted"}},
		Image:        &NodeImageBoot{UserData: "#cloud-config\n# node\n", Checks: [][]string{{"getenforce"}}},
		Cleanup:      [][]string{{cloudInit, "clean", "--logs"}},
		DiskGiB:      8, BuilderDiskGiB: 12,
		Log: "/var/log/kubehenge-bake.log",
	}
}

// bake is a build's VM made as Create makes it, ready for the rest of the bake.
type bake struct {
	b      *Builder
	node   *proxmox.Node
	vm     *proxmox.VirtualMachine
	tmpl   Template
	recipe *Recipe
}

func (k *bake) finish(ctx context.Context) error {
	return k.b.finish(ctx, k.node, k.vm, 101, &k.tmpl, k.recipe)
}

// bakeVM creates the VM of an RKE2 build as Create does.
func bakeVM(ctx context.Context, t *testing.T, h *bakeHost, recipe *Recipe) *bake {
	t.Helper()
	line, _ := rke2(t)
	tmpl, err := line.Build(time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	b := &Builder{
		Client: h.client(), Node: "pve1", Storage: "local-lvm", Pool: testPool, Bridge: "vmbr0",
		selectableSettle: time.Nanosecond, selectablePoll: time.Millisecond,
	}
	node, err := b.Client.Node(ctx, "pve1")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.createVM(ctx, node, 101, &tmpl, "local:import/image.qcow2", DefaultCPUType, recipe); err != nil {
		t.Fatal(err)
	}
	vm, err := node.VirtualMachine(ctx, 101)
	if err != nil {
		t.Fatal(err)
	}
	h.forget()
	return &bake{b: b, node: node, vm: vm, tmpl: tmpl, recipe: recipe}
}

// seedFile reads one file of a cloud-init ISO the host was sent.
func seedFile(t *testing.T, iso []byte, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.iso")
	if err := os.WriteFile(path, iso, 0o600); err != nil {
		t.Fatal(err)
	}
	disk, err := file.OpenFromPath(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	fs, err := iso9660.Read(disk, 0, 0, isoBlockSize)
	if err != nil {
		t.Fatal(err)
	}
	f, err := fs.OpenFile("/"+name, os.O_RDONLY)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFinishBakesAnImageModeBuildStepByStep(t *testing.T) {
	t.Parallel()
	h := newBakeHost(t)
	ctx := t.Context()
	recipe := imageRecipe()
	k := bakeVM(ctx, t, h, recipe)

	if err := k.finish(ctx); err != nil {
		t.Fatal(err)
	}
	const seed = "local:iso/user-data-101.iso"
	attach := "config ide2=" + seed + ",media=cdrom"
	want := []string{
		"config tags=" + kubehengeTag + ";" + buildingTag,
		// The builder's disk grows, as the image boots from it.
		"resize scsi1 12G",
		"upload " + seed, attach, "start",
		"run true", "run cloud-init status --wait",
		"shutdown", "start",
		"run true", "run test -e /run/rebooted",
		// The node image boots from the template's own disk, the builder's gone for good.
		"shutdown", "unlink scsi1", "config boot=order=scsi0 delete=ide2", "remove " + seed,
		"upload " + seed, attach, "start",
		"run true", "run getenforce",
		"run cloud-init clean --logs",
		"shutdown", "config delete=ide2,net0",
		"remove " + seed,
		"config boot=order=scsi0 cores=1 memory=512 tags=" + strings.Join(k.tmpl.Tags, ";"),
		"template",
	}
	if got := h.steps(); !slices.Equal(got, want) {
		t.Errorf("steps\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.uploads) != 2 {
		t.Fatalf("%d seeds uploaded, want the bake's and the node image's", len(h.uploads))
	}
	for i, stage := range []string{"bake", "node"} {
		if meta := seedFile(t, h.uploads[i], "meta-data"); !strings.Contains(meta, "instance-id: kubehenge-"+stage+"-101\n") {
			t.Errorf("seed %d's meta-data %q is not the %s boot's", i, meta, stage)
		}
		if network := seedFile(t, h.uploads[i], "network-config"); !strings.Contains(network, `macaddress: "bc:24:11:0a:0b:0c"`) {
			t.Errorf("seed %d's network-config %q does not match the VM's MAC address", i, network)
		}
	}
	if user := seedFile(t, h.uploads[1], "user-data"); user != recipe.Image.UserData {
		t.Errorf("the node image booted with user data %q", user)
	}
	if c := h.vms[101]; c["template"] != 1 || c["net0"] != nil || c["ide2"] != nil || c["scsi1"] != nil || h.running[101] {
		t.Errorf("the template is left as %v, running %v", c, h.running[101])
	}
	if len(h.volumes) != 0 {
		t.Errorf("volumes left behind: %v", h.volumes)
	}
}

func TestAFailedCheckReportsCloudInitAndTheBakeLog(t *testing.T) {
	t.Parallel()
	h := newBakeHost(t)
	recipe := imageRecipe()
	h.exit[strings.Join(recipe.Checks[0], " ")] = guestResult{code: 1, out: "status: error"}
	h.exit["cloud-init status --long"] = guestResult{code: 1, out: "detail: package install failed"}
	h.exit[strings.Join([]string{"sh", "-c", bakeAccount, "sh", recipe.Log}, " ")] = guestResult{out: "12:00:01 dnf install rke2-selinux"}
	ctx := t.Context()
	k := bakeVM(ctx, t, h, recipe)

	err := k.finish(ctx)
	for _, want := range []string{`"cloud-init status --wait" exited 1: status: error`, "detail: package install failed", recipe.Log + ", the first line of each second", "dnf install rke2-selinux"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("finish gave %v; want it to carry %q", err, want)
		}
	}
	steps := h.steps()
	if slices.Contains(steps, "template") || slices.Contains(steps, "run getenforce") {
		t.Errorf("the bake went on after its check failed: %v", steps)
	}
	if last := steps[len(steps)-1]; last != "remove local:iso/user-data-101.iso" {
		t.Errorf("the last step was %q, not the seed's removal", last)
	}
}

func TestABakeWaitsForTheGuestAgentNoLongerThanItsTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newBakeHost(t)
		h.agentDown = true
		ctx := t.Context()
		recipe := imageRecipe()
		k := bakeVM(ctx, t, h, recipe)
		start := time.Now()

		err := k.finish(ctx)
		if !errors.Is(err, proxmox.ErrTimeout) || !strings.Contains(err.Error(), "the guest agent did not start") {
			t.Fatalf("finish gave %v; want the guest agent's timeout", err)
		}
		if waited := time.Since(start); waited < bakeTimeout {
			t.Errorf("gave up on the guest agent after %s, before the bake's %s", waited, bakeTimeout)
		}
		steps := h.steps()
		if slices.ContainsFunc(steps, func(s string) bool { return strings.HasPrefix(s, "run ") }) || slices.Contains(steps, "template") {
			t.Errorf("ran something without a guest agent: %v", steps)
		}
		if last := steps[len(steps)-1]; last != "remove local:iso/user-data-101.iso" {
			t.Errorf("the last step was %q, not the seed's removal", last)
		}
	})
}

func TestAFailedTaskStopsTheBakeBeforeTheVMStarts(t *testing.T) {
	t.Parallel()
	h := newBakeHost(t)
	h.failing = "resize"
	ctx := t.Context()
	recipe := imageRecipe()
	k := bakeVM(ctx, t, h, recipe)

	err := k.finish(ctx)
	if err == nil || !strings.Contains(err.Error(), "growing the disk") || !strings.Contains(err.Error(), "failed: resize went wrong") {
		t.Errorf("finish gave %v; want the resize task's failure", err)
	}
	if got, want := h.steps(), []string{"config tags=" + kubehengeTag + ";" + buildingTag, "resize scsi1 12G"}; !slices.Equal(got, want) {
		t.Errorf("steps %v, want %v", got, want)
	}
}

func TestARebootThatLeavesTheVMRunningFails(t *testing.T) {
	t.Parallel()
	h := newBakeHost(t)
	h.keepsRunning = true
	ctx := t.Context()
	recipe := imageRecipe()
	k := bakeVM(ctx, t, h, recipe)

	err := k.finish(ctx)
	if err == nil || !strings.Contains(err.Error(), "VM 101 did not stop after shutdown") {
		t.Fatalf("finish gave %v; want the VM that kept running refused", err)
	}
	starts := 0
	for _, s := range h.steps() {
		if s == "start" {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("the VM was started %d times; want it left after the shutdown that did not stop it", starts)
	}
}

func TestATemplateOfABakeWithoutAnImageGrowsItsOwnDisk(t *testing.T) {
	t.Parallel()
	h := newBakeHost(t)
	ctx := t.Context()
	recipe := &Recipe{UserData: "#cloud-config\n", Checks: [][]string{{"true"}}, DiskGiB: 8}
	k := bakeVM(ctx, t, h, recipe)
	k.b.ISOStorage = "local"

	if err := k.finish(ctx); err != nil {
		t.Fatal(err)
	}
	steps := h.steps()
	if steps[1] != "resize scsi0 8G" || slices.Contains(steps, "unlink scsi1") {
		t.Errorf("steps %v; want the template's own disk grown and no builder's disk", steps)
	}
}

func TestISOStorage(t *testing.T) {
	t.Parallel()
	h := newBakeHost(t)
	ctx := t.Context()
	node, err := h.client().Node(ctx, "pve1")
	if err != nil {
		t.Fatal(err)
	}
	if s, err := (&Builder{ISOStorage: "isos"}).isoStorage(ctx, node); err != nil || s != "isos" {
		t.Errorf("isoStorage gave %q, %v; want the one named", s, err)
	}
	if s, err := (&Builder{}).isoStorage(ctx, node); err != nil || s != "local" {
		t.Errorf("isoStorage gave %q, %v; want the node's storage for ISO images", s, err)
	}
	h.noISO = true
	if s, err := (&Builder{}).isoStorage(ctx, node); err == nil || !strings.Contains(err.Error(), "no storage on pve1 takes ISO images") {
		t.Errorf("isoStorage gave %q, %v; want the missing ISO storage named", s, err)
	}
}

func TestMACAddress(t *testing.T) {
	t.Parallel()
	for net0, want := range map[string]string{
		"virtio=BC:24:11:0A:0B:0C,bridge=vmbr0":       "bc:24:11:0a:0b:0c",
		"e1000=bc:24:11:0a:0b:0c,bridge=vmbr0,tag=12": "bc:24:11:0a:0b:0c",
	} {
		if got, err := macAddress(net0); err != nil || got != want {
			t.Errorf("macAddress(%q) = %q, %v; want %q", net0, got, err, want)
		}
	}
	for _, net0 := range []string{"", "virtio,bridge=vmbr0", "virtio=BC:24:11:0A:0B,bridge=vmbr0"} {
		if got, err := macAddress(net0); err == nil {
			t.Errorf("macAddress(%q) = %q; want no MAC address found", net0, got)
		}
	}
}

func nodeOSTemplate(t *testing.T, d Distribution, nodeOS, kubernetes string) Template {
	t.Helper()
	o, err := ParseNodeOS(nodeOS)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := Plan(&Request{Distribution: d, NodeOS: o, KubernetesVersion: kubernetes})
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func TestCreateRefusesANodeOSTheNodesCPUCannotRun(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		nodeOS, flags, want string
	}{
		{"rocky-10", v2Flags, "node pve1's CPU reaches x86-64-v2, and rocky-10 needs x86-64-v3"},
		{"almalinux-10", "fpu", "node pve1's CPU reaches x86-64-v1, and almalinux-10 needs x86-64-v2"},
	} {
		h := newBakeHost(t)
		h.flags = c.flags
		tmpl := nodeOSTemplate(t, K3s, c.nodeOS, "")
		b := &Builder{Client: h.client(), Node: "pve1", Pool: testPool, Sources: &Sources{HTTP: &http.Client{}}}
		_, err := b.Create(t.Context(), &tmpl)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s on %q: Create gave %v; want %q", c.nodeOS, c.flags, err, c.want)
		}
		if steps := h.steps(); len(steps) != 0 {
			t.Errorf("%s: Create changed %v before refusing", c.nodeOS, steps)
		}
	}
	h := newBakeHost(t)
	h.flags = v3Flags
	rocky, err := ParseNodeOS("rocky-10")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Builder{Client: h.client(), Node: "pve1"}).checkCPU(t.Context(), rocky); err != nil {
		t.Errorf("a node reaching x86-64-v3 was refused Rocky Linux 10: %v", err)
	}
}

func TestCreateRefusesARepositoryKeyOfAnotherFingerprint(t *testing.T) {
	t.Parallel()
	sources, files := fakePublishers(t)
	// AlmaLinux's key is a valid key, but not the one pkgs.k8s.io signs with.
	files[k8sKeyPath] = almaLinux10Key
	h := newBakeHost(t)
	tmpl := nodeOSTemplate(t, Kubeadm, "almalinux-10", "v1.37.1")
	b := &Builder{Client: h.client(), Node: "pve1", Pool: testPool, Sources: sources}

	_, err := b.Create(t.Context(), &tmpl)
	if err == nil || !strings.Contains(err.Error(), "serves a key other than "+KubernetesPackagesKeyFingerprint) {
		t.Errorf("Create gave %v; want pkgs.k8s.io's key refused", err)
	}
	if steps := h.steps(); len(steps) != 0 {
		t.Errorf("Create changed %v before refusing", steps)
	}
}

// talosBuilder is a builder of Talos's template, whose image the fake publishers' Image Factory
// names, and that image's file name in the import storage.
func talosBuilder(t *testing.T, h *bakeHost) (*Builder, Template, string) {
	t.Helper()
	sources, _ := fakePublishers(t)
	tmpl, err := Plan(&Request{Distribution: Talos})
	if err != nil {
		t.Fatal(err)
	}
	image, err := sources.Resolve(t.Context(), tmpl.Image)
	if err != nil {
		t.Fatal(err)
	}
	b := &Builder{
		Client: h.client(), Sources: sources, Node: "pve1", ImportStorage: "local", Storage: "local-lvm", Pool: testPool,
		selectableSettle: time.Nanosecond, selectablePoll: time.Millisecond,
	}
	return b, tmpl, "local:import/" + image.FileName
}

func TestCreateMakesATemplateAndRemovesItsDownload(t *testing.T) {
	t.Parallel()
	h := newBakeHost(t)
	b, tmpl, volume := talosBuilder(t, h)
	// A download an interrupted run left, which carries no checksum to trust it by.
	h.volumes = []string{volume}

	vmid, err := b.Create(t.Context(), &tmpl)
	if err != nil || vmid != 101 {
		t.Fatalf("Create gave %d, %v; want VM 101", vmid, err)
	}
	want := []string{
		"remove " + volume,
		"download " + volume,
		"create 101",
		"config tags=" + kubehengeTag + ";" + buildingTag,
		"config boot=order=scsi0 cores=1 memory=512 tags=" + strings.Join(tmpl.Tags, ";"),
		"template",
		"remove " + volume,
	}
	if got := h.steps(); !slices.Equal(got, want) {
		t.Errorf("steps\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.vms[101]
	if c["template"] != 1 || c["cpu"] != DefaultCPUType || c["pool"] != testPool || c["scsi0"] != "local-lvm:0,import-from="+volume+",discard=on,ssd=1" {
		t.Errorf("the template is %v", c)
	}
	if c["net0"] != nil {
		t.Errorf("a template that bakes nothing has a network: %v", c["net0"])
	}
}

func TestCreateRemovesWhatAFailedBuildMade(t *testing.T) {
	t.Parallel()
	for _, keep := range []bool{false, true} {
		h := newBakeHost(t)
		h.failing = "qmtemplate"
		b, tmpl, volume := talosBuilder(t, h)
		b.KeepOnFailure = keep

		_, err := b.Create(t.Context(), &tmpl)
		if err == nil || !strings.Contains(err.Error(), "failed: qmtemplate went wrong") {
			t.Fatalf("keep %v: Create gave %v; want the conversion's failure", keep, err)
		}
		steps := h.steps()
		h.mu.Lock()
		_, left := h.vms[101]
		volumes := slices.Clone(h.volumes)
		h.mu.Unlock()
		if left != keep || slices.Contains(steps, "delete 101") == keep {
			t.Errorf("keep %v: VM 101 left %v after steps %v", keep, left, steps)
		}
		if steps[len(steps)-1] != "remove "+volume || len(volumes) != 0 {
			t.Errorf("keep %v: the download is left in %v after steps %v", keep, volumes, steps)
		}
	}
}

func TestCreateMakesNoVMWhenTheDownloadFails(t *testing.T) {
	t.Parallel()
	h := newBakeHost(t)
	h.failing = "download"
	b, tmpl, volume := talosBuilder(t, h)

	_, err := b.Create(t.Context(), &tmpl)
	if err == nil || !strings.Contains(err.Error(), "failed: download went wrong") {
		t.Errorf("Create gave %v; want the download's failure", err)
	}
	if got, want := h.steps(), []string{"download " + volume}; !slices.Equal(got, want) {
		t.Errorf("steps %v, want %v", got, want)
	}
}
