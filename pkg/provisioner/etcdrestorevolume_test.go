package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const receiverImage = "ghcr.io/kubehenge/kubehenge@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// handOverAPI is a workload cluster's API server. A pod it is given starts at once, and an attach
// runs the receiver on what it is sent, as the pod would.
type handOverAPI struct {
	kube client.WithWatch
	// found is false until Cluster API has made the kubeconfig.
	found bool
	// free is the room the receiver finds on the node. Zero is plenty.
	free uint64
	// cut ends the stream after so many bytes. Zero sends it whole.
	cut int64

	mu       sync.Mutex
	received map[string][]byte
	pods     []*corev1.Pod
}

func (w *handOverAPI) Client(context.Context, *clusterv1.Cluster) (client.Client, bool, error) {
	return w.kube, w.found, nil
}

// Attach runs `kubehenge etcd-snapshot receive` as the pod's container would, and ends the pod.
func (w *handOverAPI) Attach(ctx context.Context, _ *clusterv1.Cluster, namespace, pod, _ string, stdin io.Reader) error {
	p := &corev1.Pod{}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pod}, p); err != nil {
		return err
	}
	if w.cut > 0 {
		stdin = io.LimitReader(stdin, w.cut)
	}
	var file bytes.Buffer
	_, err := ReceiveEtcdSnapshot(stdin, &file)
	_, _ = io.Copy(io.Discard, stdin)
	w.mu.Lock()
	w.received[pod] = file.Bytes()
	w.mu.Unlock()
	end(p, err)
	return w.kube.Status().Update(ctx, p)
}

// end has the pod's container exit as the receiver does on err.
func end(p *corev1.Pod, err error) {
	p.Status.Phase = corev1.PodSucceeded
	state := &corev1.ContainerStateTerminated{}
	if err != nil {
		p.Status.Phase, state.ExitCode, state.Message = corev1.PodFailed, 1, err.Error()
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: receiverContainer, State: corev1.ContainerState{Terminated: state}}}
}

// newFakeWorkloads is a workload API server, its calls changed by funcs where given.
func newFakeWorkloads(t *testing.T, funcs *interceptor.Funcs) *handOverAPI {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	w := &handOverAPI{found: true, received: map[string][]byte{}}
	start := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		p, ok := o.(*corev1.Pod)
		if !ok {
			return c.Create(ctx, o, opts...)
		}
		w.mu.Lock()
		w.pods = append(w.pods, p.DeepCopy())
		w.mu.Unlock()
		if err := c.Create(ctx, p, opts...); err != nil {
			return err
		}
		p.Status.Phase = corev1.PodRunning
		args := p.Spec.Containers[0].Args
		need := args[slices.Index(args, "--need")+1]
		if err := ReceiveRoom(RestoreWorkDir, mustInt(t, need), w.freeRoom()); err != nil {
			end(p, err)
		}
		return c.Status().Update(ctx, p)
	}}
	if funcs != nil && funcs.Create != nil {
		start.Create = funcs.Create
	}
	w.kube = fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(start).Build()
	return w
}

