package provisioner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// drainCluster is a Talos cluster to upgrade to v1.14.4 whose control plane and pool md-0 set the
// drain timeouts given, nil for none.
func drainCluster(controlPlane, pool *int32) *clusterv1.Cluster {
	c := talosCluster("web", "v1.14.4")
	c.Spec.Topology.ControlPlane.Deletion.NodeDrainTimeoutSeconds = controlPlane
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "default-worker", Name: "md-0"}}
	c.Spec.Topology.Workers.MachineDeployments[0].Deletion.NodeDrainTimeoutSeconds = pool
	return c
}

// talosClassOf is the class of drainCluster, with the drain timeouts given for its control plane and workers.
func talosClassOf(controlPlane, workers *int32) *clusterv1.ClusterClass {
	cc := &clusterv1.ClusterClass{Namespace: "kubehenge-system", Name: ClassPrefix + "talos"}
	cc.Spec.ControlPlane.Deletion.NodeDrainTimeoutSeconds = controlPlane
	cc.Spec.Workers.MachineDeployments = []clusterv1.MachineDeploymentClass{{Class: "default-worker"}}
	cc.Spec.Workers.MachineDeployments[0].Deletion.NodeDrainTimeoutSeconds = workers
	return cc
}

// drainPoolMachine is a Ready worker of pool md-0 on a Talos release.
func drainPoolMachine(name, address, release string) *clusterv1.Machine {
	m := talosMachine(name, address, release)
	m.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel] = "md-0"
	return m
}

