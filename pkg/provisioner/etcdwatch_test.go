package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type etcdWatchFixture struct {
	t      *testing.T
	kube   client.Client
	pve    fakeProxmox
	apis   *fakeAPIs
	talos  *fakeTalos
	events *events.FakeRecorder
	now    time.Time
	w      *EtcdWatch
}

// newEtcdWatchFixture is web with three control-plane machines, VMs 101 to 103 on pve1 to pve3, all
// running. Its etcd pods are all ready.
func newEtcdWatchFixture(t *testing.T, change func(*clusterv1.Cluster), extra ...client.Object) *etcdWatchFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	listGVK := proxmoxMachineGVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(proxmoxMachineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	c := etcdCluster(`{}`)
	if change != nil {
		change(c)
	}
	objs := make([]client.Object, 1, 10+len(extra))
	objs[0] = c
	for i := range 3 {
		m, pm := controlPlane(fmt.Sprintf("web-cp-%d", i), int64(101+i), fmt.Sprintf("pve%d", i+1))
		pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "web"})
		m.Status.Addresses = clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: fmt.Sprintf("10.0.0.%d", 11+i)}}
		objs = append(objs, m, pm, etcdPod(fmt.Sprintf("web-cp-%d", i), corev1.ConditionTrue))
	}
	objs = append(objs, extra...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	f := &etcdWatchFixture{
		t: t, kube: kube, pve: fakeProxmox{}, apis: &fakeAPIs{}, talos: &fakeTalos{down: map[string]bool{}},
		events: events.NewFakeRecorder(10), now: etcdNow,
	}
	f.w = &EtcdWatch{
		Client: kube, Reader: kube, ClassNamespace: "kubehenge-system", Proxmox: f.pve, APIs: f.apis, Events: f.events,
		Workload: func(context.Context, *clusterv1.Cluster) (client.Reader, bool, error) { return kube, true, nil },
		Talos:    f.talos.dial, Now: func() time.Time { return f.now },
	}
	f.vms("running", "running", "running")
	return f
}

