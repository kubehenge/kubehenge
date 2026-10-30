package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/redact"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// bootFiles holds each VM's files, of those that run; a file it lacks is read as the guest agent
// fails to open one, and every read fails with refused.
type bootFiles struct {
	files   map[int64]map[string]string
	off     map[int64]bool
	refused error
	// long are files longer than any read.
	long map[string]bool
}

func (g *bootFiles) Running(_ context.Context, vmid int64) (bool, error) { return !g.off[vmid], nil }

func (g *bootFiles) ReadFile(_ context.Context, vmid int64, file string, limit int) (string, bool, error) {
	if g.refused != nil {
		return "", false, g.refused
	}
	if limit < 1<<20 {
		return "", false, fmt.Errorf("read %d bytes of %s, short of the whole file", limit, file)
	}
	content, ok := g.files[vmid][file]
	if !ok {
		return "", false, fmt.Errorf("VM %d: guest-file-open: %s: No such file or directory", vmid, file)
	}
	return content, g.long[file], nil
}

// bootTalos is a Talos node whose services each log one line, and whose kernel messages cannot be read.
type bootTalos struct{ address string }

func (*bootTalos) Dmesg(context.Context) ([]string, error) {
	return nil, errors.New("permission denied")
}

func (n *bootTalos) ServiceLog(_ context.Context, service string) ([]string, error) {
	return []string{n.address + " " + service + " started"}, nil
}

func (*bootTalos) Close() error { return nil }

func bootLogKube(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	scheme.AddKnownTypeWithName(proxmoxMachineGVK, &unstructured.Unstructured{})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func readBootLogs(t *testing.T, kube client.Client) map[string]BootLog {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-boot-logs"}, cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Kind != "Cluster" || cm.OwnerReferences[0].Name != "web" {
		t.Errorf("owners = %v", cm.OwnerReferences)
	}
	logs := map[string]BootLog{}
	for k, v := range cm.Data {
		var l BootLog
		if err := json.Unmarshal([]byte(v), &l); err != nil {
			t.Fatal(err)
		}
		logs[k] = l
	}
	return logs
}

func TestBootLogsAreReadThroughTheGuestAgent(t *testing.T) {
	t.Parallel()
	c := cluster()
	c.UID = "uid-web"
	c.Annotations = map[string]string{BootLogRequestAnnotation: "web-cp-a, web-w-1,ghost"}
	cp, cpVM := controlPlane("web-cp-a", 101, "pve1")
	worker, workerVM := controlPlane("web-w-1", 102, "pve2")
	other, otherVM := controlPlane("other-cp-a", 103, "pve1")
	other.Labels[clusterv1.ClusterNameLabel] = "other"
	kube := bootLogKube(t, c, cp, cpVM, worker, workerVM, other, otherVM)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	r := &BootLogReconciler{
		Client: kube, Reader: kube, ClassNamespace: "kubehenge-system", Now: func() time.Time { return now },
		Files: &bootFiles{
			files: map[int64]map[string]string{101: {
				templates.CloudInitOutput: "Cloud-init v. 24.4 running\nCloud-init finished\n",
				templates.BootLogFile: templates.BootLogMark + "journal of cloud-init\ncloud-final done\n" +
					templates.BootLogMark + "journal of rke2-server\nkubeadm join --token abcdef.0123456789abcdef\nrke2 waiting\n",
			}},
			off: map[int64]bool{102: true},
		},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	logs := readBootLogs(t, kube)
	got := logs["web-cp-a"]
	names := make([]string, 0, len(got.Sections))
	for _, s := range got.Sections {
		names = append(names, s.Name)
	}
	if got.Error != "" || !slices.Equal(names, []string{"cloud-init-output.log", "journal of cloud-init", "journal of rke2-server"}) {
		t.Fatalf("web-cp-a = %+v", got)
	}
	if lines := got.Sections[0].Lines; !slices.Equal(lines, []string{"Cloud-init v. 24.4 running", "Cloud-init finished"}) {
		t.Errorf("cloud-init's output = %q", lines)
	}
	if lines := strings.Join(got.Sections[2].Lines, "\n"); strings.Contains(lines, "abcdef.0123456789abcdef") || !strings.Contains(lines, "rke2 waiting") {
		t.Errorf("rke2's journal = %q", lines)
	}
	if !got.Read.Equal(now) {
		t.Errorf("read %v", got.Read)
	}
	if e := logs["web-w-1"].Error; e != "VM 102 is not running" {
		t.Errorf("web-w-1: %q", e)
	}
	if e := logs["ghost"].Error; e != "the cluster has no machine ghost" {
		t.Errorf("ghost: %q", e)
	}
	after := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), after); err != nil {
		t.Fatal(err)
	}
	if v, ok := after.Annotations[BootLogRequestAnnotation]; ok {
		t.Errorf("the request stayed: %q", v)
	}
}

