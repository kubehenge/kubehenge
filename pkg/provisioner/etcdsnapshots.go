package provisioner

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	etcdSnapshotsVariable = "etcdSnapshots"
	// EtcdSnapshotAnnotation on a Cluster records its last etcd snapshot, as an EtcdSnapshot in JSON.
	EtcdSnapshotAnnotation = "kubehenge.dev/etcd-snapshot"
	// EtcdSnapshotRequestAnnotation asks for a snapshot at once, as an EtcdSnapshotRequest in JSON;
	// the controller takes it away once it has tried.
	EtcdSnapshotRequestAnnotation = "kubehenge.dev/etcd-snapshot-requested"
	// EtcdSnapshotCondition on a Cluster says whether its last etcd snapshot was taken.
	EtcdSnapshotCondition = "EtcdSnapshotSucceeded"
	// EtcdSnapshotFolder is the folder of a cluster's own in its target that holds its snapshots.
	EtcdSnapshotFolder = "etcd"

	// EtcdSnapshotSuffix ends the name of every snapshot: an etcd database encrypted with age.
	EtcdSnapshotSuffix     = ".db.age"
	etcdSnapshotTimeLayout = "20060102T150405Z"

	// The defaults of the etcdSnapshots variable, as the chart's schema has them: twice a day, as
	// RKE2 and k3s take their own, keeping a week.
	defaultEtcdSnapshotSchedule = "0 */12 * * *"
	defaultEtcdSnapshotKeep     = 14
	maxEtcdSnapshotKeep         = 1000

	// etcdSnapshotJitter spreads the snapshots of clusters on the same schedule over the minutes
	// after it, so that they do not all stream through the management cluster at once.
	etcdSnapshotJitter = 10 * time.Minute
	// etcdSnapshotRetry is how soon a snapshot that failed is tried again.
	etcdSnapshotRetry = 15 * time.Minute
	// etcdSnapshotTimeout bounds one snapshot: etcd's largest database at a few MB/s.
	etcdSnapshotTimeout = 20 * time.Minute
	// etcdSnapshotWait is how soon a cluster that cannot be snapshotted yet is looked at again.
	etcdSnapshotWait = time.Minute
	// etcdTrailerSize is the SHA-256 etcd appends to every snapshot it streams.
	etcdTrailerSize = sha256.Size
)

// The metadata each snapshot's object carries.
const (
	SnapshotMetaKubernetes   = "kubernetes-version"
	SnapshotMetaDistribution = "distribution"
	SnapshotMetaRevision     = "etcd-revision"
	SnapshotMetaEtcd         = "etcd-version"
	SnapshotMetaSHA256       = "sha256"
	SnapshotMetaSize         = "size"
)

