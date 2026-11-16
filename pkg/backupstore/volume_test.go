package backupstore

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
		&corev1.Secret{Name: ZonesSecret, Namespace: Namespace, Data: map[string][]byte{"token": []byte("zones"), "nodes": []byte("mgmt-cp-0")}},
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

// The store's status changes between a Done's read and its scale, and the scale conflicts. Done
// reads the store again and lets it sleep. A holder that asked in between keeps it awake.
func TestDoneReadsTheStoreAgainOnAConflict(t *testing.T) {
	t.Parallel()
	for name, asks := range map[string]bool{"a status write": false, "another holder's request": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
			kube := fake.NewClientset(sleepingStore()...)
			v := &Volume{Kube: kube, VMs: onPVE1, Now: func() time.Time { return now }}
			ctx := t.Context()
			if err := v.Wake(ctx, "snapshots of a/web"); !errors.Is(err, provisioner.ErrVolumeAsleep) {
				t.Fatal(err)
			}
			conflicted := false
			kube.PrependReactor("patch", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "scale" || conflicted {
					return false, nil, nil
				}
				conflicted = true
				if asks {
					gvr := appsv1.SchemeGroupVersion.WithResource("deployments")
					obj, err := kube.Tracker().Get(gvr, Namespace, Service)
					d, ok := obj.(*appsv1.Deployment)
					if err != nil || !ok {
						t.Errorf("the tracker holds %T: %v", obj, err)
						return false, nil, nil
					}
					d = d.DeepCopy()
					d.Annotations[WakeRequestKey(WakeRequestAnnotation, "restore of b/db")] = now.Add(WakeRequestLife).Format(time.RFC3339)
					if err = kube.Tracker().Update(gvr, d, Namespace); err != nil {
						t.Error(err)
					}
				}
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, Service, errors.New("changed"))
			})
			if err := v.Done(ctx, "snapshots of a/web"); err != nil {
				t.Fatal(err)
			}
			want := int32(0)
			if asks {
				want = 1
			}
			if got := replicasOf(t, kube); !conflicted || got != want {
				t.Fatalf("after a conflict the store has %d replicas, want %d", got, want)
			}
		})
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

// A holder that never lets go, as a look at the snapshots, holds the store while its request is in
// force. Sleeper puts the store back to sleep once it ran out, with no holder's Done.
func TestTheSleeperPutsALeftStoreToSleep(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	kube := fake.NewClientset(sleepingStore()...)
	look := &Volume{Kube: kube, VMs: onPVE1, Now: func() time.Time { return start }}
	if err := look.Wake(t.Context(), "a look"); !errors.Is(err, provisioner.ErrVolumeAsleep) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		after time.Duration
		want  int32
	}{{35 * time.Minute, 1}, {WakeRequestLife + time.Minute, 0}} {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		v := &Volume{Kube: kube, Now: func() time.Time { return start.Add(tc.after) }}
		if err := (&Sleeper{Volume: v, Every: time.Millisecond}).Start(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		if got := replicasOf(t, kube); got != tc.want {
			t.Fatalf("%s after the look the store has %d replicas, want %d", tc.after, got, tc.want)
		}
	}
}

// unzoned is sleepingStore with its node joined without a zone label, as the machine config makes it.
func unzoned(t *testing.T) []runtime.Object {
	t.Helper()
	objects := sleepingStore()
	n, ok := objects[3].(*corev1.Node)
	if !ok {
		t.Fatalf("sleepingStore's fourth object is %T", objects[3])
	}
	n.Labels = nil
	return objects
}

// sleepFor runs a Sleeper on v for a moment, ticking every millisecond.
func sleepFor(t *testing.T, v *Volume) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := (&Sleeper{Volume: v, Every: time.Millisecond}).Start(ctx); err != nil {
		t.Fatal(err)
	}
}

func zoneOf(t *testing.T, kube *fake.Clientset, name string) string {
	t.Helper()
	n, err := kube.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n.Labels[corev1.LabelTopologyZone]
}

// The CSI node plugin exits on a node without a zone label, so the Sleeper sets one with no wake.
func TestTheSleeperLabelsANodeWithoutAZone(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(unzoned(t)...)
	sleepFor(t, &Volume{Kube: kube, VMs: onPVE1})
	if got := zoneOf(t, kube, "mgmt-cp-0"); got != "pve1" {
		t.Fatalf("the node's zone is %q, want pve1", got)
	}
}

// A labelled node keeps the label a wake set, and costs no call to Proxmox VE.
func TestTheSleeperReadsNoVMsWhileEveryNodeHasAZone(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(sleepingStore()...)
	reads := 0
	sleepFor(t, &Volume{Kube: kube, VMs: func(ctx context.Context, token string) (*VMIndex, error) {
		reads++
		return onPVE1(ctx, token)
	}})
	if reads != 0 {
		t.Fatalf("the Sleeper read the VMs %d times", reads)
	}
	for _, a := range kube.Actions() {
		if a.GetResource().Resource == "nodes" && a.GetVerb() == "patch" {
			t.Fatalf("the Sleeper changed a labelled node: %v", a)
		}
	}
}

