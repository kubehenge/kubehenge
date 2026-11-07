package provisioner

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EtcdLatencyCondition is true on a Cluster while the disk under a control-plane machine's etcd is
// too slow for it.
const EtcdLatencyCondition = "EtcdDiskSlow"

// EtcdLatencySlow is the reason of EtcdLatencyCondition.
const EtcdLatencySlow = "SlowWALSync"

const (
	// EtcdFsyncLimit is the 99th percentile of the time etcd's write-ahead log takes to reach its
	// disk, above which etcd's own guide says its disk is too slow. The leader's heartbeats and
	// elections ride on it.
	EtcdFsyncLimit = 25 * time.Millisecond
	// etcdFsyncSamples is how many syncs a member must have made before its figure counts, so that
	// a member just started, whose few syncs include its first and slowest, raises no alarm.
	etcdFsyncSamples = 100
	// etcdMetricsMax bounds what is read of a member's metrics.
	etcdMetricsMax = 16 << 20
	// etcdMetricsLine and etcdMetricsLineMax are the scanner's first and largest line buffers.
	etcdMetricsLine    = 64 << 10
	etcdMetricsLineMax = 1 << 20
	// fsyncQuantileRank is the quantile that is watched.
	fsyncQuantileRank = 0.99
	// etcdMetricsTimeout bounds one member's reading.
	etcdMetricsTimeout = 20 * time.Second
	// etcdLatencySuffix ends the condition's message: why it matters and what to do.
	etcdLatencySuffix = " etcd needs local SSD or NVMe storage. Ceph and NFS add latency to every write. Move the control-plane disks (storage variable), or the machines to faster storage."
)

// walFsyncBucket is the metric of how long etcd's write-ahead log takes to be synced to disk.
const walFsyncBucket = "etcd_disk_wal_fsync_duration_seconds_bucket"

// MemberFsync is how long one etcd member's write-ahead log syncs take, over the time it has run.
type MemberFsync struct {
	// Member is the pod the figure was read through.
	Member string
	// P99 is the 99th percentile of the syncs.
	P99 time.Duration
	// Syncs is how many it made.
	Syncs uint64
}

// EtcdMetrics reads etcd's metrics for a workload cluster.
type EtcdMetrics interface {
	// WALFsync is the 99th percentile of the syncs of each member's write-ahead log.
	WALFsync(ctx context.Context, cluster *clusterv1.Cluster) ([]MemberFsync, error)
}

var _ EtcdMetrics = &EtcdDialer{}

