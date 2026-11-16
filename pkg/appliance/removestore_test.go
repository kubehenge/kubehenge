package appliance

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
)

const (
	shortStop  = 50 * time.Millisecond
	backupDisk = "local-lvm:vm-9000-disk-0"
	nodeUUID   = "0B6E3F4A-1C2D-4E5F-8A9B-0C1D2E3F4A5B"
)

// storeObjects is a store with replicas, its claim and volume, and the node the volume is
// attached to.
func storeObjects(replicas int32) []runtime.Object {
	labels := map[string]string{"app": backupstore.Service}
	return []runtime.Object{
		&appsv1.Deployment{
			Name: backupstore.Service, Namespace: backupstore.Namespace,
			Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels}},
		},
		&corev1.PersistentVolumeClaim{
			Name: backupstore.Claim, Namespace: backupstore.Namespace,
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: backupstore.Claim},
		},
		&corev1.PersistentVolume{
			Name: backupstore.Claim,
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.proxmox.sinextra.dev", VolumeHandle: "region/pve2/" + strings.Replace(backupDisk, ":", "/", 1)},
			}},
		},
		&corev1.Node{Name: "cp-0", Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: strings.ToLower(nodeUUID)}}},
	}
}

func storePod() *corev1.Pod {
	return &corev1.Pod{Name: "store-0", Namespace: backupstore.Namespace, Labels: map[string]string{"app": backupstore.Service}}
}

func attachment(pv string) *storagev1.VolumeAttachment {
	return &storagev1.VolumeAttachment{
		Name: "csi-0",
		Spec: storagev1.VolumeAttachmentSpec{NodeName: "cp-0", Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv}},
	}
}

func replicasOf(t *testing.T, kube kubernetes.Interface) int32 {
	t.Helper()
	d, err := kube.AppsV1().Deployments(backupstore.Namespace).Get(t.Context(), backupstore.Service, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

func noDetach(t *testing.T) func(context.Context, storeAttachment) error {
	t.Helper()
	return func(context.Context, storeAttachment) error {
		t.Error("detached a volume the store left no attachment of")
		return nil
	}
}

// fakeVM is a qemu VM in nodeVMs.
type fakeVM struct {
	id          int
	name, node  string
	uuid        string
	protection  int
	description string
	digest      int
	drives      map[string]string
	// offline answers every request as Proxmox VE does for a VM on a node that is down.
	offline bool
	running bool
}

// nodeVMs answers as Proxmox VE does for its VMs: it refuses to unlink a drive of a protected VM
// and to change a configuration whose digest moved on, and records each change.
type nodeVMs struct {
	mu    sync.Mutex
	vms   []*fakeVM
	calls []string

	failUnlink bool
	// liftAnswerLost applies a lift and then answers 502, as a proxy that drops the answer does.
	liftAnswerLost bool
	// pluginLiftsFirst has the CSI plugin lift the protection, with its marker, just before the
	// appliance's lift arrives.
	pluginLiftsFirst bool
	failProtect      bool
}

// withNodeVM is cp-0's VM, protected and holding disk on scsi1. A VM named after the node but
// with another SMBIOS UUID is listed before it, so that only the UUID tells them apart.
func withNodeVM(disk string) *nodeVMs {
	return &nodeVMs{vms: []*fakeVM{
		{id: 901, name: "cp-0", node: "pve1", uuid: "11111111-2222-3333-4444-555555555555", running: true, drives: map[string]string{}},
		{id: 1001, name: "mgmt-a", node: "pve2", uuid: nodeUUID, running: true, protection: 1, description: "Node VM", drives: map[string]string{
			"scsi0":   "local-lvm:vm-1001-disk-0,size=20G",
			"scsi1":   disk + ",backup=0,wwn=0x5056432d49443031",
			"scsihw":  "virtio-scsi-single",
			"unused0": "local-lvm:vm-1001-disk-1",
		}},
	}}
}

func (f *nodeVMs) vm(id int) *fakeVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.vms {
		if v.id == id {
			return v
		}
	}
	return nil
}

// reply writes data as Proxmox VE's answer.
func reply(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Error(err)
	}
}

