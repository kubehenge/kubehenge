package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const gib = int64(1) << 30

type fakeMaintenance struct {
	mu      sync.Mutex
	members []EtcdMember
	readErr error
	// order are the members defragmented, in order. A defragmented member holds only its data.
	order    []string
	failOn   string
	disarmed int
}

func (f *fakeMaintenance) Members(context.Context, *clusterv1.Cluster) ([]EtcdMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.members), f.readErr
}

func (f *fakeMaintenance) Defragment(_ context.Context, _ *clusterv1.Cluster, member string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if member == f.failOn {
		return errors.New("deadline exceeded")
	}
	f.order = append(f.order, member)
	for i := range f.members {
		if f.members[i].Name == member {
			f.members[i].DBSize = f.members[i].DBSizeInUse
		}
	}
	return nil
}

func (f *fakeMaintenance) Disarm(context.Context, *clusterv1.Cluster) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disarmed++
	return nil
}

type maintenanceRig struct {
	m    *EtcdMaintainer
	etcd *fakeMaintenance
	kube client.Client
	rec  *events.FakeRecorder
	now  time.Time
}

// newMaintenanceRig is a maintainer over cluster web, settled with its control plane initialised.
func newMaintenanceRig(t *testing.T, variables map[string]string, members ...EtcdMember) *maintenanceRig {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clusterv1.AddToScheme, clientgoscheme.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := settledCluster("web", "v1.36.5+rke2r1", variables)
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue})
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).Build()
	r := &maintenanceRig{etcd: &fakeMaintenance{members: members}, kube: kube, rec: events.NewFakeRecorder(50), now: day(10).Add(3 * time.Hour)}
	r.m = &EtcdMaintainer{
		Client: kube, Reader: kube, ClassNamespace: "kubehenge-system", Etcd: r.etcd, Events: r.rec,
		Now: func() time.Time { return r.now },
	}
	return r
}

func (r *maintenanceRig) reconcile(t *testing.T) (*clusterv1.Cluster, ctrl.Result) {
	t.Helper()
	var req ctrl.Request
	req.Namespace, req.Name = "clusters", "web"
	res, err := r.m.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	c := &clusterv1.Cluster{}
	if err := r.kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	return c, res
}