// WALFsync reads each running member's /metrics through the same port-forward and client
// certificate a snapshot uses. A Talos member runs as a service, not a pod, so it is read at its
// node's address instead.
func (d *EtcdDialer) WALFsync(ctx context.Context, cluster *clusterv1.Cluster) ([]MemberFsync, error) {
	var (
		ca  *etcdCAPair
		err error
	)
	if isTalos(cluster) {
		ca, err = talosEtcdCA(ctx, d.Reader, cluster)
	} else {
		ca, err = etcdCA(ctx, d.Reader, cluster)
	}
	if err != nil {
		return nil, err
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	tlsConfig, err := etcdClientTLS(ca, now())
	if err != nil {
		return nil, err
	}
	if isTalos(cluster) {
		return d.talosWALFsync(ctx, cluster, tlsConfig, etcdPort)
	}
	workload, found, err := d.Remotes.get(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("the cluster has no kubeconfig yet")
	}
	pods, err := etcdPods(ctx, workload.client, distributionOf(cluster))
	if err != nil {
		d.Remotes.forget(cluster)
		return nil, err
	}
	var out []MemberFsync
	var errs []error
	for i := range pods {
		namespace, name := pods[i].Namespace, pods[i].Name
		dial := func(context.Context) (net.Conn, error) {
			return portForward(workload.config, namespace, name, etcdPort)
		}
		fsync, err := memberFsync(ctx, dial, tlsConfig)
		if err != nil {
			errs = append(errs, fmt.Errorf("member %s: %w", name, err))
			continue
		}
		fsync.Member = name
		out = append(out, fsync)
	}
	return out, errors.Join(errs...)
}

// memberFsync reads one member's metrics over dial, from the client port that serves them beside
// etcd's API.
func memberFsync(ctx context.Context, dial func(context.Context) (net.Conn, error), tlsConfig *tls.Config) (MemberFsync, error) {
	ctx, cancel := context.WithTimeout(ctx, etcdMetricsTimeout)
	defer cancel()
	transport := &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		TLSClientConfig: tlsConfig,
		// One request per connection: a port-forward is one SPDY connection per dial.
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(etcdServerName, strconv.Itoa(etcdPort))+"/metrics", http.NoBody)
	if err != nil {
		return MemberFsync{}, fmt.Errorf("asking for the metrics: %w", err)
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return MemberFsync{}, fmt.Errorf("reading the metrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return MemberFsync{}, fmt.Errorf("the metrics answered %s", resp.Status)
	}
	p99, syncs, err := fsyncQuantile(io.LimitReader(resp.Body, etcdMetricsMax))
	return MemberFsync{P99: p99, Syncs: syncs}, err
}

// fsyncQuantile is the 99th percentile of etcd's write-ahead log syncs, read from the cumulative
// buckets of its histogram as Prometheus's histogram_quantile does: linearly within the bucket the
// quantile falls in. Values past the last finite bucket give that bucket's bound.
func fsyncQuantile(metrics io.Reader) (time.Duration, uint64, error) {
	buckets, err := fsyncBuckets(metrics)
	if err != nil {
		return 0, 0, err
	}
	total := buckets[len(buckets)-1].count
	if total == 0 {
		return 0, 0, nil
	}
	rank := fsyncQuantileRank * total
	lower, below := 0.0, 0.0
	for _, b := range buckets {
		if b.count >= rank {
			if math.IsInf(b.le, 1) {
				return seconds(lower), uint64(total), nil
			}
			in := b.count - below
			if in <= 0 {
				return seconds(b.le), uint64(total), nil
			}
			return seconds(lower + (b.le-lower)*(rank-below)/in), uint64(total), nil
		}
		lower, below = b.le, b.count
	}
	return seconds(lower), uint64(total), nil
}

// fsyncBucket is one cumulative bucket of the histogram: the syncs that took at most le seconds.
type fsyncBucket struct {
	le    float64
	count float64
}

// fsyncBuckets reads the write-ahead log sync histogram from a member's metrics, sorted by bound.
func fsyncBuckets(metrics io.Reader) ([]fsyncBucket, error) {
	var buckets []fsyncBucket
	scanner := bufio.NewScanner(metrics)
	scanner.Buffer(make([]byte, 0, etcdMetricsLine), etcdMetricsLineMax)
	for scanner.Scan() {
		rest, ok := strings.CutPrefix(scanner.Text(), walFsyncBucket+"{")
		if !ok {
			continue
		}
		labels, value, ok := strings.Cut(rest, "} ")
		if !ok {
			return nil, fmt.Errorf("the metric line %q has no value", scanner.Text())
		}
		le, ok := leOf(labels)
		if !ok {
			return nil, fmt.Errorf("the metric line %q has no le label", scanner.Text())
		}
		count, err := strconv.ParseFloat(strings.Fields(value)[0], 64)
		if err != nil {
			return nil, fmt.Errorf("the metric line %q: %w", scanner.Text(), err)
		}
		buckets = append(buckets, fsyncBucket{le, count})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading the metrics: %w", err)
	}
	if len(buckets) == 0 {
		return nil, errors.New("the member reports no write-ahead log sync times")
	}
	slices.SortFunc(buckets, func(a, b fsyncBucket) int { return cmp.Compare(a.le, b.le) })
	return buckets, nil
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// leOf reads the le label of a histogram bucket.
func leOf(labels string) (float64, bool) {
	for label := range strings.SplitSeq(labels, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(label), `le="`); ok {
			bound, err := strconv.ParseFloat(strings.TrimSuffix(v, `"`), 64)
			return bound, err == nil
		}
	}
	return 0, false
}

// EtcdLatencyWatch reads how long each control plane's etcd members take to sync their
// write-ahead log, and says on the Cluster when that is above EtcdFsyncLimit: EtcdLatencyCondition,
// with an event when it starts and ends.
type EtcdLatencyWatch struct {
	Client client.Client
	// Reader reads a cluster past the cache, after a write to it conflicted.
	Reader         client.Reader
	ClassNamespace string
	Etcd           EtcdMetrics
	Events         events.EventRecorder
	Interval       time.Duration
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*EtcdLatencyWatch) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (w *EtcdLatencyWatch) Start(ctx context.Context) error {
	return every(ctx, w.Interval, func(ctx context.Context) {
		eachCluster(ctx, w.Client, w.ClassNamespace, "etcd-latency-watch", w.watch)
	})
}

// watch reads one cluster's members. A cluster whose etcd cannot be read keeps the condition it has.
func (w *EtcdLatencyWatch) watch(ctx context.Context, c *clusterv1.Cluster) error {
	if !c.DeletionTimestamp.IsZero() || RestoreInProgress(c) ||
		!meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
		return nil
	}
	members, err := w.Etcd.WALFsync(ctx, c)
	if len(members) == 0 {
		if err != nil {
			return fmt.Errorf("reading the etcd members of %s: %w", c.Name, err)
		}
		return nil
	}
	return errors.Join(err, w.mark(ctx, c, EtcdLatencyConditionOf(members)))
}

