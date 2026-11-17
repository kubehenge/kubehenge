package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var talosControlPlaneGVK = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1alpha3", Kind: "TalosControlPlane"}

// newTalosEtcdFixture is web as a new Talos cluster, its control plane not initialised and its
// TalosControlPlane bootstrapped as given, with the control-plane machines web-cp-0 to web-cp-2 at
// 10.0.0.11 to 10.0.0.13.
func newTalosEtcdFixture(t *testing.T, bootstrapped bool, change func(*clusterv1.Cluster)) *etcdWatchFixture {
	t.Helper()
	cp := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"bootstrapped": bootstrapped}}}
	cp.SetGroupVersionKind(talosControlPlaneGVK)
	cp.SetNamespace("team-a")
	cp.SetName("web-cp")
	return newEtcdWatchFixture(t, func(c *clusterv1.Cluster) {
		meta.RemoveStatusCondition(&c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition)
		c.Spec.Topology.ClassRef.Name = ClassPrefix + "talos"
		c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: talosControlPlaneGVK.Group, Kind: talosControlPlaneGVK.Kind, Name: "web-cp"}
		if change != nil {
			change(c)
		}
	}, cp, &corev1.Secret{Namespace: "team-a", Name: TalosconfigSecret("web"), Data: map[string][]byte{TalosconfigKey: []byte("talosconfig of web")}})
}

// remove deletes control-plane machines, gone at once.
func (f *etcdWatchFixture) remove(names ...string) {
	f.t.Helper()
	for _, name := range names {
		m := f.machine(name)
		m.Finalizers = nil
		if err := f.kube.Update(context.Background(), m); err != nil {
			f.t.Fatal(err)
		}
		f.deleting(name)
	}
}

