package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParseVolumeHandle(t *testing.T) {
	t.Parallel()
	for handle, want := range map[string]volume{
		"zone-a/pve-2/local/102/vm-102-pvc-a900c737.raw": {node: "pve-2", storage: "local", volid: "local:102/vm-102-pvc-a900c737.raw", owner: 102},
		"region/pve-1/local-lvm/vm-4242-pvc-1234":        {node: "pve-1", storage: "local-lvm", volid: "local-lvm:vm-4242-pvc-1234", owner: 4242},
	} {
		got, err := parseVolumeHandle(handle)
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", handle, got, err, want)
		}
	}
	for _, bad := range []string{"", "a/b/c", "a//local/vm-1-disk", "a/b/local/base-100-disk-0", "a/b/local/vm-x-disk"} {
		if _, err := parseVolumeHandle(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestReleased(t *testing.T) {
	t.Parallel()
	pv := &corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: csiDriver}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	if !released(pv) {
		t.Error("a released volume of the plugin is not reclaimed")
	}
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	if released(pv) {
		t.Error("a retained volume is reclaimed")
	}
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
	pv.Status.Phase = corev1.VolumeBound
	if released(pv) {
		t.Error("a bound volume is reclaimed")
	}
}

// fakePVE is a Proxmox VE cluster of nodes a and b holding cluster web's volume owner VM 4242, with
// node-local storage "local" and shared storage "ceph". Like Proxmox VE, it deletes an unused disk
// on the node its VM is on, and migration copies a VM's unused disks along.
type fakePVE struct {
	mu      sync.Mutex
	at      string
	offline map[string]bool
	config  map[string]string
	// volumes maps node/storage to the volumes there; shared storage is under */ceph.
	volumes   map[string][]string
	moves     []string
	writes    int
	acl       []map[string]any
	aclWrites int
	destroyed bool
	// unlisted leaves the VM out of pool and resource listings, as for seconds after creating it.
	unlisted bool
	pool     string
	// token is whether the cluster user's token exists; tokens counts those created.
	token  bool
	tokens int
	// hang keeps every task running.
	hang bool
	// requests are those answered, as "<method> <path>".
	requests []string
	// clusterName, when set, is what /cluster/status names the Proxmox VE cluster.
	clusterName string
	// nextIDs are what /cluster/nextid offers, in turn; taken are VMIDs other VMs hold. Creating
	// VM 4242 makes it the volume owner.
	nextIDs []int
	taken   map[int]bool
	// userMissing has the cluster's user not exist yet; groups are those it is in, and userWrites
	// the bodies of its creation and changes.
	userMissing bool
	groups      []string
	userWrites  []map[string]any
}

func newFakePVE(at string, volumes map[string][]string) *fakePVE {
	return &fakePVE{at: at, offline: map[string]bool{}, config: map[string]string{"name": webNames.VolumeOwner}, volumes: volumes, pool: webNames.Pool}
}

// webNames are the names of the cluster team-a/web, whose volume owner the fake holds.
var webNames, _ = NamesFor("kubehenge", "team-a", "web")

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := answer{w}
	body := map[string]any{}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}
	path := strings.Split(strings.TrimPrefix(r.URL.Path, "/api2/json/"), "/")
	f.requests = append(f.requests, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api2/json"))
	switch {
	case f.creation(a, r, path, body):
	case r.URL.Path == "/api2/json/cluster/resources":
		f.resources(a)
	case r.URL.Path == "/api2/json/storage":
		a.reply([]map[string]any{
			{"storage": "local", "content": "images,iso", "shared": 0},
			{"storage": "ceph", "content": "images", "shared": 1},
		})
	case r.URL.Path == "/api2/json/nodes":
		a.reply(f.nodes())
	case r.URL.Path == "/api2/json/pools":
		f.pools(a, r)
	case r.URL.Path == "/api2/json/access/acl" && r.Method == http.MethodPut:
		f.writeACL(body)
		a.reply(nil)
	case r.URL.Path == "/api2/json/access/acl":
		a.reply(f.acl)
	case len(path) == 5 && path[0] == "access" && path[3] == "token":
		f.tokenRequest(a, r.Method, path[2])
	case r.URL.Path == "/api2/json/access/users" && r.Method == http.MethodPost,
		len(path) == 3 && path[0] == "access" && path[1] == "users":
		f.user(a, r.Method, body)
	case path[0] == "access":
		a.reply(nil)
	case len(path) == 5 && path[2] == "storage" && path[4] == "content":
		a.reply(f.content(path[1]+"/"+path[3], r.URL.Query().Get("vmid")))
	case len(path) >= 4 && path[2] == "tasks" && f.hang:
		a.reply(map[string]any{"status": "running"})
	case len(path) >= 4 && path[2] == "tasks":
		a.reply(map[string]any{"status": "stopped", "exitstatus": "OK"})
	case len(path) >= 4 && path[2] == "qemu" && path[3] == "4242":
		f.vm(a, r.Method, path, body)
	default:
		http.NotFound(w, r)
	}
}

