package diagnose

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/redact"
)

var (
	talosConfigGVK    = schema.GroupVersionKind{Group: "bootstrap.cluster.x-k8s.io", Version: "v1alpha3", Kind: "TalosConfig"}
	proxmoxMachineGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha2", Kind: "ProxmoxMachine"}
)

func unstructuredOf(gvk schema.GroupVersionKind, name, cluster string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: fields}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace("team-a")
	u.SetName(name)
	u.SetLabels(map[string]string{clusterv1.ClusterNameLabel: cluster})
	return u
}

func machine(name, cluster string) *clusterv1.Machine {
	return &clusterv1.Machine{Namespace: "team-a", Name: name, Labels: map[string]string{clusterv1.ClusterNameLabel: cluster}}
}

// controllerLog is the controller's log as it reads: a line about web's machine, one about the
// cluster web in another namespace, one about web-2, whose name starts the same, and one carrying
// a password.
const controllerLog = `{"msg":"reconciling","Cluster":{"name":"web","namespace":"team-a"}}
{"msg":"reconciling","Cluster":{"name":"web","namespace":"team-b"}}
{"msg":"waiting","machine":"team-a/web-2-cp-a"}
{"msg":"VM cloned","machine":"team-a/web-cp-a","password":"hunter2"}
`

// newSource is a management cluster holding cluster web in team-a, its machine and its VM, its
// TalosConfig with the talosconfig CABPT writes into its status, a Secret of it, and cluster web-2
// beside it, and the controller's pod, whose log is controllerLog.
func newSource(t *testing.T) *Source {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	for _, gvk := range []schema.GroupVersionKind{talosConfigGVK, proxmoxMachineGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	objs := []runtime.Object{
		&clusterv1.Cluster{Namespace: "team-a", Name: "web"},
		&clusterv1.Cluster{Namespace: "team-a", Name: "web-2"},
		machine("web-cp-a", "web"),
		machine("web-2-cp-a", "web-2"),
		unstructuredOf(proxmoxMachineGVK, "web-cp-a", "web", map[string]any{"spec": map[string]any{"virtualMachineID": int64(101)}}),
		unstructuredOf(talosConfigGVK, "web-cp-a", "web", map[string]any{"status": map[string]any{"talosConfig": "context: web\ncontexts: {}\n"}}),
		&corev1.Secret{
			Namespace: "team-a", Name: "web-kubeconfig", Labels: map[string]string{clusterv1.ClusterNameLabel: "web"},
			Data: map[string][]byte{"value": []byte("secret kubeconfig")},
		},
		&corev1.ConfigMap{
			Namespace: "team-a", Name: provisioner.BootLogsConfigMap("web"),
			Data: map[string]string{"web-cp-a": `{"read":"2026-10-28T10:00:00Z","error":"VM 101 is not running"}`},
		},
		&corev1.Event{
			Namespace: "team-a", Name: "e1",
			InvolvedObject: corev1.ObjectReference{Kind: "Machine", Name: "web-cp-a"}, Reason: "Failed", Message: "token: abc", Type: "Warning",
		},
		&corev1.Event{
			Namespace: "team-a", Name: "e2",
			InvolvedObject: corev1.ObjectReference{Kind: "Machine", Name: "web-2-cp-a"}, Reason: "Other", Message: "of web-2",
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	cs := kubefake.NewClientset(&corev1.Pod{
		Namespace: "kubehenge-system", Name: "controller-1", Labels: map[string]string{"app.kubernetes.io/name": "kubehenge-controller"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "controller"}}},
	})
	cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "log" {
			return false, nil, nil
		}
		return true, &runtime.Unknown{Raw: []byte(controllerLog)}, nil
	})
	cs.Resources = []*metav1.APIResourceList{
		{GroupVersion: "cluster.x-k8s.io/v1beta2", APIResources: []metav1.APIResource{
			{Name: "clusters", Kind: "Cluster", Namespaced: true, Verbs: []string{"get", "list"}},
			{Name: "machines", Kind: "Machine", Namespaced: true, Verbs: []string{"get", "list"}},
			{Name: "machines/status", Kind: "Machine", Namespaced: true, Verbs: []string{"get"}},
		}},
		{GroupVersion: "infrastructure.cluster.x-k8s.io/v1alpha2", APIResources: []metav1.APIResource{
			{Name: "proxmoxmachines", Kind: "ProxmoxMachine", Namespaced: true, Verbs: []string{"get", "list"}},
		}},
		{GroupVersion: "bootstrap.cluster.x-k8s.io/v1alpha3", APIResources: []metav1.APIResource{
			{Name: "talosconfigs", Kind: "TalosConfig", Namespaced: true, Verbs: []string{"get", "list"}},
		}},
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: []string{"get", "list"}},
		}},
	}
	return &Source{
		Kube: kube, Clientset: cs, ControllerNamespace: "kubehenge-system",
		Now: func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) },
	}
}

