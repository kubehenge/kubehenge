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
	"strings"
	"sync"
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

const shortStop = 50 * time.Millisecond

const backupDisk = "local-lvm:vm-9000-disk-0"

// storeObjects is a running store with its claim and volume.
func storeObjects() []runtime.Object {
	labels := map[string]string{"app": backupstore.Service}
	replicas := int32(1)
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
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.proxmox.sinextra.dev", VolumeHandle: "region/pve1/" + strings.Replace(backupDisk, ":", "/", 1)},
			}},
		},
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

// nodeVMs answers as Proxmox VE does for node VM cp-0, protected and holding the backup disk, and
// records each change.
type nodeVMs struct {
	mu         sync.Mutex
	calls      []string
	failUnlink bool
}

func (f *nodeVMs) appliance(t *testing.T) *Appliance {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		vm := "/api2/json/nodes/pve2/qemu/1001"
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cluster/resources"):
			_, _ = w.Write([]byte(`{"data":[` +
				`{"vmid":1000,"name":"cp-0","node":"pve1","type":"lxc"},` +
				`{"vmid":1001,"name":"cp-0","node":"pve2","type":"qemu"},` +
				`{"vmid":1002,"name":"cp-1","node":"pve1","type":"qemu"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == vm+"/config":
			_, _ = fmt.Fprintf(w, `{"data":{"digest":"d","protection":1,"scsi0":"local-lvm:vm-1001-disk-0","scsi1":"%s,backup=0"}}`, backupDisk)
		case r.Method == http.MethodPut && (r.URL.Path == vm+"/config" || r.URL.Path == vm+"/unlink"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if strings.HasSuffix(r.URL.Path, "/unlink") {
				f.calls = append(f.calls, fmt.Sprintf("unlink %v", body["idlist"]))
				if f.failUnlink {
					http.Error(w, "unable to unlink the drive", http.StatusInternalServerError)
					return
				}
			} else {
				f.calls = append(f.calls, fmt.Sprintf("protection=%v", body["protection"]))
			}
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Appliance{ProxmoxURL: srv.URL, ProxmoxCAFile: ca}
}

func (a *Appliance) detachWith(t *testing.T) func(context.Context, storeAttachment) error {
	t.Helper()
	return func(ctx context.Context, at storeAttachment) error { return a.detachVolume(ctx, ticket(), at) }
}

// A destroy starts only once the store is stopped, with no pod left and its volume detached.
func TestStopStoreScalesTheStoreToZero(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(append(storeObjects(), attachment("another-volume"))...)
	if err := stopStore(t.Context(), &Job{}, kube, shortStop, noDetach(t)); err != nil {
		t.Fatal(err)
	}
	if r := replicasOf(t, kube); r != 0 {
		t.Fatalf("replicas %d, want 0", r)
	}
}

// A store whose pod does not go fails the removal and runs on.
func TestStopStoreWaitsForThePod(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(append(storeObjects(), storePod())...)
	err := stopStore(t.Context(), &Job{}, kube, shortStop, noDetach(t))
	if err == nil || !strings.Contains(err.Error(), "pod store-0 is still there") {
		t.Fatalf("error %v, want the pod named", err)
	}
	if r := replicasOf(t, kube); r != 1 {
		t.Fatalf("replicas %d after the failed stop, want 1", r)
	}
}

// A volume the CSI plugin leaves attached is unlinked from its node VM with the protection lifted
// for the unlink alone.
func TestStopStoreDetachesWhatThePluginLeft(t *testing.T) {
	t.Parallel()
	f := &nodeVMs{}
	kube := fake.NewClientset(append(storeObjects(), attachment(backupstore.Claim))...)
	if err := stopStore(t.Context(), &Job{}, kube, shortStop, f.appliance(t).detachWith(t)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"protection=0", "unlink scsi1", "protection=1"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
	if r := replicasOf(t, kube); r != 0 {
		t.Fatalf("replicas %d, want 0", r)
	}
}

// A detach that fails names the VM and the disk, sets the protection again and starts the store
// again.
func TestStopStoreStartsTheStoreAgainWhenTheDetachFails(t *testing.T) {
	t.Parallel()
	f := &nodeVMs{failUnlink: true}
	kube := fake.NewClientset(append(storeObjects(), attachment(backupstore.Claim))...)
	err := stopStore(t.Context(), &Job{}, kube, shortStop, f.appliance(t).detachWith(t))
	if err == nil || !strings.Contains(err.Error(), "VM 1001 (cp-0)") || !strings.Contains(err.Error(), backupDisk) {
		t.Fatalf("error %v, want the VM and the disk named", err)
	}
	if want := []string{"protection=0", "unlink scsi1", "protection=1"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
	if r := replicasOf(t, kube); r != 1 {
		t.Fatalf("replicas %d after the failed detach, want 1", r)
	}
}

// A management cluster that does not answer is removed all the same, as the interface allows once
// its VMs are gone.
func TestStopStoreOnAClusterThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(storeObjects()...)
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
	management := fake.NewClientset(storeObjects()...)
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

func TestStopStoreWithoutAStore(t *testing.T) {
	t.Parallel()
	if err := stopStore(t.Context(), &Job{}, fake.NewClientset(), shortStop, noDetach(t)); err != nil {
		t.Fatal(err)
	}
}