// Another cluster's machine is not read, though its name is asked for.
func TestBootLogsOfAnotherClustersMachineAreNotRead(t *testing.T) {
	t.Parallel()
	c := cluster()
	c.Annotations = map[string]string{BootLogRequestAnnotation: "other-cp-a"}
	other, otherVM := controlPlane("other-cp-a", 103, "pve1")
	other.Labels[clusterv1.ClusterNameLabel] = "other"
	kube := bootLogKube(t, c, other, otherVM)
	r := &BootLogReconciler{Client: kube, Reader: kube, ClassNamespace: "kubehenge-system", Files: &bootFiles{files: map[int64]map[string]string{103: {templates.CloudInitOutput: "secret"}}}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	if got := readBootLogs(t, kube)["other-cp-a"]; got.Error != "the cluster has no machine other-cp-a" || len(got.Sections) != 0 {
		t.Errorf("got %+v", got)
	}
}

// readBootLog reads web-cp-a's boot log through files, or with none when files is nil.
func readBootLog(t *testing.T, files GuestFiles) BootLog {
	t.Helper()
	c := cluster()
	c.Annotations = map[string]string{BootLogRequestAnnotation: "web-cp-a"}
	cp, cpVM := controlPlane("web-cp-a", 101, "pve1")
	kube := bootLogKube(t, c, cp, cpVM)
	r := &BootLogReconciler{Client: kube, Reader: kube, ClassNamespace: "kubehenge-system", Files: files}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	return readBootLogs(t, kube)["web-cp-a"]
}

func TestBootLogsSwitchedOff(t *testing.T) {
	t.Parallel()
	if e := readBootLog(t, nil).Error; !strings.Contains(e, "switched off") || !strings.Contains(e, "node_boot_logs") {
		t.Errorf("error = %q", e)
	}
}

// A token without VM.GuestAgent.FileRead is told apart from a node that lacks the files.
func TestBootLogsWithoutTheFileReadPrivilege(t *testing.T) {
	t.Parallel()
	got := readBootLog(t, &bootFiles{refused: fmt.Errorf("VM 101: reading %s through the guest agent: %w", templates.CloudInitOutput, proxmox.ErrNotAuthorized)})
	if !strings.Contains(got.Error, "VM.GuestAgent.FileRead") || len(got.Sections) != 0 {
		t.Errorf("got %+v", got)
	}
}

// Each file that cannot be read says why beside the other, and an output longer than the read
// says its lines are not the end.
func TestBootLogsReadWhatTheNodeHas(t *testing.T) {
	t.Parallel()
	got := readBootLog(t, &bootFiles{
		files: map[int64]map[string]string{101: {templates.CloudInitOutput: "first boot\nsecond boot\n"}},
		long:  map[string]bool{templates.CloudInitOutput: true},
	})
	if got.Error != "" || len(got.Sections) != 2 {
		t.Fatalf("got %+v", got)
	}
	if s := got.Sections[0]; !strings.Contains(s.Error, "longer than the 8 MiB read") || !slices.Equal(s.Lines, []string{"first boot", "second boot"}) {
		t.Errorf("cloud-init's output = %+v", s)
	}
	if s := got.Sections[1]; s.Name != "journals" || !strings.Contains(s.Error, "No such file or directory") {
		t.Errorf("journals = %+v", s)
	}
}

// The guest agent's file reads go to the node the VM is on, and the number Proxmox VE sends for a
// file longer than the read counts as truncated.
func TestProxmoxGuestFilesReadsThroughTheVMsNode(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api2/json/cluster/resources":
			_, _ = w.Write([]byte(`{"data":[{"vmid":101,"node":"pve2","status":"running"}]}`))
		case r.URL.Path == "/api2/json/nodes/pve2/qemu/101/agent/file-read" &&
			r.URL.Query().Get("file") == templates.BootLogFile && r.URL.Query().Get("count") == "2097152":
			_, _ = w.Write([]byte(`{"data":{"content":"lines","truncated":1}}`))
		default:
			http.Error(w, `{"data":null}`, http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	g := &ProxmoxGuestFiles{Client: proxmox.NewClient(srv.URL + "/api2/json")}
	content, truncated, err := g.ReadFile(context.Background(), 101, templates.BootLogFile, templates.BootLogRead)
	if err != nil || content != "lines" || !truncated {
		t.Errorf("read %q, truncated %v, error %v", content, truncated, err)
	}
	if _, _, err := g.ReadFile(context.Background(), 101, templates.CloudInitOutput, templates.BootLogRead); !proxmox.IsNotAuthorized(err) {
		t.Errorf("a refused read: %v", err)
	}
	if _, _, err := g.ReadFile(context.Background(), 102, templates.BootLogFile, templates.BootLogRead); err == nil {
		t.Error("read a VM that is not there")
	}
}

func TestBootLogsOfTalosNodes(t *testing.T) {
	t.Parallel()
	c := cluster()
	c.Spec.Topology.ClassRef.Name = ClassPrefix + "talos"
	c.Annotations = map[string]string{BootLogRequestAnnotation: "web-cp-a,web-w-1,web-w-2"}
	cp, _ := controlPlane("web-cp-a", 101, "pve1")
	cp.Status.Addresses = clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: "192.0.2.21"}}
	worker, _ := controlPlane("web-w-1", 102, "pve1")
	delete(worker.Labels, clusterv1.MachineControlPlaneLabel)
	joined, _ := controlPlane("web-w-2", 103, "pve1")
	delete(joined.Labels, clusterv1.MachineControlPlaneLabel)
	joined.Status.Addresses = clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: "192.0.2.23"}}
	secret := &corev1.Secret{
		Namespace: "team-a", Name: TalosconfigSecret("web"),
		Data: map[string][]byte{TalosconfigKey: []byte("context: web")},
	}
	kube := bootLogKube(t, c, cp, worker, joined, secret)
	r := &BootLogReconciler{
		Client: kube, Reader: kube, ClassNamespace: "kubehenge-system",
		Talos: func(_ context.Context, raw []byte, address string) (TalosLogs, error) {
			if string(raw) != "context: web" {
				return nil, errors.New("wrong talosconfig")
			}
			return &bootTalos{address: address}, nil
		},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	logs := readBootLogs(t, kube)
	got := logs["web-cp-a"]
	if names := sectionNames(got); names != "kernel messages,machined,apid,kubelet,trustd,etcd" {
		t.Errorf("sections = %v", names)
	}
	if got.Sections[0].Error == "" || len(got.Sections[3].Lines) != 1 || got.Sections[3].Lines[0] != "192.0.2.21 kubelet started" {
		t.Errorf("web-cp-a = %+v", got)
	}
	if e := logs["web-w-1"].Error; e != "the machine has no address yet" {
		t.Errorf("web-w-1: %q", e)
	}
	// A worker runs neither trustd nor etcd, whose logs it would only answer with an error.
	if names := sectionNames(logs["web-w-2"]); names != "kernel messages,machined,apid,kubelet" {
		t.Errorf("web-w-2's sections = %v", names)
	}
}

