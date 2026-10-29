package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
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

			// Looked at again, nothing new is said; once the cluster is paused for a recovery, the
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
