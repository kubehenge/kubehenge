package appliance

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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

func zonesToken() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: zonesSecret, Namespace: backupstore.Namespace},
		Data:       map[string][]byte{zonesKey: []byte(standInToken)},
	}
}

// standInToken stands in for the token the module makes. testdata/storage-zones-values.yaml holds
// it too, base64.
const standInToken = "fake"

func zoned(name, zone string, beta bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		corev1.LabelTopologyRegion: "kubehenge",
		corev1.LabelTopologyZone:   zone,
	}}}
	if beta {
		n.Labels[corev1.LabelFailureDomainBetaZone] = zone
	}
	return n
}

// hostsAt stands in for Proxmox VE, which says where each VM runs, and fails a token it never gave.
func hostsAt(t *testing.T, hosts map[string]string) func(context.Context, string) (map[string]string, error) {
	t.Helper()
	return func(_ context.Context, token string) (map[string]string, error) {
		if token != standInToken {
			t.Errorf("Proxmox VE was asked with token %q, not the one in the chart's Secret", token)
		}
		return hosts, nil
	}
}

// On a node's own storage the store sleeps, so that its disk is attached to a node VM only while a
// backup or recovery runs. A wake waits for the pod, and the store goes back to sleep after.
func TestTheStoreSleepsBetweenBackupsOnLocalStorage(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(storeDeployment(0, "0"), zonesToken())
	a := &Appliance{vmHosts: hostsAt(t, nil)}
	deps := kube.AppsV1().Deployments(backupstore.Namespace)
	ctx := t.Context()
	woke := make(chan error, 1)
	go func() { woke <- a.wakeStore(ctx, kube) }()
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
	a := &Appliance{vmHosts: func(context.Context, string) (map[string]string, error) {
		t.Error("a store on shared storage asked Proxmox VE where the node VMs run")
		return nil, nil
	}}
	ctx := t.Context()
	if err := a.wakeStore(ctx, kube); err != nil {
		t.Fatal(err)
	}
	if err := restStore(ctx, kube); err != nil {
		t.Fatal(err)
	}
	if got, _ := kube.AppsV1().Deployments(backupstore.Namespace).Get(ctx, backupstore.Service, metav1.GetOptions{}); *got.Spec.Replicas != 1 {
		t.Fatalf("a store on shared storage was put to %d replicas", *got.Spec.Replicas)
	}

	stuck := fake.NewClientset(storeDeployment(0, "0"), zonesToken())
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	sleeper := &Appliance{vmHosts: hostsAt(t, nil)}
	if err := sleeper.wakeStore(short, stuck); err == nil {
		t.Fatal("a store whose pod never got ready counts as woken")
	}
}

// HA or an administrator moved a node VM from pve1 to pve2. Its node still says pve1, and the store
// would land there and fail to attach a disk on pve1's own storage. The wake corrects the label,
// the beta one too where the node has it, before the store starts.
func TestAMovedNodeVMsZoneIsCorrectedBeforeTheStoreWakes(t *testing.T) {
	t.Parallel()
	ready := storeDeployment(0, "0")
	ready.Status.ReadyReplicas = 1
	kube := fake.NewClientset(ready, zonesToken(),
		zoned("mgmt-cp-0", "pve1", true), zoned("mgmt-cp-1", "pve1", false), zoned("mgmt-cp-2", "pve3", false))
	var atWake map[string]map[string]string
	kube.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		atWake = map[string]map[string]string{}
		nodes, err := kube.Tracker().List(corev1.SchemeGroupVersion.WithResource("nodes"), corev1.SchemeGroupVersion.WithKind("Node"), "")
		if err != nil {
			t.Error(err)
			return false, nil, nil
		}
		for _, n := range nodes.(*corev1.NodeList).Items {
			atWake[n.Name] = n.Labels
		}
		return false, nil, nil
	})
	a := &Appliance{vmHosts: hostsAt(t, map[string]string{
		"mgmt-cp-0": "pve2", "mgmt-cp-1": "pve1", "mgmt-cp-2": "pve3", "mgmt-backups": "pve1",
	})}
	if err := a.wakeStore(t.Context(), kube); err != nil {
		t.Fatal(err)
	}
	if atWake == nil {
		t.Fatal("the sleeping store was not started")
	}
	for node, want := range map[string]string{"mgmt-cp-0": "pve2", "mgmt-cp-1": "pve1", "mgmt-cp-2": "pve3"} {
		if got := atWake[node][corev1.LabelTopologyZone]; got != want {
			t.Errorf("when the store woke, node %s was labelled zone %s, but its VM runs on %s", node, got, want)
		}
	}
	if got := atWake["mgmt-cp-0"][corev1.LabelFailureDomainBetaZone]; got != "pve2" {
		t.Errorf("node mgmt-cp-0's beta zone label says %s, not pve2", got)
	}
	if _, beta := atWake["mgmt-cp-1"][corev1.LabelFailureDomainBetaZone]; beta {
		t.Error("node mgmt-cp-1 gained a beta zone label it never had")
	}
	if atWake["mgmt-cp-0"][corev1.LabelTopologyRegion] != "kubehenge" {
		t.Error("correcting the zone dropped the node's region label")
	}

	// Without the token the wake cannot tell where the VMs run, and does not start the store.
	blind := fake.NewClientset(storeDeployment(0, "0"), zoned("mgmt-cp-0", "pve1", false))
	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := a.wakeStore(short, blind); err == nil {
		t.Fatal("a wake that could not read where the node VMs run started the store")
	}
	if got, _ := blind.AppsV1().Deployments(backupstore.Namespace).Get(t.Context(), backupstore.Service, metav1.GetOptions{}); *got.Spec.Replicas != 0 {
		t.Fatal("a wake that could not read where the node VMs run started the store")
	}
}