// user answers for the cluster's user as Proxmox VE does: a change replaces its groups unless told
// to append.
func (f *fakePVE) user(a answer, method string, body map[string]any) {
	switch method {
	case http.MethodGet:
		if f.userMissing {
			a.fail("no such user")
			return
		}
		a.reply(map[string]any{"groups": f.groups})
	case http.MethodPost, http.MethodPut:
		f.userWrites = append(f.userWrites, body)
		f.userMissing = false
		if body["append"] == nil {
			f.groups = nil
		}
		groups, ok := body["groups"].(string)
		if !ok {
			a.fail("groups: not a list")
			return
		}
		f.groups = append(f.groups, splitList(groups)...)
		a.reply(nil)
	default:
		a.reply(nil)
	}
}

func (f *fakePVE) resources(a answer) {
	if f.destroyed || f.unlisted {
		a.reply([]any{})
		return
	}
	a.reply([]map[string]any{{"vmid": 4242, "node": f.at, "pool": f.pool}})
}

func (f *fakePVE) nodes() []map[string]any {
	names := []string{"a", "b"}
	nodes := make([]map[string]any, 0, len(names))
	for _, n := range names {
		status := "online"
		if f.offline[n] {
			status = "offline"
		}
		nodes = append(nodes, map[string]any{"node": n, "status": status})
	}
	return nodes
}

func (f *fakePVE) pools(a answer, r *http.Request) {
	if r.Method == http.MethodDelete {
		a.reply(nil)
		return
	}
	if f.destroyed || f.unlisted || r.URL.Query().Get("poolid") != f.pool {
		a.reply([]map[string]any{{"members": []any{}}})
		return
	}
	a.reply([]map[string]any{{"members": []map[string]any{{"type": "qemu", "vmid": 4242, "name": webNames.VolumeOwner, "node": f.at}}}})
}

func (f *fakePVE) writeACL(body map[string]any) {
	f.aclWrites++
	entry := map[string]any{"path": body["path"], "type": "user", "ugid": body["users"], "roleid": body["roles"]}
	f.acl = slices.DeleteFunc(f.acl, func(e map[string]any) bool {
		return e["path"] == entry["path"] && e["ugid"] == entry["ugid"] && e["roleid"] == entry["roleid"]
	})
	if body["delete"] == nil {
		entry["propagate"] = body["propagate"]
		f.acl = append(f.acl, entry)
	}
}

func (f *fakePVE) tokenRequest(a answer, method, user string) {
	switch method {
	case http.MethodPost:
		f.token = true
		f.tokens++
		a.reply(map[string]any{"value": fmt.Sprintf("secret-%d", f.tokens)})
	case http.MethodDelete:
		f.token = false
		a.reply(nil)
	default:
		if !f.token {
			a.fail("no such token 'cluster' for user '" + user + "'")
			return
		}
		a.reply(map[string]any{})
	}
}

func (f *fakePVE) content(storage, vmid string) []map[string]any {
	var content []map[string]any
	for _, v := range f.volumes[storage] {
		if strings.Contains(v, "vm-"+vmid+"-") {
			content = append(content, map[string]any{"volid": v})
		}
	}
	return content
}