func (f *nodeVMs) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cluster/resources") {
		list := make([]map[string]any, 0, 1+len(f.vms))
		list = append(list, map[string]any{"vmid": 900, "name": "cp-0", "node": "pve1", "type": "lxc"})
		for _, v := range f.vms {
			status := "stopped"
			switch {
			case v.offline:
				status = "unknown"
			case v.running:
				status = "running"
			}
			list = append(list, map[string]any{"vmid": v.id, "name": v.name, "node": v.node, "type": "qemu", "status": status})
		}
		reply(t, w, list)
		return
	}
	var node, op string
	var id int
	path := strings.ReplaceAll(strings.TrimPrefix(r.URL.Path, "/api2/json/nodes/"), "/", " ")
	if _, err := fmt.Sscanf(path, "%s qemu %d %s", &node, &id, &op); err != nil {
		http.NotFound(w, r)
		return
	}
	var v *fakeVM
	for _, vm := range f.vms {
		if vm.id == id && vm.node == node {
			v = vm
		}
	}
	switch {
	case v == nil:
		http.NotFound(w, r)
	case v.offline:
		http.Error(w, "no route to host", 595)
	case r.Method == http.MethodGet && op == "config":
		config := map[string]any{"digest": strconv.Itoa(v.digest), "protection": v.protection, "smbios1": "uuid=" + v.uuid + ",manufacturer=UHJveG1veA==,base64=1"}
		if v.description != "" {
			config["description"] = v.description
		}
		for key, value := range v.drives {
			config[key] = value
		}
		reply(t, w, config)
	case r.Method == http.MethodPut && op == "config":
		f.change(t, w, r, v)
	case r.Method == http.MethodPut && op == "unlink":
		f.unlink(t, w, r, v)
	default:
		http.NotFound(w, r)
	}
}

func (f *nodeVMs) change(t *testing.T, w http.ResponseWriter, r *http.Request, v *fakeVM) {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Error(err)
	}
	value := fmt.Sprint(body["protection"])
	f.calls = append(f.calls, fmt.Sprintf("%d protection=%s", v.id, value))
	if value == "0" && f.pluginLiftsFirst {
		v.protection, v.description = 0, v.description+"\n"+csiLiftedMarker
		v.digest++
	}
	if d, ok := body["digest"]; ok && d != strconv.Itoa(v.digest) {
		http.Error(w, "detected modified configuration - file changed by other user? Try again.", http.StatusInternalServerError)
		return
	}
	if value == "1" && f.failProtect {
		http.Error(w, "unable to write the configuration", http.StatusInternalServerError)
		return
	}
	v.protection, _ = strconv.Atoi(value)
	v.digest++
	if value == "0" && f.liftAnswerLost {
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	_, _ = w.Write([]byte(`{"data":null}`))
}

func (f *nodeVMs) unlink(t *testing.T, w http.ResponseWriter, r *http.Request, v *fakeVM) {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Error(err)
	}
	drive := fmt.Sprint(body["idlist"])
	f.calls = append(f.calls, fmt.Sprintf("%d unlink %s", v.id, drive))
	if v.protection == 1 {
		http.Error(w, "can't remove drive '"+drive+"' - protection mode enabled", http.StatusInternalServerError)
		return
	}
	if f.failUnlink {
		http.Error(w, "unable to unlink the drive", http.StatusInternalServerError)
		return
	}
	delete(v.drives, drive)
	v.digest++
	_, _ = w.Write([]byte(`{"data":null}`))
}

func (f *nodeVMs) appliance(t *testing.T) *Appliance {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Appliance{ProxmoxURL: srv.URL, ProxmoxCAFile: ca}
}

func (f *nodeVMs) detach(t *testing.T) func(context.Context, storeAttachment) error {
	t.Helper()
	a := f.appliance(t)
	return func(ctx context.Context, at storeAttachment) error { return a.detachVolume(ctx, ticket(), at) }
}

func (f *nodeVMs) detachHandle(t *testing.T, handle string) error {
	t.Helper()
	return f.appliance(t).detachVolume(t.Context(), ticket(), storeAttachment{Node: "cp-0", SystemUUID: strings.ToLower(nodeUUID), Volume: handle})
}

func (f *nodeVMs) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

var detachedCalls = []string{"1001 protection=0", "1001 unlink scsi1", "1001 protection=1"}

// A destroy starts only once the store is stopped, with no pod left and its volume detached.
func TestStopStoreScalesTheStoreToZero(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(append(storeObjects(1), attachment("another-volume"))...)
	if err := stopStore(t.Context(), &Job{}, kube, shortStop, noDetach(t)); err != nil {
		t.Fatal(err)
	}
	if r := replicasOf(t, kube); r != 0 {
		t.Fatalf("replicas %d, want 0", r)
	}
}