func etcdPod(node string, status corev1.ConditionStatus) *corev1.Pod {
	return &corev1.Pod{
		Namespace: metav1.NamespaceSystem, Name: "etcd-" + node, Labels: map[string]string{"component": "etcd"},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

// vms sets the states of VMs 101 to 103. An empty state leaves the VM out of the list, as one that
// was destroyed.
func (f *etcdWatchFixture) vms(states ...string) {
	var guests []string
	for i, s := range states {
		if s != "" {
			guests = append(guests, fmt.Sprintf(`{"vmid":%d,"node":"pve%d","type":"qemu","status":%q}`, 101+i, i+1, s))
		}
	}
	f.pve["/cluster/resources?type=vm"] = "[" + strings.Join(guests, ",") + "]"
	f.pve["/cluster/resources?type=node"] = `[{"node":"pve1","status":"online"},{"node":"pve2","status":"online"},{"node":"pve3","status":"online"}]`
}

func (f *etcdWatchFixture) cluster() *clusterv1.Cluster {
	f.t.Helper()
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil {
		f.t.Fatal(err)
	}
	return c
}

// look has the watch look once, after the time given, and returns the quorum condition and members.
func (f *etcdWatchFixture) look(after time.Duration) (*metav1.Condition, []EtcdMember) {
	f.t.Helper()
	f.now = f.now.Add(after)
	look, err := f.w.readProxmox(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	members, err := f.w.watch(context.Background(), f.cluster(), look)
	if err != nil {
		f.t.Fatal(err)
	}
	return meta.FindStatusCondition(f.cluster().Status.Conditions, QuorumLostCondition), members
}

// A cluster whose API is silent for ten minutes while fewer than a majority of its control-plane
// machines have a running VM has lost its quorum. Anything less is not that.
func TestQuorumLost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		states []string
		change func(*clusterv1.Cluster)
		down   bool
		want   string
	}{
		{name: "two of three stopped", states: []string{"running", "stopped", "stopped"}, down: true, want: "1 of 3 control-plane machines have a running VM (web-cp-0)"},
		{name: "two of three destroyed", states: []string{"", "", "running"}, down: true, want: "1 of 3 control-plane machines have a running VM (web-cp-2)"},
		{name: "none run", states: []string{"stopped", "stopped", "stopped"}, down: true, want: "0 of 3 control-plane machines have a running VM (none)"},
		{name: "a majority runs", states: []string{"running", "running", "stopped"}, down: true},
		{name: "the API answers", states: []string{"running", "stopped", "stopped"}},
		{name: "paused", states: []string{"running", "stopped", "stopped"}, down: true, change: func(c *clusterv1.Cluster) { c.Spec.Paused = new(true) }},
		{name: "never came up", states: []string{"running", "stopped", "stopped"}, down: true, change: func(c *clusterv1.Cluster) { c.Status.Conditions = nil }},
		{
			name: "being restored", states: []string{"running", "stopped", "stopped"}, down: true,
			change: func(c *clusterv1.Cluster) { askReset("web-cp-0")(c) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newEtcdWatchFixture(t, tc.change)
			f.vms(tc.states...)
			if tc.down {
				f.apis.down = 100
			}
			// The first look starts the silence.
			if cond, _ := f.look(0); cond != nil {
				t.Fatalf("condition %+v at the first look", cond)
			}
			if cond, _ := f.look(9 * time.Minute); cond != nil {
				t.Fatalf("condition %+v before the API was silent for ten minutes", cond)
			}
			cond, _ := f.look(2 * time.Minute)
			if tc.want == "" {
				if cond != nil {
					t.Fatalf("condition %+v", cond)
				}
				return
			}
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != QuorumLostReason {
				t.Fatalf("condition %+v", cond)
			}
			for _, want := range []string{tc.want, "etcd needs 2", "silent for 10m0s", EtcdRestoreAnnotation, "What only the lost members held is gone"} {
				if !strings.Contains(cond.Message, want) {
					t.Errorf("message has no %q: %s", want, cond.Message)
				}
			}
			if head := QuorumLostHead(cond.Message); !strings.HasSuffix(head, "10m0s or more.") {
				t.Errorf("head %q", head)
			}
			if !NeedsRecovery(f.cluster()) {
				t.Error("a recovery is not offered")
			}
			select {
			case e := <-f.events.Events:
				if !strings.Contains(e, "Warning "+QuorumLostCondition) {
					t.Errorf("event %q", e)
				}
			default:
				t.Error("no event")
			}
		})
	}
}

// The condition ends once the API answers again, and says so.
func TestQuorumLostEnds(t *testing.T) {
	t.Parallel()
	f := newEtcdWatchFixture(t, nil)
	f.vms("running", "stopped", "stopped")
	f.apis.down = 2
	f.look(0)
	if cond, _ := f.look(11 * time.Minute); cond == nil {
		t.Fatal("no condition")
	}
	<-f.events.Events
	if cond, _ := f.look(time.Minute); cond != nil {
		t.Fatalf("condition %+v kept after the API answered", cond)
	}
	if e := <-f.events.Events; !strings.Contains(e, "ControlPlaneQuorumBack") {
		t.Errorf("event %q", e)
	}
	if NeedsRecovery(f.cluster()) {
		t.Error("a recovery is still offered")
	}
}

func TestNeedsRecovery(t *testing.T) {
	t.Parallel()
	c := etcdCluster(`{}`)
	if NeedsRecovery(c) {
		t.Error("a healthy cluster needs a recovery")
	}
	for _, kind := range []string{RollStalledCondition, QuorumLostCondition} {
		c := etcdCluster(`{}`)
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: kind, Status: metav1.ConditionTrue, Reason: "Test"})
		if !NeedsRecovery(c) {
			t.Errorf("%s does not offer a recovery", kind)
		}
	}
}

