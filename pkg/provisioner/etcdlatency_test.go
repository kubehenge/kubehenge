package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// etcdBounds are the buckets etcd's histogram of write-ahead log syncs has: 1 ms, doubling.
var etcdBounds = []string{"0.001", "0.002", "0.004", "0.008", "0.016", "0.032", "0.064", "0.128", "+Inf"}

// fsyncMetrics is an etcd member's /metrics with these counts in the buckets (not cumulative), and
// other metrics around them.
func fsyncMetrics(counts ...uint64) string {
	var b strings.Builder
	b.WriteString("# HELP etcd_disk_wal_fsync_duration_seconds The latency distributions of fsync called by WAL.\n")
	b.WriteString("# TYPE etcd_disk_wal_fsync_duration_seconds histogram\n")
	b.WriteString("etcd_disk_backend_commit_duration_seconds_bucket{le=\"0.001\"} 7\n")
	var total uint64
	for i, bound := range etcdBounds {
		if i < len(counts) {
			total += counts[i]
		}
		fmt.Fprintf(&b, "etcd_disk_wal_fsync_duration_seconds_bucket{le=%q} %d\n", bound, total)
	}
	fmt.Fprintf(&b, "etcd_disk_wal_fsync_duration_seconds_sum 1.5\netcd_disk_wal_fsync_duration_seconds_count %d\n", total)
	return b.String()
}

func TestFsyncQuantile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		counts []uint64
		low    time.Duration
		high   time.Duration
		syncs  uint64
	}{
		{"a fast disk", []uint64{0, 900, 90, 10}, time.Millisecond, 8 * time.Millisecond, 1000},
		{"just under the limit", []uint64{0, 0, 0, 0, 990, 10}, 16 * time.Millisecond, 25 * time.Millisecond, 1000},
		{"just over the limit", []uint64{0, 0, 0, 0, 900, 100}, 25*time.Millisecond + 1, 32 * time.Millisecond, 1000},
		{"a slow disk", []uint64{0, 0, 0, 0, 0, 400, 500, 100}, 32 * time.Millisecond, 128 * time.Millisecond, 1000},
		{"past the last bound", []uint64{0, 0, 0, 0, 0, 0, 0, 0, 1000}, 128 * time.Millisecond, 128 * time.Millisecond, 1000},
		{"no syncs yet", nil, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p99, syncs, err := fsyncQuantile(strings.NewReader(fsyncMetrics(tc.counts...)))
			if err != nil || syncs != tc.syncs || p99 < tc.low || p99 > tc.high {
				t.Errorf("p99 %s over %d syncs, %v; want %s to %s over %d", p99, syncs, err, tc.low, tc.high, tc.syncs)
			}
		})
	}
	for name, text := range map[string]string{
		"no histogram":   "etcd_server_has_leader 1\n",
		"a broken line":  `etcd_disk_wal_fsync_duration_seconds_bucket{le="0.001"}` + "\n",
		"a bad number":   `etcd_disk_wal_fsync_duration_seconds_bucket{le="0.001"} many` + "\n",
		"no le label":    `etcd_disk_wal_fsync_duration_seconds_bucket{foo="x"} 3` + "\n",
		"a bad le label": `etcd_disk_wal_fsync_duration_seconds_bucket{le="soon"} 3` + "\n",
	} {
		if _, _, err := fsyncQuantile(strings.NewReader(text)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestEtcdLatencyConditionThreshold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		members []MemberFsync
		slow    bool
	}{
		{"under the limit", []MemberFsync{{Member: "etcd-a", P99: 24 * time.Millisecond, Syncs: 5000}}, false},
		{"exactly at the limit", []MemberFsync{{Member: "etcd-a", P99: EtcdFsyncLimit, Syncs: 5000}}, false},
		{"over the limit", []MemberFsync{{Member: "etcd-a", P99: 26 * time.Millisecond, Syncs: 5000}}, true},
		{"one member of three", []MemberFsync{{Member: "etcd-a", P99: 3 * time.Millisecond, Syncs: 5000}, {Member: "etcd-b", P99: 90 * time.Millisecond, Syncs: 5000}, {Member: "etcd-c", Syncs: 5000}}, true},
		{"too few syncs to tell", []MemberFsync{{Member: "etcd-a", P99: 400 * time.Millisecond, Syncs: 12}}, false},
	} {
		cond := EtcdLatencyConditionOf(tc.members)
		if (cond != nil) != tc.slow {
			t.Errorf("%s: condition %+v", tc.name, cond)
		}
	}
	cond := EtcdLatencyConditionOf([]MemberFsync{{Member: "etcd-b", P99: 90 * time.Millisecond, Syncs: 5000}})
	if cond == nil || !strings.Contains(cond.Message, "etcd-b 90 ms") || !strings.Contains(cond.Message, "Ceph and NFS") {
		t.Fatalf("message %+v", cond)
	}
	if summary := EtcdLatencySummary(cond.Message); strings.Contains(summary, "Ceph") || !strings.HasSuffix(summary, "etcd-b 90 ms.") {
		t.Errorf("summary %q", summary)
	}
}

