package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// rollMachine is a control-plane machine of web made at made, whose Ready condition has status.
func rollMachine(name string, made time.Time, status metav1.ConditionStatus) *clusterv1.Machine {
	m, _ := controlPlane(name, 100, "pve1")
	m.CreationTimestamp = metav1.NewTime(made)
	m.Status.Conditions = []metav1.Condition{{
		Type: clusterv1.MachineReadyCondition, Status: status, Reason: "Test", Message: "* NodeHealthy: Waiting for a Node with spec.providerID",
	}}
	return m
}

func TestRollWatch(t *testing.T) {
	t.Parallel()
	before := etcdNow.Add(-24 * time.Hour)
	for _, tc := range []struct {
		name       string
		replicas   int32
		paused     bool
		successor  time.Duration
		ready      bool
		down       bool
		wantReason string
	}{
		{name: "the API answers", replicas: 1, successor: 25 * time.Minute, wantReason: RollStalledAnswering},
		{name: "the API does not answer", replicas: 1, successor: 25 * time.Minute, down: true, wantReason: RollStalledUnreachable},
		{name: "not long enough", replicas: 1, successor: 10 * time.Minute, down: true},
		{name: "ready", replicas: 1, successor: time.Hour, ready: true, down: true},
		{name: "three control planes", replicas: 3, successor: time.Hour, down: true},
		{name: "paused", replicas: 1, paused: true, successor: time.Hour, down: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := etcdCluster(`{}`)
			c.Spec.Topology.ControlPlane.Replicas = new(tc.replicas)
			if tc.paused {
				c.Spec.Paused = new(true)
			}
			ready := metav1.ConditionFalse
			if tc.ready {
				ready = metav1.ConditionTrue
			}
			f := newEtcdFixture(t, c, rollMachine("web-cp-old", before, metav1.ConditionTrue), rollMachine("web-cp-new", etcdNow.Add(-tc.successor), ready))
			apis := &fakeAPIs{}
			if tc.down {
				apis.down = 100
			}
			recorder := events.NewFakeRecorder(10)
			w := &RollWatch{Client: f.kube, ClassNamespace: "kubehenge-system", APIs: apis, Events: recorder, Now: func() time.Time { return etcdNow }}
			if err := w.watch(context.Background(), f.cluster()); err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(f.cluster().Status.Conditions, RollStalledCondition)
			if tc.wantReason == "" {
				if cond != nil {
					t.Fatalf("condition %+v", cond)
				}
				return
			}
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tc.wantReason {
				t.Fatalf("condition %+v, want %s", cond, tc.wantReason)
			}
			for _, want := range []string{"web-cp-new, which replaces web-cp-old", "in 25m0s", "Waiting for a Node", `{\"machine\":\"web-cp-old\"`} {
				if !strings.Contains(cond.Message, want) {
					t.Errorf("message has no %q: %s", want, cond.Message)
				}
			}
			if head := RollStallHead(cond.Message); !strings.HasPrefix(head, "control-plane machine web-cp-new") || !strings.HasSuffix(head, "spec.providerID.") {
				t.Errorf("head %q", head)
			}
			select {
			case e := <-recorder.Events:
				if !strings.Contains(e, "Warning "+RollStalledCondition) {
					t.Errorf("event %q", e)
				}
			default:
				t.Error("no event")
			}

			// Looked at again, nothing new is said. Once the cluster is paused for a recovery, the
			// condition goes.
			if err := w.watch(context.Background(), f.cluster()); err != nil {
				t.Fatal(err)
			}
			select {
			case e := <-recorder.Events:
				t.Errorf("event %q on a second look", e)
			default:
			}
			paused := f.cluster()
			paused.Spec.Paused = new(true)
			if err := w.watch(context.Background(), paused); err != nil {
				t.Fatal(err)
			}
			if cond := meta.FindStatusCondition(f.cluster().Status.Conditions, RollStalledCondition); cond != nil {
				t.Errorf("condition %+v kept", cond)
			}
		})
	}
}

