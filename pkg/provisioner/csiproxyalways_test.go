package provisioner

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	if _, ok := wrapper.Data["kubehenge-proxmox-proxied.yaml"]; !ok {
		t.Error("the resource set lacks the proxied configuration")
	}
	if _, ok := wrapper.Data["kubehenge-volume-copies.yaml"]; ok {
		t.Error("the proxy of a cluster that never had copies gets their namespace and rights")
	}
	checkHeldCSIBehindTheProxy(t, kube)
	_, csi, _ := proxyOf(t, kube, "web-proxmox-csi")
	args := lookup[[]any](t, csi, "initContainers", 0, "args")
	if !slices.Equal(args[len(args)-2:], []any{"--copies=false", "--find-copies=false"}) {
		t.Errorf("the proxy of a cluster that takes no snapshots and has no copies reads their records: %v", args)
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

// A claim Kubehenge's release of an add-on made that stays Pending because its volume cannot be
// provisioned shows as a wait on the cluster, naming the storage where the plugin's held release
// names only a refusal. A user's own claims in the add-on's namespace never do, and the line goes
// once the claim is bound.
func TestPendingAddonClaimShowsAsAWaitingLine(t *testing.T) {
	t.Parallel()
	web := withAddons(storedCluster("team-a", "web", "shared-lvm"), map[string]string{"monitoring": formMonitoring, "logs": formLogs})
	web.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	r, kube := newReconciler(t, &fakeCloud{}, web)
	pending := corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}
	prometheus := &corev1.PersistentVolumeClaim{
		Namespace: monitoringAddon, Name: "prometheus-kube-prometheus-stack-prometheus-db-prometheus-kube-prometheus-stack-prometheus-0",
		Labels: map[string]string{"operator.prometheus.io/name": "kube-prometheus-stack-prometheus"}, Status: pending,
	}
	loki := &corev1.PersistentVolumeClaim{
		Namespace: logsNamespace, Name: "loki-storage",
		Annotations: map[string]string{"meta.helm.sh/release-name": "loki", "meta.helm.sh/release-namespace": logsNamespace}, Status: pending,
	}
	theirs := []*corev1.PersistentVolumeClaim{
		{Namespace: monitoringAddon, Name: "their-own", Status: pending},
		{Namespace: logsNamespace, Name: "their-loki", Annotations: map[string]string{"meta.helm.sh/release-name": "their-loki"}, Status: pending},
		{Namespace: argoCDNamespace, Name: "their-repo", Status: pending},
	}
	now := time.Now()
	failed := func(claim *corev1.PersistentVolumeClaim, name, reason string, at time.Time) *corev1.Event {
		return &corev1.Event{
			Namespace: claim.Namespace, Name: name,
			InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: claim.Namespace, Name: claim.Name},
			Reason:         provisioningFailed, Message: "failed to provision volume with StorageClass \"proxmox\": rpc error: code = Internal desc = " + reason, LastTimestamp: metav1.NewTime(at),
		}
	}
	objects := make([]client.Object, 0, 5+2*len(theirs))
	objects = append(objects,
		prometheus, loki,
		failed(prometheus, "old", "an older reason", now.Add(-time.Hour)),
		failed(prometheus, "new", "failed to get proxmox storage config: not authorized to access endpoint", now),
		failed(loki, "loki", "failed to get proxmox storage config: not found", now),
	)
	for i, claim := range theirs {
		objects = append(objects, claim, failed(claim, "theirs-"+strconv.Itoa(i), "their own driver's reason", now))
	}
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(objects...).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(web): {client: workload, until: now.Add(time.Hour)}}}
	ctx := context.Background()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(web)}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	setCertManagerReady(t, kube, web)

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
	cond := meta.FindStatusCondition(got.Status.Conditions, AddonsWaitingCondition)
	const want = "monitoring has no volume: the cluster's token may not see storage shared-lvm and needs Datastore.Audit on /storage/shared-lvm. " +
		"logs has no volume: storage shared-lvm is not shown to the cluster's token, which needs Datastore.Audit on /storage/shared-lvm, or it does not exist"
	if cond == nil || cond.Reason != AddonsWaitingVolumeMissing || cond.Message != want {
		t.Fatalf("condition %+v, want %s with the message %q", cond, AddonsWaitingVolumeMissing, want)
	}
	if conflict := meta.FindStatusCondition(got.Status.Conditions, AddonConflictCondition); conflict != nil {
		t.Errorf("a missing volume reads as a conflict: %+v", conflict)
	}

	for _, claim := range []*corev1.PersistentVolumeClaim{prometheus, loki} {
		claim.Status.Phase = corev1.ClaimBound
		if err = workload.Status().Update(ctx, claim); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err = kube.Get(ctx, client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, AddonsWaitingCondition); cond != nil {
		t.Errorf("a line stayed once Kubehenge's claims were bound, with the user's own still Pending: %+v", cond)
	}
}

// Add-ons switched off say nothing of claims in their namespaces, whoever made them.
func TestClaimsOfAddonsSwitchedOffShowNoLine(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "shared-lvm")
	r, kube := newReconciler(t, &fakeCloud{}, web)
	claim := &corev1.PersistentVolumeClaim{
		Namespace: monitoringAddon, Name: "prometheus-db",
		Labels: map[string]string{"operator.prometheus.io/name": "kube-prometheus-stack-prometheus"},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	event := &corev1.Event{
		Namespace: monitoringAddon, Name: "failed",
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: monitoringAddon, Name: claim.Name},
		Reason:         provisioningFailed, Message: "desc = a reason", LastTimestamp: metav1.Now(),
	}
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(claim, event).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(web): {client: workload, until: time.Now().Add(time.Hour)}}}
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{AddonsWaitingCondition, AddonConflictCondition} {
		if cond := meta.FindStatusCondition(got.Status.Conditions, kind); cond != nil {
			t.Errorf("monitoring is off, yet %+v", cond)
		}
	}
}

// A workload cluster whose API does not answer leaves the add-on reconcile as it was: the copies
// are asked for only where some may be left, the volume lines are read only where they can be,
// and the proxies are applied.
func TestUnansweredWorkloadAPIKeepsTheAddonReconcile(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "local-zfs")
	r, kube := newReconciler(t, &fakeCloud{}, web)
	r.VolumeSnapshots = testVolumeSnapshots
	refused := errors.New("dial tcp 192.0.2.10:6443: connect: connection refused")
	down := fake.NewClientBuilder().WithScheme(remoteScheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return refused
		},
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return refused
		},
	}).Build()
	unanswered := func() *Remotes {
		return &Remotes{byCluster: map[string]remote{remoteKey(web): {client: down, until: time.Now().Add(time.Hour)}}}
	}
	r.Remotes = unanswered()
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := proxyOf(t, kube, "web-proxmox-csi"); !ok {
		t.Error("the CSI plugin was not applied")
	}
	r.Remotes = unanswered()
	addons := &Addons{Monitoring: Monitoring{Enabled: true}, Logs: Logs{Enabled: true}}
	if lines := r.addonVolumeWaits(context.Background(), web, addons); len(lines) != 0 {
		t.Errorf("lines from a cluster that does not answer: %q", lines)
	}
}