// A store whose pod does not go fails the removal and is left with the replicas it had: 0 while it
// sleeps between backups.
func TestStopStoreWaitsForThePod(t *testing.T) {
	t.Parallel()
	for _, had := range []int32{0, 1, 2} {
		kube := fake.NewClientset(append(storeObjects(had), storePod())...)
		err := stopStore(t.Context(), &Job{}, kube, shortStop, noDetach(t))
		if err == nil || !strings.Contains(err.Error(), "pod store-0 is still there") {
			t.Fatalf("store at %d: error %v, want the pod named", had, err)
		}
		if r := replicasOf(t, kube); r != had {
			t.Fatalf("replicas %d after the failed stop, want the %d it had", r, had)
		}
	}
}

// A volume the CSI plugin leaves attached is unlinked from its node VM with the protection lifted
// for the unlink alone.
func TestStopStoreDetachesWhatThePluginLeft(t *testing.T) {
	t.Parallel()
	f := withNodeVM(backupDisk)
	kube := fake.NewClientset(append(storeObjects(1), attachment(backupstore.Claim))...)
	if err := stopStore(t.Context(), &Job{}, kube, shortStop, f.detach(t)); err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); !slices.Equal(calls, detachedCalls) {
		t.Fatalf("calls %q, want %q", calls, detachedCalls)
	}
	if r := replicasOf(t, kube); r != 0 {
		t.Fatalf("replicas %d, want 0", r)
	}
}

// A detach that fails names the VM and the disk, sets the protection again and starts the store
// again with the replicas it had.
func TestStopStoreStartsTheStoreAgainWhenTheDetachFails(t *testing.T) {
	t.Parallel()
	for _, had := range []int32{0, 2} {
		f := withNodeVM(backupDisk)
		f.failUnlink = true
		kube := fake.NewClientset(append(storeObjects(had), attachment(backupstore.Claim))...)
		err := stopStore(t.Context(), &Job{}, kube, shortStop, f.detach(t))
		if err == nil || !strings.Contains(err.Error(), "VM 1001 (mgmt-a)") || !strings.Contains(err.Error(), backupDisk) {
			t.Fatalf("error %v, want the VM and the disk named", err)
		}
		if calls := f.callList(); !slices.Equal(calls, detachedCalls) {
			t.Fatalf("calls %q, want %q", calls, detachedCalls)
		}
		if r := replicasOf(t, kube); r != had {
			t.Fatalf("replicas %d after the failed detach, want the %d it had", r, had)
		}
	}
}

// A deadline that cuts off a look at the store in flight still ends in the appliance's detach.
func TestStopStoreDetachesWhenTheDeadlineCutsALookOff(t *testing.T) {
	t.Parallel()
	f := withNodeVM(backupDisk)
	kube := fake.NewClientset(append(storeObjects(1), attachment(backupstore.Claim))...)
	var lists atomic.Int32
	kube.PrependReactor("list", "volumeattachments", func(clienttesting.Action) (bool, runtime.Object, error) {
		if lists.Add(1) > 1 {
			return false, nil, nil
		}
		time.Sleep(2 * shortStop)
		return true, nil, context.DeadlineExceeded
	})
	if err := stopStore(t.Context(), &Job{}, kube, shortStop, f.detach(t)); err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); !slices.Equal(calls, detachedCalls) {
		t.Fatalf("calls %q, want %q", calls, detachedCalls)
	}
}

// A management cluster that does not answer is removed all the same, as the interface allows once
// its VMs are gone.
func TestStopStoreOnAClusterThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(storeObjects(1)...)
	kube.PrependReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp 127.0.0.1:6443: connect: connection refused")
	})
	if err := stopStore(t.Context(), &Job{}, kube, shortStop, noDetach(t)); err != nil {
		t.Fatal(err)
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatalf("patched %s of a cluster that does not answer", action.GetResource().Resource)
		}
	}
}

func TestStopStoreWithoutAStore(t *testing.T) {
	t.Parallel()
	if err := stopStore(t.Context(), &Job{}, fake.NewClientset(), shortStop, noDetach(t)); err != nil {
		t.Fatal(err)
	}
}

