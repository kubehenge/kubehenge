package appliance

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
)

func storeDeployment(replicas int32, idle string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: backupstore.Service, Namespace: backupstore.Namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	if idle != "" {
		d.Annotations = map[string]string{idleReplicas: idle}
	}
	return d
}

// On a node's own storage the store sleeps, so that its disk is attached to a node VM only while a
// backup or recovery runs. A wake waits for the pod, and the store goes back to sleep after.
func TestTheStoreSleepsBetweenBackupsOnLocalStorage(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(storeDeployment(0, "0"))
	deps := kube.AppsV1().Deployments(backupstore.Namespace)
	ctx := t.Context()
	woke := make(chan error, 1)
	go func() { woke <- wakeStore(ctx, kube) }()
	var d *appsv1.Deployment
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		got, err := deps.Get(ctx, backupstore.Service, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if *got.Spec.Replicas == 1 {
			d = got
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sleeping store was not started")
		}
	}
	select {
	case err := <-woke:
		t.Fatalf("the wake returned before the store's pod was ready: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	d.Status.ReadyReplicas = 1
	if _, err := deps.UpdateStatus(ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := <-woke; err != nil {
		t.Fatal(err)
	}
	if err := restStore(ctx, kube); err != nil {
		t.Fatal(err)
	}
	if got, _ := deps.Get(ctx, backupstore.Service, metav1.GetOptions{}); *got.Spec.Replicas != 0 {
		t.Fatalf("after the backup the store keeps %d replicas, and its disk stays attached", *got.Spec.Replicas)
	}
}

// On shared storage the store stays up, and a wake that cannot see it start gives up with ctx.
func TestTheStoreStaysUpOnSharedStorage(t *testing.T) {
	t.Parallel()
	up := storeDeployment(1, "1")
	up.Status.ReadyReplicas = 1
	kube := fake.NewClientset(up)
	ctx := t.Context()
	if err := wakeStore(ctx, kube); err != nil {
		t.Fatal(err)
	}
	if err := restStore(ctx, kube); err != nil {
		t.Fatal(err)
	}
	if got, _ := kube.AppsV1().Deployments(backupstore.Namespace).Get(ctx, backupstore.Service, metav1.GetOptions{}); *got.Spec.Replicas != 1 {
		t.Fatalf("a store on shared storage was put to %d replicas", *got.Spec.Replicas)
	}

	stuck := fake.NewClientset(storeDeployment(0, "0"))
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := wakeStore(short, stuck); err == nil {
		t.Fatal("a store whose pod never got ready counts as woken")
	}
}
