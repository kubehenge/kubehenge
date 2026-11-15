package backupstore

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// The controller asks for the store through an annotation, reaches it once its pod is ready, and
// takes the request away when done. The store says how much room the volume has.
func TestTheControllerAsksForTheVolume(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer((&Server{Dir: t.TempDir(), Token: "secret", DiskUse: func(string) (uint64, uint64, error) {
		return 15 << 30, 5 << 30, nil
	}}).Handler())
	defer srv.Close()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	kube := fake.NewClientset(
		&appsv1.Deployment{Name: Service, Namespace: Namespace, Annotations: map[string]string{"kubehenge.dev/idle-replicas": "0"}},
		&corev1.Secret{Name: TokenSecret, Namespace: Namespace, Data: map[string][]byte{TokenKey: []byte("secret")}},
	)
	v := &Volume{Kube: kube, Base: srv.URL, Now: func() time.Time { return now }}
	ctx := t.Context()
	deps := kube.AppsV1().Deployments(Namespace)
	if _, err := v.Open(ctx); !errors.Is(err, provisioner.ErrVolumeAsleep) {
		t.Fatalf("a sleeping store opened: %v", err)
	}
	d, err := deps.Get(ctx, Service, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !WakeRequested(d, now) || WakeRequested(d, now.Add(WakeRequestLife)) {
		t.Fatalf("the request reads %v", d.Annotations)
	}
	d.Status.ReadyReplicas = 1
	if _, err = deps.UpdateStatus(ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	store, err := v.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	free, size, err := store.Room(ctx)
	if err != nil || free != 5<<30 || size != 20<<30 {
		t.Fatalf("room %d of %d: %v", free, size, err)
	}
	if err = v.Done(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _ = deps.Get(ctx, Service, metav1.GetOptions{}); WakeRequested(d, now) || d.Annotations["kubehenge.dev/idle-replicas"] != "0" {
		t.Fatalf("after the snapshot the store's annotations are %v", d.Annotations)
	}
	// The interface's own request outlives the controller's.
	ui := &Volume{Kube: kube, Base: srv.URL, Annotation: InterfaceWakeAnnotation, Now: v.Now}
	if _, err = ui.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err = v.Done(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _ = deps.Get(ctx, Service, metav1.GetOptions{}); !WakeRequested(d, now) {
		t.Fatal("taking the controller's request away dropped the interface's")
	}
}