type fakeEtcdMetrics struct {
	members []MemberFsync
	err     error
	asked   int
}

func (f *fakeEtcdMetrics) WALFsync(context.Context, *clusterv1.Cluster) ([]MemberFsync, error) {
	f.asked++
	return f.members, f.err
}

func TestEtcdLatencyWatch(t *testing.T) {
	t.Parallel()
	c := etcdCluster(`{}`)
	c.Status.Conditions = []metav1.Condition{{
		Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue, Reason: "Initialised", LastTransitionTime: metav1.Now(),
	}}
	f := newEtcdFixture(t, c)
	etcd := &fakeEtcdMetrics{members: []MemberFsync{{Member: "etcd-a", P99: 60 * time.Millisecond, Syncs: 9000}}}
	recorder := events.NewFakeRecorder(10)
	w := &EtcdLatencyWatch{Client: f.kube, Reader: f.kube, ClassNamespace: "kubehenge-system", Etcd: etcd, Events: recorder}
	look := func() *metav1.Condition {
		t.Helper()
		if err := w.watch(t.Context(), f.cluster()); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(f.cluster().Status.Conditions, EtcdLatencyCondition)
	}
	if cond := look(); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != EtcdLatencySlow {
		t.Fatalf("condition %+v for a slow disk", cond)
	}
	if e := <-recorder.Events; !strings.Contains(e, "Warning "+EtcdLatencyCondition) {
		t.Errorf("event %q", e)
	}
	// A look that fails leaves what is said.
	etcd.members, etcd.err = nil, errors.New("no etcd member of the cluster is running and ready")
	if err := w.watch(t.Context(), f.cluster()); err == nil {
		t.Error("a failed reading was not reported")
	}
	if meta.FindStatusCondition(f.cluster().Status.Conditions, EtcdLatencyCondition) == nil {
		t.Error("a failed reading cleared the condition")
	}
	// Once the disk is fast, it goes.
	etcd.members, etcd.err = []MemberFsync{{Member: "etcd-a", P99: 4 * time.Millisecond, Syncs: 9500}}, nil
	if cond := look(); cond != nil {
		t.Errorf("condition %+v kept for a fast disk", cond)
	}
	if e := <-recorder.Events; !strings.Contains(e, "EtcdDiskFast") {
		t.Errorf("event %q", e)
	}
}

func TestEtcdLatencyWatchLeavesOtherClustersAlone(t *testing.T) {
	t.Parallel()
	ready := metav1.Condition{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue, Reason: "Initialised", LastTransitionTime: metav1.Now()}
	etcd := &fakeEtcdMetrics{members: []MemberFsync{{Member: "etcd-a", P99: time.Second, Syncs: 9000}}}
	w := func(f *etcdFixture) *EtcdLatencyWatch {
		return &EtcdLatencyWatch{Client: f.kube, Reader: f.kube, ClassNamespace: "kubehenge-system", Etcd: etcd}
	}
	notUp := etcdCluster(`{}`)
	notUp.Status.Conditions = nil
	if err := w(newEtcdFixture(t, notUp)).watch(t.Context(), notUp); err != nil || etcd.asked != 0 {
		t.Errorf("a cluster whose control plane is not up was read (%d reads, %v)", etcd.asked, err)
	}
	talos := talosCluster("web", "v1.14.4")
	talos.Namespace = "team-a"
	talos.Status.Conditions = []metav1.Condition{ready}
	if err := w(newEtcdFixture(t, talos)).watch(t.Context(), talos); err != nil || etcd.asked != 0 {
		t.Errorf("a Talos cluster was read (%d reads, %v)", etcd.asked, err)
	}
}
