package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// memStore is a bucket in memory.
type memStore struct {
	mu      sync.Mutex
	objects map[string]*memObject
	// failPut makes Put fail once it has read that many bytes.
	failPut   error
	failAfter int
}

type memObject struct {
	data     []byte
	metadata map[string]string
}

func newMemStore() *memStore { return &memStore{objects: map[string]*memObject{}} }

func (m *memStore) Put(_ context.Context, key string, r io.Reader, metadata map[string]string) (int64, error) {
	var buf bytes.Buffer
	if m.failPut != nil {
		if _, err := io.CopyN(&buf, r, int64(m.failAfter)); err != nil {
			return 0, err
		}
		return 0, m.failPut
	}
	if _, err := io.Copy(&buf, r); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = &memObject{data: buf.Bytes(), metadata: lower(metadata)}
	return int64(buf.Len()), nil
}

func lower(metadata map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range metadata {
		out[strings.ToLower(k)] = v
	}
	return out
}

func (m *memStore) SetMetadata(_ context.Context, key string, metadata map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return ErrNoObject
	}
	o.metadata = lower(metadata)
	return nil
}

func (m *memStore) List(_ context.Context, prefix string) ([]StoredObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []StoredObject
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m.objects {
			if !yield(k) {
				return
			}
		}
	}) {
		if strings.HasPrefix(k, prefix) {
			out = append(out, StoredObject{Key: k, Size: int64(len(m.objects[k].data)), Metadata: m.objects[k].metadata})
		}
	}
	return out, nil
}

func (m *memStore) Stat(_ context.Context, key string) (StoredObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return StoredObject{}, fmt.Errorf("%s: %w", key, ErrNoObject)
	}
	return StoredObject{Key: key, Size: int64(len(o.data)), Metadata: o.metadata}, nil
}

func (m *memStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("%s: %w", key, ErrNoObject)
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

func (m *memStore) Remove(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (*memStore) PresignGet(_ context.Context, key string, expires time.Duration) (string, error) {
	return fmt.Sprintf("https://s3.example.org:8333/backups/%s?X-Amz-Expires=%d", key, int(expires.Seconds())), nil
}

func (m *memStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(func(yield func(string) bool) {
		for k := range m.objects {
			if !yield(k) {
				return
			}
		}
	})
}

// etcdData is a database as etcd streams it: the file, then its SHA-256.
func etcdData(n int) []byte {
	data := make([]byte, n+sha256.Size)
	rng := rand.NewChaCha8([32]byte{1})
	_, _ = rng.Read(data[:n])
	sum := sha256.Sum256(data[:n])
	copy(data[n:], sum[:])
	return data
}

// fakeEtcd hands out a snapshot of data.
type fakeEtcd struct {
	data  []byte
	err   error
	calls atomic.Int32
}

func (f *fakeEtcd) Snapshot(context.Context, *clusterv1.Cluster) (*EtcdStream, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &EtcdStream{ReadCloser: io.NopCloser(bytes.NewReader(f.data)), Revision: 4711, Version: "3.6.5"}, nil
}

const snapshotFolder = "site/a/team-a.web/etcd"

var etcdNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// etcdCluster is web with etcd snapshots as raw sets them, its control plane up for an hour.
func etcdCluster(raw string) *clusterv1.Cluster {
	c := withAddons(storageCluster(), map[string]string{etcdSnapshotsVariable: raw})
	c.UID = types.UID("web-uid")
	c.CreationTimestamp = metav1.NewTime(etcdNow.Add(-time.Hour))
	c.Status.Conditions = []metav1.Condition{{
		Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue, Reason: clusterv1.ClusterControlPlaneInitializedReason, LastTransitionTime: metav1.Now(),
	}}
	return c
}

type etcdFixture struct {
	t       *testing.T
	kube    client.Client
	store   *memStore
	targets *ClusterTargets
}

func newEtcdFixture(t *testing.T, objs ...client.Object) *etcdFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	scheme.AddKnownTypeWithName(proxmoxMachineGVK, &unstructured.Unstructured{})
	class := &clusterv1.ClusterClass{
		Name: "kubehenge-rke2", Namespace: "kubehenge-system",
		Annotations: map[string]string{BackupTargetsAnnotation: testTargets},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(objs, class, storeKeys("web.backup", "web", backupKeysData))...).
		WithStatusSubresource(&clusterv1.Cluster{}).Build()
	store := newMemStore()
	targets := &ClusterTargets{Client: kube, ClassNamespace: "kubehenge-system", OpenStore: func(target *BackupTarget, credential map[string][]byte) (ObjectStore, error) {
		if target.Name != "store" || string(credential[backupAccessKeyID]) != "AKID" {
			return nil, fmt.Errorf("opened %s with %q", target.Name, credential[backupAccessKeyID])
		}
		return store, nil
	}}
	return &etcdFixture{t: t, kube: kube, store: store, targets: targets}
}

func (f *etcdFixture) snapshotter(etcd EtcdSource, now time.Time) *EtcdSnapshotter {
	return &EtcdSnapshotter{
		Client: f.kube, Reader: f.kube, ClassNamespace: "kubehenge-system", Etcd: etcd, Targets: f.targets,
		Now: func() time.Time { return now },
	}
}

func (f *etcdFixture) cluster() *clusterv1.Cluster {
	f.t.Helper()
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *etcdFixture) identity() string {
	f.t.Helper()
	keys := &corev1.Secret{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: BackupKeysSecretName("web")}, keys); err != nil {
		f.t.Fatal(err)
	}
	return string(keys.Data[BackupKeysKeyEtcd])
}