// EtcdSnapshots is a cluster's etcdSnapshots variable.
type EtcdSnapshots struct {
	Enabled  bool   `json:"enabled"`
	Target   string `json:"target,omitempty"`
	Schedule string `json:"schedule,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
	Keep     int    `json:"keep,omitempty"`
	// DistributionUpload has RKE2's or k3s's servers also upload snapshots of their own to the
	// target, on the same schedule read in UTC and keeping as many (etcdupload.go).
	DistributionUpload bool `json:"distributionUpload,omitempty"`
}

// withDefaults fills in what the cluster left to the chart's schema.
func (s *EtcdSnapshots) withDefaults() {
	s.Schedule = cmp.Or(s.Schedule, defaultEtcdSnapshotSchedule)
	s.TimeZone = cmp.Or(s.TimeZone, defaultBackupTimeZone)
	s.Keep = cmp.Or(s.Keep, defaultEtcdSnapshotKeep)
}

func (s *EtcdSnapshots) valid() error {
	if _, err := parseSchedule(etcdSnapshotsVariable, s.Schedule, s.TimeZone); err != nil {
		return err
	}
	if s.Keep < 1 || s.Keep > maxEtcdSnapshotKeep {
		return fmt.Errorf("etcdSnapshots: keep 1 to %d, not %d", maxEtcdSnapshotKeep, s.Keep)
	}
	return nil
}

// EtcdSnapshotsOf reads a cluster's etcdSnapshots variable with the chart's defaults; unset, they
// are off.
func EtcdSnapshotsOf(c *clusterv1.Cluster) (EtcdSnapshots, error) {
	s, _, err := optionalVariable[EtcdSnapshots](c, etcdSnapshotsVariable)
	s.withDefaults()
	return s, err
}

// checkEtcdSnapshots holds a cluster's etcdSnapshots variable to a target of the class and a
// schedule cron reads, as checkBackups holds its backups.
func checkEtcdSnapshots(old, c *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	s, err := EtcdSnapshotsOf(c)
	if err != nil {
		return err
	}
	if err := checkEtcdUpload(c, &s); err != nil || !s.Enabled {
		return err
	}
	if s.Target == "" {
		return errors.New("etcdSnapshots: choose a target")
	}
	if err := s.valid(); err != nil {
		return err
	}
	if old != nil {
		if was, werr := EtcdSnapshotsOf(old); werr == nil && was.Enabled && was.Target == s.Target {
			return nil
		}
	}
	return targetOffered(class, s.Target, "etcd snapshots")
}

// EtcdSnapshotPrefix is the folder of a cluster's etcd snapshots in its target, beside its Velero
// backups.
func EtcdSnapshotPrefix(prefix, namespace, cluster string) string {
	return targetFolder(prefix, namespace, cluster, EtcdSnapshotFolder)
}

// EtcdSnapshot is one snapshot of a cluster's etcd in its target.
type EtcdSnapshot struct {
	// Name is the object's name in the cluster's snapshot folder.
	Name              string    `json:"name"`
	Time              time.Time `json:"time"`
	KubernetesVersion string    `json:"kubernetesVersion,omitempty"`
	Distribution      string    `json:"distribution,omitempty"`
	Revision          int64     `json:"revision,omitempty"`
	EtcdVersion       string    `json:"etcdVersion,omitempty"`
	// SHA256 and Size are of the database as etcd sent it, before encryption.
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// StoredSize is the encrypted object's.
	StoredSize int64 `json:"storedSize,omitempty"`
}

// EtcdSnapshotRequest asks for a snapshot at once.
type EtcdSnapshotRequest struct {
	By string    `json:"by,omitempty"`
	At time.Time `json:"at"`
}

var unsafeInName = regexp.MustCompile(`[^A-Za-z0-9.-]`)

// etcdSnapshotName names a snapshot by when it was taken, in UTC, and the Kubernetes version it
// holds the objects of, so that names sort by time.
func etcdSnapshotName(at time.Time, version string) string {
	return at.UTC().Format(etcdSnapshotTimeLayout) + "-" + unsafeInName.ReplaceAllString(version, "-") + EtcdSnapshotSuffix
}

// snapshotOf reads a snapshot from its object in folder, or false for an object that is none.
func snapshotOf(o StoredObject, folder string) (EtcdSnapshot, bool) {
	name, inside := strings.CutPrefix(o.Key, folder+"/")
	if !inside || strings.Contains(name, "/") || !strings.HasSuffix(name, EtcdSnapshotSuffix) || len(name) < len(etcdSnapshotTimeLayout) {
		return EtcdSnapshot{}, false
	}
	at, err := time.Parse(etcdSnapshotTimeLayout, name[:len(etcdSnapshotTimeLayout)])
	if err != nil {
		return EtcdSnapshot{}, false
	}
	m := o.Metadata
	s := EtcdSnapshot{
		Name: name, Time: at, StoredSize: o.Size,
		KubernetesVersion: m[SnapshotMetaKubernetes], Distribution: m[SnapshotMetaDistribution],
		EtcdVersion: m[SnapshotMetaEtcd], SHA256: m[SnapshotMetaSHA256],
	}
	s.Revision, _ = strconv.ParseInt(m[SnapshotMetaRevision], 10, 64)
	s.Size, _ = strconv.ParseInt(m[SnapshotMetaSize], 10, 64)
	return s, true
}

// ValidSnapshotName tells whether name is one a snapshot could have, so that nothing else of a
// cluster's folder is ever read or restored as one.
func ValidSnapshotName(name string) bool {
	_, ok := snapshotOf(StoredObject{Key: "f/" + name}, "f")
	return ok
}

// ListEtcdSnapshots lists a cluster's snapshots in its folder, newest first.
func ListEtcdSnapshots(ctx context.Context, store ObjectStore, folder string) ([]EtcdSnapshot, error) {
	objects, err := store.List(ctx, folder+"/")
	if err != nil {
		return nil, fmt.Errorf("listing the snapshots: %w", err)
	}
	var out []EtcdSnapshot
	for _, o := range objects {
		if s, ok := snapshotOf(o, folder); ok {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b EtcdSnapshot) int { return strings.Compare(b.Name, a.Name) })
	return out, nil
}

// pruneEtcdSnapshots deletes all but the newest keep snapshots and tells how many went.
func pruneEtcdSnapshots(ctx context.Context, store ObjectStore, folder string, keep int) (int, error) {
	snapshots, err := ListEtcdSnapshots(ctx, store, folder)
	if err != nil || len(snapshots) <= keep {
		return 0, err
	}
	var errs []error
	removed := 0
	for i := keep; i < len(snapshots); i++ {
		if err := store.Remove(ctx, folder+"/"+snapshots[i].Name); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// trailerCheck holds back the last bytes written, which end every snapshot etcd streams as the
// SHA-256 of what came before, and checks them once the stream has ended, so that a stream cut off
// is never stored as a snapshot.
type trailerCheck struct {
	hash    hash.Hash
	tail    [etcdTrailerSize]byte
	tailLen int
}

func newTrailerCheck() *trailerCheck { return &trailerCheck{hash: sha256.New()} }

func (t *trailerCheck) Write(p []byte) (int, error) {
	if len(p) >= etcdTrailerSize {
		t.hash.Write(t.tail[:t.tailLen])
		t.hash.Write(p[:len(p)-etcdTrailerSize])
		t.tailLen = copy(t.tail[:], p[len(p)-etcdTrailerSize:])
		return len(p), nil
	}
	var both [2 * etcdTrailerSize]byte
	n := copy(both[:], t.tail[:t.tailLen])
	n += copy(both[n:], p)
	if over := n - etcdTrailerSize; over > 0 {
		t.hash.Write(both[:over])
		t.tailLen = copy(t.tail[:], both[over:n])
	} else {
		t.tailLen = copy(t.tail[:], both[:n])
	}
	return len(p), nil
}

func (t *trailerCheck) check() error {
	if t.tailLen < etcdTrailerSize || !bytes.Equal(t.hash.Sum(nil), t.tail[:]) {
		return errors.New("etcd's checksum at the end of the snapshot does not match it: the stream was cut off")
	}
	return nil
}

// sealed streams a snapshot encrypted to an age recipient, hashing it as it goes. It reads from the
// snapshot only as fast as its reader takes the encrypted bytes, through buffers of fixed size, so
// a database of any size streams in the same memory.
type sealed struct {
	r       *io.PipeReader
	plain   hash.Hash
	trailer *trailerCheck
	size    int64
	// err is how the stream ended, once done is closed.
	err  error
	done chan struct{}
}

func seal(snapshot io.Reader, to age.Recipient) *sealed {
	pr, pw := io.Pipe()
	s := &sealed{r: pr, plain: sha256.New(), trailer: newTrailerCheck(), done: make(chan struct{})}
	go func() {
		s.err = s.encrypt(snapshot, pw, to)
		_ = pw.CloseWithError(s.err)
		close(s.done)
	}()
	return s
}

func (s *sealed) Read(p []byte) (int, error) {
	return s.r.Read(p) //nolint:wrapcheck // a reader hands on its source's errors, io.EOF among them, as they are
}

func (s *sealed) encrypt(snapshot io.Reader, out io.Writer, to age.Recipient) error {
	w, err := age.Encrypt(out, to)
	if err != nil {
		return fmt.Errorf("encrypting the snapshot: %w", err)
	}
	n, err := io.Copy(w, io.TeeReader(snapshot, io.MultiWriter(s.plain, s.trailer)))
	s.size = n
	if err != nil {
		return fmt.Errorf("reading the snapshot from etcd: %w", err)
	}
	if err := s.trailer.check(); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("encrypting the snapshot: %w", err)
	}
	return nil
}

// finish waits for the stream to end and returns the SHA-256 and size of what etcd sent.
func (s *sealed) finish() (string, int64, error) {
	<-s.done
	if s.err != nil {
		return "", s.size, s.err
	}
	return hex.EncodeToString(s.plain.Sum(nil)), s.size, nil
}

// abort stops the stream, whose reader gave up, and waits for it to end.
func (s *sealed) abort() {
	_ = s.r.Close()
	<-s.done
}

// EtcdSnapshotter snapshots every cluster's etcd on its etcdSnapshots schedule and when asked
// through EtcdSnapshotRequestAnnotation, into its backup target, encrypted to the age identity in
// its backup keys, keeping the newest the cluster says. No etcd key or S3 credential enters the
// workload cluster: the controller signs itself an etcd client certificate for each snapshot,
// reaches etcd through the cluster's API server, and stores the snapshot itself.
type EtcdSnapshotter struct {
	Client client.Client
	// Reader reads the cluster past any cache: a reconcile queued while a snapshot streamed can
	// start before the cache has the request taken away, and would take another.
	Reader         client.Reader
	ClassNamespace string
	Etcd           EtcdSource
	Targets        *ClusterTargets
	// Events reports on a Cluster each snapshot and why one failed; may be nil.
	Events events.EventRecorder
	Now    func() time.Time

	mu     sync.Mutex
	failed map[string]time.Time
}

// SetupWithManager registers the snapshotter. One snapshot streams at a time, so that the
// management cluster holds one snapshot's buffers however many clusters are due.
func (s *EtcdSnapshotter) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Named("etcd-snapshots").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1, ReconciliationTimeout: etcdSnapshotTimeout + time.Minute}).
		Complete(s)
	if err != nil {
		return fmt.Errorf("setting up the etcd snapshotter: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler.
func (s *EtcdSnapshotter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := s.Reader.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading the cluster: %w", err)
	}
	if !Owns(cluster, s.ClassNamespace) || !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	settings, err := EtcdSnapshotsOf(cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !settings.Enabled {
		return ctrl.Result{}, s.off(ctx, cluster)
	}
	_, requested := cluster.Annotations[EtcdSnapshotRequestAnnotation]
	if RestoreInProgress(cluster) || !meta.IsStatusConditionTrue(cluster.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
		return ctrl.Result{RequeueAfter: etcdSnapshotWait}, nil
	}
	now := s.now()
	next, err := s.nextSnapshot(cluster, &settings)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !requested && now.Before(next) {
		return ctrl.Result{RequeueAfter: next.Sub(now)}, nil
	}
	tctx, cancel := context.WithTimeout(ctx, etcdSnapshotTimeout)
	defer cancel()
	t, err := s.Targets.Open(tctx, cluster, settings.Target, true)
	var taken EtcdSnapshot
	if err == nil {
		taken, err = s.take(tctx, cluster, t)
	}
	if err != nil {
		s.failure(ctx, cluster, err)
		return ctrl.Result{RequeueAfter: etcdSnapshotRetry}, s.record(ctx, cluster, nil, &metav1.Condition{
			Type: EtcdSnapshotCondition, Status: metav1.ConditionFalse, Reason: "Failed", Message: err.Error(),
		})
	}
	_, pruneErr := pruneEtcdSnapshots(tctx, t.Store, t.Folder, settings.Keep)
	return s.succeeded(ctx, cluster, &settings, &taken, pruneErr)
}

// succeeded records a snapshot taken, and says when the next is due.
func (s *EtcdSnapshotter) succeeded(ctx context.Context, cluster *clusterv1.Cluster, settings *EtcdSnapshots, taken *EtcdSnapshot, pruneErr error) (ctrl.Result, error) {
	s.mu.Lock()
	delete(s.failed, string(cluster.UID))
	s.mu.Unlock()
	ctrl.LoggerFrom(ctx).Info("took an etcd snapshot", "snapshot", taken.Name, "bytes", taken.Size)
	s.eventf(cluster, corev1.EventTypeNormal, "EtcdSnapshotTaken", "took etcd snapshot %s, %d bytes", taken.Name, taken.Size)
	message := "Took " + taken.Name + "."
	if pruneErr != nil {
		s.eventf(cluster, corev1.EventTypeWarning, "EtcdSnapshotsNotPruned", "deleting the oldest etcd snapshots: %v", pruneErr)
		message += " Deleting the oldest failed: " + pruneErr.Error()
	}
	cond := &metav1.Condition{Type: EtcdSnapshotCondition, Status: metav1.ConditionTrue, Reason: "Taken", Message: message}
	if err := s.record(ctx, cluster, taken, cond); err != nil {
		return ctrl.Result{}, err
	}
	next, err := s.nextSnapshot(cluster, settings)
	return ctrl.Result{RequeueAfter: max(next.Sub(s.now()), etcdSnapshotWait)}, err
}

// off clears what a cluster that no longer snapshots shows of its snapshots: a request nobody will
// answer, and the condition; the last snapshot stays recorded.
func (s *EtcdSnapshotter) off(ctx context.Context, cluster *clusterv1.Cluster) error {
	if _, requested := cluster.Annotations[EtcdSnapshotRequestAnnotation]; requested {
		patch := client.MergeFrom(cluster.DeepCopy())
		delete(cluster.Annotations, EtcdSnapshotRequestAnnotation)
		if err := s.Client.Patch(ctx, cluster, patch); err != nil {
			return fmt.Errorf("taking the snapshot request away: %w", err)
		}
	}
	if meta.FindStatusCondition(cluster.Status.Conditions, EtcdSnapshotCondition) == nil {
		return nil
	}
	return s.condition(ctx, cluster, nil)
}

// nextSnapshot is when the cluster's next snapshot is due: the schedule's next time after its last
// snapshot, or after it was created, a few minutes later by the cluster's own jitter, and no sooner
// than a while after one that failed.
func (s *EtcdSnapshotter) nextSnapshot(cluster *clusterv1.Cluster, settings *EtcdSnapshots) (time.Time, error) {
	schedule, err := parseSchedule(etcdSnapshotsVariable, settings.Schedule, settings.TimeZone)
	if err != nil {
		return time.Time{}, err
	}
	base := cluster.CreationTimestamp.Time
	if last, ok := LastEtcdSnapshot(cluster); ok {
		base = last.Time
	}
	next := schedule.Next(base).Add(jitter(string(cluster.UID), etcdSnapshotJitter))
	s.mu.Lock()
	failed, ok := s.failed[string(cluster.UID)]
	s.mu.Unlock()
	if ok && failed.Add(etcdSnapshotRetry).After(next) {
		next = failed.Add(etcdSnapshotRetry)
	}
	return next, nil
}

// jitter is a cluster's own offset within spread, the same every time.
func jitter(of string, spread time.Duration) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(of))
	return time.Duration(h.Sum64()%uint64(spread.Seconds())) * time.Second //nolint:gosec // the remainder is below spread
}

// LastEtcdSnapshot reads the snapshot recorded on a cluster as its last.
func LastEtcdSnapshot(c *clusterv1.Cluster) (EtcdSnapshot, bool) {
	raw, ok := c.Annotations[EtcdSnapshotAnnotation]
	if !ok {
		return EtcdSnapshot{}, false
	}
	var last EtcdSnapshot
	if err := json.Unmarshal([]byte(raw), &last); err != nil || last.Time.IsZero() {
		return EtcdSnapshot{}, false
	}
	return last, true
}

// take snapshots the cluster's etcd into its target.
func (s *EtcdSnapshotter) take(ctx context.Context, cluster *clusterv1.Cluster, t *ClusterTarget) (EtcdSnapshot, error) {
	return takeEtcdSnapshot(ctx, s.Etcd, s.now(), cluster, t)
}

// takeEtcdSnapshot snapshots the cluster's etcd from etcd into target t, named for at.
func takeEtcdSnapshot(ctx context.Context, etcd EtcdSource, at time.Time, cluster *clusterv1.Cluster, t *ClusterTarget) (EtcdSnapshot, error) {
	identity, err := t.EtcdIdentity()
	if err != nil {
		return EtcdSnapshot{}, err
	}
	stream, err := etcd.Snapshot(ctx, cluster)
	if err != nil {
		return EtcdSnapshot{}, fmt.Errorf("reaching etcd: %w", err)
	}
	defer func() { _ = stream.Close() }()
	taken := EtcdSnapshot{
		Name:              etcdSnapshotName(at, cluster.Spec.Topology.Version),
		KubernetesVersion: cluster.Spec.Topology.Version,
		Distribution:      string(distributionOf(cluster)),
		Revision:          stream.Revision,
		EtcdVersion:       stream.Version,
	}
	taken.Time, _ = time.Parse(etcdSnapshotTimeLayout, taken.Name[:len(etcdSnapshotTimeLayout)])
	key := t.Folder + "/" + taken.Name
	metadata := map[string]string{
		SnapshotMetaKubernetes:   taken.KubernetesVersion,
		SnapshotMetaDistribution: taken.Distribution,
		SnapshotMetaRevision:     strconv.FormatInt(taken.Revision, 10),
		SnapshotMetaEtcd:         taken.EtcdVersion,
	}
	body := seal(stream, identity.Recipient())
	stored, err := t.Store.Put(ctx, key, body, metadata)
	if err != nil {
		body.abort()
		return EtcdSnapshot{}, fmt.Errorf("taking the snapshot: %w", err)
	}
	sum, size, err := body.finish()
	if err != nil {
		// The store has what etcd sent before it failed, which is no snapshot.
		_ = t.Store.Remove(context.WithoutCancel(ctx), key)
		return EtcdSnapshot{}, err
	}
	taken.SHA256, taken.Size, taken.StoredSize = sum, size, stored
	// The checksum is known only once the snapshot has been read, after its upload began.
	metadata[SnapshotMetaSHA256] = sum
	metadata[SnapshotMetaSize] = strconv.FormatInt(size, 10)
	if err := t.Store.SetMetadata(ctx, key, metadata); err != nil {
		_ = t.Store.Remove(context.WithoutCancel(ctx), key)
		return EtcdSnapshot{}, fmt.Errorf("recording the snapshot's checksum: %w", err)
	}
	return taken, nil
}

// ClusterTargets opens the targets clusters store their etcd snapshots in.
type ClusterTargets struct {
	Client         client.Client
	ClassNamespace string
	OpenStore      OpenStore
}

// ClusterTarget is a cluster's way into its snapshot folder in its target, with its keys.
type ClusterTarget struct {
	Store  ObjectStore
	Folder string
	Keys   *corev1.Secret
	// CACertificate is the target's CA, when it has one, for whatever else fetches from the store.
	CACertificate string
}

// EtcdIdentity is the age identity the cluster's snapshots are encrypted to.
func (t *ClusterTarget) EtcdIdentity() (*age.X25519Identity, error) {
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(t.Keys.Data[BackupKeysKeyEtcd])))
	if err != nil {
		return nil, fmt.Errorf("the cluster's etcd snapshot key: %w", err)
	}
	return identity, nil
}

// OpenEtcdSnapshot decrypts a snapshot as it is read.
func (t *ClusterTarget) OpenEtcdSnapshot(ctx context.Context, name string) (io.ReadCloser, error) {
	if !ValidSnapshotName(name) {
		return nil, fmt.Errorf("%q: %w", name, ErrNoObject)
	}
	identity, err := t.EtcdIdentity()
	if err != nil {
		return nil, err
	}
	r, err := t.Store.Get(ctx, t.Folder+"/"+name)
	if err != nil {
		return nil, fmt.Errorf("reading snapshot %s: %w", name, err)
	}
	plain, err := age.Decrypt(r, identity)
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("decrypting snapshot %s: %w", name, err)
	}
	return &closing{ReadCloser: io.NopCloser(plain), then: r.Close}, nil
}

// DecryptEtcdSnapshot decrypts a snapshot with the age identity of the cluster's backup keys into
// w, checks etcd's own checksum at its end, and returns the SHA-256 and size of what it wrote.
func DecryptEtcdSnapshot(r io.Reader, identity string, w io.Writer) (string, int64, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(identity))
	if err != nil {
		return "", 0, fmt.Errorf("the etcd snapshot key: %w", err)
	}
	plain, err := age.Decrypt(r, id)
	if err != nil {
		return "", 0, fmt.Errorf("decrypting the snapshot: %w", err)
	}
	sum, trailer := sha256.New(), newTrailerCheck()
	n, err := io.Copy(io.MultiWriter(w, sum, trailer), plain)
	if err != nil {
		return "", n, fmt.Errorf("decrypting the snapshot: %w", err)
	}
	if err := trailer.check(); err != nil {
		return "", n, err
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}

// Open opens the target a cluster's etcd snapshots name, with the credential its backups use, and
// reads its keys, making them first when create is set.
func (ts *ClusterTargets) Open(ctx context.Context, cluster *clusterv1.Cluster, name string, create bool) (*ClusterTarget, error) {
	target, found, err := classTarget(ctx, ts.Client, ts.ClassNamespace, cluster, name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("backup target %q does not exist", name)
	}
	credential, missing, err := backupCredential(ctx, ts.Client, cluster)
	if err != nil {
		return nil, err
	}
	if missing != "" {
		return nil, errors.New(missing)
	}
	keys, err := clusterKeys(ctx, ts.Client, cluster, create)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, errors.New("the cluster has no backup keys yet")
	}
	store, err := ts.OpenStore(&target, credential)
	if err != nil {
		return nil, err
	}
	return &ClusterTarget{
		Store: store, Folder: EtcdSnapshotPrefix(target.Prefix, cluster.Namespace, cluster.Name), Keys: keys,
		CACertificate: target.CACertificate,
	}, nil
}

func (s *EtcdSnapshotter) failure(ctx context.Context, cluster *clusterv1.Cluster, err error) {
	s.mu.Lock()
	if s.failed == nil {
		s.failed = map[string]time.Time{}
	}
	s.failed[string(cluster.UID)] = s.now()
	s.mu.Unlock()
	ctrl.LoggerFrom(ctx).Info("etcd snapshot failed", "error", err.Error())
	s.eventf(cluster, corev1.EventTypeWarning, "EtcdSnapshotFailed", "etcd snapshot failed: %v", err)
}

func (s *EtcdSnapshotter) eventf(cluster *clusterv1.Cluster, kind, reason, format string, args ...any) {
	if s.Events != nil {
		s.Events.Eventf(cluster, nil, kind, reason, "SnapshotEtcd", format, args...)
	}
}

// record sets the condition, then takes the request away and records the snapshot taken, if any, as
// the last. The condition is a write of its own, to the status, so it goes first: whoever asked
// reads the request gone as the answer, and the condition then says how it went.
func (s *EtcdSnapshotter) record(ctx context.Context, cluster *clusterv1.Cluster, taken *EtcdSnapshot, cond *metav1.Condition) error {
	if err := s.condition(ctx, cluster, cond); err != nil {
		return err
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	delete(cluster.Annotations, EtcdSnapshotRequestAnnotation)
	if taken != nil {
		raw, err := json.Marshal(taken)
		if err != nil {
			return fmt.Errorf("recording the snapshot: %w", err)
		}
		metav1.SetMetaDataAnnotation(&cluster.ObjectMeta, EtcdSnapshotAnnotation, string(raw))
	}
	if err := s.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("recording the snapshot: %w", err)
	}
	return nil
}

// condition sets the snapshot condition on the cluster, or takes it away when cond is nil.
func (s *EtcdSnapshotter) condition(ctx context.Context, cluster *clusterv1.Cluster, cond *metav1.Condition) error {
	_, err := setClusterCondition(ctx, s.Client, client.ObjectKeyFromObject(cluster), EtcdSnapshotCondition, cond)
	return err
}

func (s *EtcdSnapshotter) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