// creation answers what creating a cluster's objects reads beyond its own VM: the cluster's status,
// the next free VMID and the create itself. It reports whether it answered.
func (f *fakePVE) creation(a answer, r *http.Request, path []string, body map[string]any) bool {
	switch {
	case r.URL.Path == "/api2/json/cluster/status" && f.clusterName != "":
		a.reply([]map[string]any{{"type": "cluster", "name": f.clusterName}})
	case r.URL.Path == "/api2/json/cluster/nextid" && len(f.nextIDs) > 0:
		a.reply(strconv.Itoa(f.nextIDs[0]))
		f.nextIDs = f.nextIDs[1:]
	case len(path) == 3 && path[0] == "nodes" && path[2] == "qemu" && r.Method == http.MethodPost:
		f.create(a, path[1], body)
	default:
		return false
	}
	return true
}

// create creates a VM on node: VM 4242 becomes the volume owner, and a taken VMID is refused as
// Proxmox VE refuses it.
func (f *fakePVE) create(a answer, node string, body map[string]any) {
	id, ok := body["vmid"].(float64)
	if !ok {
		a.fail("no vmid")
		return
	}
	vmid := int(id)
	switch {
	case f.taken[vmid]:
		a.fail(fmt.Sprintf("unable to create VM %d - VM %d already exists on node '%s'", vmid, vmid, node))
	case vmid == 4242:
		f.destroyed, f.at = false, node
		f.config = map[string]string{"name": fmt.Sprint(body["name"])}
		f.pool = fmt.Sprint(body["pool"])
		a.reply("UPID:create")
	default:
		a.fail(fmt.Sprintf("creating VM %d is not modelled", vmid))
	}
}

// vm answers for the volume owner VM.
func (f *fakePVE) vm(a answer, method string, path []string, body map[string]any) {
	if f.destroyed || path[1] != f.at {
		a.fail("Configuration file 'nodes/" + path[1] + "/qemu-server/4242.conf' does not exist")
		return
	}
	switch {
	case len(path) == 4 && method == http.MethodDelete:
		for key, vols := range f.volumes {
			if key == f.at+"/local" || key == "*/ceph" {
				f.volumes[key] = slices.DeleteFunc(vols, func(v string) bool { return strings.Contains(v, "vm-4242-") })
			}
		}
		f.destroyed = true
		a.reply("UPID:destroy")
	case len(path) == 5 && path[4] == "migrate":
		f.migrate(a, body)
	case method == http.MethodPut:
		f.writeConfig(a, body)
	default:
		a.reply(f.config)
	}
}

func (f *fakePVE) migrate(a answer, body map[string]any) {
	for k, v := range f.config {
		if strings.HasPrefix(k, "unused") && !strings.HasPrefix(v, "ceph:") {
			a.fail("copying local disk " + v + " is not modelled")
			return
		}
	}
	target, ok := body["target"].(string)
	if !ok {
		a.fail("no target node")
		return
	}
	f.moves = append(f.moves, f.at+"->"+target)
	f.at = target
	a.reply("UPID:migrate")
}

func (f *fakePVE) writeConfig(a answer, body map[string]any) {
	f.writes++
	if keys, ok := body["delete"].(string); ok {
		for k := range strings.SplitSeq(keys, ",") {
			if !f.free(f.at, f.config[k]) {
				a.fail("no such volume '" + f.config[k] + "' on node " + f.at)
				return
			}
			delete(f.config, k)
		}
	}
	for k, v := range body {
		if k != "delete" {
			f.config[k] = fmt.Sprint(v)
		}
	}
	a.reply(nil)
}

func (f *fakePVE) free(node, volid string) bool {
	storage, _, ok := strings.Cut(volid, ":")
	if !ok {
		return false
	}
	key := node + "/" + storage
	if storage == "ceph" {
		key = "*/ceph"
	}
	i := slices.Index(f.volumes[key], volid)
	if i < 0 {
		return false
	}
	f.volumes[key] = slices.Delete(f.volumes[key], i, i+1)
	return true
}

// answer replies as Proxmox VE does.
type answer struct{ w http.ResponseWriter }

func (a answer) reply(data any) {
	if err := json.NewEncoder(a.w).Encode(map[string]any{"data": data}); err != nil {
		panic(err)
	}
}

// fail gives the reason for a failure in the status line, the only part go-proxmox reads.
func (a answer) fail(msg string) {
	hijacker, ok := a.w.(http.Hijacker)
	if !ok {
		panic("the test server's connections cannot be taken over")
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 500 %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", msg)
	_ = buf.Flush()
}

// moved records owner VM moves through the controller's token.
type moved struct{ pve *proxmox.Client }

func (m moved) MoveVolumeOwner(ctx context.Context, vmid int, from, to string) error {
	p := &Proxmox{Client: m.pve}
	return p.MoveVolumeOwner(ctx, vmid, from, to)
}

func serve(t *testing.T, f *fakePVE) *proxmox.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return proxmox.NewClient(srv.URL + "/api2/json")
}