func (w *handOverAPI) freeRoom() uint64 {
	if w.free == 0 {
		return 1 << 40
	}
	return w.free
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// podsLeft lists the pods left in the workload cluster.
func podsLeft(t *testing.T, w *handOverAPI) []corev1.Pod {
	t.Helper()
	var list corev1.PodList
	if err := w.kube.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

var receivedLine = regexp.MustCompile(`(?m)^received='([^']+)'$`)

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
	f.r.ReceiverImage = receiverImage
	return f, vol
}

// kubeadm, RKE2 and k3s restore from the backup volume through the cluster's own API server: a pod
// of Kubehenge's image on the first node receives the snapshot on its stdin into the restore's
// directory, and the node decrypts it there. The pod and the file go once it has. No link is
// signed, and nothing goes into the cluster's etcd.
func TestEtcdRestoreFromTheVolumeThroughTheAPIServer(t *testing.T) {
	t.Parallel()
	for _, d := range []templates.Distribution{templates.Kubeadm, templates.RKE2, templates.K3s} {
		t.Run(string(d), func(t *testing.T) {
			t.Parallel()
			f, vol := volumeRestoreFixture(t, d)
			workloads := newFakeWorkloads(t, nil)
			f.r.Workloads = workloads
			fetched, removed := false, false
			var pod *corev1.Pod
			var file []string
			f.guests.seen = func(_ int64, step, script string) {
				if strings.HasPrefix(script, "rm -f -- '"+RestoreWorkDir+"/received-") {
					removed = true
				}
				if step != stepFetch {
					return
				}
				fetched = true
				if strings.Contains(script, "X-Amz") || strings.Contains(script, "url=") {
					t.Errorf("the fetch has a link:\n%s", script)
				}
				file = receivedLine.FindStringSubmatch(script)
				password, sum := passwordLine.FindStringSubmatch(script), sha256Line.FindStringSubmatch(script)
				if file == nil || password == nil || sum == nil {
					t.Fatalf("the fetch script lacks its values:\n%s", script)
				}
				workloads.mu.Lock()
				pod = workloads.pods[0]
				sealed := workloads.received[pod.Name]
				workloads.mu.Unlock()
				plain, err := opensslOpen(sealed, password[1])
				if err != nil {
					t.Fatalf("the received snapshot does not decrypt: %v", err)
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
			checkReceiver(t, pod, file[1], st.Node)
			// The snapshot is on the node, checked, before anything stops, and nothing is reset.
			runs := f.guests.runs()
			fetch := slices.Index(runs, "101 fetch")
			stop := slices.IndexFunc(runs, func(r string) bool { return strings.HasSuffix(r, " stop") })
			if fetch < 0 || stop < fetch || slices.Contains(runs, "101 reset") {
				t.Errorf("ran %v", runs)
			}
			if left := podsLeft(t, workloads); len(left) != 0 || !removed {
				t.Errorf("left %d pods in the cluster, file removed %v", len(left), removed)
			}
			// The restore holds the volume apart from the cluster's snapshots, which run beside it.
			if want := "restore of " + f.cluster().Namespace + "/" + f.cluster().Name; len(vol.holders) == 0 || slices.ContainsFunc(vol.holders, func(h string) bool { return h != want }) {
				t.Errorf("the volume was held by %q, want %q", vol.holders, want)
			}
			if vol.released == 0 {
				t.Error("the restore did not let the volume go")
			}
		})
	}
}

// checkReceiver holds the pod to the node, Kubehenge's image by digest, and the file in the
// restore's directory on the node.
func checkReceiver(t *testing.T, p *corev1.Pod, file, node string) {
	t.Helper()
	c := p.Spec.Containers[0]
	if p.Spec.NodeName != node || c.Image != receiverImage || !c.Stdin || !c.StdinOnce {
		t.Errorf("pod on %q, image %q, stdin %v once %v", p.Spec.NodeName, c.Image, c.Stdin, c.StdinOnce)
	}
	if !slices.Equal(c.Args[:5], []string{"etcd-snapshot", "receive", "--out", file, "--need"}) || !strings.HasPrefix(file, RestoreWorkDir+"/") {
		t.Errorf("args %q", c.Args)
	}
	if v := p.Spec.Volumes; len(v) != 1 || v[0].HostPath == nil || v[0].HostPath.Path != RestoreWorkDir {
		t.Errorf("volumes %+v", v)
	}
	if p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken {
		t.Error("the receiver gets a service account token")
	}
	// SELinux keeps a confined container from writing the host's directory.
	if sc := c.SecurityContext; sc == nil || sc.SELinuxOptions == nil || *sc.SELinuxOptions != *hostSELinux() {
		t.Errorf("the receiver writes the host's directory without the context for it: %+v", sc)
	}
}

// A controller that cannot reach workload clusters, or has no receiver image, refuses a restore
// from the volume before etcd stops, and its status and event say what to do.
func TestEtcdRestoreFromTheVolumeNeedsAWayToTheNode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		workloads *handOverAPI
		image     string
	}{
		"no workload clients": {nil, receiverImage},
		"no receiver image":   {newFakeWorkloads(t, nil), ""},
	} {
		f, vol := volumeRestoreFixture(t, templates.Kubeadm)
		f.r.Workloads, f.r.ReceiverImage = nil, tc.image
		if tc.workloads != nil {
			f.r.Workloads = tc.workloads
		}
		f.reconcile()
		st := f.status()
		if st.Phase != RestoreFailed || !strings.Contains(st.Message, "restore by hand") {
			t.Errorf("%s: status %+v", name, st)
		}
		if runs := f.guests.runs(); slices.ContainsFunc(runs, func(r string) bool { return strings.HasSuffix(r, " stop") }) {
			t.Errorf("%s: etcd was stopped without a way for the snapshot: %v", name, runs)
		}
		if vol.openedAwake == 0 || vol.released != vol.openedAwake {
			t.Errorf("%s: the volume was opened %d times and let go %d", name, vol.openedAwake, vol.released)
		}
		checkFailedEvent(t, f, name)
	}
}

// A cluster that lost its quorum, whose API server does not answer, restores from the volume: the
// first node comes up alone on its own data, its API server answers, and the snapshot streams in.
// Then the restore runs as from a bucket, and the other control planes are replaced.
func TestEtcdRestoreFromTheVolumeAfterTheQuorumIsLost(t *testing.T) {
	t.Parallel()
	for _, d := range []templates.Distribution{templates.Kubeadm, templates.RKE2, templates.K3s} {
		f, _ := volumeRestoreFixture(t, d)
		w := newFakeWorkloads(t, nil)
		f.r.Workloads = w
		f.apis.down = 1 << 20
		f.guests.seen = func(_ int64, step, _ string) {
			if step == stepStart {
				f.apis.mu.Lock()
				f.apis.down = 0
				f.apis.mu.Unlock()
			}
		}
		f.reconcile()
		st := f.status()
		if st.Phase != RestoreSucceeded || len(st.Replaced) != 2 {
			t.Fatalf("%s: status %+v", d, st)
		}
		want := []string{"101 stop", "102 stop", "103 stop", "101 reset", "101 start", "101 fetch", "101 stop", "101 start"}
		if runs := f.guests.runs(); !isSubsequence(want, runs) {
			t.Errorf("%s: ran %v, want in order %v", d, runs, want)
		}
		if left := podsLeft(t, w); len(left) != 0 {
			t.Errorf("%s: left %d pods", d, len(left))
		}
	}
}

// isSubsequence tells whether want comes in got, in order.
func isSubsequence(want, got []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

func checkFailedEvent(t *testing.T, f *restoreFixture, name string) {
	t.Helper()
	if e := f.event(); !strings.Contains(e, "EtcdRestoreFailed") {
		t.Errorf("%s: event %q", name, e)
	}
}

// checkUntouched holds a restore that failed before the snapshot was on the first node, in full and
// checked, to every node left as it was: nothing stopped, reset or restored, and nothing replaced.
func checkUntouched(t *testing.T, f *restoreFixture, name string) {
	t.Helper()
	runs := f.guests.runs()
	for _, step := range []string{stepStop, stepReset, stepRestore, stepStart, stepStartAs, stepHold} {
		if slices.ContainsFunc(runs, func(r string) bool { return strings.HasSuffix(r, " "+step) }) {
			t.Errorf("%s: ran %s before the snapshot was on the node: %v", name, step, runs)
		}
	}
	if st := f.status(); len(st.Replaced) != 0 || st.Alone {
		t.Errorf("%s: replaced %v, alone %v", name, st.Replaced, st.Alone)
	}
	if gone := f.deleting(); len(gone) != 0 {
		t.Errorf("%s: deleted %v", name, gone)
	}
	checkFailedEvent(t, f, name)
}

// checkKeptAlone holds a restore that failed past resetting etcd to the first node to that node
// started alone, the others never started and held stopped, nothing replaced, and the state on the
// cluster as a condition and in the status.
func checkKeptAlone(t *testing.T, f *restoreFixture, name string) {
	t.Helper()
	runs := f.guests.runs()
	reset := slices.Index(runs, "101 reset")
	if reset < 0 {
		t.Fatalf("%s: etcd was not reset: %v", name, runs)
	}
	for _, r := range runs[reset:] {
		if (strings.HasPrefix(r, "102 ") || strings.HasPrefix(r, "103 ")) && strings.Contains(r, "start") {
			t.Errorf("%s: an old member started beside the reset one: %v", name, runs)
		}
	}
	if !isSubsequence([]string{"101 reset", "101 start as it was"}, runs) || !slices.Contains(runs, "102 hold") || !slices.Contains(runs, "103 hold") {
		t.Errorf("%s: ran %v", name, runs)
	}
	st := f.status()
	if st.Phase != RestoreFailed || !st.Alone || len(st.Replaced) != 0 || !strings.Contains(st.Message, "etcd runs on web-cp-0 alone") {
		t.Errorf("%s: status %+v", name, st)
	}
	if gone := f.deleting(); len(gone) != 0 {
		t.Errorf("%s: deleted %v", name, gone)
	}
	if c := meta.FindStatusCondition(f.cluster().Status.Conditions, EtcdAloneCondition); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("%s: condition %+v", name, c)
	}
	checkFailedEvent(t, f, name)
}

// Past the reset of etcd to the first node of a cluster that lost its quorum, a stream cut short or
// a second stop that fails keeps the other control planes stopped: their members would make a
// cluster of their own beside the first node's.
func TestEtcdRestoreFromTheVolumeKeepsTheOthersStoppedPastTheReset(t *testing.T) {
	t.Parallel()
	for _, d := range []templates.Distribution{templates.Kubeadm, templates.RKE2, templates.K3s} {
		for name, tc := range map[string]struct {
			cut      int64
			stopFail bool
		}{
			"stream cut short":   {cut: 100},
			"second stop failed": {stopFail: true},
		} {
			f, _ := volumeRestoreFixture(t, d)
			w := newFakeWorkloads(t, nil)
			w.cut = tc.cut
			f.r.Workloads = w
			f.apis.down = 1 << 20
			stops := 0
			f.guests.seen = func(_ int64, step, _ string) {
				if step == stepStart {
					f.apis.mu.Lock()
					f.apis.down = 0
					f.apis.mu.Unlock()
				}
			}
			if tc.stopFail {
				f.guests.fail = func(vmid int64, step string) error {
					if step != stepStop || vmid != 103 {
						return nil
					}
					stops++
					if stops == 2 {
						return errors.New("the guest agent timed out")
					}
					return nil
				}
			}
			f.reconcile()
			checkKeptAlone(t, f, string(d)+", "+name)
		}
	}
}

// A damaged snapshot fails its check on the controller, before any node changes, whether the
// cluster's API server answers or not.
func TestEtcdRestoreFromTheVolumeRefusesADamagedSnapshot(t *testing.T) {
	t.Parallel()
	for _, down := range []bool{false, true} {
		f, _ := volumeRestoreFixture(t, templates.RKE2)
		f.r.Workloads = newFakeWorkloads(t, nil)
		if down {
			f.apis.down = 1 << 20
		}
		o := f.store.objects[volumeFolder+"/"+restoreSnapshot]
		o.data[len(o.data)/2] ^= 0xff
		f.reconcile()
		name := fmt.Sprintf("API server down %v", down)
		if st := f.status(); st.Phase != RestoreFailed {
			t.Errorf("%s: status %+v", name, st)
		}
		checkUntouched(t, f, name)
	}
}

// A cluster without a kubeconfig, or whose API server refuses the receiving pod, fails the restore
// before anything on the nodes changes.
func TestEtcdRestoreFromTheVolumeWithoutAnAPIServer(t *testing.T) {
	t.Parallel()
	for name, w := range map[string]*handOverAPI{
		"no kubeconfig": {found: false},
		"API server down": newFakeWorkloads(t, &interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errors.New("dial tcp 10.0.0.10:6443: connect: connection refused")
		}}),
	} {
		f, _ := volumeRestoreFixture(t, templates.Kubeadm)
		f.r.Workloads = w
		f.reconcile()
		if st := f.status(); st.Phase != RestoreFailed || !strings.Contains(st.Message, "restore by hand") {
			t.Errorf("%s: status %+v", name, st)
		}
		checkUntouched(t, f, name)
	}
}

