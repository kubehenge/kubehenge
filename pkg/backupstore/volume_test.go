package backupstore

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// sleepingStore is the store on a node's own storage, asleep, with its token and the token that
// reads where the management VMs run.
func sleepingStore() []runtime.Object {
	var none int32
	return []runtime.Object{
		&appsv1.Deployment{
			Name: Service, Namespace: Namespace, Annotations: map[string]string{IdleReplicas: "0", VolumeZone: "pve1"},
			Spec: appsv1.DeploymentSpec{Replicas: &none},
		},
		&corev1.Secret{Name: TokenSecret, Namespace: Namespace, Data: map[string][]byte{TokenKey: []byte("secret")}},
		&corev1.Secret{Name: ZonesSecret, Namespace: Namespace, Data: map[string][]byte{"token": []byte("zones")}},
		&corev1.Node{
			Name: "mgmt-cp-0", Labels: map[string]string{corev1.LabelTopologyZone: "pve2"},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: "uuid-0"}},
		},
	}
}

// onPVE1 stands in for Proxmox VE, where the one management VM runs on pve1.
func onPVE1(_ context.Context, token string) (*VMIndex, error) {
	if token != "zones" {
		return nil, errors.New("not the zones token")
	}
	return &VMIndex{ByUUID: map[string][]VM{"uuid-0": {{VMID: 100, Name: "mgmt-cp-0", Node: "pve1", Type: "qemu"}}}}, nil
}

func replicasOf(t *testing.T, kube *fake.Clientset) int32 {
	t.Helper()
	d, err := kube.AppsV1().Deployments(Namespace).Get(t.Context(), Service, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

// The controller wakes the sleeping store itself, with no appliance: it points the node's zone at
// where its VM runs, starts the store, reaches it once its pod is ready, and puts it back to sleep
// when done. The store says how much room the volume has.
func TestTheControllerWakesTheVolume(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer((&Server{Dir: t.TempDir(), Token: "secret", DiskUse: func(string) (uint64, uint64, error) {
		return 15 << 30, 5 << 30, nil
	}}).Handler())
	defer srv.Close()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	kube := fake.NewClientset(sleepingStore()...)
	v := &Volume{Kube: kube, Base: srv.URL, VMs: onPVE1, Now: func() time.Time { return now }}
	ctx := t.Context()
	deps := kube.AppsV1().Deployments(Namespace)
	const holder = "snapshots of a/web"
	if _, err := v.Open(ctx, holder); !errors.Is(err, provisioner.ErrVolumeAsleep) {
		t.Fatalf("a sleeping store opened: %v", err)
	}
	if got := replicasOf(t, kube); got != 1 {
		t.Fatalf("the controller left the sleeping store at %d replicas", got)
	}
	if n, _ := kube.CoreV1().Nodes().Get(ctx, "mgmt-cp-0", metav1.GetOptions{}); n.Labels[corev1.LabelTopologyZone] != "pve1" {
		t.Fatalf("the store was started with its node labelled %v, not where its VM runs", n.Labels)
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
	store, err := v.Open(ctx, holder)
	if err != nil {
		t.Fatal(err)
	}
	free, size, err := store.Room(ctx)
	if err != nil || free != 5<<30 || size != 20<<30 {
		t.Fatalf("room %d of %d: %v", free, size, err)
	}
	// The interface's own request outlives the controller's.
	ui := &Volume{Kube: kube, Base: srv.URL, Annotation: InterfaceWakeAnnotation, Now: v.Now}
	if _, err = ui.Open(ctx, holder); err != nil {
		t.Fatal(err)
	}
	if err = v.Done(ctx, holder); err != nil {
		t.Fatal(err)
	}
	if d, _ = deps.Get(ctx, Service, metav1.GetOptions{}); !WakeRequested(d, now) || *d.Spec.Replicas != 1 {
		t.Fatal("taking the controller's request away dropped the interface's")
	}
	if err = ui.Done(ctx, holder); err != nil {
		t.Fatal(err)
	}
	if d, _ = deps.Get(ctx, Service, metav1.GetOptions{}); WakeRequested(d, now) || *d.Spec.Replicas != 0 {
		t.Fatalf("with no holder left the store keeps %d replicas and the annotations %v", *d.Spec.Replicas, d.Annotations)
	}
}

// A request its holder never took away runs out, and the next holder's Done puts the store back to
// sleep. A client that cannot read where the VMs run says so, and leaves the store asleep.
func TestALeftRequestRunsOut(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	kube := fake.NewClientset(sleepingStore()...)
	ctx := t.Context()
	blind := &Volume{Kube: kube, Now: func() time.Time { return now }}
	if err := blind.Wake(ctx, "a look"); err == nil || errors.Is(err, provisioner.ErrVolumeAsleep) || replicasOf(t, kube) != 0 {
		t.Fatalf("a wake that cannot read where the VMs run: %v", err)
	}
	gone := &Volume{Kube: kube, VMs: onPVE1, Now: func() time.Time { return now }}
	if err := gone.Wake(ctx, "restore of b/db"); !errors.Is(err, provisioner.ErrVolumeAsleep) {
		t.Fatal(err)
	}
	later := &Volume{Kube: kube, VMs: onPVE1, Now: func() time.Time { return now.Add(WakeRequestLife + time.Minute) }}
	if err := later.Wake(ctx, "snapshots of a/web"); err != nil && !errors.Is(err, provisioner.ErrVolumeAsleep) {
		t.Fatal(err)
	}
	if err := later.Done(ctx, "snapshots of a/web"); err != nil {
		t.Fatal(err)
	}
	if got := replicasOf(t, kube); got != 0 {
		t.Fatalf("with only a request that ran out the store keeps %d replicas", got)
	}
}

// Holders that overlap each hold the store: a snapshot ending leaves it awake for a restore still
// running, and it sleeps once the last holder is done.
func TestTheVolumeSleepsOnceNoHolderIsLeft(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	up := int32(1)
	kube := fake.NewClientset(&appsv1.Deployment{Name: Service, Namespace: Namespace, Spec: appsv1.DeploymentSpec{Replicas: &up}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}},
		&corev1.Secret{Name: TokenSecret, Namespace: Namespace})
	v := &Volume{Kube: kube, Now: func() time.Time { return now }}
	ctx := t.Context()
	asked := func() bool {
		d, err := kube.AppsV1().Deployments(Namespace).Get(ctx, Service, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return WakeRequested(d, now)
	}
	for _, holder := range []string{"restore of b/db", "snapshots of a/web"} {
		if _, err := v.Open(ctx, holder); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Done(ctx, "snapshots of a/web"); err != nil {
		t.Fatal(err)
	}
	if !asked() {
		t.Fatal("the snapshot's Done let the store sleep under the restore")
	}
	if err := v.Done(ctx, "restore of b/db"); err != nil {
		t.Fatal(err)
	}
	if asked() {
		t.Fatal("the store is still asked for with no holder left")
	}
}