// The health check replaces a successor in 20 minutes, so a roll it keeps replacing is timed from
// the control plane's RollingOut condition. The watch reads it afresh, so a restart keeps the bound.
func TestRollWatchReplacedSuccessors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rolling time.Duration
		want    bool
	}{
		{name: "a roll whose successors keep being replaced", rolling: time.Hour, want: true},
		{name: "a healthy roll", rolling: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := etcdCluster(`{}`)
			c.Spec.Topology.ControlPlane.Replicas = new(int32(1))
			c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: siteControlGVK.Group, Kind: siteControlGVK.Kind, Name: "web-cp"}
			cp := controlPlaneIn(c, rolloutState{Desired: 1})
			cp.Object["status"] = map[string]any{"conditions": []any{map[string]any{
				"type": clusterv1.RollingOutCondition, "status": "True", "reason": "RollingOut",
				"lastTransitionTime": etcdNow.Add(-tc.rolling).UTC().Format(time.RFC3339),
			}}}
			scheme := testScheme(t)
			scheme.AddKnownTypeWithName(siteControlGVK, &unstructured.Unstructured{})
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{siteControlGVK.GroupVersion()})
			mapper.Add(siteControlGVK, meta.RESTScopeNamespace)
			kept := rollMachine("web-cp-old", etcdNow.Add(-24*time.Hour), metav1.ConditionTrue)
			kube := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithStatusSubresource(&clusterv1.Cluster{}).
				WithObjects(c, cp, kept, rollMachine("web-cp-new", etcdNow.Add(-5*time.Minute), metav1.ConditionFalse)).Build()
			look := func() (*metav1.Condition, []string) {
				t.Helper()
				recorder := events.NewFakeRecorder(10)
				// A new watch each time, as after a restart of the controller.
				w := &RollWatch{Client: kube, ClassNamespace: "kubehenge-system", APIs: &fakeAPIs{}, Events: recorder, Now: func() time.Time { return etcdNow }}
				got := &clusterv1.Cluster{}
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
					t.Fatal(err)
				}
				if err := w.watch(context.Background(), got); err != nil {
					t.Fatal(err)
				}
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
					t.Fatal(err)
				}
				close(recorder.Events)
				var said []string
				for e := range recorder.Events {
					said = append(said, e)
				}
				return meta.FindStatusCondition(got.Status.Conditions, RollStalledCondition), said
			}
			cond, said := look()
			if !tc.want {
				if cond != nil || len(said) != 0 {
					t.Fatalf("condition %+v, events %q", cond, said)
				}
				return
			}
			if cond == nil || !strings.Contains(cond.Message, "web-cp-new, which replaces web-cp-old, has not become ready in 1h0m0s") {
				t.Fatalf("condition %+v", cond)
			}
			if len(said) != 1 || !strings.Contains(said[0], "Warning "+RollStalledCondition) {
				t.Errorf("events %q", said)
			}
			if cond, said := look(); cond == nil || len(said) != 0 {
				t.Errorf("after a restart: condition %+v, events %q", cond, said)
			}
			// The roll ends once the old machine is gone.
			if err := kube.Delete(context.Background(), kept); err != nil {
				t.Fatal(err)
			}
			if cond, said := look(); cond != nil || len(said) != 1 || !strings.Contains(said[0], "ControlPlaneRollResumed") {
				t.Errorf("after the roll: condition %+v, events %q", cond, said)
			}
		})
	}
}

func TestRollPair(t *testing.T) {
	t.Parallel()
	old, _ := controlPlane("a", 1, "pve1")
	gone, _ := controlPlane("b", 2, "pve1")
	newer, _ := controlPlane("c", 3, "pve1")
	old.CreationTimestamp = metav1.NewTime(etcdNow.Add(-2 * time.Hour))
	gone.CreationTimestamp = metav1.NewTime(etcdNow.Add(-time.Hour))
	gone.DeletionTimestamp = new(metav1.NewTime(etcdNow))
	newer.CreationTimestamp = metav1.NewTime(etcdNow)
	r, ok := rollPair([]clusterv1.Machine{*newer, *gone, *old})
	if !ok || r.successor.Name != "c" || r.kept.Name != "a" {
		t.Errorf("roll %+v, %v", r, ok)
	}
	if _, ok := rollPair([]clusterv1.Machine{*newer}); ok {
		t.Error("found a roll in one machine")
	}
	if _, ok := rollPair(nil); ok {
		t.Error("found a roll in no machine")
	}
}
