package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var progressStart = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func progressMachine(name string, created time.Time, status metav1.ConditionStatus) *clusterv1.Machine {
	m := &clusterv1.Machine{
		Name: name, Namespace: "team-a", CreationTimestamp: metav1.NewTime(created),
		Labels: map[string]string{clusterv1.ClusterNameLabel: "web"},
		Spec:   clusterv1.MachineSpec{ClusterName: "web", InfrastructureRef: clusterv1.ContractVersionedObjectReference{Name: name}},
	}
	message := "* NodeHealthy: Waiting for a Node with spec.providerID proxmox://" + name + " to exist"
	if status == metav1.ConditionTrue {
		message = ""
		m.Status.NodeRef.Name = name
	}
	m.Status.Conditions = []metav1.Condition{{
		Type: clusterv1.MachineReadyCondition, Status: status, Reason: "Waiting", Message: message,
		LastTransitionTime: metav1.NewTime(created),
	}}
	return m
}

func proxmoxMachineFailing(name, message string) *unstructured.Unstructured {
	pm := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "severity": "Info", "message": "waiting"},
			map[string]any{"type": "VirtualMachineProvisioned", "status": "False", "severity": "Error", "reason": "VMProvisionFailed", "message": message},
		}},
	}}
	pm.SetGroupVersionKind(schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: proxmoxMachineVersion, Kind: "ProxmoxMachine"})
	pm.SetNamespace("team-a")
	pm.SetName(name)
	pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "web"})
	return pm
}

func stalledCount(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	if err := rolloutsTotal.WithLabelValues(outcomeStalled).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

type progressFixture struct {
	r     *ProgressReconciler
	kube  client.Client
	clock time.Time
	sent  *events.FakeRecorder
}

func newProgress(t *testing.T, objs ...client.Object) *progressFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gvk := schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: proxmoxMachineVersion, Kind: "ProxmoxMachine"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	gvk.Kind += "List"
	scheme.AddKnownTypeWithName(gvk, &unstructured.UnstructuredList{})
	c := cluster()
	c.CreationTimestamp = metav1.NewTime(progressStart)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{c}, objs...)...).
		WithStatusSubresource(&clusterv1.Cluster{}).Build()
	f := &progressFixture{kube: kube, clock: progressStart, sent: events.NewFakeRecorder(10)}
	f.r = &ProgressReconciler{Client: kube, ClassNamespace: "kubehenge-system", Events: f.sent, Now: func() time.Time { return f.clock }}
	return f
}

func (f *progressFixture) reconcile(t *testing.T) (ctrl.Result, *metav1.Condition) {
	t.Helper()
	key := client.ObjectKey{Namespace: "team-a", Name: "web"}
	result, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	got := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	return result, meta.FindStatusCondition(got.Status.Conditions, MachinesStalledCondition)
}

func TestProgressSettledClusterIsNotLookedAtAgain(t *testing.T) {
	t.Parallel()
	f := newProgress(t, progressMachine("web-cp-1", progressStart, metav1.ConditionTrue))
	f.clock = progressStart.Add(time.Hour)
	result, cond := f.reconcile(t)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != SettledReason {
		t.Errorf("condition %+v, want False %s", cond, SettledReason)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("a settled cluster requeued after %v", result.RequeueAfter)
	}
}

func TestProgressRolloutUnderWayIsFollowed(t *testing.T) {
	t.Parallel()
	f := newProgress(t, progressMachine("web-cp-1", progressStart, metav1.ConditionTrue), progressMachine("web-cp-2", progressStart.Add(50*time.Minute), metav1.ConditionFalse))
	f.clock = progressStart.Add(55 * time.Minute)
	result, cond := f.reconcile(t)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ProgressingReason || cond.Message != "1 of 2 machines ready" {
		t.Errorf("condition %+v, want False %s with the count", cond, ProgressingReason)
	}
	if result.RequeueAfter != progressCheck {
		t.Errorf("requeued after %v, want %v", result.RequeueAfter, progressCheck)
	}
}

