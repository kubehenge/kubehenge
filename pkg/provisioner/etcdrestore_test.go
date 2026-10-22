package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// The steps a restore runs, told apart by what their scripts do.
const (
	stepPing     = "ping"
	stepFetch    = "fetch"
	stepStop     = "stop"
	stepRestore  = "restore"
	stepStart    = "start"
	stepStartAs  = "start as it was"
	stepRestored = "restored?"
	stepClean    = "clean"
)

func stepOf(script string) string {
	switch {
	case script == "true\n":
		return stepPing
	case strings.Contains(script, "openssl enc -d"):
		return stepFetch
	case strings.Contains(script, "systemctl stop"):
		return stepStop
	case strings.Contains(script, "--cluster-reset"):
		return stepRestore
	case strings.Contains(script, "wait_for 600 ready"):
		return stepStart
	case strings.Contains(script, "started the control plane"):
		return stepStartAs
	case strings.Contains(script, `test -e "$work/restored"`):
		return stepRestored
	case strings.Contains(script, `rm -rf "$work"`):
		return stepClean
	}
	return "unknown"
}

// fakeGuests runs nothing, recording what it was asked to run where; fail decides which runs fail.
type fakeGuests struct {
	mu   sync.Mutex
	off  map[int64]bool
	ran  []string
	fail func(vmid int64, step string) error
	// seen checks a script as it runs.
	seen func(vmid int64, step, script string)
}

func (g *fakeGuests) Running(_ context.Context, vmid int64) (bool, error) {
	return !g.off[vmid], nil
}

func (g *fakeGuests) Run(_ context.Context, vmid int64, script string, _ time.Duration) (string, error) {
	step := stepOf(script)
	g.mu.Lock()
	g.ran = append(g.ran, fmt.Sprintf("%d %s", vmid, step))
	g.mu.Unlock()
	if g.seen != nil {
		g.seen(vmid, step, script)
	}
	if g.fail != nil {
		if err := g.fail(vmid, step); err != nil {
			return "it failed", err
		}
	}
	return "done", nil
}

func (g *fakeGuests) runs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.ran)
}

const restoreSnapshot = "20261021T060000Z-v1.37.1-rke2r1.db.age"

var restoreAsked = time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)

type restoreFixture struct {
	*etcdFixture

	guests *fakeGuests
	data   []byte
	r      *EtcdRestorer
}

// newRestoreFixture is web with three control-plane VMs, 101 to 103, a snapshot of its own in the
// target, and a restore from it asked for.
func newRestoreFixture(t *testing.T, change func(*clusterv1.Cluster)) *restoreFixture {
	t.Helper()
	c := etcdCluster(`{"enabled":true,"target":"store"}`)
	metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdRestoreAnnotation,
		fmt.Sprintf(`{"snapshot":%q,"target":"store","by":"root@pam","at":%q}`, restoreSnapshot, restoreAsked.Format(time.RFC3339)))
	if change != nil {
		change(c)
	}
	objs := make([]client.Object, 0, 7)
	objs = append(objs, c)
	for i := range 3 {
		m, pm := controlPlane(fmt.Sprintf("web-cp-%d", i), int64(101+i), fmt.Sprintf("pve%d", i+1))
		objs = append(objs, m, pm)
	}
	f := &restoreFixture{etcdFixture: newEtcdFixture(t, objs...), guests: &fakeGuests{off: map[int64]bool{}}}
	keys, err := clusterKeys(context.Background(), f.kube, c, true)
	if err != nil {
		t.Fatal(err)
	}
	f.data = etcdData(64 << 10)
	f.putSnapshot(restoreSnapshot, string(keys.Data[BackupKeysKeyEtcd]), "v1.37.1+rke2r1")
	f.r = &EtcdRestorer{
		Client: f.kube, ClassNamespace: "kubehenge-system", Targets: f.targets, Guests: f.guests,
		Now: func() time.Time { return etcdNow },
	}
	return f
}

// putSnapshot stores the fixture's data as a snapshot, as the snapshotter does.
func (f *restoreFixture) putSnapshot(name, identity, version string) {
	f.t.Helper()
	id, err := age.ParseX25519Identity(identity)
	if err != nil {
		f.t.Fatal(err)
	}
	body := seal(bytes.NewReader(f.data), id.Recipient())
	if _, err = f.store.Put(context.Background(), snapshotFolder+"/"+name, body, nil); err != nil {
		f.t.Fatal(err)
	}
	sum, size, err := body.finish()
	if err != nil {
		f.t.Fatal(err)
	}
	metadata := map[string]string{
		SnapshotMetaSHA256: sum, SnapshotMetaSize: strconv.FormatInt(size, 10), SnapshotMetaDistribution: "rke2", SnapshotMetaKubernetes: version,
	}
	if err = f.store.SetMetadata(context.Background(), snapshotFolder+"/"+name, metadata); err != nil {
		f.t.Fatal(err)
	}
}