// deleting deletes a control-plane machine, which its finalizer keeps being deleted.
func (f *etcdWatchFixture) deleting(name string) {
	f.t.Helper()
	if err := f.kube.Delete(context.Background(), f.machine(name)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *etcdWatchFixture) machine(name string) *clusterv1.Machine {
	f.t.Helper()
	m := &clusterv1.Machine{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *etcdWatchFixture) failEtcd(address string) {
	f.talos.service = map[string]TalosService{address: {State: talosEtcdFailed, Event: fakePullFailure}}
}

// A joining control-plane node whose etcd Talos left Failed is rebooted, once per boot, and never
// bootstrapped.
func TestEtcdWatchRebootsAFailedJoiner(t *testing.T) {
	t.Parallel()
	f := newTalosEtcdFixture(t, true, nil)
	f.failEtcd("10.0.0.12")
	f.look(0)
	f.look(time.Minute)
	if !slices.Equal(f.talos.rebooted, []string{"10.0.0.12"}) {
		t.Fatalf("rebooted %v in one boot", f.talos.rebooted)
	}
	f.talos.boots = map[string]int{"10.0.0.12": 1}
	f.look(time.Minute)
	if !slices.Equal(f.talos.rebooted, []string{"10.0.0.12", "10.0.0.12"}) {
		t.Errorf("rebooted %v after the next boot", f.talos.rebooted)
	}
	if calls := f.talos.restoreCalls(); len(calls) > 0 {
		t.Errorf("calls %v", calls)
	}
}

// The only control-plane node, whose etcd CACPPT bootstrapped once, is bootstrapped again each time
// Talos leaves its etcd Failed, since a reboot would leave it waiting for a bootstrap.
func TestEtcdWatchBootstrapsTheOnlyEtcdAgain(t *testing.T) {
	t.Parallel()
	f := newTalosEtcdFixture(t, true, nil)
	f.remove("web-cp-1")
	f.remove("web-cp-2")
	f.failEtcd("10.0.0.11")
	f.look(0)
	f.look(time.Minute)
	if calls := f.talos.restoreCalls(); !slices.Equal(calls, []string{"10.0.0.11 plain bootstrap", "10.0.0.11 plain bootstrap"}) {
		t.Errorf("calls %v", calls)
	}
	if len(f.talos.rebooted) > 0 {
		t.Errorf("rebooted %v", f.talos.rebooted)
	}
}

// The only node whose bootstrap Talos refuses, as it does while etcd's data directory holds
// anything, is rebooted instead, once in that boot, to start etcd from its data.
func TestEtcdWatchRebootsOnARefusedBootstrap(t *testing.T) {
	t.Parallel()
	f := newTalosEtcdFixture(t, true, nil)
	f.remove("web-cp-1")
	f.remove("web-cp-2")
	f.failEtcd("10.0.0.11")
	f.talos.fail = func(_, call string) error {
		if call == "plain bootstrap" {
			return status.Error(codes.AlreadyExists, "etcd data directory is not empty")
		}
		return nil
	}
	f.look(0)
	f.look(time.Minute)
	if calls := f.talos.restoreCalls(); !slices.Equal(calls, []string{"10.0.0.11 plain bootstrap"}) {
		t.Errorf("calls %v", calls)
	}
	if !slices.Equal(f.talos.rebooted, []string{"10.0.0.11"}) {
		t.Errorf("rebooted %v", f.talos.rebooted)
	}
	want := "Talos left etcd on web-cp-0 Failed: " + fakePullFailure +
		". Talos refused to bootstrap it again: rpc error: code = AlreadyExists desc = etcd data directory is not empty. Rebooted the node to start etcd again."
	if got := recorded(f.events); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("events %v", got)
	}
}

// The starts of a rebooted node are counted on its Machine, with the boot it was rebooted from, and
// forgotten once its etcd runs, so that etcd failing again in the same boot is started again.
func TestEtcdWatchForgetsRepairsOnceEtcdRuns(t *testing.T) {
	t.Parallel()
	f := newTalosEtcdFixture(t, true, nil)
	f.talos.service = map[string]TalosService{
		"10.0.0.12": {State: talosEtcdFailed, Event: fakePullFailure},
		"10.0.0.13": {State: talosEtcdFailed, Event: fakePullFailure},
	}
	f.look(0)
	for _, name := range []string{"web-cp-1", "web-cp-2"} {
		if got := f.machine(name).Annotations[EtcdRepairsAnnotation]; got != `{"boot":"0","repairs":1}` {
			t.Fatalf("%s: %s %q", name, EtcdRepairsAnnotation, got)
		}
	}
	f.talos.service["10.0.0.12"] = TalosService{State: talosEtcdRunning, Healthy: true}
	f.look(time.Minute)
	if got, held := f.machine("web-cp-1").Annotations[EtcdRepairsAnnotation]; held {
		t.Errorf("%s %q once etcd runs", EtcdRepairsAnnotation, got)
	}
	if _, held := f.machine("web-cp-2").Annotations[EtcdRepairsAnnotation]; !held {
		t.Errorf("%s gone while etcd stays Failed", EtcdRepairsAnnotation)
	}
	f.talos.service["10.0.0.12"] = TalosService{State: talosEtcdFailed, Event: fakePullFailure}
	f.look(time.Minute)
	if !slices.Equal(f.talos.rebooted, []string{"10.0.0.12", "10.0.0.13", "10.0.0.12"}) {
		t.Errorf("rebooted %v", f.talos.rebooted)
	}
}

// A controller that starts again, or another one that takes the lease, reads the count from the
// Machine: two watches one after the other together start etcd maxEtcdRepairs times and give up
// once.
func TestEtcdWatchKeepsTheCountAcrossControllers(t *testing.T) {
	t.Parallel()
	f := newTalosEtcdFixture(t, true, nil)
	f.failEtcd("10.0.0.12")
	first := f.w
	second := &EtcdWatch{
		Client: first.Client, Reader: first.Reader, ClassNamespace: first.ClassNamespace, Proxmox: first.Proxmox,
		APIs: first.APIs, Events: first.Events, Workload: first.Workload, Talos: first.Talos, Now: first.Now,
	}
	for boot := range 6 {
		f.w = first
		if boot >= 2 {
			f.w = second
		}
		f.talos.boots = map[string]int{"10.0.0.12": boot}
		f.look(3 * time.Minute)
	}
	if len(f.talos.rebooted) != 3 {
		t.Errorf("rebooted %v", f.talos.rebooted)
	}
	got := recorded(f.events)
	gaveUp := slices.DeleteFunc(slices.Clone(got), func(e string) bool { return !strings.Contains(e, "TalosEtcdLeftFailed") })
	if len(got) != 4 || len(gaveUp) != 1 {
		t.Errorf("events %v", got)
	}
}

// An upgrade the upgrader records after the round listed the cluster still keeps the watch off its
// nodes.
func TestEtcdWatchReadsTheClusterAgainBeforeAStart(t *testing.T) {
	t.Parallel()
	for _, only := range []bool{false, true} {
		f := newTalosEtcdFixture(t, true, nil)
		if only {
			f.remove("web-cp-1", "web-cp-2")
		}
		f.failEtcd("10.0.0.11")
		listed := f.cluster()
		stored := listed.DeepCopy()
		stored.Annotations = map[string]string{TalosUpgradingAnnotation: "web-cp-0 to v1.14.6 since 2026-11-17T10:00:00Z"}
		if err := f.kube.Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		look, err := f.w.readProxmox(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.w.watch(context.Background(), listed, look); err != nil {
			t.Fatal(err)
		}
		if calls := f.talos.restoreCalls(); len(calls) > 0 || len(f.talos.rebooted) > 0 {
			t.Errorf("only %t: calls %v, rebooted %v", only, calls, f.talos.rebooted)
		}
	}
}

// A node is the cluster's only etcd only while CACPPT has bootstrapped the control plane and no other
// control-plane machine exists, not even one being deleted. Otherwise it is a joiner.
func TestEtcdWatchBootstrapsOnlyTheOnlyEtcd(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		bootstrapped bool
		drop         func(f *etcdWatchFixture)
	}{
		"a second machine": {true, func(f *etcdWatchFixture) { f.remove("web-cp-2") }},
		"a second machine being deleted": {true, func(f *etcdWatchFixture) {
			f.deleting("web-cp-1")
			f.remove("web-cp-2")
		}},
		"not bootstrapped": {false, func(f *etcdWatchFixture) {
			f.remove("web-cp-1")
			f.remove("web-cp-2")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newTalosEtcdFixture(t, tc.bootstrapped, nil)
			tc.drop(f)
			f.failEtcd("10.0.0.11")
			f.look(0)
			if !slices.Equal(f.talos.rebooted, []string{"10.0.0.11"}) {
				t.Errorf("rebooted %v", f.talos.rebooted)
			}
			if calls := f.talos.restoreCalls(); len(calls) > 0 {
				t.Errorf("calls %v", calls)
			}
		})
	}
}

// The event of each start says which node it was and what Talos said of the failure.
func TestEtcdWatchEventCarriesTalosReason(t *testing.T) {
	t.Parallel()
	f := newTalosEtcdFixture(t, true, nil)
	f.failEtcd("10.0.0.13")
	f.look(0)
	want := "Talos left etcd on web-cp-2 Failed: " + fakePullFailure + ". Rebooted the node to start etcd again."
	if got := recorded(f.events); len(got) != 1 || !strings.Contains(got[0], "TalosEtcdFailed") || !strings.Contains(got[0], want) {
		t.Errorf("events %v", got)
	}

	f = newTalosEtcdFixture(t, true, nil)
	f.remove("web-cp-1")
	f.remove("web-cp-2")
	f.failEtcd("10.0.0.11")
	f.look(0)
	want = "Talos left etcd on web-cp-0 Failed: " + fakePullFailure + ". Bootstrapped it again."
	if got := recorded(f.events); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("events %v", got)
	}
}