func TestVolumeOwnerDescribedOnlyWhenItDiffers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakePVE("a", nil)
	p := &Proxmox{Client: serve(t, f)}
	n := webNames
	describe := func() {
		t.Helper()
		found, err := p.isVolumeOwner(ctx, n, 4242, "a", nil)
		if err != nil || !found.ours {
			t.Fatalf("ours = %v, %v", found.ours, err)
		}
		if err := p.describeVolumeOwner(ctx, "a", 4242, n, found.config); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		describe()
	}
	if f.writes != 1 || f.config["tags"] != "kubehenge;volumes" || !strings.Contains(f.config["description"], "kubehenge/team-a.web") {
		t.Fatalf("%d writes, config %v", f.writes, f.config)
	}
	f.config["description"] = "changed by hand"
	describe()
	if f.writes != 2 || !strings.Contains(f.config["description"], "kubehenge/team-a.web") {
		t.Errorf("a changed description was not put back: %d writes, config %v", f.writes, f.config)
	}
}

func TestGrantWrittenOnlyWhenItDiffers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakePVE("a", nil)
	p := &Proxmox{Client: serve(t, f), Settings: Settings{Role: "kubehenge-cluster-vms"}}
	n := Names{Pool: "kubehenge/team-a.web", User: "kubehenge.team-a.web@pve"}
	// Another user's grant on the same pool stays as it is.
	f.acl = []map[string]any{{"path": "/pool/kubehenge/team-a.web", "type": "user", "ugid": "admin@pve", "roleid": "Administrator", "propagate": 1}}
	for range 3 {
		if err := p.ensureGrant(ctx, n, Access{}); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]any{"path": "/pool/kubehenge/team-a.web", "type": "user", "ugid": n.User, "roleid": "kubehenge-cluster-vms", "propagate": float64(1)}
	if f.aclWrites != 1 || len(f.acl) != 2 || !maps.Equal(f.acl[1], want) {
		t.Fatalf("%d writes, ACL %v", f.aclWrites, f.acl)
	}

	p.Settings.Role = "kubehenge-cluster-vms-v2"
	if err := p.ensureGrant(ctx, n, Access{}); err != nil {
		t.Fatal(err)
	}
	if len(f.acl) != 2 || f.acl[0]["ugid"] != "admin@pve" || f.acl[1]["roleid"] != "kubehenge-cluster-vms-v2" {
		t.Errorf("after a role change the ACL is %v", f.acl)
	}
}

func TestDeleteThroughOwner(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{"a/local": {"local:4242/vm-4242-other.raw", "local:4242/vm-4242-pvc-1.raw"}})
	f.config["unused0"] = "local:4242/vm-4242-other.raw"
	pve := serve(t, f)

	if err := deleteThroughOwner(context.Background(), pve, "a", 4242, "local:4242/vm-4242-pvc-1.raw"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.volumes["a/local"], []string{"local:4242/vm-4242-other.raw"}) {
		t.Errorf("volumes left %v", f.volumes)
	}
	if f.config["unused0"] != "local:4242/vm-4242-other.raw" {
		t.Errorf("an unrelated unused disk was touched: %v", f.config)
	}
}

func TestDeleteVolumeMovesTheOwnerToANodeLocalVolume(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{"b/local": {"local:4242/vm-4242-pvc-1.raw"}})
	pve := serve(t, f)
	o := &ownerVM{pve: pve, mover: moved{pve}, vmid: 4242, created: "a"}

	v, err := parseVolumeHandle("lab/b/local/4242/vm-4242-pvc-1.raw")
	if err != nil {
		t.Fatal(err)
	}
	if err := o.deleteVolume(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if len(f.volumes["b/local"]) != 0 || !slices.Equal(f.moves, []string{"a->b"}) {
		t.Errorf("volumes %v, moves %v", f.volumes, f.moves)
	}
}