func TestBootLogLinesHoldNoTerminalEscapesOrControls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"\x1b[0;32m  OK  \x1b[0m] Started k3s.service", "  OK  ] Started k3s.service"},
		{"\x1b]8;;file://node\x07link\x1b]8;;\x07", "link"},
		{"progress\r 50%\x7f\u0085\u009b", "progress 50%"},
		{"a\ttab and ✓ stay", "a\ttab and ✓ stay"},
		{"--token abc", "--token " + redact.Mark},
	} {
		if got := logLine(tc.in); got != tc.want {
			t.Errorf("logLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func sectionNames(log BootLog) string {
	names := make([]string, 0, len(log.Sections))
	for _, s := range log.Sections {
		names = append(names, s.Name)
	}
	return strings.Join(names, ",")
}

func TestTrimBootLogsDropsTheOldest(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", bootLogsBytes/2)
	entry := func(at int) string {
		raw, err := json.Marshal(BootLog{Read: time.Date(2026, 1, at, 0, 0, 0, 0, time.UTC), Error: big})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	data := map[string]string{"a": entry(1), "b": entry(3), "c": entry(2)}
	trimBootLogs(data)
	if _, ok := data["a"]; ok || len(data) != 1 || data["b"] == "" {
		t.Errorf("kept %d logs, a: %v, b: %v", len(data), data["a"] != "", data["b"] != "")
	}
}

func TestParseBootLogsKeepsTheEnd(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	out.WriteString(templates.BootLogMark + "journal of kubelet\n")
	for range 2 * bootLogSectionBytes / 100 {
		out.WriteString(strings.Repeat("y", 99) + "\n")
	}
	out.WriteString("the last line")
	s := parseBootLogs(out.String())
	if len(s) != 1 || s[0].Lines[len(s[0].Lines)-1] != "the last line" {
		t.Fatalf("got %d sections", len(s))
	}
	size := 0
	for _, l := range s[0].Lines {
		size += len(l) + 1
	}
	if size > bootLogSectionBytes {
		t.Errorf("kept %d bytes", size)
	}
}
