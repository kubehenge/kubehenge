package appliance

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
)

// The controller wakes the store for the clusters' etcd snapshots without the appliance, and the
// appliance wakes it for its own backups. Each holds it awake under its own request, so that a
// snapshot ending leaves it up through the appliance's backup, and it sleeps once both are done.
func TestTheApplianceAndTheControllerShareTheStore(t *testing.T) {
	t.Parallel()
	a := &Appliance{vmHosts: hostsAt(t, nil)}
	// The store's pod is ready as soon as it is started.
	store := storeDeployment(0, "0")
	store.Status.ReadyReplicas = 1
	kube := fake.NewClientset(store, zonesToken())
	ctx := t.Context()
	replicas := func() int32 {
		t.Helper()
		d, err := kube.AppsV1().Deployments(backupstore.Namespace).Get(ctx, backupstore.Service, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return *d.Spec.Replicas
	}
	controller := &backupstore.Volume{Kube: kube, VMs: hostsAt(t, nil)}
	const snapshots = "snapshots of a/web"
	if err := controller.Wake(ctx, snapshots); err == nil || replicas() != 1 {
		t.Fatalf("the controller's wake left the store at %d replicas: %v", replicas(), err)
	}
	if err := a.wakeStore(ctx, kube); err != nil {
		t.Fatal(err)
	}
	if err := controller.Done(ctx, snapshots); err != nil || replicas() != 1 {
		t.Fatalf("a snapshot ending put the store the appliance's backup holds to %d replicas: %v", replicas(), err)
	}
	if err := a.storeVolume(kube).Done(ctx, applianceHolder); err != nil || replicas() != 0 {
		t.Fatalf("with no holder left the store keeps %d replicas: %v", replicas(), err)
	}
}