func reconcileEtcd(t *testing.T, s *EtcdSnapshotter) ctrl.Result {
	t.Helper()
	res, err := s.Reconcile(context.Background(), ctrl.Request{Namespace: "team-a", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func withRequest(c *clusterv1.Cluster) *clusterv1.Cluster {
	metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdSnapshotRequestAnnotation, `{"by":"root@pam","at":"2026-10-21T11:59:00Z"}`)
	return c
}

// A snapshot asked for is encrypted to the cluster's own key, stored with what a restore checks,
// recorded on the cluster, and the oldest beyond keep are deleted, and nothing else in the folder.
func TestEtcdSnapshotterTakesEncryptsAndPrunes(t *testing.T) {
	t.Parallel()
	f := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true,"target":"store","keep":2}`)))
	for _, k := range []string{
		"20261020T000000Z-v1.37.1-rke2r1.db.age", "20261021T000000Z-v1.37.1-rke2r1.db.age", "restore/abc.enc", "notes.txt",
	} {
		f.store.objects[snapshotFolder+"/"+k] = &memObject{data: []byte("old"), metadata: map[string]string{}}
	}
	data := etcdData(300 << 10)
	etcd := &fakeEtcd{data: data}
	res := reconcileEtcd(t, f.snapshotter(etcd, etcdNow))

	name := "20261021T120000Z-v1.37.1-rke2r1.db.age"
	want := []string{
		snapshotFolder + "/20261021T000000Z-v1.37.1-rke2r1.db.age",
		snapshotFolder + "/" + name,
		snapshotFolder + "/notes.txt",
		snapshotFolder + "/restore/abc.enc",
	}
	if got := f.store.keys(); !slices.Equal(got, want) {
		t.Errorf("objects %v, want %v", got, want)
	}
	o := f.store.objects[snapshotFolder+"/"+name]
	var plain bytes.Buffer
	sum, size, err := DecryptEtcdSnapshot(bytes.NewReader(o.data), f.identity(), &plain)
	if err != nil {
		t.Fatal(err)
	}
	whole := sha256.Sum256(data)
	if !bytes.Equal(plain.Bytes(), data) || sum != hex.EncodeToString(whole[:]) || size != int64(len(data)) {
		t.Errorf("decrypted %d bytes, sum %s", size, sum)
	}
	if bytes.Contains(o.data, data[:1024]) {
		t.Error("the snapshot is stored in the clear")
	}
	for k, v := range map[string]string{
		SnapshotMetaSHA256: sum, SnapshotMetaSize: strconv.Itoa(len(data)), SnapshotMetaRevision: "4711",
		SnapshotMetaEtcd: "3.6.5", SnapshotMetaDistribution: "rke2", SnapshotMetaKubernetes: "v1.37.1+rke2r1",
	} {
		if o.metadata[k] != v {
			t.Errorf("metadata %s = %q, want %q", k, o.metadata[k], v)
		}
	}

	c := f.cluster()
	last, ok := LastEtcdSnapshot(c)
	if !ok || last.Name != name || last.SHA256 != sum || !last.Time.Equal(etcdNow) {
		t.Errorf("last snapshot %+v", last)
	}
	if _, requested := c.Annotations[EtcdSnapshotRequestAnnotation]; requested {
		t.Error("the request is still there")
	}
	if cond := meta.FindStatusCondition(c.Status.Conditions, EtcdSnapshotCondition); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("condition %+v", cond)
	}
	if res.RequeueAfter < 11*time.Hour || res.RequeueAfter > 12*time.Hour+etcdSnapshotJitter {
		t.Errorf("next look in %s, want at the schedule's next time", res.RequeueAfter)
	}

	// Within the schedule and unasked, nothing is taken.
	reconcileEtcd(t, f.snapshotter(etcd, etcdNow.Add(time.Hour)))
	if etcd.calls.Load() != 1 {
		t.Errorf("%d snapshots taken", etcd.calls.Load())
	}
}

// A stream cut off before etcd's checksum is no snapshot: nothing stays in the target, the cluster
// says why, and the next try waits.
func TestEtcdSnapshotterRefusesAStreamCutOff(t *testing.T) {
	t.Parallel()
	f := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true,"target":"store"}`)))
	data := etcdData(100 << 10)
	etcd := &fakeEtcd{data: data[:len(data)-10]}
	s := f.snapshotter(etcd, etcdNow)
	res := reconcileEtcd(t, s)
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("left %v in the target", keys)
	}
	c := f.cluster()
	cond := meta.FindStatusCondition(c.Status.Conditions, EtcdSnapshotCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "cut off") {
		t.Errorf("condition %+v", cond)
	}
	if _, requested := c.Annotations[EtcdSnapshotRequestAnnotation]; requested || res.RequeueAfter != etcdSnapshotRetry {
		t.Errorf("request kept %v, next in %s", requested, res.RequeueAfter)
	}
	if _, ok := LastEtcdSnapshot(c); ok {
		t.Error("recorded a snapshot that failed")
	}
	// The schedule falls due a few minutes later, but the retry waits its time.
	s.Now = func() time.Time { return etcdNow.Add(11 * time.Minute) }
	res = reconcileEtcd(t, s)
	if etcd.calls.Load() != 1 || res.RequeueAfter != 4*time.Minute {
		t.Errorf("%d snapshots, next in %s", etcd.calls.Load(), res.RequeueAfter)
	}
}

