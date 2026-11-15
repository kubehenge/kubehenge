package appliance

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
)

func storeDeployment(replicas int32, idle string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		Name: backupstore.Service, Namespace: backupstore.Namespace,
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
	}
	if idle != "" {
		d.Annotations = map[string]string{idleReplicas: idle}
	}
	return d
}

func zonesToken() *corev1.Secret {
	return &corev1.Secret{
		Name: zonesSecret, Namespace: backupstore.Namespace,
		Data: map[string][]byte{zonesKey: []byte(standInToken)},
	}
}

// standInToken stands in for the token the module makes. testdata/storage-zones-values.yaml holds
// it too, base64.
const standInToken = "fake"

// zoned is a management node in the region with the zone labels given, each set to zone. Its
// SMBIOS UUID is its name, so that hostsAt's keys name the nodes.
func zoned(name, zone string, labels ...string) *corev1.Node {
	n := &corev1.Node{
		Name: name, Labels: map[string]string{corev1.LabelTopologyRegion: "kubehenge"},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: name}},
	}
	for _, l := range labels {
		n.Labels[l] = zone
	}
	return n
}

// Zone labels for zoned.
var (
	zoneOnly = []string{corev1.LabelTopologyZone}
	withBeta = []string{corev1.LabelTopologyZone, corev1.LabelFailureDomainBetaZone}
)

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
		return map[string]string{}, nil
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
		zoned("mgmt-cp-0", "pve1", withBeta...), zoned("mgmt-cp-1", "pve1", zoneOnly...), zoned("mgmt-cp-2", "pve3", zoneOnly...))
	var atWake map[string]map[string]string
	kube.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		atWake = map[string]map[string]string{}
		nodes, err := kube.Tracker().List(corev1.SchemeGroupVersion.WithResource("nodes"), corev1.SchemeGroupVersion.WithKind("Node"), "")
		if err != nil {
			t.Error(err)
			return false, nil, nil
		}
		list, ok := nodes.(*corev1.NodeList)
		if !ok {
			t.Errorf("the tracker listed nodes as %T", nodes)
			return false, nil, nil
		}
		for _, n := range list.Items {
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
	blind := fake.NewClientset(storeDeployment(0, "0"), zoned("mgmt-cp-0", "pve1", zoneOnly...))
	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := a.wakeStore(short, blind); err == nil {
		t.Fatal("a wake that could not read where the node VMs run started the store")
	}
	if got, _ := blind.AppsV1().Deployments(backupstore.Namespace).Get(t.Context(), backupstore.Service, metav1.GetOptions{}); *got.Spec.Replicas != 0 {
		t.Fatal("a wake that could not read where the node VMs run started the store")
	}
}

// One management VM is gone, lost with its host's disk, and its node stays. The backups go on while
// a VM runs on the node that holds the disk, and stop, naming that node, once none does.
func TestAWakeGoesOnWithOneManagementVMGone(t *testing.T) {
	t.Parallel()
	sleeping := func() *appsv1.Deployment {
		d := storeDeployment(0, "0")
		d.Annotations[volumeZone] = "pve1"
		d.Status.ReadyReplicas = 1
		return d
	}
	replicas := func(kube *fake.Clientset) int32 {
		got, err := kube.AppsV1().Deployments(backupstore.Namespace).Get(t.Context(), backupstore.Service, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return *got.Spec.Replicas
	}
	nodes := func() []runtime.Object {
		return []runtime.Object{
			sleeping(), zonesToken(),
			zoned("mgmt-cp-0", "pve1", zoneOnly...), zoned("mgmt-cp-1", "pve2", zoneOnly...), zoned("mgmt-cp-2", "pve3", zoneOnly...),
		}
	}

	kube := fake.NewClientset(nodes()...)
	a := &Appliance{vmHosts: hostsAt(t, map[string]string{"mgmt-cp-0": "pve1", "mgmt-cp-1": "pve3"})}
	if err := a.wakeStore(t.Context(), kube); err != nil {
		t.Fatalf("a wake with mgmt-cp-2's VM gone: %v", err)
	}
	if got := replicas(kube); got != 1 {
		t.Fatalf("with mgmt-cp-2's VM gone the store keeps %d replicas", got)
	}
	if n, _ := kube.CoreV1().Nodes().Get(t.Context(), "mgmt-cp-1", metav1.GetOptions{}); n.Labels[corev1.LabelTopologyZone] != "pve3" {
		t.Errorf("node mgmt-cp-1, whose VM runs on pve3, is labelled %v", n.Labels)
	}

	lost := fake.NewClientset(nodes()...)
	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	a = &Appliance{vmHosts: hostsAt(t, map[string]string{"mgmt-cp-1": "pve2", "mgmt-cp-2": "pve3"})}
	err := a.wakeStore(short, lost)
	if err == nil || !strings.Contains(err.Error(), "no management VM runs on Proxmox VE node pve1, which holds the backup volume") {
		t.Fatalf("a wake with no VM on the disk's node: %v", err)
	}
	if rows := troubleshootingRowsFor(t, err.Error()); len(rows) != 1 || !strings.HasPrefix(rows[0], "no management VM runs") {
		t.Errorf("the error %q reads as the troubleshooting rows %q, want its own alone", err, rows)
	}
	if got := replicas(lost); got != 0 {
		t.Fatalf("with no VM on the disk's node the store was started to %d replicas", got)
	}
}

// The machine config sets only the region, so each apply leaves the zones to the appliance. It
// adds the zone a new node lacks and moves a stale one, through the admin kubeconfig.
func TestAnApplyLabelsTheManagementNodesZones(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	a.Talos = &fakeManagementTalos{}
	management := fake.NewClientset(storeDeployment(0, "0"), zonesToken(), zoned("mgmt-cp-0", ""), zoned("mgmt-cp-1", "pve1", withBeta...))
	a.ManagementClient = func(admin []byte) (kubernetes.Interface, error) {
		if string(admin) != "admin-kubeconfig" {
			t.Errorf("the management cluster was reached with %q, not the admin kubeconfig", admin)
		}
		return management, nil
	}
	a.vmHosts = hostsAt(t, map[string]string{"mgmt-cp-0": "pve2", "mgmt-cp-1": "pve3"})
	if err := a.Apply(settings("pve1"), passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("apply: %+v", j)
	}
	for node, want := range map[string]string{"mgmt-cp-0": "pve2", "mgmt-cp-1": "pve3"} {
		n, err := management.CoreV1().Nodes().Get(t.Context(), node, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := n.Labels[corev1.LabelTopologyZone]; got != want {
			t.Errorf("after the apply node %s is labelled zone %q, but its VM runs on %s", node, got, want)
		}
		_, beta := n.Labels[corev1.LabelFailureDomainBetaZone]
		if beta != (node == "mgmt-cp-1") || (beta && n.Labels[corev1.LabelFailureDomainBetaZone] != want) {
			t.Errorf("after the apply node %s has the beta zone label %q", node, n.Labels[corev1.LabelFailureDomainBetaZone])
		}
	}

	// A node of unknown zone fails no apply. The job's log says which one.
	a.vmHosts = hostsAt(t, map[string]string{"mgmt-cp-0": "pve2"})
	if err := a.Apply(settings("pve1"), passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	if j.State != StateSucceeded || !strings.Contains(strings.Join(j.Log, "\n"), "Proxmox VE lists no VM for node mgmt-cp-1, whose zone stays as it is") {
		t.Fatalf("an apply with node mgmt-cp-1 unlisted: %+v", j)
	}
}
