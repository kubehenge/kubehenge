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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var talosControlPlaneGVK = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1alpha3", Kind: "TalosControlPlane"}

// newTalosEtcdFixture is web as a Talos cluster, its TalosControlPlane bootstrapped as given, with
// the control-plane machines web-cp-0 to web-cp-2 at 10.0.0.11 to 10.0.0.13.
func newTalosEtcdFixture(t *testing.T, bootstrapped bool, change func(*clusterv1.Cluster)) *etcdWatchFixture {
	t.Helper()
	cp := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"bootstrapped": bootstrapped}}}
	cp.SetGroupVersionKind(talosControlPlaneGVK)
	cp.SetNamespace("team-a")
	cp.SetName("web-cp")
	return newEtcdWatchFixture(t, func(c *clusterv1.Cluster) {
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

// What is held of a rebooted node goes once its etcd runs, so that etcd failing again in the same
// boot is started again, and once its machine is gone.
func TestEtcdWatchForgetsRebootedNodes(t *testing.T) {
	t.Parallel()
	f := newTalosEtcdFixture(t, true, nil)
	f.talos.service = map[string]TalosService{
		"10.0.0.12": {State: talosEtcdFailed, Event: fakePullFailure},
		"10.0.0.13": {State: talosEtcdFailed, Event: fakePullFailure},
	}
	f.look(0)
	held := func() []string {
		f.w.mu.Lock()
		defer f.w.mu.Unlock()
		keys := make([]string, 0, len(f.w.etcdLeft))
		for key := range f.w.etcdLeft {
			keys = append(keys, key[strings.LastIndex(key, "/")+1:])
		}
		slices.Sort(keys)
		return keys
	}
	if got := held(); !slices.Equal(got, []string{"web-cp-1", "web-cp-2"}) {
		t.Fatalf("held %v", got)
	}
	f.talos.service["10.0.0.12"] = TalosService{State: talosEtcdRunning, Healthy: true}
	f.remove("web-cp-2")
	f.look(time.Minute)
	if got := held(); len(got) > 0 {
		t.Errorf("held %v once etcd runs and the machine is gone", got)
	}
	f.talos.service["10.0.0.12"] = TalosService{State: talosEtcdFailed, Event: fakePullFailure}
	f.look(time.Minute)
	if !slices.Equal(f.talos.rebooted, []string{"10.0.0.12", "10.0.0.13", "10.0.0.12"}) {
		t.Errorf("rebooted %v", f.talos.rebooted)
	}
	c := f.cluster()
	c.Finalizers = []string{clusterv1.ClusterFinalizer}
	if err := f.kube.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.look(time.Minute)
	if got := held(); len(got) > 0 {
		t.Errorf("held %v once the cluster is being deleted", got)
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

// Only Failed is acted on, and never in a cluster being deleted, under a restore or paused.
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