func TestProgressStalledClusterCarriesTheWorstReport(t *testing.T) {
	t.Parallel()
	before := stalledCount(t)
	f := newProgress(t,
		progressMachine("web-cp-1", progressStart, metav1.ConditionTrue),
		progressMachine("web-cp-2", progressStart.Add(10*time.Minute), metav1.ConditionFalse),
		proxmoxMachineFailing("web-cp-2", "VM template not found"),
	)
	f.clock = progressStart.Add(40 * time.Minute)
	_, cond := f.reconcile(t)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != StalledReason {
		t.Fatalf("condition %+v, want True %s", cond, StalledReason)
	}
	for _, want := range []string{"since 2026-10-01 12:10 UTC", "web-cp-2: VirtualMachineProvisioned: VM template not found"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q lacks %q", cond.Message, want)
		}
	}
	select {
	case e := <-f.sent.Events:
		if !strings.HasPrefix(e, corev1.EventTypeWarning+" "+stalledEventReason) || !strings.Contains(e, "VM template not found") {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("no event for the stall")
	}
	if got := stalledCount(t); got < before+1 {
		t.Errorf("stalled rollouts counted %v, were %v", got, before)
	}
	// Still stalled a minute later: no second event.
	f.clock = f.clock.Add(time.Minute)
	f.reconcile(t)
	select {
	case e := <-f.sent.Events:
		t.Errorf("a second event for the same stall: %q", e)
	default:
	}
}

// A node that never joins is replaced by the health check every nodeStartupTimeout; the new
// machines are no progress.
func TestProgressReplacementsAreNoProgress(t *testing.T) {
	t.Parallel()
	first := progressMachine("web-cp-2", progressStart, metav1.ConditionFalse)
	f := newProgress(t, progressMachine("web-cp-1", progressStart, metav1.ConditionTrue), first)
	if _, cond := f.reconcile(t); cond.Status != metav1.ConditionFalse {
		t.Fatalf("stalled at once: %+v", cond)
	}
	f.clock = progressStart.Add(15 * time.Minute)
	if err := f.kube.Delete(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Create(context.Background(), progressMachine("web-cp-3", f.clock, metav1.ConditionFalse)); err != nil {
		t.Fatal(err)
	}
	if _, cond := f.reconcile(t); cond.Status != metav1.ConditionFalse {
		t.Fatalf("stalled too soon: %+v", cond)
	}
	f.clock = progressStart.Add(stallAfter + time.Minute)
	if _, cond := f.reconcile(t); cond.Status != metav1.ConditionTrue {
		t.Errorf("a machine replaced without ever joining was taken for progress: %+v", cond)
	}
}

func TestProgressAStallOutlastsARestartedController(t *testing.T) {
	t.Parallel()
	f := newProgress(t, progressMachine("web-cp-1", progressStart, metav1.ConditionTrue), progressMachine("web-cp-2", progressStart.Add(40*time.Minute), metav1.ConditionFalse))
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	c.Status.Conditions = []metav1.Condition{{
		Type: MachinesStalledCondition, Status: metav1.ConditionTrue, Reason: StalledReason,
		LastTransitionTime: metav1.NewTime(progressStart.Add(30 * time.Minute)),
	}}
	if err := f.kube.Status().Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.clock = progressStart.Add(45 * time.Minute)
	if _, cond := f.reconcile(t); cond.Status != metav1.ConditionTrue {
		t.Errorf("a stall was taken back after the controller started over: %+v", cond)
	}
	select {
	case e := <-f.sent.Events:
		t.Errorf("the same stall reported again: %q", e)
	default:
	}
}

func TestProgressAMachineBecomingReadyIsProgress(t *testing.T) {
	t.Parallel()
	waiting := progressMachine("web-cp-2", progressStart, metav1.ConditionFalse)
	f := newProgress(t, progressMachine("web-cp-1", progressStart, metav1.ConditionTrue), waiting, progressMachine("web-cp-3", progressStart, metav1.ConditionFalse))
	f.reconcile(t)
	f.clock = progressStart.Add(15 * time.Minute)
	ready := progressMachine("web-cp-2", progressStart, metav1.ConditionTrue)
	waiting.Status = ready.Status
	if err := f.kube.Update(context.Background(), waiting); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.clock = progressStart.Add(stallAfter + time.Minute)
	if _, cond := f.reconcile(t); cond.Status != metav1.ConditionFalse || cond.Message != "2 of 3 machines ready" {
		t.Errorf("condition %+v, want progressing: a machine became ready six minutes ago", cond)
	}
}