func TestDeleteVolumeOnSharedStorageLeavesTheOwnerWhereItIs(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{"*/ceph": {"ceph:vm-4242-pvc-1"}})
	pve := serve(t, f)
	o := &ownerVM{pve: pve, vmid: 4242, created: "a"}

	if err := o.deleteVolume(context.Background(), volume{node: "b", storage: "ceph", volid: "ceph:vm-4242-pvc-1", owner: 4242}); err != nil {
		t.Fatal(err)
	}
	if len(f.volumes["*/ceph"]) != 0 || len(f.moves) != 0 {
		t.Errorf("volumes %v, moves %v", f.volumes, f.moves)
	}
}

func TestOwnerWithUnusedDisksDoesNotMove(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{
		"a/local": {"local:4242/vm-4242-pvc-0.raw"},
		"b/local": {"local:4242/vm-4242-pvc-1.raw"},
	})
	f.config["unused0"] = "local:4242/vm-4242-pvc-0.raw"
	pve := serve(t, f)
	o := &ownerVM{pve: pve, mover: moved{pve}, vmid: 4242, created: "a"}

	if err := o.deleteVolume(context.Background(), volume{node: "b", storage: "local", volid: "local:4242/vm-4242-pvc-1.raw", owner: 4242}); err == nil {
		t.Fatal("moved an owner VM with an unused disk")
	}
	if len(f.moves) != 0 || len(f.volumes["a/local"]) != 1 {
		t.Errorf("volumes %v, moves %v", f.volumes, f.moves)
	}
	// The half-done deletion finishes when its own volume comes round, and the move follows.
	if err := o.deleteVolume(context.Background(), volume{node: "a", storage: "local", volid: "local:4242/vm-4242-pvc-0.raw", owner: 4242}); err != nil {
		t.Fatal(err)
	}
	if err := o.deleteVolume(context.Background(), volume{node: "b", storage: "local", volid: "local:4242/vm-4242-pvc-1.raw", owner: 4242}); err != nil {
		t.Fatal(err)
	}
	if len(f.volumes["a/local"])+len(f.volumes["b/local"]) != 0 {
		t.Errorf("volumes left %v", f.volumes)
	}
}

func TestRemoveDeletesVolumesOnEveryNode(t *testing.T) {
	t.Parallel()
	// The owner VM has moved to b since its creation on a, and a deletion there stopped half-way.
	f := newFakePVE("b", map[string][]string{
		"a/local": {"local:4242/vm-4242-pvc-0.raw", "local:100/vm-100-disk-0.raw"},
		"b/local": {"local:4242/vm-4242-pvc-1.raw", "local:4242/vm-4242-pvc-2.raw"},
		"*/ceph":  {"ceph:vm-4242-pvc-3"},
	})
	f.config["unused0"] = "local:4242/vm-4242-pvc-2.raw"
	p := &Proxmox{Client: serve(t, f)}
	n, err := NamesFor("kubehenge", "team-a", "web")
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Remove(context.Background(), n, Credentials{}); err != nil {
		t.Fatal(err)
	}
	if !f.destroyed {
		t.Error("the owner VM is still there")
	}
	for key, vols := range f.volumes {
		for _, v := range vols {
			if strings.Contains(v, "vm-4242-") {
				t.Errorf("volume %s left on %s", v, key)
			}
		}
	}
	if !slices.Equal(f.volumes["a/local"], []string{"local:100/vm-100-disk-0.raw"}) {
		t.Errorf("another VM's volume was touched: %v", f.volumes["a/local"])
	}
}

func TestRemoveWaitsForAnOfflineNode(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{"b/local": {"local:4242/vm-4242-pvc-1.raw"}})
	f.offline["b"] = true
	p := &Proxmox{Client: serve(t, f)}
	n, err := NamesFor("kubehenge", "team-a", "web")
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Remove(context.Background(), n, Credentials{}); err == nil {
		t.Fatal("removed the cluster while a node with its volumes is offline")
	}
	if f.destroyed {
		t.Error("destroyed the owner VM, which would leave the offline node's volumes without one")
	}
}