// Whoever asked for a snapshot reads the request gone as the answer, so by then the condition says
// how it went, also for the first snapshot a cluster takes.
func TestEtcdSnapshotterAnswersOnceTheConditionIsSet(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		data []byte
		want metav1.ConditionStatus
	}{
		"taken":  {etcdData(10 << 10), metav1.ConditionTrue},
		"failed": {etcdData(10 << 10)[:100], metav1.ConditionFalse},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true,"target":"store"}`)))
			kube, ok := f.kube.(client.WithWatch)
			if !ok {
				t.Fatal("the fixture's client does not watch")
			}
			var answered *metav1.Condition
			watched := interceptor.NewClient(kube, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					if _, requested := obj.GetAnnotations()[EtcdSnapshotRequestAnnotation]; !requested && answered == nil {
						now := &clusterv1.Cluster{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(obj), now); err != nil {
							return err
						}
						answered = &metav1.Condition{Type: "none"}
						if cond := meta.FindStatusCondition(now.Status.Conditions, EtcdSnapshotCondition); cond != nil {
							answered = cond
						}
					}
					return nil
				},
			})
			s := f.snapshotter(&fakeEtcd{data: tc.data}, etcdNow)
			s.Client = watched
			reconcileEtcd(t, s)
			if answered == nil || answered.Type != EtcdSnapshotCondition || answered.Status != tc.want {
				t.Errorf("the request went with the condition %+v, want %s", answered, tc.want)
			}
		})
	}
}

// A reconcile queued while a snapshot streamed can start before the cache has the request taken
// away: the request answered is not taken again.
func TestEtcdSnapshotterReadsPastTheCache(t *testing.T) {
	t.Parallel()
	asked := withRequest(etcdCluster(`{"enabled":true,"target":"store"}`))
	f := newEtcdFixture(t, asked)
	etcd := &fakeEtcd{data: etcdData(10 << 10)}
	s := f.snapshotter(etcd, etcdNow)
	reconcileEtcd(t, s)

	kube, ok := f.kube.(client.WithWatch)
	if !ok {
		t.Fatal("the fixture's client does not watch")
	}
	served := false
	s.Client = interceptor.NewClient(kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if cached, isCluster := obj.(*clusterv1.Cluster); isCluster && !served {
				served = true
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				cached.Annotations[EtcdSnapshotRequestAnnotation] = asked.Annotations[EtcdSnapshotRequestAnnotation]
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	reconcileEtcd(t, s)
	if etcd.calls.Load() != 1 {
		t.Errorf("%d snapshots taken for one request", etcd.calls.Load())
	}
}

// A store that fails mid-upload leaves the stream to end without anyone reading it.
func TestEtcdSnapshotterSurvivesAStoreThatFails(t *testing.T) {
	t.Parallel()
	f := newEtcdFixture(t, withRequest(etcdCluster(`{"enabled":true,"target":"store"}`)))
	f.store.failPut, f.store.failAfter = errors.New("quota exceeded"), 4096
	reconcileEtcd(t, f.snapshotter(&fakeEtcd{data: etcdData(2 << 20)}, etcdNow))
	cond := meta.FindStatusCondition(f.cluster().Status.Conditions, EtcdSnapshotCondition)
	if cond == nil || !strings.Contains(cond.Message, "quota exceeded") {
		t.Errorf("condition %+v", cond)
	}
}

func TestEtcdSnapshotterWaits(t *testing.T) {
	t.Parallel()
	uninitialised := withRequest(etcdCluster(`{"enabled":true,"target":"store"}`))
	uninitialised.Status.Conditions = nil
	restoring := withRequest(etcdCluster(`{"enabled":true,"target":"store"}`))
	metav1.SetMetaDataAnnotation(&restoring.ObjectMeta, EtcdRestoreAnnotation, `{}`)
	for name, c := range map[string]*clusterv1.Cluster{"before its control plane": uninitialised, "during a restore": restoring} {
		f := newEtcdFixture(t, c)
		etcd := &fakeEtcd{data: etcdData(1024)}
		if res := reconcileEtcd(t, f.snapshotter(etcd, etcdNow)); res.RequeueAfter != etcdSnapshotWait || etcd.calls.Load() != 0 {
			t.Errorf("%s: %d snapshots, next in %s", name, etcd.calls.Load(), res.RequeueAfter)
		}
	}
}

// Switched off, a request nobody will answer and the condition go; the last snapshot stays known.
func TestEtcdSnapshotterOff(t *testing.T) {
	t.Parallel()
	c := withRequest(etcdCluster(`{"enabled":false,"target":"store"}`))
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
		Type: EtcdSnapshotCondition, Status: metav1.ConditionFalse, Reason: "Failed", LastTransitionTime: metav1.Now(),
	})
	metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdSnapshotAnnotation, `{"name":"20261021T000000Z-v1.37.1-rke2r1.db.age","time":"2026-10-21T00:00:00Z"}`)
	f := newEtcdFixture(t, c)
	etcd := &fakeEtcd{}
	reconcileEtcd(t, f.snapshotter(etcd, etcdNow))
	got := f.cluster()
	if _, requested := got.Annotations[EtcdSnapshotRequestAnnotation]; requested || etcd.calls.Load() != 0 {
		t.Errorf("request kept %v, %d snapshots", requested, etcd.calls.Load())
	}
	if meta.FindStatusCondition(got.Status.Conditions, EtcdSnapshotCondition) != nil {
		t.Error("the condition stayed")
	}
	if _, ok := LastEtcdSnapshot(got); !ok {
		t.Error("the last snapshot was forgotten")
	}
}

func TestCheckEtcdSnapshots(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"off", `{"enabled":false,"target":"gone"}`, ""},
		{"defaults", `{"enabled":true,"target":"store"}`, ""},
		{"own schedule", `{"enabled":true,"target":"aws","schedule":"0 */6 * * *","timeZone":"Europe/Zurich","keep":30}`, ""},
		{"no target", `{"enabled":true}`, "need a target"},
		{"unknown target", `{"enabled":true,"target":"other"}`, `"other"`},
		{"bad schedule", `{"enabled":true,"target":"store","schedule":"61 * * * *"}`, "schedule"},
		{"bad zone", `{"enabled":true,"target":"store","timeZone":"Mars/Olympus"}`, "time zone"},
		{"keeps none", `{"enabled":true,"target":"store","keep":-1}`, "keep"},
		{"keeps too many", `{"enabled":true,"target":"store","keep":1001}`, "keep"},
	} {
		err := checkEtcdSnapshots(nil, etcdCluster(tc.raw), targetClass())
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
	was := etcdCluster(`{"enabled":true,"target":"old"}`)
	if err := checkEtcdSnapshots(was, etcdCluster(`{"enabled":true,"target":"old","keep":3}`), targetClass()); err != nil {
		t.Errorf("an edit of a cluster on a target since taken away: %v", err)
	}
}

// The webhook refuses what checkEtcdSnapshots does, naming the variable.
func TestValidatorHoldsEtcdSnapshots(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	_, err := v.ValidateCreate(context.Background(), etcdCluster(`{"enabled":true,"target":"store"}`))
	if err == nil || !strings.Contains(err.Error(), "variables[etcdSnapshots]") {
		t.Errorf("snapshots to a target the class does not offer: %v", err)
	}
	if _, err := v.ValidateCreate(context.Background(), etcdCluster(`{"enabled":false}`)); err != nil && strings.Contains(err.Error(), "etcdSnapshots") {
		t.Errorf("snapshots off: %v", err)
	}
}

func TestSnapshotNames(t *testing.T) {
	t.Parallel()
	if got := EtcdSnapshotName(time.Date(2026, 10, 21, 13, 4, 5, 0, time.FixedZone("CEST", 7200)), "v1.37.1+k3s1"); got != "20261021T110405Z-v1.37.1-k3s1.db.age" {
		t.Errorf("named %s", got)
	}
	for name, valid := range map[string]bool{
		"20261021T110405Z-v1.37.1-k3s1.db.age": true,
		"20261021T110405Z.db.age":              true,
		"x.db.age":                             false,
		"20261021T110405Z-v1.37.1-k3s1.db":     false,
		"../20261021T110405Z-v1.db.age":        false,
		"restore/20261021T110405Z-v1.db.age":   false,
		"20261321T110405Z-v1.db.age":           false,
	} {
		if ValidSnapshotName(name) != valid {
			t.Errorf("%s valid: %v", name, !valid)
		}
	}
	t.Run("listed newest first", func(t *testing.T) {
		t.Parallel()
		store := newMemStore()
		for _, k := range []string{"20261020T000000Z-v1.db.age", "20261021T000000Z-v1.db.age", "restore/20261022T000000Z-v1.db.age", "other.txt"} {
			store.objects["f/"+k] = &memObject{metadata: map[string]string{SnapshotMetaRevision: "7"}}
		}
		got, err := ListEtcdSnapshots(context.Background(), store, "f")
		if err != nil || len(got) != 2 || got[0].Name != "20261021T000000Z-v1.db.age" || got[0].Revision != 7 {
			t.Errorf("listed %+v, %v", got, err)
		}
	})
}

// trailerCheck finds etcd's checksum however the stream is cut into writes.
func TestTrailerCheck(t *testing.T) {
	t.Parallel()
	data := etcdData(1000)
	for _, chunk := range []int{1, 7, 31, 32, 33, 64, 1000, len(data)} {
		tc := newTrailerCheck()
		for b := range slices.Chunk(data, chunk) {
			_, _ = tc.Write(b)
		}
		if err := tc.check(); err != nil {
			t.Errorf("in writes of %d: %v", chunk, err)
		}
	}
	for name, cut := range map[string][]byte{"cut off": data[:len(data)-1], "short": data[:20], "changed": append([]byte{data[0] ^ 1}, data[1:]...)} {
		tc := newTrailerCheck()
		_, _ = tc.Write(cut)
		if tc.check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// countingReader is a snapshot of size bytes, generated as it is read, counting what it gave.
type countingReader struct {
	rng   *rand.ChaCha8
	left  int
	sum   []byte
	given atomic.Int64
	hash  hash.Hash
}

func newCountingReader(size int) *countingReader {
	return &countingReader{rng: rand.NewChaCha8([32]byte{2}), left: size, hash: sha256.New()}
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.left > 0 {
		n := min(len(p), r.left)
		_, _ = r.rng.Read(p[:n])
		_, _ = r.hash.Write(p[:n])
		r.left -= n
		r.given.Add(int64(n))
		return n, nil
	}
	if r.sum == nil {
		r.sum = r.hash.Sum(nil)
	}
	if len(r.sum) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.sum)
	r.sum = r.sum[n:]
	r.given.Add(int64(n))
	return n, nil
}

// countedReader counts what is read from it.
type countedReader struct {
	r    io.Reader
	read atomic.Int64
	most *atomic.Int64
	from *countingReader
}

func (c *countedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	read := c.read.Add(int64(n))
	if ahead := c.from.given.Load() - read; ahead > c.most.Load() {
		c.most.Store(ahead)
	}
	return n, err
}

// A snapshot of any size streams through sealing in the same few buffers: etcd is read no further
// ahead of the store than they hold.
func TestSealStreamsInBoundedMemory(t *testing.T) {
	t.Parallel()
	const size = 64 << 20
	const bound = 1 << 20
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	source := newCountingReader(size)
	body := seal(source, identity.Recipient())
	var most atomic.Int64
	stored := &countedReader{r: body, most: &most, from: source}
	sum, n, err := DecryptEtcdSnapshot(stored, identity.String(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	sealedSum, sealedSize, err := body.finish()
	if err != nil || sealedSum != sum || sealedSize != n || n != size+sha256.Size {
		t.Errorf("sealed %d bytes, %s, %v; decrypted %d, %s", sealedSize, sealedSum, err, n, sum)
	}
	if most.Load() > bound {
		t.Errorf("read %d bytes ahead of the store", most.Load())
	}
}

func TestOpenEtcdSnapshotReadsOnlySnapshots(t *testing.T) {
	t.Parallel()
	tgt := &ClusterTarget{Store: newMemStore(), Folder: "f", Keys: &corev1.Secret{}}
	if _, err := tgt.OpenEtcdSnapshot(context.Background(), "../keys"); !errors.Is(err, ErrNoObject) {
		t.Errorf("opened another object: %v", err)
	}
}
