package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// handOverAPI is a workload cluster's API server, which keeps the Secrets of a hand-over.
type handOverAPI struct {
	kube client.WithWatch
	// found is false until Cluster API has made the kubeconfig.
	found bool
}

func (w *handOverAPI) Client(context.Context, *clusterv1.Cluster) (client.Client, bool, error) {
	return w.kube, w.found, nil
}

// newFakeWorkloads is a workload API server, its calls changed by funcs where given.
func newFakeWorkloads(t *testing.T, funcs *interceptor.Funcs) *handOverAPI {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return &handOverAPI{kube: b.Build(), found: true}
}

// handOverSecrets lists the Secrets left of hand-overs.
func handOverSecrets(t *testing.T, w *handOverAPI) []corev1.Secret {
	t.Helper()
	var list corev1.SecretList
	if err := w.kube.List(context.Background(), &list, client.InNamespace(metav1.NamespaceSystem)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

var (
	nameLine  = regexp.MustCompile(`(?m)^name='([^']+)'$`)
	countLine = regexp.MustCompile(`(?m)^count='(\d+)'$`)
)

// volumeRestoreFixture is newRestoreFixture with the cluster's snapshots on the backup volume and
// the restore asked from there.
func volumeRestoreFixture(t *testing.T, d templates.Distribution) (*restoreFixture, *fakeVolume) {
	t.Helper()
	f := newRestoreFixture(t, func(c *clusterv1.Cluster) {
		c.Spec.Topology.ClassRef.Name = ClassPrefix + string(d)
		for i := range c.Spec.Topology.Variables {
			if c.Spec.Topology.Variables[i].Name == etcdSnapshotsVariable {
				c.Spec.Topology.Variables[i].Value.Raw = []byte(`{"enabled":true}`)
			}
		}
		metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdRestoreAnnotation,
			fmt.Sprintf(`{"snapshot":%q,"by":"root@pam","at":%q}`, restoreSnapshot, restoreAsked.Format(time.RFC3339)))
	})
	vol := newFakeVolume()
	vol.store = f.store
	for _, k := range f.store.keys() {
		if rest, ok := strings.CutPrefix(k, snapshotFolder+"/"); ok {
			f.store.objects[volumeFolder+"/"+rest] = f.store.objects[k]
			delete(f.store.objects, k)
		}
	}
	f.targets.Volume = vol
	f.r.Targets = f.targets
	return f, vol
}

// kubeadm, RKE2 and k3s restore from the backup volume through the cluster's own API server: the
// snapshot goes into Secrets in kube-system, the first node reads them with its admin kubeconfig,
// and they go once it has. No link is signed, and the bucket is never opened.
func TestEtcdRestoreFromTheVolumeThroughTheAPIServer(t *testing.T) {
	t.Parallel()
	for _, d := range []templates.Distribution{templates.Kubeadm, templates.RKE2, templates.K3s} {
		t.Run(string(d), func(t *testing.T) {
			t.Parallel()
			f, vol := volumeRestoreFixture(t, d)
			workloads := newFakeWorkloads(t, nil)
			f.r.Workloads = workloads
			fetched := false
			f.guests.seen = func(_ int64, step, script string) {
				if step != stepFetch {
					return
				}
				fetched = true
				if strings.Contains(script, "X-Amz") || strings.Contains(script, "url=") {
					t.Errorf("the fetch has a link:\n%s", script)
				}
				if !strings.HasPrefix(script, "kubeconfig='"+adminKubeconfig(d)+"'\n") {
					t.Errorf("the fetch reads no %s:\n%s", adminKubeconfig(d), script)
				}
				name, count := nameLine.FindStringSubmatch(script), countLine.FindStringSubmatch(script)
				password, sum := passwordLine.FindStringSubmatch(script), sha256Line.FindStringSubmatch(script)
				if name == nil || count == nil || password == nil || sum == nil {
					t.Fatalf("the fetch script lacks its values:\n%s", script)
				}
				n, _ := strconv.Atoi(count[1])
				var sealed bytes.Buffer
				for i := range n {
					s := &corev1.Secret{}
					key := client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: fmt.Sprintf("%s-%04d", name[1], i)}
					if err := workloads.kube.Get(context.Background(), key, s); err != nil {
						t.Fatalf("chunk %d: %v", i, err)
					}
					sealed.Write(s.Data[handOverKey])
				}
				plain, err := opensslOpen(sealed.Bytes(), password[1])
				if err != nil {
					t.Fatalf("the handed over snapshot does not decrypt: %v", err)
				}
				if d != templates.Kubeadm {
					checkBumpedForNode(t, plain, sum[1])
					return
				}
				stored := sha256.Sum256(f.data)
				if !bytes.Equal(plain, f.data) || sum[1] != hex.EncodeToString(stored[:]) {
					t.Error("kubeadm's node does not get the snapshot as stored")
				}
			}
			f.reconcile()
			st := f.status()
			if !fetched || st.Phase != RestoreSucceeded {
				t.Fatalf("fetched %v, status %+v", fetched, st)
			}
			if left := handOverSecrets(t, workloads); len(left) != 0 {
				t.Errorf("left %d Secrets in the cluster", len(left))
			}
			if vol.released == 0 {
				t.Error("the restore did not let the volume go")
			}
		})
	}
}

