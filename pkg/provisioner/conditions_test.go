package provisioner

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func kubehengeReady(t *testing.T, kube client.Client, c *clusterv1.Cluster) *metav1.Condition {
	t.Helper()
	got := &clusterv1.Cluster{}
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(got.Status.Conditions, KubehengeReadyCondition)
}

func TestReconcileReportsItsOutcomeOnTheCluster(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	cloud := &fakeCloud{failAfterRecord: true}
	r, kube := newReconciler(t, cloud, c)
	recorder := events.NewFakeRecorder(10)
	r.Events = recorder
	if err := reconcile(t, r, c); err == nil {
		t.Fatal("no error from a failed creation")
	}
	cond := kubehengeReady(t, kube, c)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReconcileFailedReason ||
		!strings.Contains(cond.Message, "describing volume owner VM 4242: not authorized") {
		t.Fatalf("%s = %+v, want False with the error", KubehengeReadyCondition, cond)
	}
	select {
	case e := <-recorder.Events:
		if !strings.HasPrefix(e, corev1.EventTypeWarning+" "+ReconcileFailedReason) || !strings.Contains(e, "not authorized") {
			t.Errorf("event %q, want a Warning carrying the error", e)
		}
	default:
		t.Error("no event for the failed reconcile")
	}

	cloud.failAfterRecord = false
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	cond = kubehengeReady(t, kube, c)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReconciledReason || cond.Message != "" {
		t.Errorf("%s = %+v, want True once a reconcile succeeds", KubehengeReadyCondition, cond)
	}
	select {
	case e := <-recorder.Events:
		if strings.HasPrefix(e, corev1.EventTypeWarning) {
			t.Errorf("a successful reconcile reported %q", e)
		}
	default:
	}
}

func TestReportLeavesAClusterLetGoOfAlone(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	now := metav1.Now()
	c.DeletionTimestamp, c.Finalizers = &now, []string{"other"}
	r, kube := newReconciler(t, &fakeCloud{}, c)
	r.report(t.Context(), c, nil)
	if cond := kubehengeReady(t, kube, c); cond != nil {
		t.Errorf("a deleted cluster was reported on: %+v", cond)
	}
}

// A condition set right after a write the cache does not hold yet is set all the same, read again
// past the cache. Without a reader past it, the conflict stays.
func TestSetClusterConditionPastAStaleCache(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	_, kube := newReconciler(t, &fakeCloud{}, c)
	watching, ok := kube.(client.WithWatch)
	if !ok {
		t.Fatal("the fixture's client does not watch")
	}
	stale := &clusterv1.Cluster{}
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(c), stale); err != nil {
		t.Fatal(err)
	}
	cached := interceptor.NewClient(watching, interceptor.Funcs{
		Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if into, isCluster := obj.(*clusterv1.Cluster); isCluster {
				stale.DeepCopyInto(into)
				return nil
			}
			return w.Get(ctx, key, obj, opts...)
		},
	})
	first := metav1.Condition{Type: VMsCondition, Status: metav1.ConditionFalse, Reason: "Stopped"}
	if _, err := markCluster(t.Context(), cached, kube, client.ObjectKeyFromObject(c), VMsCondition, &first); err != nil {
		t.Fatal(err)
	}
	second := metav1.Condition{Type: StorageCondition, Status: metav1.ConditionFalse, Reason: "NearlyFull"}
	if _, err := markCluster(t.Context(), cached, kube, client.ObjectKeyFromObject(c), StorageCondition, &second); err != nil {
		t.Fatalf("the second condition: %v", err)
	}
	now := &clusterv1.Cluster{}
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(c), now); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(now.Status.Conditions, VMsCondition) == nil || meta.FindStatusCondition(now.Status.Conditions, StorageCondition) == nil {
		t.Errorf("conditions %+v", now.Status.Conditions)
	}
	third := metav1.Condition{Type: KubehengeReadyCondition, Status: metav1.ConditionTrue, Reason: ReconciledReason}
	if _, err := setClusterCondition(t.Context(), cached, nil, client.ObjectKeyFromObject(c), KubehengeReadyCondition, &third); !apierrors.IsConflict(err) {
		t.Errorf("with the cache alone: %v, want a conflict", err)
	}
}

func TestClipKeepsWholeCharacters(t *testing.T) {
	t.Parallel()
	if got := clip("short", 10); got != "short" {
		t.Errorf("clip = %q", got)
	}
	got := clip(strings.Repeat("ä", 10), 9)
	if len(got) > 9 || !strings.HasSuffix(got, "...") || strings.ContainsRune(got, '�') {
		t.Errorf("clip = %q, want whole characters and the mark within 9 bytes", got)
	}
}