// A node without room for the snapshot refuses it before any of it is sent, and a stream cut short
// fails its checksum. Either way every node is left as it was, and the pod goes.
func TestEtcdRestoreFromTheVolumeRefusesWhatTheNodeCannotTake(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		free uint64
		cut  int64
		want string
	}{
		"no room":   {free: 1024, want: "the node has 1.0 KiB free at " + RestoreWorkDir},
		"cut short": {cut: 100, want: "cut short"},
	} {
		f, _ := volumeRestoreFixture(t, templates.RKE2)
		w := newFakeWorkloads(t, nil)
		w.free, w.cut = tc.free, tc.cut
		f.r.Workloads = w
		f.reconcile()
		st := f.status()
		if st.Phase != RestoreFailed || !strings.Contains(st.Message, tc.want) {
			t.Errorf("%s: status %+v", name, st)
		}
		checkUntouched(t, f, name)
		if left := podsLeft(t, w); len(left) != 0 {
			t.Errorf("%s: left %d pods", name, len(left))
		}
	}
}

// The stream ends on its checksum, which the receiver holds it to.
func TestReceiveEtcdSnapshot(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("etcd"), 50_000)
	var stream bytes.Buffer
	if err := SendEtcdSnapshot(&stream, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if n, err := ReceiveEtcdSnapshot(iotest.HalfReader(bytes.NewReader(stream.Bytes())), &out); err != nil || n != int64(len(body)) || !bytes.Equal(out.Bytes(), body) {
		t.Fatalf("received %d, %v", n, err)
	}
	changed := slices.Clone(stream.Bytes())
	changed[10] ^= 1
	for name, in := range map[string][]byte{
		"cut":     stream.Bytes()[:stream.Len()-1],
		"changed": changed,
		"empty":   nil,
		"alone":   stream.Bytes()[len(body):],
	} {
		if _, err := ReceiveEtcdSnapshot(bytes.NewReader(in), io.Discard); !errors.Is(err, errStreamCut) {
			t.Errorf("%s: %v", name, err)
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
	if err := run.readSnapshot(ctx); !errors.Is(err, ErrVolumeAsleep) {
		t.Fatalf("read a snapshot from a sleeping volume: %v", err)
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