// The handle's disk is everything after the storage, which on file storage holds a slash itself.
func TestDetachVolumeOnEveryStorage(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ handle, disk string }{
		{"region/pve2/local-lvm/vm-9000-disk-0", "local-lvm:vm-9000-disk-0"},
		{"region/pve2/lvm/vm-9000-disk-0", "lvm:vm-9000-disk-0"},
		{"region/pve2/local-zfs/vm-9000-disk-0", "local-zfs:vm-9000-disk-0"},
		{"region//ceph/vm-9000-disk-0", "ceph:vm-9000-disk-0"},
		{"region/pve2/local/9000/vm-9000-disk-0.raw", "local:9000/vm-9000-disk-0.raw"},
		{"region//nfs/9000/vm-9000-disk-0.raw", "nfs:9000/vm-9000-disk-0.raw"},
		{"region//cifs/9000/vm-9000-disk-0.qcow2", "cifs:9000/vm-9000-disk-0.qcow2"},
		{"region//cephfs/9000/vm-9000-disk-0.raw", "cephfs:9000/vm-9000-disk-0.raw"},
	} {
		f := withNodeVM(c.disk)
		if err := f.detachHandle(t, c.handle); err != nil {
			t.Fatalf("%s: %v", c.handle, err)
		}
		if _, left := f.vm(1001).drives["scsi1"]; left {
			t.Fatalf("%s: scsi1 is still attached", c.handle)
		}
	}
	if err := withNodeVM(backupDisk).detachHandle(t, "region/pve2/local-lvm"); err == nil {
		t.Fatal("a handle without a disk was taken")
	}
}

// The node's VM is the one with its SMBIOS UUID, not one of its name. Without such a VM the detach
// fails naming the node, since the volume may still be attached.
func TestDetachVolumeFindsTheVMByItsUUID(t *testing.T) {
	t.Parallel()
	f := withNodeVM(backupDisk)
	if err := f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0"); err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); !slices.Equal(calls, detachedCalls) {
		t.Fatalf("calls %q, want %q", calls, detachedCalls)
	}
	f = withNodeVM(backupDisk)
	f.vms[1].uuid = "99999999-2222-3333-4444-555555555555"
	err := f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0")
	if err == nil || !strings.Contains(err.Error(), "node cp-0") {
		t.Fatalf("error %v, want the node named", err)
	}
}

// A node's zone is where the VM with its SMBIOS UUID runs, not a VM of its name.
func TestFollowZonesFindsTheVMByItsUUID(t *testing.T) {
	t.Parallel()
	f := withNodeVM(backupDisk)
	kube := fake.NewClientset(
		&corev1.Node{Name: "cp-0", Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: strings.ToLower(nodeUUID)}}},
		&corev1.Secret{
			Name: zonesSecret, Namespace: backupstore.Namespace,
			Data: map[string][]byte{zonesKey: []byte("mgmt-management-zones@pve!zones=00000000-0000-0000-0000-000000000000")},
		},
	)
	unknown, err := f.appliance(t).followZones(t.Context(), kube, "")
	if err != nil || len(unknown) > 0 {
		t.Fatalf("unknown %v, error %v", unknown, err)
	}
	n, err := kube.CoreV1().Nodes().Get(t.Context(), "cp-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if zone := n.Labels[corev1.LabelTopologyZone]; zone != "pve2" {
		t.Fatalf("zone %q, want pve2, where the VM with the node's SMBIOS UUID runs", zone)
	}
}

const offlineUUID = "99999999-8888-7777-6666-555555555555"

func managementNode(name, uuid string) *corev1.Node {
	return &corev1.Node{Name: name, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: strings.ToLower(uuid)}}}
}

func zonesTokenSecret() *corev1.Secret {
	return &corev1.Secret{
		Name: zonesSecret, Namespace: backupstore.Namespace,
		Data: map[string][]byte{zonesKey: []byte("mgmt-management-zones@pve!zones=00000000-0000-0000-0000-000000000000")},
	}
}