// Only Failed is acted on, and never in a cluster being deleted, under a restore or a Talos upgrade,
// or paused.
func TestEtcdWatchLeavesEtcdAlone(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		state  string
		change func(*clusterv1.Cluster)
	}{
		"running":   {talosEtcdRunning, nil},
		"preparing": {talosEtcdPreparing, nil},
		"waiting":   {"Waiting", nil},
		"deleting": {talosEtcdFailed, func(c *clusterv1.Cluster) {
			c.Finalizers = []string{clusterv1.ClusterFinalizer}
			c.DeletionTimestamp = &metav1.Time{Time: etcdNow}
		}},
		"restoring": {talosEtcdFailed, func(c *clusterv1.Cluster) { c.Annotations = map[string]string{EtcdRestoreAnnotation: "{}"} }},
		"paused":    {talosEtcdFailed, func(c *clusterv1.Cluster) { c.Spec.Paused = new(true) }},
		"upgrading": {talosEtcdFailed, func(c *clusterv1.Cluster) {
			c.Annotations = map[string]string{TalosUpgradingAnnotation: "web-cp-0 to v1.14.6 since 2026-11-17T10:00:00Z"}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, only := range []bool{false, true} {
				f := newTalosEtcdFixture(t, true, tc.change)
				if only {
					f.remove("web-cp-1")
					f.remove("web-cp-2")
				}
				f.talos.service = map[string]TalosService{"10.0.0.11": {State: tc.state, Event: fakePullFailure}}
				f.look(0)
				if calls := f.talos.restoreCalls(); len(calls) > 0 || len(f.talos.rebooted) > 0 {
					t.Errorf("only %t: calls %v, rebooted %v", only, calls, f.talos.rebooted)
				}
				if got := recorded(f.events); len(got) > 0 {
					t.Errorf("only %t: events %v", only, got)
				}
			}
		})
	}
}