func (f *restoreFixture) reconcile() {
	f.t.Helper()
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{Namespace: "team-a", Name: "web"}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *restoreFixture) status() EtcdRestoreStatus {
	f.t.Helper()
	c := f.cluster()
	if RestoreInProgress(c) {
		f.t.Error("the request is still there")
	}
	if c.Spec.Paused != nil {
		f.t.Errorf("the cluster is left paused: %v", *c.Spec.Paused)
	}
	st, ok := EtcdRestoreStatusOf(c)
	if !ok || st.Finished == nil {
		f.t.Fatalf("status %+v", st)
	}
	return st
}

// deleting lists the control-plane machines being deleted.
func (f *restoreFixture) deleting() []string {
	f.t.Helper()
	var machines clusterv1.MachineList
	if err := f.kube.List(context.Background(), &machines); err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for i := range machines.Items {
		if !machines.Items[i].DeletionTimestamp.IsZero() {
			out = append(out, machines.Items[i].Name)
		}
	}
	slices.Sort(out)
	return out
}

func (f *restoreFixture) transcoded() []string {
	var out []string
	for _, k := range f.store.keys() {
		if strings.Contains(k, "/"+restoreObjectFolder+"/") {
			out = append(out, k)
		}
	}
	return out
}

var passwordLine = regexp.MustCompile(`(?m)^password='([^']+)'$`)

// A restore pauses the cluster, has the first node fetch and check the snapshot, stops every
// control plane, restores on the first, starts it, and replaces the others.
func TestEtcdRestorerRestores(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, nil)
	f.guests.seen = func(vmid int64, step, script string) {
		c := f.cluster()
		if step != stepPing && (c.Spec.Paused == nil || !*c.Spec.Paused) {
			t.Errorf("%s ran on %d with the cluster not paused", step, vmid)
		}
		if step != stepFetch {
			return
		}
		sum := sha256.Sum256(f.data)
		if !strings.Contains(script, hex.EncodeToString(sum[:])) || !strings.Contains(script, "X-Amz-Expires=3600") {
			t.Errorf("the fetch checks no checksum or has no link:\n%s", script)
		}
		objects := f.transcoded()
		password := passwordLine.FindStringSubmatch(script)
		if len(objects) != 1 || password == nil {
			t.Fatalf("transcoded %v, password %v", objects, password)
		}
		plain, err := opensslOpen(f.store.objects[objects[0]].data, password[1])
		if err != nil || !bytes.Equal(plain, f.data) {
			t.Errorf("the transcoded snapshot does not decrypt to it: %v", err)
		}
	}
	f.reconcile()

	want := []string{
		"101 ping", "102 ping", "103 ping", "101 fetch", "101 stop", "102 stop", "103 stop", "101 restore", "101 start",
	}
	if got := f.guests.runs(); !slices.Equal(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
	st := f.status()
	if st.Phase != RestoreSucceeded || st.Machine != "web-cp-0" || !slices.Equal(st.Replaced, []string{"web-cp-1", "web-cp-2"}) || !st.Paused {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); !slices.Equal(got, []string{"web-cp-1", "web-cp-2"}) {
		t.Errorf("deleting %v", got)
	}
	if left := f.transcoded(); len(left) != 0 {
		t.Errorf("left %v in the target", left)
	}

	// The request taken away, the restore is not run again.
	f.reconcile()
	if got := len(f.guests.runs()); got != len(want) {
		t.Errorf("ran %d steps after the restore", got-len(want))
	}
}

// A restore that fails before etcd was restored starts every control plane again as it was and
// replaces no machine.
func TestEtcdRestorerUndoesWhatFailedBeforeRestoring(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, failing string
		vmid          int64
		marker        bool
		want          []string
		replaced      bool
	}{
		{"stopping", stepStop, 102, false, []string{"101 stop", "102 stop", "101 start as it was", "102 start as it was", "103 start as it was"}, false},
		{"restoring", stepRestore, 101, false, []string{"101 restore", "101 restored?", "101 start as it was", "102 start as it was", "103 start as it was"}, false},
		{"restoring after the swap", stepRestore, 101, true, []string{"101 restore", "101 restored?", "101 start"}, true},
		{"fetching", stepFetch, 101, false, []string{"101 fetch", "101 clean"}, false},
		{"starting", stepStart, 101, false, []string{"101 restore", "101 start", "101 start"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRestoreFixture(t, nil)
			f.guests.fail = func(vmid int64, step string) error {
				if step == stepRestored && !tc.marker {
					return errors.New("exit status 1")
				}
				if step == tc.failing && vmid == tc.vmid {
					return errors.New("exit status 1")
				}
				return nil
			}
			f.reconcile()
			runs := slices.DeleteFunc(f.guests.runs(), func(r string) bool { return strings.HasSuffix(r, stepPing) })
			if !hasTail(runs, tc.want) {
				t.Errorf("ran %v, want it to end %v", runs, tc.want)
			}
			st := f.status()
			if st.Phase != RestoreFailed || !strings.Contains(st.Message, "exit status 1") {
				t.Errorf("status %+v", st)
			}
			if got := f.deleting(); (len(got) == 2) != tc.replaced {
				t.Errorf("deleting %v", got)
			}
			if left := f.transcoded(); len(left) != 0 {
				t.Errorf("left %v in the target", left)
			}
		})
	}
}