func zoneOf(t *testing.T, kube kubernetes.Interface, node string) string {
	t.Helper()
	n, err := kube.CoreV1().Nodes().Get(t.Context(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n.Labels[corev1.LabelTopologyZone]
}

// A VM on a Proxmox VE node that is down leaves the nodes on healthy hosts working: their zone
// labels follow, the store wakes and the backup volume is detached. Only the node on the host that
// is down stays as it is, and a detach from it names the VM that could not be read.
func TestAVMOnANodeThatIsDownLeavesTheOthersWorking(t *testing.T) {
	t.Parallel()
	f := withNodeVM(backupDisk)
	f.vms = append(f.vms, &fakeVM{id: 1500, name: "mgmt-c", node: "pve3", uuid: offlineUUID, offline: true})
	a := f.appliance(t)
	store := storeDeployment(0, "0")
	store.Annotations[volumeZone] = "pve2"
	store.Status.ReadyReplicas = 1
	kube := fake.NewClientset(managementNode("cp-0", nodeUUID), managementNode("cp-2", offlineUUID), zonesTokenSecret(), store)
	ctx := t.Context()

	unknown, err := a.followZones(ctx, kube, "pve2")
	if err != nil || !slices.Equal(unknown, []string{"cp-2"}) {
		t.Fatalf("unknown %v, error %v, want cp-2 alone and no error", unknown, err)
	}
	if zone := zoneOf(t, kube, "cp-0"); zone != "pve2" {
		t.Fatalf("cp-0's zone %q, want pve2", zone)
	}
	if err = a.wakeStore(ctx, kube); err != nil {
		t.Fatal(err)
	}
	if r := replicasOf(t, kube); r != 1 {
		t.Fatalf("the store has %d replicas after the wake, want 1", r)
	}
	if err = f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0"); err != nil {
		t.Fatal(err)
	}
	if got := f.callList(); !slices.Equal(got, detachedCalls) {
		t.Fatalf("calls %v, want %v", got, detachedCalls)
	}
	err = a.detachVolume(ctx, ticket(), storeAttachment{Node: "cp-2", SystemUUID: offlineUUID, Volume: "region/pve3/local-lvm/vm-9000-disk-0"})
	if err == nil || !strings.Contains(err.Error(), "VM 1500 (mgmt-c) on pve3 could not be read") {
		t.Fatalf("error %v, want one naming VM 1500 as unreadable", err)
	}
}

// Two VMs with the node's SMBIOS UUID, as a restore without a new UUID leaves, stop the detach and
// the zone labels. The error names both VMs and the command that gives one a new UUID.
func TestTwoVMsWithTheNodesUUIDFailLoudly(t *testing.T) {
	t.Parallel()
	f := withNodeVM(backupDisk)
	f.vms = append(f.vms, &fakeVM{id: 1700, name: "mgmt-a-restored", node: "pve3", uuid: nodeUUID, running: true, drives: map[string]string{}})
	kube := fake.NewClientset(managementNode("cp-0", nodeUUID), zonesTokenSecret())
	_, zerr := f.appliance(t).followZones(t.Context(), kube, "")
	derr := f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0")
	for _, err := range []error{zerr, derr} {
		if err == nil || !strings.Contains(err.Error(), "VM 1001 (mgmt-a) on pve2 and VM 1700 (mgmt-a-restored) on pve3") ||
			!strings.Contains(err.Error(), "qm set <id> --smbios1 uuid=<new>") {
			t.Fatalf("error %v, want both VMs and the fix named", err)
		}
	}
	if got := f.callList(); len(got) > 0 {
		t.Fatalf("calls %v with two VMs of one UUID, want none", got)
	}
	if zone := zoneOf(t, kube, "cp-0"); zone != "" {
		t.Fatalf("cp-0 got zone %q with two VMs of its UUID", zone)
	}
}

// A stopped copy restored with the node's UUID is never the node's VM. With the node's own VM
// unreadable on a Proxmox VE node that is down, the detach fails naming it and cp-0's zone stays.
// With the node's VM readable, the detach takes it.
func TestAStoppedCopyIsNeverTheNodesVM(t *testing.T) {
	t.Parallel()
	copied := func() *nodeVMs {
		f := withNodeVM(backupDisk)
		f.vms = append(f.vms, &fakeVM{id: 1700, name: "mgmt-a-restored", node: "pve3", uuid: nodeUUID, drives: map[string]string{}})
		return f
	}
	f := copied()
	f.vm(1001).offline = true
	kube := fake.NewClientset(managementNode("cp-0", nodeUUID), zonesTokenSecret())
	unknown, err := f.appliance(t).followZones(t.Context(), kube, "")
	if err != nil || !slices.Equal(unknown, []string{"cp-0"}) {
		t.Fatalf("unknown %v, error %v, want cp-0 unknown", unknown, err)
	}
	if zone := zoneOf(t, kube, "cp-0"); zone != "" {
		t.Fatalf("cp-0 got zone %q from the stopped copy", zone)
	}
	err = f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0")
	if err == nil || !strings.Contains(err.Error(), "VM 1001 (mgmt-a) on pve2 could not be read") {
		t.Fatalf("error %v, want the node's unreadable VM named", err)
	}
	if got := f.callList(); len(got) > 0 {
		t.Fatalf("calls %v on the stopped copy, want none", got)
	}

	f = copied()
	if err = f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0"); err != nil {
		t.Fatal(err)
	}
	if got := f.callList(); !slices.Equal(got, detachedCalls) {
		t.Fatalf("calls %v, want %v on the running node VM", got, detachedCalls)
	}
}

