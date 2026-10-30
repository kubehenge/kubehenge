package provisioner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// newTalosRestoreFixture is newRestoreFixture with web a Talos cluster, its control-plane nodes at
// 10.0.0.11 to 10.0.0.13.
func newTalosRestoreFixture(t *testing.T, change func(*clusterv1.Cluster)) (*restoreFixture, *fakeTalos) {
	t.Helper()
	f := newRestoreFixture(t, func(c *clusterv1.Cluster) {
		c.Spec.Topology.ClassRef.Name = ClassPrefix + "talos"
		if change != nil {
			change(c)
		}
	})
	class := &clusterv1.ClusterClass{
		Name: ClassPrefix + "talos", Namespace: "kubehenge-system",
		Annotations: map[string]string{BackupTargetsAnnotation: testTargets},
	}
	if err := f.kube.Create(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	talos := &fakeTalos{down: map[string]bool{}}
	f.r.Guests, f.r.Talos, f.r.poll = nil, talos.dial, time.Millisecond
	return f, talos
}

var talosWipes = []string{"10.0.0.11 wipe", "10.0.0.12 wipe", "10.0.0.13 wipe"}

// A Talos restore wipes etcd on every control-plane node through the Talos API, bootstraps the first
// from the snapshot, and replaces no machine: the others join it again.
func TestTalosEtcdRestore(t *testing.T) {
	t.Parallel()
	f, talos := newTalosRestoreFixture(t, nil)
	talos.fail = func(address, call string) error {
		if c := f.cluster(); call != "boot" && (c.Spec.Paused == nil || !*c.Spec.Paused) {
			t.Errorf("%s on %s with the cluster not paused", call, address)
		}
		return nil
	}
	f.reconcile()

	want := append(slices.Clone(talosWipes), "10.0.0.11 upload", "10.0.0.11 bootstrap")
	if got := talos.restoreCalls(); !slices.Equal(got, want) {
		t.Errorf("called %v, want %v", got, want)
	}
	if !bytes.Equal(talos.uploaded["10.0.0.11"], f.data) {
		t.Error("the node was not handed the snapshot")
	}
	st := f.status()
	if st.Phase != RestoreSucceeded || st.Machine != "web-cp-0" || len(st.Replaced) != 0 || !st.Paused ||
		!strings.Contains(st.Message, "other 2 control-plane nodes join it again") {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); len(got) != 0 {
		t.Errorf("deleting %v", got)
	}
}

// A node whose Talos API does not answer is left out, and its machine made again.
func TestTalosEtcdRestoreReplacesANodeThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	f, talos := newTalosRestoreFixture(t, nil)
	talos.down["10.0.0.13"] = true
	f.reconcile()
	for _, c := range talos.restoreCalls() {
		if strings.HasPrefix(c, "10.0.0.13 ") {
			t.Errorf("called %s on a node that does not answer", c)
		}
	}
	if st := f.status(); st.Phase != RestoreSucceeded || !slices.Equal(st.Replaced, []string{"web-cp-2"}) {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); !slices.Equal(got, []string{"web-cp-2"}) {
		t.Errorf("deleting %v", got)
	}
}

// A node that is not ready yet is asked again; any other failure once etcd may be wiped tries the
// whole restore once more.
func TestTalosEtcdRestoreTriesAgain(t *testing.T) {
	t.Parallel()
	errNotEmpty := errors.New("etcd data directory is not empty")
	retried := append(append(slices.Clone(talosWipes), "10.0.0.11 upload", "10.0.0.11 bootstrap"), talosWipes...)
	for _, tc := range []struct {
		name     string
		failures int
		err      error
		want     []string
		phase    RestorePhase
		message  string
	}{
		{"a node still booting", 2, errTalosNotReady, nil, RestoreSucceeded, "join it again"},
		{"once", 1, errNotEmpty, retried, RestoreSucceeded, "The first attempt failed"},
		{"for good", 100, errNotEmpty, nil, RestoreFailed, "may be wiped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, talos := newTalosRestoreFixture(t, nil)
			failed := 0
			talos.fail = func(_, call string) error {
				if call != "bootstrap" || failed >= tc.failures {
					return nil
				}
				failed++
				return tc.err
			}
			f.reconcile()
			if got := talos.restoreCalls(); tc.want != nil && (len(got) < len(tc.want) || !slices.Equal(got[:len(tc.want)], tc.want)) {
				t.Errorf("called %v, want it to start %v", got, tc.want)
			}
			if st := f.status(); st.Phase != tc.phase || !strings.Contains(st.Message, tc.message) {
				t.Errorf("status %+v, want %s naming %q", st, tc.phase, tc.message)
			}
			if got := f.deleting(); len(got) != 0 {
				t.Errorf("deleting %v", got)
			}
		})
	}
}

func TestTalosEtcdRestoreRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		change     func(*restoreFixture, *fakeTalos)
	}{
		{"without the Talos API", "does not reach Talos nodes", func(f *restoreFixture, _ *fakeTalos) { f.r.Talos = nil }},
		{"no node answering", "no control-plane node", func(_ *restoreFixture, talos *fakeTalos) {
			talos.down = map[string]bool{"10.0.0.11": true, "10.0.0.12": true, "10.0.0.13": true}
		}},
		{"a snapshot that does not match its checksum", "does not match its checksum", func(f *restoreFixture, _ *fakeTalos) {
			key := snapshotFolder + "/" + restoreSnapshot
			meta := f.store.objects[key].metadata
			meta[SnapshotMetaSHA256] = strings.Repeat("0", 64)
			if err := f.store.SetMetadata(context.Background(), key, meta); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, talos := newTalosRestoreFixture(t, nil)
			tc.change(f, talos)
			f.reconcile()
			if calls := talos.restoreCalls(); len(calls) != 0 {
				t.Errorf("called %v", calls)
			}
			if st := f.status(); st.Phase != RestoreFailed || !strings.Contains(st.Message, tc.want) {
				t.Errorf("status %+v, want a message naming %q", st, tc.want)
			}
		})
	}
}

func TestPlanTalosEtcdRestore(t *testing.T) {
	t.Parallel()
	f, _ := newTalosRestoreFixture(t, nil)
	snapshot := &EtcdSnapshot{Name: restoreSnapshot, KubernetesVersion: "v1.37.1", Distribution: "talos", SHA256: strings.Repeat("a", 64)}
	plan, err := PlanEtcdRestore(context.Background(), f.kube, f.cluster(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	commands := make([]string, len(plan.Steps))
	for i, s := range plan.Steps {
		commands[i] = s.Command
	}
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		`'{"spec":{"paused":true}}'`,
		"get secret web-talosconfig --output jsonpath='{.data.talosconfig}'",
		strings.Repeat("a", 64) + "  db.snapshot",
		"--nodes 10.0.0.11,10.0.0.12,10.0.0.13 reset --graceful=false --reboot --system-labels-to-wipe=EPHEMERAL",
		"--nodes 10.0.0.11 bootstrap --recover-from=db.snapshot",
		`'{"spec":{"paused":false}}'`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no step runs %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "sudo") || len(plan.Replaced) != 0 {
		t.Errorf("a Talos node has no shell, and none is replaced:\n%s", joined)
	}
}

// A controller that stopped during a Talos restore restores once more if etcd may be wiped, and
// waits for etcd if it was restored.
func TestTalosEtcdRestoreInterrupted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		phase   RestorePhase
		want    []string
		message string
	}{
		{RestoreRestoring, append(slices.Clone(talosWipes), "10.0.0.11 upload", "10.0.0.11 bootstrap"), "The first attempt failed, the controller restarted"},
		{RestoreStarting, nil, "restarted"},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			t.Parallel()
			f, talos := newTalosRestoreFixture(t, func(c *clusterv1.Cluster) {
				c.Spec.Paused = new(true)
				metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdRestoreStatusAnnotation, fmt.Sprintf(
					`{"snapshot":%q,"requestedAt":%q,"phase":%q,"machine":"web-cp-0","started":%q,"paused":true}`,
					restoreSnapshot, restoreAsked.Format(time.RFC3339), tc.phase, restoreAsked.Format(time.RFC3339)))
			})
			f.reconcile()
			if got := talos.restoreCalls(); !slices.Equal(got, tc.want) {
				t.Errorf("called %v, want %v", got, tc.want)
			}
			if st := f.status(); !strings.Contains(st.Message, tc.message) {
				t.Errorf("status %+v, want a message naming %q", st, tc.message)
			}
		})
	}
}