// untar reads a bundle's files by their names inside its directory.
func untar(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		name, ok := strings.CutPrefix(hdr.Name, "team-a-web/")
		if !ok {
			t.Errorf("%s is outside the bundle's directory", hdr.Name)
		}
		files[name] = string(data)
	}
}

func TestWriteHoldsTheClusterAndNoSecret(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, newSource(t), "team-a", "web"); err != nil {
		t.Fatal(err)
	}
	files := untar(t, buf.Bytes())
	for _, want := range []string{
		"objects/cluster.x-k8s.io/Cluster/web.yaml",
		"objects/cluster.x-k8s.io/Machine/web-cp-a.yaml",
		"objects/infrastructure.cluster.x-k8s.io/ProxmoxMachine/web-cp-a.yaml",
		"objects/bootstrap.cluster.x-k8s.io/TalosConfig/web-cp-a.yaml",
		"boot-logs.yaml", "events.yaml", "manifest.txt",
		"logs/kubehenge-system/controller-1_controller.log",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("no %s in %v", want, keys(files))
		}
	}
	for name, data := range files {
		if strings.Contains(name, "web-2") || strings.Contains(name, "Secret") {
			t.Errorf("the bundle holds %s", name)
		}
		for _, secret := range []string{"secret kubeconfig", "hunter2", "context: web", "token: abc"} {
			if strings.Contains(data, secret) {
				t.Errorf("%s holds %q", name, secret)
			}
		}
	}
	if !strings.Contains(files["objects/bootstrap.cluster.x-k8s.io/TalosConfig/web-cp-a.yaml"], redact.Mark) {
		t.Error("the TalosConfig's talosconfig was not replaced")
	}
	if events := files["events.yaml"]; !strings.Contains(events, "Machine/web-cp-a") || strings.Contains(events, "of web-2") {
		t.Errorf("events = %s", events)
	}
	if !strings.Contains(files["boot-logs.yaml"], "VM 101 is not running") {
		t.Errorf("boot logs = %s", files["boot-logs.yaml"])
	}
	if !strings.Contains(files["manifest.txt"], "logs/kubehenge-system/controller-1_controller.log") {
		t.Errorf("manifest = %s", files["manifest.txt"])
	}
}

// The controller's log keeps only the lines naming web's own objects: not the cluster of the same
// name in another namespace, nor web-2's machine, whose name starts like web's.
func TestWriteKeepsOnlyTheClustersLogLines(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, newSource(t), "team-a", "web"); err != nil {
		t.Fatal(err)
	}
	got := untar(t, buf.Bytes())["logs/kubehenge-system/controller-1_controller.log"]
	want := `{"msg":"reconciling","Cluster":{"name":"web","namespace":"team-a"}}
{"msg":"VM cloned","machine":"team-a/web-cp-a","password":"[REDACTED]"}
`
	if got != want {
		t.Errorf("log =\n%s\nwant\n%s", got, want)
	}
}

func TestWriteFailsWithoutTheCluster(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, newSource(t), "team-a", "gone"); err == nil {
		t.Error("wrote a bundle of a cluster that does not exist")
	}
	if buf.Len() != 0 {
		t.Error("wrote part of a bundle")
	}
}

func TestWriteTakesTheAppliancesJournal(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	src.Journal = func(context.Context) ([]byte, error) {
		return []byte(`2026-10-28T10:00:00+00:00 kubehenge[1]: {"msg":"changed cluster","cluster":"team-a/web"}
2026-10-28T10:00:01+00:00 kubehenge[1]: {"msg":"changed cluster","cluster":"team-a/web-2"}
`), nil
	}
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, src, "team-a", "web"); err != nil {
		t.Fatal(err)
	}
	got := untar(t, buf.Bytes())["logs/appliance/kubehenge.log"]
	if !strings.Contains(got, `"team-a/web"`) || strings.Contains(got, "web-2") {
		t.Errorf("journal = %s", got)
	}
}

func TestWriteWithoutAClientsetSaysSo(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	src.Clientset = nil
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, src, "team-a", "web"); err != nil {
		t.Fatal(err)
	}
	if m := untar(t, buf.Bytes())["manifest.txt"]; !strings.Contains(m, "Not collected") {
		t.Errorf("manifest = %s", m)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
