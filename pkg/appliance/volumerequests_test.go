package appliance

import (
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
)

// The controller asks for the sleeping store to store the etcd snapshots of clusters without a
// bucket. The appliance wakes it with its own wake, keeps it awake through its own backup while the
// request holds, and puts it back to sleep once the request is gone or has run out.
func TestTheApplianceWakesTheStoreTheControllerAskedFor(t *testing.T) {
	t.Parallel()
	a := &Appliance{Dir: t.TempDir(), vmHosts: hostsAt(t, nil)}
	if err := os.MkdirAll(a.etcdBackupDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(a.etcdBackupFile("volume.json"), &volumeBackupKeys{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	asleep := storeDeployment(0, "0")
	kube := fake.NewClientset(asleep, zonesToken())
	// The store's pod is ready as soon as it is started.
	kube.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := kube.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("deployments"), backupstore.Namespace, backupstore.Service)
		if err != nil {
			return true, nil, err
		}
		d, ok := obj.(*appsv1.Deployment)
		if !ok {
			return true, nil, fmt.Errorf("the tracker holds %T", obj)
		}
		d = d.DeepCopy()
		d.Status.ReadyReplicas = *d.Spec.Replicas
		return true, d, nil
	})
	deps := kube.AppsV1().Deployments(backupstore.Namespace)
	ctx := t.Context()
	replicas := func() int32 {
		t.Helper()
		d, err := deps.Get(ctx, backupstore.Service, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return *d.Spec.Replicas
	}
	ask := func(at string) {
		t.Helper()
		d, err := deps.Get(ctx, backupstore.Service, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		key := backupstore.WakeRequestKey(backupstore.WakeRequestAnnotation, "snapshots")
		if at == "" {
			delete(d.Annotations, key)
		} else {
			d.Annotations[key] = at
		}
		if _, err := deps.Update(ctx, d, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	if err := a.ServeVolumeRequests(ctx, kube, now); err != nil || replicas() != 0 {
		t.Fatalf("with no request the store runs %d replicas: %v", replicas(), err)
	}
	ask(now.Format(time.RFC3339))
	if err := a.ServeVolumeRequests(ctx, kube, now); err != nil || replicas() != 1 {
		t.Fatalf("the controller's request left the store at %d replicas: %v", replicas(), err)
	}
	// The appliance's own backup ends with the store asked for, and leaves it awake.
	if err := restStore(ctx, kube, now.Add(time.Minute)); err != nil || replicas() != 1 {
		t.Fatalf("a backup put the store the controller holds to %d replicas: %v", replicas(), err)
	}
	// A request the controller never took away runs out.
	if err := a.ServeVolumeRequests(ctx, kube, now.Add(backupstore.WakeRequestLife+time.Minute)); err != nil || replicas() != 0 {
		t.Fatalf("a request that ran out keeps the store at %d replicas: %v", replicas(), err)
	}
	ask(now.Format(time.RFC3339))
	if err := a.ServeVolumeRequests(ctx, kube, now); err != nil || replicas() != 1 {
		t.Fatalf("a second request left the store at %d replicas: %v", replicas(), err)
	}
	ask("")
	if err := a.ServeVolumeRequests(ctx, kube, now); err != nil || replicas() != 0 {
		t.Fatalf("once the request is gone the store keeps %d replicas: %v", replicas(), err)
	}
}