// The watch records each control-plane machine's etcd member, with the node its VM runs on and why
// one is not healthy.
func TestEtcdWatchRecordsMembers(t *testing.T) {
	t.Parallel()
	f := newEtcdWatchFixture(t, nil)
	f.vms("running", "stopped", "running")
	if err := f.kube.Delete(context.Background(), etcdPod("web-cp-2", corev1.ConditionTrue)); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Create(context.Background(), etcdPod("web-cp-2", corev1.ConditionFalse)); err != nil {
		t.Fatal(err)
	}
	if err := f.w.round(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, found, err := ReadEtcdHealth(context.Background(), f.kube, "kubehenge-system")
	if err != nil || !found {
		t.Fatalf("record %+v, found %v, err %v", record, found, err)
	}
	if !record.Checked.Equal(etcdNow) || !slices.Equal(record.Nodes, []string{"pve1", "pve2", "pve3"}) || len(record.Clusters) != 1 {
		t.Fatalf("record %+v", record)
	}
	got := map[string]EtcdMember{}
	for _, m := range record.Clusters[0].Members {
		got[m.Machine] = m
	}
	if m := got["web-cp-0"]; !m.Healthy || m.Node != "pve1" {
		t.Errorf("web-cp-0 %+v", m)
	}
	if m := got["web-cp-1"]; m.Healthy || m.Reason != "its VM is stopped" || m.Node != "pve2" {
		t.Errorf("web-cp-1 %+v", m)
	}
	if m := got["web-cp-2"]; m.Healthy || !strings.Contains(m.Reason, "etcd pod on web-cp-2 is not ready") {
		t.Errorf("web-cp-2 %+v", m)
	}
	if record.Clusters[0].Cluster != "team-a/web" {
		t.Errorf("cluster %q", record.Clusters[0].Cluster)
	}
}

func TestEtcdWatchMemberReasons(t *testing.T) {
	t.Parallel()
	f := newEtcdWatchFixture(t, nil)
	f.vms("running", "", "running")
	f.apis.down = 100
	_, members := f.look(0)
	want := []string{"the cluster's API does not answer", "its VM does not exist in Proxmox VE", "the cluster's API does not answer"}
	for i, m := range members {
		if m.Healthy || m.Reason != want[i] {
			t.Errorf("%s: %+v, want %q", m.Machine, m, want[i])
		}
	}
	f.apis.down = 0
	f.apis.asked = 0
	if err := f.kube.Delete(context.Background(), etcdPod("web-cp-0", corev1.ConditionTrue)); err != nil {
		t.Fatal(err)
	}
	f.vms("running", "running", "running")
	_, members = f.look(0)
	if members[0].Healthy || members[0].Reason != "no etcd pod runs on web-cp-0" || !members[1].Healthy || !members[2].Healthy {
		t.Errorf("members %+v", members)
	}
}

// A Talos cluster's members are read from the etcd service of each node's Talos API.
func TestEtcdWatchTalosMembers(t *testing.T) {
	t.Parallel()
	f := newEtcdWatchFixture(t, func(c *clusterv1.Cluster) { c.Spec.Topology.ClassRef.Name = ClassPrefix + "talos" },
		&corev1.Secret{Namespace: "team-a", Name: TalosconfigSecret("web"), Data: map[string][]byte{TalosconfigKey: []byte("talosconfig of web")}})
	f.talos.down["10.0.0.12"] = true
	f.talos.wiped = map[string]bool{"10.0.0.13": true}
	f.apis.down = 100
	_, members := f.look(0)
	if !members[0].Healthy {
		t.Errorf("web-cp-0 %+v", members[0])
	}
	if members[1].Healthy || !strings.Contains(members[1].Reason, "its etcd service cannot be read") {
		t.Errorf("web-cp-1 %+v", members[1])
	}
	if members[2].Healthy || !strings.Contains(members[2].Reason, "its etcd service is "+talosEtcdPreparing+" and not healthy") {
		t.Errorf("web-cp-2 %+v", members[2])
	}
}

func set(cluster string, members ...EtcdMember) EtcdSet {
	return EtcdSet{Cluster: cluster, Members: members}
}

func member(machine, node string) EtcdMember {
	return EtcdMember{Machine: machine, Node: node, Healthy: true}
}

// stopped is a member whose VM is stopped, on pve2.
func stopped(machine string) EtcdMember {
	return EtcdMember{Machine: machine, Node: "pve2", Reason: "its VM is stopped"}
}

func TestRebootBlocker(t *testing.T) {
	t.Parallel()
	web := set("team-a/web", member("web-cp-0", "pve1"), member("web-cp-1", "pve2"), member("web-cp-2", "pve3"))
	management := set(ManagementEtcdName, member("kubehenge-cp-0", "pve1"), member("kubehenge-cp-1", "pve2"), member("kubehenge-cp-2", "pve3"))
	one := set("team-a/one", member("one-cp-0", "pve2"))
	sick := set("team-a/db", member("db-cp-0", "pve1"), stopped("db-cp-1"), member("db-cp-2", "pve3"))
	for _, tc := range []struct {
		name string
		sets []EtcdSet
		node string
		want string
	}{
		{"all healthy", []EtcdSet{management, web}, "", ""},
		{"all healthy, one node", []EtcdSet{management, web}, "pve1", ""},
		{"a member is down", []EtcdSet{management, sick, web}, "", "team-a/db: etcd member db-cp-1 on pve2 is not healthy: its VM is stopped"},
		{"a member is down, another node", []EtcdSet{management, sick}, "pve1", "team-a/db: etcd member db-cp-1 on pve2 is not healthy"},
		{"the management cluster first", []EtcdSet{set(ManagementEtcdName, stopped("kubehenge-cp-1")), sick}, "", "management: etcd member kubehenge-cp-1 on pve2"},
		{"a single control plane on the node", []EtcdSet{management, web, one}, "pve2", "team-a/one: stopping pve2 would leave 0 of 1 etcd members, and its quorum is 1 (one-cp-0)"},
		{
			"two members on the node",
			[]EtcdSet{set("team-a/dense", member("dense-cp-0", "pve1"), member("dense-cp-1", "pve1"), member("dense-cp-2", "pve2"))},
			"pve1", "team-a/dense: stopping pve1 would leave 1 of 3 etcd members, and its quorum is 2 (dense-cp-0, dense-cp-1)",
		},
		{"a node without members", []EtcdSet{management, web}, "pve4", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RebootBlocker(tc.sets, tc.node)
			if (tc.want == "") != (got == "") || !strings.HasPrefix(got, tc.want) {
				t.Errorf("blocker %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadyForReboot(t *testing.T) {
	t.Parallel()
	web := set("team-a/web", member("web-cp-0", "pve1"), member("web-cp-1", "pve2"), member("web-cp-2", "pve3"))
	management := set(ManagementEtcdName, member("kubehenge-cp-0", "pve1"), member("kubehenge-cp-1", "pve2"), member("kubehenge-cp-2", "pve3"))
	now := etcdNow
	record := EtcdHealth{Checked: now.Add(-time.Minute), Nodes: []string{"pve1", "pve2", "pve3", "pve4"}, Clusters: []EtcdSet{web}}

	r := ReadyForReboot(record, true, &management, now)
	if !r.Ready || len(r.Nodes) != 4 || !slices.ContainsFunc(r.Nodes, func(a RebootAnswer) bool { return a.Node == "pve4" && a.Ready }) {
		t.Errorf("readiness %+v", r)
	}
	if a := r.For("pve2"); !a.Ready {
		t.Errorf("pve2 %+v", a)
	}
	if a := r.For("pve9"); a.Ready || !strings.Contains(a.Blocker, "pve9") {
		t.Errorf("a node outside the cluster: %+v", a)
	}

	// A member down blocks every node, and the whole.
	sick := record
	sick.Clusters = []EtcdSet{set("team-a/web", member("web-cp-0", "pve1"), stopped("web-cp-1"), member("web-cp-2", "pve3"))}
	r = ReadyForReboot(sick, true, &management, now)
	if r.Ready || !strings.Contains(r.Blocker, "web-cp-1") || r.For("pve1").Ready || r.For("").Ready {
		t.Errorf("readiness %+v", r)
	}

	// A record the controller has not kept up blocks as well, and so does none at all.
	old := record
	old.Checked = now.Add(-EtcdHealthStale - time.Second)
	if r = ReadyForReboot(old, true, &management, now); r.Ready || !strings.Contains(r.Blocker, "has not checked") {
		t.Errorf("a stale record: %+v", r)
	}
	if r = ReadyForReboot(EtcdHealth{}, false, &management, now); r.Ready || !strings.Contains(r.Blocker, "not recorded") {
		t.Errorf("no record: %+v", r)
	}

	// The management cluster's members count: one down blocks the others' reboots.
	hurt := set(ManagementEtcdName, member("kubehenge-cp-0", "pve1"), stopped("kubehenge-cp-1"), member("kubehenge-cp-2", "pve3"))
	if r = ReadyForReboot(record, true, &hurt, now); r.Ready || !strings.HasPrefix(r.Blocker, "management: etcd member kubehenge-cp-1") {
		t.Errorf("a management member down: %+v", r)
	}
}

func TestManagementEtcd(t *testing.T) {
	t.Parallel()
	node := func(name string, ready corev1.ConditionStatus) *corev1.Node {
		return &corev1.Node{
			Name: name, Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}},
		}
	}
	apiserver := func(nodeName string, ready corev1.ConditionStatus) *corev1.Pod {
		p := etcdPod(nodeName, ready)
		p.Name, p.Labels = "kube-apiserver-"+nodeName, nil
		return p
	}
	kube := fake.NewClientBuilder().WithObjects(
		node("kubehenge-cp-0", corev1.ConditionTrue), node("kubehenge-cp-1", corev1.ConditionTrue),
		node("kubehenge-cp-2", corev1.ConditionFalse), node("kubehenge-cp-3", corev1.ConditionTrue),
		&corev1.Node{Name: "worker", Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		apiserver("kubehenge-cp-0", corev1.ConditionTrue), apiserver("kubehenge-cp-1", corev1.ConditionFalse), apiserver("kubehenge-cp-2", corev1.ConditionTrue),
	).Build()
	got, err := ManagementEtcd(context.Background(), kube, []string{"pve1", "pve2", "pve3"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Cluster != ManagementEtcdName || len(got.Members) != 4 || got.Healthy() != 1 {
		t.Fatalf("set %+v", got)
	}
	for i, want := range []struct {
		node, reason string
	}{
		{"pve1", ""},
		{"pve2", "its kube-apiserver pod is not ready, which includes etcd's health"},
		{"pve3", "its node is not ready"},
		{"pve1", "its kube-apiserver pod is missing"},
	} {
		if m := got.Members[i]; m.Node != want.node || m.Reason != want.reason {
			t.Errorf("member %d %+v, want node %s, reason %q", i, m, want.node, want.reason)
		}
	}
}
