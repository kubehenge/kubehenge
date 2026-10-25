package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func TestKubeadmTemplateVersionsRecordedOnTheClass(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	tags := []string{
		"build-202610010000;kubeadm;kubehenge;kubernetes-v1.36.5;os-almalinux-10",
		"build-202610080000;kubeadm;kubehenge;kubernetes-v1.36.5;os-almalinux-10",
		"build-202610010000;kubeadm;kubehenge;kubernetes-v1.37.1;os-almalinux-10",
		"build-202610010000;kubeadm;kubehenge;kubernetes-v1.36.4;os-almalinux-10",
		// Not a build, which no class selects.
		"kubeadm;kubehenge;kubernetes-v1.38.1;os-almalinux-10",
		"build-202610010000;kubeadm;kubehenge;kubernetes-v1.38.0;os-rocky-10",
		// A node OS Kubehenge does not know.
		"build-202610010000;kubeadm;kubehenge;kubernetes-v1.38.0;os-plan9-4",
		"build-202610010000;kubehenge;os-almalinux-10;rke2",
		"build-202610010000;kubehenge;os-rocky-10;rke2",
		"build-202610010000;k3s;kubehenge;os-rocky-10",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = []any{}
		if r.URL.Path == "/api2/json/cluster/resources" {
			mu.Lock()
			vms := make([]map[string]any, 0, len(tags))
			for i, tag := range tags {
				vms = append(vms, map[string]any{"vmid": 9000 + i, "type": "qemu", "node": "pve1", "template": 1, "tags": tag})
			}
			mu.Unlock()
			data = vms
		} else if strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pve1/qemu/") && strings.HasSuffix(r.URL.Path, "/config") {
			data = map[string]any{"template": 1, "scsi0": "ceph:base-9000-disk-0,size=30G", "ide2": "none,media=cdrom"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	class := &clusterv1.ClusterClass{}
	class.Namespace, class.Name = "kubehenge-system", ClassPrefix+"kubeadm"
	rke2 := &clusterv1.ClusterClass{}
	rke2.Namespace, rke2.Name = "kubehenge-system", ClassPrefix+"rke2"
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, rke2).Build()
	p := &TemplateVersions{Client: kube, ClassNamespace: "kubehenge-system", Proxmox: proxmox.NewClient(srv.URL + "/api2/json")}
	ctx := context.Background()
	recorded := func(name, key string) (string, bool) {
		t.Helper()
		got := &clusterv1.ClusterClass{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: "kubehenge-system", Name: name}, got); err != nil {
			t.Fatal(err)
		}
		v, ok := got.Annotations[key]
		return v, ok
	}

	// The k3s class is not there, and is passed over.
	if err := p.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := recorded(class.Name, KubeadmVersionsAnnotation); got != `{"almalinux-10":["v1.37.1","v1.36.5","v1.36.4"],"rocky-10":["v1.38.0"]}` {
		t.Errorf("recorded %q", got)
	}
	if _, ok := recorded(rke2.Name, KubeadmVersionsAnnotation); ok {
		t.Error("the RKE2 class got kubeadm's versions")
	}
	if got, _ := recorded(rke2.Name, NodeOSTemplatesAnnotation); got != "almalinux-10,rocky-10" {
		t.Errorf("RKE2's templates recorded as %q", got)
	}
	for _, name := range []string{class.Name, rke2.Name} {
		if got, _ := recorded(name, TemplateStoragesAnnotation); got != "ceph" {
			t.Errorf("%s records the templates' storages as %q, want ceph", name, got)
		}
	}
	raw, _ := recorded(rke2.Name, NodeImageBuildsAnnotation)
	builds, err := ParseNodeImageBuilds(raw)
	if err != nil {
		t.Fatal(err)
	}
	line := "kubeadm;kubehenge;kubernetes-v1.36.5;" + templates.DefaultNodeOS.Tag()
	if got := builds[line]; len(got) != 2 || !got[0].Before(got[1]) || len(builds) != 7 {
		t.Errorf("builds recorded %s", raw)
	}
	if again, _ := recorded(class.Name, NodeImageBuildsAnnotation); again != raw {
		t.Errorf("the classes record the builds differently: %s and %s", raw, again)
	}
	if versions, err := ParseKubeadmVersions(`{"rocky-10":["v1.38.0"]}`); err != nil || versions["rocky-10"][0] != "v1.38.0" {
		t.Errorf("read back %v, %v", versions, err)
	}

	// The last template deleted: none, which is not the same as not looked at yet. A template of a
	// node OS Kubehenge does not know is no source for the classes.
	mu.Lock()
	tags = []string{"build-202610010000;kubehenge;os-plan9-4;rke2"}
	mu.Unlock()
	if err := p.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := recorded(class.Name, KubeadmVersionsAnnotation); !ok || got != "{}" {
		t.Errorf("recorded %q, %v; want none", got, ok)
	}
	if got, ok := recorded(rke2.Name, NodeOSTemplatesAnnotation); !ok || got != "" {
		t.Errorf("RKE2's templates recorded as %q once gone", got)
	}
}