// EtcdLatencyConditionOf is the condition for the members read, nil when none is too slow.
func EtcdLatencyConditionOf(members []MemberFsync) *metav1.Condition {
	var slow []string
	for _, m := range members {
		if m.Syncs >= etcdFsyncSamples && m.P99 > EtcdFsyncLimit {
			slow = append(slow, fmt.Sprintf("%s %d ms", m.Member, m.P99.Milliseconds()))
		}
	}
	if len(slow) == 0 {
		return nil
	}
	return &metav1.Condition{
		Status: metav1.ConditionTrue, Reason: EtcdLatencySlow,
		Message: fmt.Sprintf("etcd write-ahead log syncs take longer than %d ms at the 99th percentile on %s.", EtcdFsyncLimit.Milliseconds(), strings.Join(slow, ", ")) + etcdLatencySuffix,
	}
}

// EtcdLatencySummary is the condition's message without the advice kubectl readers get.
func EtcdLatencySummary(message string) string {
	summary, _, _ := strings.Cut(message, etcdLatencySuffix)
	return summary
}

func (w *EtcdLatencyWatch) mark(ctx context.Context, c *clusterv1.Cluster, cond *metav1.Condition) error {
	if cond == nil && meta.FindStatusCondition(c.Status.Conditions, EtcdLatencyCondition) == nil {
		return nil
	}
	before, err := markCluster(ctx, w.Client, w.Reader, client.ObjectKeyFromObject(c), EtcdLatencyCondition, cond)
	if err != nil || w.Events == nil {
		return err
	}
	switch {
	case newlyMarked(before, cond):
		w.Events.Eventf(c, nil, corev1.EventTypeWarning, EtcdLatencyCondition, "WatchEtcdLatency", "%s", shorten(cond.Message, watchEventMax))
	case before != nil && cond == nil:
		w.Events.Eventf(c, nil, corev1.EventTypeNormal, "EtcdDiskFast", "WatchEtcdLatency", "etcd write-ahead log syncs are within %d ms", EtcdFsyncLimit.Milliseconds())
	}
	return nil
}