// A lift that takes effect but loses its answer still ends with the protection set again. When
// that fails too, the error says how to set it.
func TestDetachVolumeSetsProtectionAgainWhenTheLiftsAnswerIsLost(t *testing.T) {
	t.Parallel()
	f := withNodeVM(backupDisk)
	f.liftAnswerLost = true
	if err := f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0"); err == nil || !strings.Contains(err.Error(), "lifting the protection") {
		t.Fatalf("error %v, want the lift's", err)
	}
	if p := f.vm(1001).protection; p != 1 {
		t.Fatalf("protection %d after a lift whose answer was lost, want 1", p)
	}
	f = withNodeVM(backupDisk)
	f.liftAnswerLost, f.failProtect = true, true
	err := f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0")
	if err == nil || !strings.Contains(err.Error(), "qm set 1001 --protection 1") {
		t.Fatalf("error %v, want the command that sets protection", err)
	}
}

// A VM the CSI plugin lifted, with its marker, while the appliance read it is the plugin's to
// protect again.
func TestDetachVolumeLeavesAVMThePluginLiftedToIt(t *testing.T) {
	t.Parallel()
	f := withNodeVM(backupDisk)
	f.pluginLiftsFirst = true
	if err := f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0"); err == nil {
		t.Fatal("a lift against a changed configuration went through")
	}
	if calls := f.callList(); !slices.Equal(calls, []string{"1001 protection=0"}) {
		t.Fatalf("calls %q, want the refused lift alone", calls)
	}
}

// An unprotected VM is unlinked without any change to its protection.
func TestDetachVolumeLeavesAnUnprotectedVMUnprotected(t *testing.T) {
	t.Parallel()
	f := withNodeVM(backupDisk)
	f.vms[1].protection = 0
	if err := f.detachHandle(t, "region/pve2/local-lvm/vm-9000-disk-0"); err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); !slices.Equal(calls, []string{"1001 unlink scsi1"}) {
		t.Fatalf("calls %q, want the unlink alone", calls)
	}
}

func TestDriveKey(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]bool{
		"scsi0": true, "scsi1": true, "scsi30": true,
		"scsi": false, "scsihw": false, "scsi1a": false, "unused0": false, "virtio0": false,
		"sata0": false, "ide2": false, "efidisk0": false, "description": false,
	} {
		if got := driveKey(key); got != want {
			t.Errorf("driveKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// A removal stops the store before its destroy, which would otherwise uninstall the CSI plugin
// under a pod still mounting the volume.
func TestRemoveStopsTheStoreBeforeTheDestroy(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if j := applyWith(t, a, "pve1"); j.State != StateSucceeded {
		t.Fatalf("apply: %+v", j)
	}
	if err := os.WriteFile(filepath.Join(a.workdir(), "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	management := fake.NewClientset(storeObjects(1)...)
	stoppedAfter := -1
	management.PrependReactor("patch", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		stoppedAfter = len(tofuCalls(t, a))
		return false, nil, nil
	})
	a.ManagementClient = func([]byte) (kubernetes.Interface, error) { return management, nil }
	if err := a.Remove("correct horse battery staple", KeepBackups, ticket(), func(context.Context, *Job) error { return nil }, func() {}); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("remove: %+v", j)
	}
	if r := replicasOf(t, management); r != 0 {
		t.Fatalf("the store has %d replicas after the removal, want 0", r)
	}
	if stoppedAfter < 0 {
		t.Fatal("the store was never stopped")
	}
	for i, args := range tofuCalls(t, a) {
		if slices.Contains(args, "-destroy") && i < stoppedAfter {
			t.Fatalf("the destroy (tofu call %d) ran before the store was stopped (after call %d)", i, stoppedAfter)
		}
	}
}