// A cluster whose API server does not answer cannot get the snapshot: the restore fails before
// etcd stops, and its status and event say why and what to do.
func TestEtcdRestoreFromTheVolumeNeedsTheAPIServer(t *testing.T) {
	t.Parallel()
	for name, workloads := range map[string]*handOverAPI{
		"no kubeconfig": {found: false},
		"API server down": newFakeWorkloads(t, &interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errors.New("dial tcp 10.0.0.10:6443: connect: connection refused")
		}}),
	} {
		f, _ := volumeRestoreFixture(t, templates.Kubeadm)
		f.r.Workloads = workloads
		f.reconcile()
		st := f.status()
		if st.Phase != RestoreFailed || !strings.Contains(st.Message, "Download the snapshot and restore by hand") {
			t.Errorf("%s: status %+v", name, st)
		}
		if runs := f.guests.runs(); slices.ContainsFunc(runs, func(r string) bool { return strings.HasSuffix(r, " stop") || strings.HasSuffix(r, " restore") }) {
			t.Errorf("%s: etcd was stopped without the snapshot: %v", name, runs)
		}
		if e := f.event(); !strings.Contains(e, "EtcdRestoreFailed") {
			t.Errorf("%s: event %q", name, e)
		}
	}
}

// A restore waits for a sleeping volume and says so, in its status and an event.
func TestEtcdRestoreWaitsForTheVolume(t *testing.T) {
	t.Parallel()
	f, vol := volumeRestoreFixture(t, templates.Talos)
	vol.asleep = true
	f.r.Workloads = newFakeWorkloads(t, nil)
	run := &restoreRun{r: f.r, key: client.ObjectKey{Namespace: "team-a", Name: "web"}, cluster: f.cluster(), talos: true}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := run.openTarget(ctx); !errors.Is(err, ErrVolumeAsleep) {
		t.Fatalf("opened a sleeping volume: %v", err)
	}
	if run.status.Message != volumeWakingMessage {
		t.Errorf("status message %q", run.status.Message)
	}
	if e := f.event(); !strings.Contains(e, "EtcdRestoreWaitingForVolume") {
		t.Errorf("event %q", e)
	}
	vol.asleep = false
	if _, err := run.openTarget(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Over maxHandOver the snapshot is refused rather than filling the cluster's etcd.
func TestHandOverRefusesALargeSnapshot(t *testing.T) {
	t.Parallel()
	w := newFakeWorkloads(t, nil)
	if _, err := writeChunks(context.Background(), w.kube, "id", bytes.NewReader(make([]byte, maxHandOver+1))); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("a snapshot over the bound: %v", err)
	}
	n, err := writeChunks(context.Background(), w.kube, "even", bytes.NewReader(make([]byte, 2*handOverChunk)))
	if err != nil || n != 2 {
		t.Fatalf("two whole chunks: %d, %v", n, err)
	}
}