func hasTail(got, want []string) bool {
	return len(got) >= len(want) && slices.Equal(got[len(got)-len(want):], want)
}

// A controller that stopped during a restore finishes what was restored when it starts again.
func TestEtcdRestorerFinishesARestoreInterrupted(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, func(c *clusterv1.Cluster) {
		c.Spec.Paused = new(true)
		metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdRestoreStatusAnnotation, fmt.Sprintf(
			`{"snapshot":%q,"requestedAt":%q,"phase":"Starting","machine":"web-cp-0","replaced":["web-cp-1","web-cp-2"],"started":%q,"paused":true}`,
			restoreSnapshot, restoreAsked.Format(time.RFC3339), restoreAsked.Format(time.RFC3339)))
	})
	f.reconcile()
	if got := f.guests.runs(); !slices.Equal(got, []string{"101 start"}) {
		t.Errorf("ran %v", got)
	}
	st := f.status()
	if st.Phase != RestoreFailed || !strings.Contains(st.Message, "restarted") {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); !slices.Equal(got, []string{"web-cp-1", "web-cp-2"}) {
		t.Errorf("deleting %v", got)
	}
}

func TestEtcdRestorerRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		change     func(*restoreFixture)
	}{
		{"without the guest agent", "does not restore through the guest agent", func(f *restoreFixture) { f.r.Guests = nil }},
		{"another minor", "Kubernetes v1.36", func(f *restoreFixture) {
			keys, err := clusterKeys(context.Background(), f.kube, f.cluster(), false)
			if err != nil {
				f.t.Fatal(err)
			}
			f.putSnapshot(restoreSnapshot, string(keys.Data[BackupKeysKeyEtcd]), "v1.36.4+rke2r1")
		}},
		{"a snapshot that is gone", "no such object", func(f *restoreFixture) {
			_ = f.store.Remove(context.Background(), snapshotFolder+"/"+restoreSnapshot)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRestoreFixture(t, nil)
			tc.change(f)
			f.reconcile()
			if runs := slices.DeleteFunc(f.guests.runs(), func(r string) bool { return strings.HasSuffix(r, stepPing) }); len(runs) != 0 {
				t.Errorf("ran %v", runs)
			}
			if st := f.status(); st.Phase != RestoreFailed || !strings.Contains(st.Message, tc.want) {
				t.Errorf("status %+v, want a message naming %q", st, tc.want)
			}
			if got := f.deleting(); len(got) != 0 {
				t.Errorf("deleting %v", got)
			}
		})
	}
}

// A control-plane VM that is off runs no etcd to stop: it is replaced with the others.
func TestEtcdRestorerReplacesAVMThatIsOff(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, nil)
	f.guests.off[103] = true
	f.reconcile()
	for _, r := range f.guests.runs() {
		if strings.HasPrefix(r, "103 ") {
			t.Errorf("ran %s on a VM that is off", r)
		}
	}
	if st := f.status(); st.Phase != RestoreSucceeded || !slices.Equal(st.Replaced, []string{"web-cp-2", "web-cp-1"}) {
		t.Errorf("status %+v", st)
	}
}

