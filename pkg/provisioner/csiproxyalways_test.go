package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// A cluster that takes no volume snapshots, on a storage shared between the nodes, still reaches
// Proxmox VE through the proxy: the plugin's read of the storage's configuration then needs only
// what the proxy reads from the storage list.
func TestClusterWithoutSnapshotsReachesProxmoxThroughTheProxy(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "shared-lvm")
	r, kube := newReconciler(t, &fakeCloud{}, web)
	r.VolumeSnapshots = testVolumeSnapshots
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	wrapper := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-proxmox"}, wrapper); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kubehenge-proxmox-proxied.yaml", "kubehenge-volume-copies.yaml"} {
		if _, ok := wrapper.Data[key]; !ok {
			t.Errorf("the resource set lacks %s", key)
		}
	}
	checkHeldCSIBehindTheProxy(t, kube)
	_, csi, _ := proxyOf(t, kube, "web-proxmox-csi")
	args := lookup[[]any](t, csi, "initContainers", 0, "args")
	if args[len(args)-1] != "--copies=false" {
		t.Errorf("the proxy makes copies for a cluster that takes no snapshots: %v", args)
	}
	if _, snapshotter := lookup[map[string]any](t, csi, "controller", "snapshotter")["enabled"]; snapshotter {
		t.Error("the snapshot sidecar runs without volume snapshots")
	}
}

// Without the proxy's image the plugin reaches Proxmox VE directly, as before.
func TestClusterGoesWithoutTheProxyWhenTheControllerHasNoImage(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "shared-lvm")
	r, kube := newReconciler(t, &fakeCloud{}, web)
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	_, csi, _ := proxyOf(t, kube, "web-proxmox-csi")
	if _, has := csi["initContainers"]; has || lookup[string](t, csi, "existingConfigSecret") != "kubehenge-proxmox" {
		t.Errorf("the plugin reaches Proxmox VE through a proxy that has no image: %v", csi)
	}
}

// A claim of an add-on that stays Pending because its volume cannot be provisioned shows as a line
// on the cluster, and the line goes once the claim is bound.
func TestPendingAddonClaimShowsAsAWaitingLine(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "shared-lvm")
	r, kube := newReconciler(t, &fakeCloud{}, web)
	claim := &corev1.PersistentVolumeClaim{
		Namespace: monitoringAddon, Name: "prometheus-db",
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	failed := func(name, message string, at time.Time) *corev1.Event {
		return &corev1.Event{
			Namespace: monitoringAddon, Name: name,
			InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: monitoringAddon, Name: claim.Name},
			Reason:         provisioningFailed, Message: message, LastTimestamp: metav1.NewTime(at),
		}
	}
	other := &corev1.PersistentVolumeClaim{
		Namespace: "apps", Name: "data",
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	now := time.Now()
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(claim, other,
		failed("old", "failed to provision volume: rpc error: code = Internal desc = an older reason", now.Add(-time.Hour)),
		failed("new", "failed to provision volume with StorageClass \"proxmox\": rpc error: code = Internal desc = failed to get proxmox storage config: not authorized to access endpoint", now),
		&corev1.Event{
			Namespace: "apps", Name: "theirs",
			InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "apps", Name: other.Name},
			Reason:         provisioningFailed, Message: "desc = not an add-on's",
		},
	).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(web): {client: workload, until: now.Add(time.Hour)}}}
	ctx := context.Background()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(web)}

	result, err := r.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != waitInterval {
		t.Errorf("requeue after %v, want %v while a claim waits", result.RequeueAfter, waitInterval)
	}
	got := &clusterv1.Cluster{}
	if err = kube.Get(ctx, client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, AddonConflictCondition)
	const want = "monitoring has no volume: failed to get proxmox storage config: not authorized to access endpoint"
	if cond == nil || cond.Message != want {
		t.Fatalf("condition %+v, want the message %q", cond, want)
	}
	if strings.Contains(cond.Message, "apps") || strings.Contains(cond.Message, "older") {
		t.Errorf("the line names a claim that is no add-on's or an older failure: %s", cond.Message)
	}

	claim.Status.Phase = corev1.ClaimBound
	if err = workload.Status().Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err = kube.Get(ctx, client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, AddonConflictCondition); cond != nil {
		t.Errorf("the line stayed after the claim was bound: %s", cond.Message)
	}
}