func TestRemoveWaitsForAnOwnerNotListedYet(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{"a/local": {"local:4242/vm-4242-pvc-1.raw"}})
	f.unlisted = true
	p := &Proxmox{Client: serve(t, f)}
	held := Credentials{VolumeOwner: 4242, VolumeOwnerNode: "a"}

	if err := p.Remove(context.Background(), webNames, held); err == nil {
		t.Fatal("removed the cluster before its owner VM's pool was confirmed")
	}
	if f.destroyed {
		t.Fatal("destroyed an owner VM whose pool was not confirmed")
	}
	f.unlisted = false
	if err := p.Remove(context.Background(), webNames, held); err != nil {
		t.Fatal(err)
	}
	if !f.destroyed {
		t.Error("the owner VM was left behind")
	}
}

func TestReclaimLeavesAnAttachedVolume(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{"a/local": {"local:4242/vm-4242-pvc-1.raw", "local:4242/vm-4242-pvc-2.raw"}})
	pve := serve(t, f)
	released := func(name string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			Name: name,
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver: csiDriver, VolumeHandle: "lab/a/local/4242/vm-4242-" + name + ".raw",
				}},
			},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
		}
	}
	pv1 := "pvc-1"
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, storagev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	workload := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		released("pvc-1"), released("pvc-2"),
		&storagev1.VolumeAttachment{
			Name: "csi-1",
			Spec: storagev1.VolumeAttachmentSpec{Attacher: csiDriver, NodeName: "gone", Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv1}},
		},
	).Build()
	r := &Reclaimer{Connect: func(string) (*proxmox.Client, error) { return pve, nil }}

	creds := Credentials{TokenID: webNames.TokenID(), TokenSecret: "s3cret", VolumeOwner: 4242, VolumeOwnerNode: "a"}
	if err := r.reclaimIn(context.Background(), cluster(), creds, workload); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.volumes["a/local"], []string{"local:4242/vm-4242-pvc-1.raw"}) {
		t.Errorf("volumes left %v; want only the attached one", f.volumes["a/local"])
	}
}

func TestHangingTaskIsGivenUpOn(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", nil)
	f.hang = true
	p := &Proxmox{Client: serve(t, f)}
	err := p.waitTask(context.Background(), "a", "UPID:a:destroy", 250*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still running after") {
		t.Fatalf("got %v", err)
	}
}

// Every cluster is reconciled again every few minutes; once its objects are there, that reads only
// what is the cluster's own: no listing of every VM or user, and the region once.
func TestSteadyReconcileReadsOnlyTheClustersOwn(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", nil)
	f.token, f.clusterName = true, "lab"
	f.acl = []map[string]any{{"path": "/pool/" + webNames.Pool, "type": "user", "ugid": webNames.User, "roleid": "kubehenge-cluster", "propagate": 1}}
	p := &Proxmox{Client: serve(t, f), Settings: Settings{Role: "kubehenge-cluster"}}
	held := Credentials{TokenID: webNames.TokenID(), TokenSecret: "s3cret", VolumeOwner: 4242, VolumeOwnerNode: "a"}
	record := func(Credentials) error { return nil }
	if _, err := p.Ensure(context.Background(), webNames, Access{}, held, record); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
	c, err := p.Ensure(context.Background(), webNames, Access{}, held, record)
	if err != nil || c.Region != "lab" || c.VolumeOwner != 4242 {
		t.Fatalf("Ensure = %+v, %v", c, err)
	}
	want := []string{
		"GET /pools",
		"GET /access/users/" + webNames.User,
		"GET /access/acl",
		"GET /nodes/a/qemu/4242/config",
		"GET /access/users/" + webNames.User + "/token/" + webNames.Token,
	}
	if !slices.Equal(f.requests, want) {
		t.Fatalf("asked\n%s\nwant\n%s", strings.Join(f.requests, "\n"), strings.Join(want, "\n"))
	}
}

func TestVolumeOwnerTakesAnotherVMIDWhenTheOfferedOneIsTaken(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", nil)
	// No owner yet; VM 100 is created by something else between nextid and the create.
	f.destroyed, f.token, f.clusterName = true, true, "lab"
	f.nextIDs, f.taken = []int{100, 4242}, map[int]bool{100: true}
	p := &Proxmox{Client: serve(t, f), Settings: Settings{Role: "kubehenge-cluster"}}
	var recorded []int
	record := func(c Credentials) error { recorded = append(recorded, c.VolumeOwner); return nil }
	c, err := p.Ensure(context.Background(), webNames, Access{}, Credentials{TokenID: webNames.TokenID(), TokenSecret: "s3cret"}, record)
	if err != nil || c.VolumeOwner != 4242 || c.VolumeOwnerNode != "a" {
		t.Fatalf("Ensure = %+v, %v; want volume owner 4242 on a", c, err)
	}
	if !slices.Equal(recorded, []int{100, 4242}) {
		t.Errorf("recorded volume owners %v; want 100, then 4242 once 100 was taken", recorded)
	}
	if f.destroyed || f.config["name"] != webNames.VolumeOwner {
		t.Errorf("volume owner VM 4242 is %v, destroyed %v", f.config, f.destroyed)
	}
}