// The only control-plane node of a cluster whose control plane was initialised once had etcd data,
// which a bootstrap would replace with an empty etcd. It is rebooted instead, whatever Talos says.
func TestEtcdWatchNeverBootstrapsAnInitialisedCluster(t *testing.T) {
	t.Parallel()
	for _, event := range []string{"Failed to run pre stage: failed to build initial etcd cluster: timeout", fakePullFailure} {
		f := newTalosEtcdFixture(t, true, func(c *clusterv1.Cluster) {
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue, Reason: clusterv1.ClusterControlPlaneInitializedReason,
			})
		})
		f.remove("web-cp-0", "web-cp-1")
		f.talos.service = map[string]TalosService{"10.0.0.13": {State: talosEtcdFailed, Event: event}}
		f.look(0)
		if calls := f.talos.restoreCalls(); len(calls) > 0 {
			t.Errorf("%s: calls %v", event, calls)
		}
		if !slices.Equal(f.talos.rebooted, []string{"10.0.0.13"}) {
			t.Errorf("%s: rebooted %v", event, f.talos.rebooted)
		}
	}
}

// A bootstrap the only node is not ready for, as before Talos can take one, is asked for again on
// the next look, and the node is not rebooted.
func TestEtcdWatchBootstrapsAgainOnceTheNodeIsReady(t *testing.T) {
	t.Parallel()
	f := newTalosEtcdFixture(t, true, nil)
	f.remove("web-cp-1", "web-cp-2")
	f.failEtcd("10.0.0.11")
	refused := false
	f.talos.fail = func(_, call string) error {
		if call == "plain bootstrap" && !refused {
			refused = true
			return notReady(status.Error(codes.FailedPrecondition, "bootstrap is not available yet"))
		}
		return nil
	}
	f.look(0)
	if len(f.talos.rebooted) > 0 || len(recorded(f.events)) > 0 {
		t.Fatalf("not ready: rebooted %v", f.talos.rebooted)
	}
	f.look(time.Minute)
	if calls := f.talos.restoreCalls(); !slices.Equal(calls, []string{"10.0.0.11 plain bootstrap", "10.0.0.11 plain bootstrap"}) {
		t.Errorf("calls %v", calls)
	}
	if len(f.talos.rebooted) > 0 {
		t.Errorf("rebooted %v", f.talos.rebooted)
	}
	if got := recorded(f.events); len(got) != 1 || !strings.Contains(got[0], "Bootstrapped it again.") {
		t.Errorf("events %v", got)
	}
	// The answer that came before Talos could take a bootstrap is not a start.
	if got := f.machine("web-cp-0").Annotations[EtcdRepairsAnnotation]; got != `{"repairs":1}` {
		t.Errorf("%s %q", EtcdRepairsAnnotation, got)
	}
}

// A node whose etcd fails on every boot is started again maxEtcdRepairs times, then left Failed
// with one event that says so, until its etcd runs again.
func TestEtcdWatchGivesUpAfterThreeRepairs(t *testing.T) {
	t.Parallel()
	f := newTalosEtcdFixture(t, true, nil)
	f.failEtcd("10.0.0.12")
	for boot := range 6 {
		f.talos.boots = map[string]int{"10.0.0.12": boot}
		f.look(3 * time.Minute)
	}
	if len(f.talos.rebooted) != 3 {
		t.Errorf("rebooted %v", f.talos.rebooted)
	}
	got := recorded(f.events)
	want := "Talos left etcd on web-cp-1 Failed: " + fakePullFailure + ". Left it Failed after 3 starts since it last ran."
	if len(got) != 4 || !strings.Contains(got[3], "TalosEtcdLeftFailed") || !strings.Contains(got[3], want) {
		t.Errorf("events %v", got)
	}
	f.talos.service["10.0.0.12"] = TalosService{State: talosEtcdRunning, Healthy: true}
	f.look(time.Minute)
	f.failEtcd("10.0.0.12")
	f.talos.boots = map[string]int{"10.0.0.12": 6}
	f.look(time.Minute)
	if len(f.talos.rebooted) != 4 {
		t.Errorf("rebooted %v once etcd ran again", f.talos.rebooted)
	}

	// The only node's bootstraps count the same, within one boot.
	g := newTalosEtcdFixture(t, true, nil)
	g.remove("web-cp-1", "web-cp-2")
	g.failEtcd("10.0.0.11")
	for range 10 {
		g.look(30 * time.Second)
	}
	if calls := g.talos.restoreCalls(); len(calls) != 3 || len(g.talos.rebooted) > 0 {
		t.Errorf("calls %v, rebooted %v", calls, g.talos.rebooted)
	}
	if got := recorded(g.events); len(got) != 4 || !strings.Contains(got[3], "TalosEtcdLeftFailed") {
		t.Errorf("events %v", got)
	}
}