func TestPlanEtcdRestore(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, nil)
	snapshot := &EtcdSnapshot{Name: restoreSnapshot, KubernetesVersion: "v1.37.1+rke2r1", Distribution: "rke2", SHA256: strings.Repeat("a", 64)}
	plan, err := PlanEtcdRestore(context.Background(), f.kube, f.cluster(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 7 || !slices.Equal(plan.Replaced, []string{"web-cp-1", "web-cp-2"}) {
		t.Fatalf("plan %+v", plan)
	}
	for i, want := range []string{`"paused":true`, "systemctl stop rke2-server", strings.Repeat("a", 64), "--cluster-reset", "systemctl start", "delete machine --wait=false web-cp-1 web-cp-2", `"paused":false`} {
		if !strings.Contains(plan.Steps[i].Command, want) {
			t.Errorf("step %d %q has no %q:\n%s", i, plan.Steps[i].Title, want, plan.Steps[i].Command)
		}
	}
	if plan.Steps[1].Where != "web-cp-0, web-cp-1, web-cp-2" || plan.Steps[3].Where != "web-cp-0" {
		t.Errorf("stopped on %q, restored on %q", plan.Steps[1].Where, plan.Steps[3].Where)
	}
	other := *snapshot
	other.KubernetesVersion = "v1.36.4+rke2r1"
	if _, err := PlanEtcdRestore(context.Background(), f.kube, f.cluster(), &other); err == nil {
		t.Error("planned a restore across a Kubernetes minor")
	}
}

func TestRestorable(t *testing.T) {
	t.Parallel()
	c := etcdCluster(`{}`)
	for _, tc := range []struct {
		s  EtcdSnapshot
		ok bool
	}{
		{EtcdSnapshot{Name: "s", Distribution: "rke2", KubernetesVersion: "v1.37.4+rke2r2"}, true},
		{EtcdSnapshot{Name: "s"}, true},
		{EtcdSnapshot{Name: "s", Distribution: "k3s", KubernetesVersion: "v1.37.1+k3s1"}, false},
		{EtcdSnapshot{Name: "s", Distribution: "rke2", KubernetesVersion: "v1.38.0+rke2r1"}, false},
	} {
		if err := Restorable(c, &tc.s); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.s, err)
		}
	}
}

// Every distribution's scripts are bash that parses, keep their input to themselves, and do what
// their distribution restores with; Talos has none.
func TestRestoreScripts(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	// The bash found first on that system may be a launcher for another one rather than a shell.
	check := err == nil && runtime.GOOS != "windows"
	for d, want := range map[templates.Distribution]string{ //nolint:exhaustive // Talos is refused, below
		templates.Kubeadm: "etcdutl snapshot restore",
		templates.RKE2:    "rke2 server --cluster-reset",
		templates.K3s:     "k3s server --cluster-reset",
	} {
		s, err := restoreScriptsFor(d)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(s.Restore, want) {
			t.Errorf("%s restores without %q", d, want)
		}
		for name, body := range map[string]string{
			"stop": s.Stop, "restore": s.Restore, "start": s.Start, "start without waiting": s.StartNoWait,
			"restored": s.Restored, "clean": s.Clean, "fetch": fetchScriptFor("https://s3.example.org/x?a=1&b='2'", "pass'word", "abc"),
		} {
			if !strings.HasSuffix(body, "} </dev/null\n") {
				t.Errorf("%s %s reads its standard input", d, name)
			}
			if check {
				cmd := exec.CommandContext(t.Context(), bash, "-n") //nolint:gosec // the bash found on the path, only parsing
				cmd.Stdin = strings.NewReader(body)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("%s %s does not parse: %v\n%s", d, name, err, out)
				}
			}
		}
	}
	if _, err := restoreScriptsFor(templates.Talos); err == nil {
		t.Error("offered a restore for Talos")
	}
	if got := script("echo\n", "password", "it's"); !strings.HasPrefix(got, `password='it'\''s'`+"\n") {
		t.Errorf("quoted %q", got)
	}
}

// opensslOpen decrypts what opensslSeal encrypted, as "openssl enc -d" does.
func opensslOpen(data []byte, password string) ([]byte, error) {
	header := len(opensslMagic) + opensslSaltSize
	if len(data) < header || string(data[:len(opensslMagic)]) != opensslMagic {
		return nil, errors.New("no openssl header")
	}
	stream, err := opensslStream(password, data[len(opensslMagic):header])
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data)-header)
	stream.XORKeyStream(out, data[header:])
	return out, nil
}

// The node decrypts the transcoded snapshot with openssl, which has to read what opensslSeal
// wrote.
func TestOpensslSealIsWhatOpensslDecrypts(t *testing.T) {
	t.Parallel()
	plain := etcdData(100 << 10)
	password, err := newRestorePassword()
	if err != nil {
		t.Fatal(err)
	}
	r, err := opensslSeal(bytes.NewReader(plain), password)
	if err != nil {
		t.Fatal(err)
	}
	sealedData, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := opensslOpen(sealedData, password)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("does not decrypt: %v", err)
	}
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Log("no openssl here; the Go decryption above stands in for it")
		return
	}
	in := t.TempDir() + "/snapshot.enc"
	if err = os.WriteFile(in, sealedData, 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // the openssl found on the path, on the test's own file
	cmd := exec.CommandContext(t.Context(), openssl, "enc", "-d", "-aes-256-ctr", "-pbkdf2", "-iter", strconv.Itoa(opensslIterations), "-md", "sha256",
		"-pass", "env:KUBEHENGE_PASSWORD", "-in", in)
	cmd.Env = append(os.Environ(), "KUBEHENGE_PASSWORD="+password)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	got, err := cmd.Output()
	if err != nil || !bytes.Equal(got, plain) {
		t.Errorf("openssl decrypted %d of %d bytes: %v %s", len(got), len(plain), err, stderr.String())
	}
}