func TestVolumeOwnerGivesUpAfterItsAttempts(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", nil)
	f.destroyed, f.token, f.clusterName = true, true, "lab"
	f.nextIDs, f.taken = slices.Repeat([]int{100}, volumeOwnerAttempts+1), map[int]bool{100: true}
	p := &Proxmox{Client: serve(t, f), Settings: Settings{Role: "kubehenge-cluster"}}
	_, err := p.Ensure(context.Background(), webNames, Access{}, Credentials{TokenID: webNames.TokenID(), TokenSecret: "s3cret"}, func(Credentials) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Ensure gave %v; want Proxmox VE's refusal once every attempt found the VMID taken", err)
	}
	if len(f.nextIDs) != 1 {
		t.Errorf("%d VMIDs left unasked; want %d attempts", len(f.nextIDs), volumeOwnerAttempts)
	}
}

func TestNewTokenKeptBeforeALaterFailure(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", nil)
	// No /cluster/status in the fake: reading the region fails after the token is created.
	p := &Proxmox{Client: serve(t, f)}
	var recorded Credentials
	record := func(c Credentials) error { recorded = c; return nil }

	_, err := p.Ensure(context.Background(), webNames, Access{}, Credentials{VolumeOwner: 4242, VolumeOwnerNode: "a"}, record)
	if err == nil || !strings.Contains(err.Error(), "cluster status") {
		t.Fatalf("got %v, want a failure to read the cluster status", err)
	}
	if recorded.TokenSecret != "secret-1" || recorded.TokenID != webNames.TokenID() || recorded.VolumeOwner != 4242 {
		t.Fatalf("recorded %+v", recorded)
	}
	p.Settings.Region = "lab"
	if _, err := p.Ensure(context.Background(), webNames, Access{}, recorded, record); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 1 {
		t.Errorf("%d tokens created; the kept secret was not used", f.tokens)
	}
}

func TestOwnerOfTheSameNameInAnotherPoolIsNeverTaken(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{"a/local": {"local:4242/vm-4242-pvc-1.raw"}})
	f.pool = "kubehenge/team-b.web"
	f.config["description"] = "kept"
	p := &Proxmox{Client: serve(t, f)}
	held := Credentials{VolumeOwner: 4242, VolumeOwnerNode: "a"}

	if found, err := p.isVolumeOwner(context.Background(), webNames, 4242, "a", nil); err != nil || found.ours {
		t.Fatalf("ours = %v, %v", found.ours, err)
	}
	// Nor through its own pool's members, where it is not.
	members, _, err := p.poolMembers(context.Background(), webNames.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := p.isVolumeOwner(context.Background(), webNames, 4242, "a", members); err != nil || found.ours {
		t.Fatalf("through the pool: ours = %v, %v", found.ours, err)
	}
	if err := p.Remove(context.Background(), webNames, held); err != nil {
		t.Fatal(err)
	}
	if f.destroyed || len(f.volumes["a/local"]) != 1 || f.writes != 0 {
		t.Errorf("another cluster's owner VM was touched: destroyed %v, volumes %v, %d writes", f.destroyed, f.volumes, f.writes)
	}
}

func TestRemoveLeavesAVMThatTookTheRecordedID(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{})
	f.unlisted = true
	f.config["name"] = "someone-elses"
	p := &Proxmox{Client: serve(t, f)}
	n, err := NamesFor("kubehenge", "team-a", "web")
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Remove(context.Background(), n, Credentials{VolumeOwner: 4242, VolumeOwnerNode: "a"}); err != nil {
		t.Fatal(err)
	}
	if f.destroyed {
		t.Error("destroyed a VM that is not the cluster's volume owner")
	}
}