// Proxmox VE not answering keeps the node unlabelled, and the store still goes to sleep.
func TestTheSleeperStillSleepsWhenZonesFail(t *testing.T) {
	t.Parallel()
	objects := unzoned(t)
	d, ok := objects[0].(*appsv1.Deployment)
	if !ok {
		t.Fatalf("sleepingStore's first object is %T", objects[0])
	}
	one := int32(1)
	d.Spec.Replicas = &one
	kube := fake.NewClientset(objects...)
	reads := 0
	sleepFor(t, &Volume{Kube: kube, VMs: func(context.Context, string) (*VMIndex, error) {
		reads++
		return nil, errors.New("no route to host")
	}})
	if reads == 0 {
		t.Fatal("the Sleeper never asked where the VMs run")
	}
	if got := replicasOf(t, kube); got != 0 {
		t.Fatalf("the store kept %d replicas", got)
	}
}

// A node the Secret names that has not joined yet is no node without a zone, and fails nothing.
func TestTheSleeperPassesOverANodeNotJoined(t *testing.T) {
	t.Parallel()
	for what, objects := range map[string][]runtime.Object{"labelled": sleepingStore(), "unlabelled": unzoned(t)} {
		zones, ok := objects[2].(*corev1.Secret)
		if !ok {
			t.Fatalf("sleepingStore's third object is %T", objects[2])
		}
		zones.Data["nodes"] = []byte("mgmt-cp-0,mgmt-cp-1")
		kube := fake.NewClientset(objects...)
		reads := 0
		v := &Volume{Kube: kube, VMs: func(ctx context.Context, token string) (*VMIndex, error) {
			reads++
			return onPVE1(ctx, token)
		}}
		if err := v.labelMissingZones(t.Context()); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		want := map[string]int{"labelled": 0, "unlabelled": 1}[what]
		if reads != want {
			t.Errorf("%s: one tick read the VMs %d times, want %d", what, reads, want)
		}
		sleepFor(t, v)
		if reads != want {
			t.Errorf("%s: later ticks read the VMs again, %d times in all", what, reads)
		}
	}
}

// A zones Secret that names no joined node says so, and leaves the store asleep.
func TestAWakeWithNoNamedNodeSaysSo(t *testing.T) {
	t.Parallel()
	for what, nodes := range map[string][]byte{"no nodes key": nil, "an empty list": {}, "a renamed node": []byte("mgmt-old-0")} {
		objects := sleepingStore()
		zones, ok := objects[2].(*corev1.Secret)
		if !ok {
			t.Fatalf("sleepingStore's third object is %T", objects[2])
		}
		zones.Data = map[string][]byte{"token": []byte("zones")}
		if nodes != nil {
			zones.Data["nodes"] = nodes
		}
		kube := fake.NewClientset(objects...)
		err := (&Volume{Kube: kube, VMs: onPVE1}).Wake(t.Context(), "snapshots of a/web")
		if err == nil || !strings.Contains(err.Error(), `no node named under "nodes" in Secret kubehenge-system/kubehenge-backup-store-zones`) || !strings.Contains(err.Error(), "backupVolume.nodes") {
			t.Errorf("%s: %v", what, err)
		}
		if got := replicasOf(t, kube); got != 0 {
			t.Errorf("%s: the store has %d replicas", what, got)
		}
	}
}

// A look's request runs out LookLife after the page last asked, and a restore's lives on.
func TestALookRunsOutLongBeforeARestore(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		holder string
		life   time.Duration
		want   int32
	}{{"a look", LookLife, 0}, {"restore of b/db", 0, 1}} {
		kube := fake.NewClientset(sleepingStore()...)
		v := &Volume{Kube: kube, VMs: onPVE1, Life: tc.life, Now: func() time.Time { return start }}
		if err := v.Wake(t.Context(), tc.holder); !errors.Is(err, provisioner.ErrVolumeAsleep) {
			t.Fatal(err)
		}
		later := &Volume{Kube: kube, Now: func() time.Time { return start.Add(LookLife + time.Minute) }}
		if err := later.sleep(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		if got := replicasOf(t, kube); got != tc.want {
			t.Fatalf("%s: %s on the store has %d replicas, want %d", tc.holder, LookLife+time.Minute, got, tc.want)
		}
	}
}

// A restore asks for the store once and uses it to its end. Another holder's Done in its last
// minutes leaves the store awake for it.
func TestARestoreOutlivesAnotherHoldersDone(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	kube := fake.NewClientset(sleepingStore()...)
	restore := &Volume{Kube: kube, VMs: onPVE1, Now: func() time.Time { return start }}
	if err := restore.Wake(t.Context(), "restore of b/db"); !errors.Is(err, provisioner.ErrVolumeAsleep) {
		t.Fatal(err)
	}
	late := &Volume{Kube: kube, VMs: onPVE1, Now: func() time.Time { return start.Add(provisioner.EtcdRestoreTimeout - time.Minute) }}
	if err := late.Wake(t.Context(), "snapshots of a/web"); err != nil && !errors.Is(err, provisioner.ErrVolumeAsleep) {
		t.Fatal(err)
	}
	if err := late.Done(t.Context(), "snapshots of a/web"); err != nil {
		t.Fatal(err)
	}
	if got := replicasOf(t, kube); got != 1 {
		t.Fatalf("a snapshot's Done in the restore's last minute left the store at %d replicas", got)
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