func (r *maintenanceRig) events() []string {
	var out []string
	for {
		select {
		case e := <-r.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// fragmented is a member with a large database that holds little data.
func fragmented(name string, leader bool) EtcdMember {
	return EtcdMember{Name: name, Leader: leader, DBSize: 2 * gib, DBSizeInUse: gib / 4}
}

func askForDefrag(t *testing.T, r *maintenanceRig) {
	t.Helper()
	c := &clusterv1.Cluster{}
	if err := r.kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(EtcdDefragRequest{By: "dev@pve", At: r.now})
	if err != nil {
		t.Fatal(err)
	}
	c.Annotations = map[string]string{EtcdDefragRequestAnnotation: string(raw)}
	if err := r.kube.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func TestEtcdSettings(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		variables map[string]string
		want      int64
	}{
		{nil, 8 * gib},
		{map[string]string{"etcd": `{}`}, 8 * gib},
		{map[string]string{"etcd": `{"quotaGiB":4}`}, 4 * gib},
	} {
		s, err := EtcdSettingsOf(settledCluster("web", "v1.36.5+rke2r1", c.variables))
		if err != nil || s.QuotaBytes() != c.want {
			t.Errorf("%v: quota %d, %v", c.variables, s.QuotaBytes(), err)
		}
	}
}

func TestEtcdStatusWarnsAbove60PercentAndOnAnAlarm(t *testing.T) {
	t.Parallel()
	quota := 8 * gib
	for _, c := range []struct {
		name    string
		members []EtcdMember
		want    string
	}{
		{"empty", []EtcdMember{{Name: "a", DBSize: 100 << 20}}, ""},
		{"at 60 percent", []EtcdMember{{Name: "a", DBSize: quota * 60 / 100}}, ""},
		{"above 60 percent", []EtcdMember{{Name: "a", DBSize: quota*60/100 + 1}}, "etcd member a is at 60% of its quota"},
		{"the alarm, whatever the size", []EtcdMember{{Name: "a", DBSize: gib, Alarms: []string{"NOSPACE"}}}, "etcd member a refuses writes, its database is full"},
		{"two members", []EtcdMember{{Name: "a", DBSize: quota / 4 * 3}, {Name: "b", DBSize: quota / 8 * 7}}, "etcd member a is at 75% of its quota, etcd member b is at 87% of its quota"},
	} {
		s := EtcdStatus{QuotaBytes: quota, Members: c.members}
		if got := s.Warning(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDefragPlanVisitsFollowersBeforeTheLeader(t *testing.T) {
	t.Parallel()
	members := []EtcdMember{fragmented("cp-0", true), fragmented("cp-2", false), fragmented("cp-1", false)}
	if got := defragPlan(members, defragInWindow); !slices.Equal(got, []string{"cp-1", "cp-2", "cp-0"}) {
		t.Errorf("plan %v", got)
	}
}

func TestDefragPlanLeavesSmallAndTidyDatabasesAlone(t *testing.T) {
	t.Parallel()
	members := []EtcdMember{
		{Name: "small", DBSize: 50 << 20, DBSizeInUse: 1 << 20},
		{Name: "tidy", DBSize: 2 * gib, DBSizeInUse: gib + gib/4},
		{Name: "half", DBSize: 2 * gib, DBSizeInUse: gib},
		fragmented("loose", false),
	}
	if got := defragPlan(members, defragInWindow); !slices.Equal(got, []string{"loose"}) {
		t.Errorf("plan %v", got)
	}
}

func TestDefragPlanWaitsForTheWindowUnlessAskedOrFull(t *testing.T) {
	t.Parallel()
	members := []EtcdMember{fragmented("cp-0", false), {Name: "cp-1", DBSize: 10 << 20, DBSizeInUse: 10 << 20}}
	if got := defragPlan(members, defragHeld); got != nil {
		t.Errorf("outside the window: %v", got)
	}
	if got := defragPlan(members, defragAsked); !slices.Equal(got, []string{"cp-0", "cp-1"}) {
		t.Errorf("asked: %v", got)
	}
	full := slices.Clone(members)
	full[1].Alarms = []string{"NOSPACE"}
	if got := defragPlan(full, defragHeld); !slices.Equal(got, []string{"cp-0", "cp-1"}) {
		t.Errorf("full: %v", got)
	}
}

func TestMaintainerDefragmentsInsideTheWindowOneAtATime(t *testing.T) {
	t.Parallel()
	r := newMaintenanceRig(t, nil, fragmented("cp-0", true), fragmented("cp-1", false), fragmented("cp-2", false))
	c, res := r.reconcile(t)
	if !slices.Equal(r.etcd.order, []string{"cp-1", "cp-2", "cp-0"}) {
		t.Fatalf("defragmented %v", r.etcd.order)
	}
	var done EtcdDefrag
	if err := json.Unmarshal([]byte(c.Annotations[EtcdDefragAnnotation]), &done); err != nil || !slices.Equal(done.Members, r.etcd.order) {
		t.Fatalf("recorded %q: %v", c.Annotations[EtcdDefragAnnotation], err)
	}
	status, ok := LastEtcdStatus(c)
	if !ok || status.QuotaBytes != 8*gib || len(status.Members) != 3 || status.Members[0].DBSize != gib/4 {
		t.Fatalf("status %+v", status)
	}
	if res.RequeueAfter != etcdReadInterval {
		t.Errorf("requeue %v", res.RequeueAfter)
	}
	var defragged int
	for _, e := range r.events() {
		if strings.Contains(e, "EtcdDefragmented") {
			defragged++
		}
	}
	if defragged != 3 {
		t.Errorf("%d events, one per member wanted", defragged)
	}
}

func TestMaintainerLeavesFragmentationAloneOutsideTheWindow(t *testing.T) {
	t.Parallel()
	r := newMaintenanceRig(t, nil, fragmented("cp-0", true), fragmented("cp-1", false))
	r.now = day(10).Add(12 * time.Hour)
	c, _ := r.reconcile(t)
	if len(r.etcd.order) != 0 {
		t.Fatalf("defragmented %v outside the window", r.etcd.order)
	}
	if _, ok := LastEtcdStatus(c); !ok {
		t.Error("the sizes are read whatever the hour")
	}
}

func TestMaintainerUsesTheUpgradeWindowOfTheCluster(t *testing.T) {
	t.Parallel()
	r := newMaintenanceRig(t, map[string]string{"upgrades": `{"mode":"off","window":{"start":"12:00","durationMinutes":60}}`}, fragmented("cp-0", true))
	r.now = day(10).Add(12*time.Hour + 5*time.Minute)
	r.reconcile(t)
	if !slices.Equal(r.etcd.order, []string{"cp-0"}) {
		t.Fatalf("defragmented %v in the cluster's own window", r.etcd.order)
	}
}

func TestMaintainerDefragmentsAtOnceWhenAsked(t *testing.T) {
	t.Parallel()
	r := newMaintenanceRig(t, nil, EtcdMember{Name: "cp-0", Leader: true, DBSize: 10 << 20, DBSizeInUse: 10 << 20}, EtcdMember{Name: "cp-1", DBSize: 10 << 20, DBSizeInUse: 10 << 20})
	r.now = day(10).Add(12 * time.Hour)
	askForDefrag(t, r)
	c, _ := r.reconcile(t)
	if !slices.Equal(r.etcd.order, []string{"cp-1", "cp-0"}) {
		t.Fatalf("defragmented %v", r.etcd.order)
	}
	if _, left := c.Annotations[EtcdDefragRequestAnnotation]; left {
		t.Error("the request is still there")
	}
}

func TestMaintainerStartsNothingWhileAMemberCannotBeRead(t *testing.T) {
	t.Parallel()
	r := newMaintenanceRig(t, nil, fragmented("cp-0", true), fragmented("cp-1", false))
	r.etcd.readErr = errors.New("member cp-2: connection refused")
	c, _ := r.reconcile(t)
	if len(r.etcd.order) != 0 {
		t.Fatalf("defragmented %v", r.etcd.order)
	}
	if status, _ := LastEtcdStatus(c); !strings.Contains(status.Error, "cp-2") {
		t.Errorf("status %+v", status)
	}
	askForDefrag(t, r)
	c, _ = r.reconcile(t)
	if len(r.etcd.order) != 0 || !strings.Contains(c.Annotations[EtcdDefragAnnotation], "could be read") {
		t.Fatalf("defragmented %v, recorded %q", r.etcd.order, c.Annotations[EtcdDefragAnnotation])
	}
}

func TestMaintainerStopsAtAFailedMember(t *testing.T) {
	t.Parallel()
	r := newMaintenanceRig(t, nil, fragmented("cp-0", true), fragmented("cp-1", false), fragmented("cp-2", false))
	r.etcd.failOn = "cp-2"
	c, _ := r.reconcile(t)
	if !slices.Equal(r.etcd.order, []string{"cp-1"}) {
		t.Fatalf("defragmented %v", r.etcd.order)
	}
	if !strings.Contains(c.Annotations[EtcdDefragAnnotation], "cp-2") {
		t.Errorf("recorded %q", c.Annotations[EtcdDefragAnnotation])
	}
}

func TestMaintainerClearsTheAlarmOnceSpaceIsFree(t *testing.T) {
	t.Parallel()
	full := EtcdMember{Name: "cp-0", Leader: true, DBSize: 8 * gib, DBSizeInUse: gib, Alarms: []string{"NOSPACE"}}
	r := newMaintenanceRig(t, nil, full)
	r.now = day(10).Add(12 * time.Hour)
	c, _ := r.reconcile(t)
	if !slices.Equal(r.etcd.order, []string{"cp-0"}) || r.etcd.disarmed != 1 {
		t.Fatalf("defragmented %v, disarmed %d", r.etcd.order, r.etcd.disarmed)
	}
	if status, _ := LastEtcdStatus(c); status.Warning() != "" {
		t.Errorf("warns %q", status.Warning())
	}
}

func TestMaintainerWarnsWhenADatabaseGrows(t *testing.T) {
	t.Parallel()
	r := newMaintenanceRig(t, map[string]string{"etcd": `{"quotaGiB":2}`}, EtcdMember{Name: "cp-0", Leader: true, DBSize: 3 * gib / 2, DBSizeInUse: 3 * gib / 2})
	r.reconcile(t)
	if ev := r.events(); len(ev) != 1 || !strings.Contains(ev[0], "EtcdDatabaseLarge") || !strings.Contains(ev[0], "75%") {
		t.Fatalf("events %v", ev)
	}
	// The same warning is not repeated at the next read.
	r.now = r.now.Add(etcdReadInterval + time.Minute)
	r.reconcile(t)
	if ev := r.events(); len(ev) != 0 {
		t.Fatalf("events %v", ev)
	}
}

func TestMaintainerReadsOnlyAtItsInterval(t *testing.T) {
	t.Parallel()
	r := newMaintenanceRig(t, nil, EtcdMember{Name: "cp-0", Leader: true, DBSize: 10 << 20, DBSizeInUse: 10 << 20})
	first, _ := r.reconcile(t)
	r.etcd.mu.Lock()
	r.etcd.members[0].DBSize = 20 << 20
	r.etcd.mu.Unlock()
	r.now = r.now.Add(time.Minute)
	c, res := r.reconcile(t)
	if c.Annotations[EtcdStatusAnnotation] != first.Annotations[EtcdStatusAnnotation] || res.RequeueAfter <= 0 || res.RequeueAfter >= etcdReadInterval {
		t.Fatalf("read again after a minute: %v, requeue %v", c.Annotations, res.RequeueAfter)
	}
}