// upgradeBoth upgrades the control plane's node and then the worker, each to the end, and returns
// the timeouts their drains were given.
func upgradeBoth(t *testing.T, objects ...client.Object) []time.Duration {
	t.Helper()
	r := newTalosRig(t, append(objects, talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"), drainPoolMachine("web-md-0", "192.0.2.20", "v1.14.3"))...)
	r.run(t)
	r.setMachine(t, "web-cp-0", "v1.14.4", metav1.ConditionTrue)
	r.run(t)
	r.run(t)
	r.setMachine(t, "web-md-0", "v1.14.4", metav1.ConditionTrue)
	if c := r.run(t); c.Annotations[TalosUpgradingAnnotation] != "" || c.Annotations[TalosWaitingAnnotation] != "" {
		t.Fatalf("the upgrade did not end: %v", c.Annotations)
	}
	if want := []string{"web-cp-0", "web-md-0"}; !slices.Equal(r.drainer.drained, want) {
		t.Fatalf("drained %v, want %v", r.drainer.drained, want)
	}
	return r.drainer.timeouts
}

// The control plane's nodes drain for the control plane's timeout and a pool's nodes for the pool's,
// whatever the class says.
func TestTalosDrainUsesTheTimeoutOfTheNodesPlace(t *testing.T) {
	t.Parallel()
	got := upgradeBoth(t, drainCluster(new(int32(120)), new(int32(7200))), talosClassOf(new(int32(5)), new(int32(6))))
	if want := []time.Duration{2 * time.Minute, 2 * time.Hour}; !slices.Equal(got, want) {
		t.Errorf("timeouts %v, want %v", got, want)
	}
}

// Without a timeout of its own a node takes its class's, then the one the shipped classes set. A
// timeout of 0 is its own and waits for ever.
func TestTalosDrainTimeoutFallsBackToTheClassThenTheDefault(t *testing.T) {
	t.Parallel()
	hour := time.Duration(DefaultNodeDrainTimeoutSeconds) * time.Second
	for name, c := range map[string]struct {
		objects []client.Object
		want    []time.Duration
	}{
		"the class's": {
			[]client.Object{drainCluster(nil, nil), talosClassOf(new(int32(1800)), new(int32(900)))},
			[]time.Duration{30 * time.Minute, 15 * time.Minute},
		},
		"the default, class without one": {
			[]client.Object{drainCluster(nil, nil), talosClassOf(nil, nil)},
			[]time.Duration{hour, hour},
		},
		"the default, no class": {
			[]client.Object{drainCluster(nil, nil)},
			[]time.Duration{hour, hour},
		},
		"zero is for ever": {
			[]client.Object{drainCluster(new(int32(0)), new(int32(0))), talosClassOf(new(int32(5)), new(int32(5)))},
			[]time.Duration{0, 0},
		},
		"the pool's own, the control plane's class's": {
			[]client.Object{drainCluster(nil, new(int32(60))), talosClassOf(new(int32(1800)), new(int32(900)))},
			[]time.Duration{30 * time.Minute, time.Minute},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := upgradeBoth(t, c.objects...); !slices.Equal(got, c.want) {
				t.Errorf("timeouts %v, want %v", got, c.want)
			}
		})
	}
}

// waiting is what cluster web says its node waits for.
func (r *talosRig) waiting(t *testing.T) string {
	t.Helper()
	c := &clusterv1.Cluster{}
	if err := r.kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	return c.Annotations[TalosWaitingAnnotation]
}

// allEvents are the events recorded so far.
func (r *talosRig) allEvents() []string {
	var got []string
	for {
		select {
		case e := <-r.events.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

const heldByBudget = "Pod apps/db-0: cannot evict pod as it would violate the pod's disruption budget. The disruption budget db-pdb needs 2 healthy pods and has 2 currently"

// A drain a disruption budget holds is on the cluster page and in one event while it waits, with the
// pod and the budget, and both are gone once the drain is done.
func TestTalosDrainHeldByABudgetIsReportedWithItsName(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	var seen []string
	r.drainer.drainFn = func(o DrainOptions) error {
		for range 3 {
			o.Blocked(heldByBudget, true)
			seen = append(seen, r.waiting(t))
		}
		return nil
	}
	c := r.run(t)
	for _, s := range seen {
		if want := "Draining web-cp-0: " + heldByBudget; s != want {
			t.Errorf("the cluster page said %q, want %q", s, want)
		}
	}
	if _, left := c.Annotations[TalosWaitingAnnotation]; left {
		t.Errorf("still waiting after the drain: %v", c.Annotations)
	}
	events := r.allEvents()
	blocked := slices.DeleteFunc(slices.Clone(events), func(e string) bool { return !strings.Contains(e, " TalosDrainBlocked ") })
	if want := "Warning TalosDrainBlocked node web-cp-0 is held while it drains: " + heldByBudget; !slices.Equal(blocked, []string{want}) {
		t.Errorf("blocked events %q, want one: %q", blocked, want)
	}
	if !slices.Contains(events, "Normal TalosDrained node web-cp-0 drained before its reboot") {
		t.Errorf("events %q", events)
	}
}

// At its timeout a held drain goes on as a Cluster API Machine's deletion does: the node reboots, the
// pods left stop with it, and the upgrade is no failure.
func TestTalosDrainTimeoutRebootsWithThePodsLeft(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, drainCluster(new(int32(60)), nil), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.drainer.drainFn = func(o DrainOptions) error {
		o.Blocked(heldByBudget, true)
		return fmt.Errorf("draining node web-cp-0: %w", ErrDrainTimeout)
	}
	c := r.run(t)
	if !slices.Equal(r.talos.rebooted, []string{"192.0.2.10"}) {
		t.Fatalf("rebooted %v", r.talos.rebooted)
	}
	if p, ok := parseTalosProgress(c.Annotations[TalosUpgradingAnnotation]); !ok || p.rebooted.IsZero() {
		t.Errorf("upgrading %q", c.Annotations[TalosUpgradingAnnotation])
	}
	if f, failed := c.Annotations[TalosUpgradeFailedAnnotation]; failed {
		t.Errorf("failed: %s", f)
	}
	if _, left := c.Annotations[TalosWaitingAnnotation]; left {
		t.Errorf("still waiting: %v", c.Annotations)
	}
	events := r.allEvents()
	if want := "Warning TalosDrain node web-cp-0 reboots without all its pods moved: the drain timeout of 1m0s ran out, with " + heldByBudget; !slices.Contains(events, want) {
		t.Errorf("events %q lack %q", events, want)
	}
	if slices.ContainsFunc(events, func(e string) bool { return strings.Contains(e, " TalosDrained ") }) {
		t.Errorf("reported the node drained: %q", events)
	}
}

// A drain that fails for another reason is tried again until the timeout, as Cluster API requeues a
// Machine's, and then the node reboots as at any timeout.
func TestTalosDrainErrorsAreRetriedUntilTheTimeout(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, drainCluster(new(int32(60)), nil), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	calls := 0
	r.drainer.drainFn = func(DrainOptions) error {
		calls++
		if calls == 3 {
			r.now = r.now.Add(time.Minute)
		}
		return errors.New("the cluster has no kubeconfig yet")
	}
	r.run(t)
	if calls != 3 || !slices.Equal(r.talos.rebooted, []string{"192.0.2.10"}) {
		t.Fatalf("drained %d times, rebooted %v", calls, r.talos.rebooted)
	}
	events := r.allEvents()
	retry := slices.DeleteFunc(slices.Clone(events), func(e string) bool {
		return !strings.HasPrefix(e, "Warning TalosDrain node web-cp-0 is not drained yet")
	})
	if len(retry) != 1 {
		t.Errorf("one report of the error expected, got %q", retry)
	}
	if want := "Warning TalosDrain node web-cp-0 reboots without all its pods moved: the drain timeout of 1m0s ran out, with the cluster has no kubeconfig yet"; !slices.Contains(events, want) {
		t.Errorf("events %q lack %q", events, want)
	}
}

// Before it drains a node the upgrade waits until no StatefulSet is short of ready replicas, says so
// on the cluster page, and drains once they are back.
func TestTalosStatefulSetWaitHoldsThenReleases(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	const short = "StatefulSet apps/db has 2 of 3 replicas ready"
	var mu sync.Mutex
	var seen []string
	r.drainer.shortFn = func(look int) []string {
		if look > 3 {
			return nil
		}
		mu.Lock()
		seen = append(seen, r.waiting(t))
		mu.Unlock()
		return []string{short}
	}
	looksAtDrain := 0
	r.drainer.drainFn = func(DrainOptions) error {
		looksAtDrain = r.drainer.looks
		if got := r.waiting(t); got != "" {
			t.Errorf("the wait for StatefulSets is still on the page: %q", got)
		}
		return nil
	}
	c := r.run(t)
	if looksAtDrain != 4 {
		t.Errorf("drained after %d looks, want 4", looksAtDrain)
	}
	// A look reads the page before it writes, so the first sees nothing.
	if want := "Waiting to drain web-cp-0: " + short; len(seen) != 3 || seen[0] != "" || seen[1] != want || seen[2] != want {
		t.Errorf("the cluster page said %q", seen)
	}
	if _, left := c.Annotations[TalosWaitingAnnotation]; left {
		t.Errorf("still waiting: %v", c.Annotations)
	}
	events := r.allEvents()
	waits := slices.DeleteFunc(slices.Clone(events), func(e string) bool { return !strings.Contains(e, " TalosStatefulSetsWaiting ") })
	if want := "Normal TalosStatefulSetsWaiting node web-cp-0 waits to drain: " + short; !slices.Equal(waits, []string{want}) {
		t.Errorf("wait events %q, want one: %q", waits, want)
	}
	if !slices.Equal(r.drainer.drained, []string{"web-cp-0"}) || len(r.talos.rebooted) != 1 {
		t.Errorf("drained %v, rebooted %v", r.drainer.drained, r.talos.rebooted)
	}
}

// The wait for StatefulSets ends at the node's drain timeout, with a warning, and the drain goes on.
func TestTalosStatefulSetWaitEndsAtTheTimeout(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, drainCluster(new(int32(600)), nil), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.drainer.shortFn = func(look int) []string {
		if look == 3 {
			r.now = r.now.Add(10 * time.Minute)
		}
		return []string{"StatefulSet apps/db has 0 of 3 replicas ready"}
	}
	r.run(t)
	if !slices.Equal(r.drainer.drained, []string{"web-cp-0"}) || len(r.talos.rebooted) != 1 {
		t.Fatalf("drained %v, rebooted %v", r.drainer.drained, r.talos.rebooted)
	}
	if r.drainer.looks != 3 {
		t.Errorf("looked %d times, want 3", r.drainer.looks)
	}
	want := "Warning TalosStatefulSetsNotReady node web-cp-0 drains after waiting 10m0s, with StatefulSet apps/db has 0 of 3 replicas ready"
	if events := r.allEvents(); !slices.Contains(events, want) {
		t.Errorf("events %q lack %q", events, want)
	}
}

// A timeout of 0 waits for the StatefulSets for ever.
func TestTalosStatefulSetWaitWithoutATimeoutWaitsForEver(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, drainCluster(new(int32(0)), nil), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.drainer.shortFn = func(look int) []string {
		r.now = r.now.Add(48 * time.Hour)
		if look >= 30 {
			return nil
		}
		return []string{"StatefulSet apps/db has 1 of 3 replicas ready"}
	}
	r.run(t)
	if r.drainer.looks != 30 || !slices.Equal(r.drainer.drained, []string{"web-cp-0"}) {
		t.Errorf("looked %d times, drained %v", r.drainer.looks, r.drainer.drained)
	}
}

// With one node there is no next node to hold back, and nothing else runs: the node is waited for,
// drained and rebooted, and the upgrade ends with nothing left on the cluster page.
func TestTalosSingleNodeClusterWaitsDrainsAndFinishes(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.drainer.shortFn = func(look int) []string {
		if look == 1 {
			return []string{"StatefulSet apps/db has 0 of 1 replicas ready"}
		}
		return nil
	}
	r.drainer.drainFn = func(o DrainOptions) error {
		o.Blocked(heldByBudget, true)
		return nil
	}
	r.run(t)
	r.setMachine(t, "web-cp-0", "v1.14.4", metav1.ConditionTrue)
	c := r.run(t)
	if _, upgrading := c.Annotations[TalosUpgradingAnnotation]; upgrading ||
		!strings.HasPrefix(c.Annotations[TalosUpgradedAnnotation], "web-cp-0 to v1.14.4 at ") {
		t.Errorf("annotations %v", c.Annotations)
	}
	if _, left := c.Annotations[TalosWaitingAnnotation]; left {
		t.Errorf("still waiting: %v", c.Annotations)
	}
	if !slices.Equal(r.drainer.drained, []string{"web-cp-0"}) || !slices.Equal(r.drainer.uncordoned, []string{"web-cp-0"}) {
		t.Errorf("drained %v, uncordoned %v", r.drainer.drained, r.drainer.uncordoned)
	}
	if again := r.run(t); again.Annotations[TalosUpgradingAnnotation] != "" || len(r.talos.rebooted) != 1 {
		t.Errorf("went on after the last node: %v", again.Annotations)
	}
}
