package provisioner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// askReset asks for web's etcd to be reset to machine's own, as its roll stalled.
func askReset(machine string) func(*clusterv1.Cluster) {
	return askResetWhile(machine, RollStalledCondition)
}

// askResetWhile asks for web's etcd to be reset to machine's own, while the cluster has the
// condition. An empty condition is a healthy cluster.
func askResetWhile(machine, condition string) func(*clusterv1.Cluster) {
	return func(c *clusterv1.Cluster) {
		metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdRestoreAnnotation,
			fmt.Sprintf(`{"machine":%q,"by":"root@pam","at":%q}`, machine, restoreAsked.Format(time.RFC3339)))
		hold(c, condition)
	}
}

// hold gives the cluster the condition, when there is one.
func hold(c *clusterv1.Cluster, condition string) {
	if condition != "" {
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: condition, Status: metav1.ConditionTrue, Reason: "Test"})
	}
}

// A reset reaches the controller from kubectl as it does from the interface, so the controller
// refuses one of a cluster whose control plane is whole, before it touches anything.
func TestControlPlaneResetRefusesAHealthyCluster(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, askResetWhile("web-cp-0", ""))
	f.apis.down = 1
	f.reconcile()
	if runs := f.guests.runs(); len(runs) != 0 {
		t.Errorf("ran %v", runs)
	}
	if st := f.status(); st.Phase != RestoreFailed || !strings.Contains(st.Message, "has not stalled and it has not lost its quorum") {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); len(got) != 0 {
		t.Errorf("deleting %v", got)
	}

	tf, talos := newTalosRestoreFixture(t, askResetWhile("web-cp-0", ""))
	tf.apis.down = 1
	tf.reconcile()
	if calls := talos.restoreCalls(); len(calls) != 0 {
		t.Errorf("called %v", calls)
	}
	if st := tf.status(); st.Phase != RestoreFailed || !strings.Contains(st.Message, "has not stalled and it has not lost its quorum") {
		t.Errorf("status %+v", st)
	}
}

// A snapshot restore is not a recovery, and goes ahead on a healthy cluster.
func TestSnapshotRestoreNeedsNoCondition(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, nil)
	f.reconcile()
	if st := f.status(); st.Phase != RestoreSucceeded {
		t.Errorf("status %+v", st)
	}
}

// A reset of a cluster whose API does not answer stops every control plane, resets etcd on the
// machine kept from its own database, starts it, and replaces the others. Nothing is fetched.
func TestControlPlaneReset(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, askReset("web-cp-1"))
	f.r.poll = time.Millisecond
	f.apis.down = 1
	f.reconcile()

	want := []string{"101 ping", "102 ping", "103 ping", "102 stop", "101 stop", "103 stop", "102 reset", "102 start"}
	if got := f.guests.runs(); !slices.Equal(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
	st := f.status()
	if st.Phase != RestoreSucceeded || !st.Reset || st.Snapshot != "" || st.Machine != "web-cp-1" || st.Answered ||
		!slices.Equal(st.Replaced, []string{"web-cp-0", "web-cp-2"}) || !strings.Contains(st.Message, "Reset etcd to web-cp-1's own") {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); !slices.Equal(got, []string{"web-cp-0", "web-cp-2"}) {
		t.Errorf("deleting %v", got)
	}
	if left := f.transcoded(); len(left) != 0 {
		t.Errorf("stored %v in the target", left)
	}
}

// While the cluster's API answers, its etcd has its quorum: the other machines are only replaced,
// which removes their members, and nothing runs on any node.
func TestControlPlaneResetOnlyReplacesWhileTheAPIAnswers(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, askReset("web-cp-0"))
	f.guests.seen = func(vmid int64, step, _ string) { t.Errorf("ran %s on %d", step, vmid) }
	f.reconcile()
	st := f.status()
	if st.Phase != RestoreSucceeded || !st.Answered || st.Paused || !strings.Contains(st.Message, "nothing was reset") {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); !slices.Equal(got, []string{"web-cp-1", "web-cp-2"}) {
		t.Errorf("deleting %v", got)
	}
}

// A machine whose guest agent does not answer is not kept, and is replaced without being stopped.
// One that is kept has to answer.
func TestControlPlaneResetLeavesOutWhatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, askReset("web-cp-0"))
	f.r.poll = time.Millisecond
	f.apis.down = 1
	f.guests.fail = func(vmid int64, _ string) error {
		if vmid == 103 {
			return errors.New("guest agent is not running")
		}
		return nil
	}
	f.reconcile()
	for _, r := range f.guests.runs() {
		if r != "103 ping" && strings.HasPrefix(r, "103 ") {
			t.Errorf("ran %s on a VM whose agent does not answer", r)
		}
	}
	if st := f.status(); st.Phase != RestoreSucceeded || !slices.Contains(st.Replaced, "web-cp-2") {
		t.Errorf("status %+v", st)
	}

	f = newRestoreFixture(t, askReset("web-cp-1"))
	f.apis.down = 1
	f.guests.off[102] = true
	f.reconcile()
	if st := f.status(); st.Phase != RestoreFailed || !strings.Contains(st.Message, "web-cp-1's VM does not run") {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); len(got) != 0 {
		t.Errorf("deleting %v", got)
	}
}

func TestControlPlaneResetRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, annotation, want string }{
		{"no such machine", `{"machine":"web-worker-0","at":"2026-10-21T11:00:00Z"}`, "not one of the cluster's control-plane machines"},
		{"a machine and a snapshot", `{"machine":"web-cp-0","snapshot":"x.db.age","target":"store","at":"2026-10-21T11:00:00Z"}`, "name one"},
		{"no time", `{"machine":"web-cp-0"}`, "names no time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRestoreFixture(t, func(c *clusterv1.Cluster) {
				metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdRestoreAnnotation, tc.annotation)
				hold(c, RollStalledCondition)
			})
			f.apis.down = 1
			f.reconcile()
			if runs := f.guests.runs(); len(runs) != 0 {
				t.Errorf("ran %v", runs)
			}
			if st := f.status(); st.Phase != RestoreFailed || !strings.Contains(st.Message, tc.want) {
				t.Errorf("status %+v, want a message naming %q", st, tc.want)
			}
		})
	}
}

// A Talos reset copies the kept node's etcd through its Talos API before wiping that node alone,
// restores the copy there, and replaces the others rather than letting them join again.
func TestTalosControlPlaneReset(t *testing.T) {
	t.Parallel()
	f, talos := newTalosRestoreFixture(t, askReset("web-cp-1"))
	f.apis.down = 1
	f.reconcile()
	want := []string{"10.0.0.12 snapshot", "10.0.0.12 wipe", "10.0.0.12 upload", "10.0.0.12 bootstrap"}
	if got := talos.restoreCalls(); !slices.Equal(got, want) {
		t.Errorf("called %v, want %v", got, want)
	}
	if string(talos.uploaded["10.0.0.12"]) != "snapshot" {
		t.Errorf("uploaded %q, not the node's own etcd", talos.uploaded["10.0.0.12"])
	}
	st := f.status()
	if st.Phase != RestoreSucceeded || !slices.Equal(st.Replaced, []string{"web-cp-0", "web-cp-2"}) {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); !slices.Equal(got, []string{"web-cp-0", "web-cp-2"}) {
		t.Errorf("deleting %v", got)
	}
}

func TestPlanControlPlaneReset(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, nil)
	plan, err := PlanControlPlaneReset(context.Background(), f.kube, f.cluster(), "web-cp-1")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Snapshot != nil || !slices.Equal(plan.Replaced, []string{"web-cp-0", "web-cp-2"}) || len(plan.Steps) != 6 {
		t.Fatalf("plan %+v", plan)
	}
	for i, want := range []string{`"paused":true`, "systemctl stop rke2-server", "rke2 server --cluster-reset --etcd-s3=false", "systemctl start", "delete machine --wait=false web-cp-0 web-cp-2", `"paused":false`} {
		if !strings.Contains(plan.Steps[i].Command, want) {
			t.Errorf("step %d %q has no %q:\n%s", i, plan.Steps[i].Title, want, plan.Steps[i].Command)
		}
	}
	if plan.Steps[2].Where != "web-cp-1" || strings.Contains(plan.Steps[2].Command, "restore-path") {
		t.Errorf("reset on %q:\n%s", plan.Steps[2].Where, plan.Steps[2].Command)
	}
	if _, err = PlanControlPlaneReset(context.Background(), f.kube, f.cluster(), "web-worker-0"); err == nil {
		t.Error("planned to keep a machine that is no control plane")
	}

	tf, _ := newTalosRestoreFixture(t, nil)
	plan, err = PlanControlPlaneReset(context.Background(), tf.kube, tf.cluster(), "web-cp-0")
	if err != nil {
		t.Fatal(err)
	}
	commands := make([]string, len(plan.Steps))
	for i, s := range plan.Steps {
		commands[i] = s.Command
	}
	all := strings.Join(commands, "\n")
	for _, want := range []string{"--nodes 10.0.0.11 etcd snapshot db.snapshot", "--nodes 10.0.0.11 reset", "bootstrap --recover-from=db.snapshot", "delete machine --wait=false web-cp-1 web-cp-2"} {
		if !strings.Contains(all, want) {
			t.Errorf("the Talos plan has no %q:\n%s", want, all)
		}
	}
}

// Two of three control planes lost: the API is silent, the lost machines' VMs are off or gone, and
// the reset keeps the survivor and replaces the others, whichever distribution runs on them.
func TestControlPlaneResetAfterQuorumLoss(t *testing.T) {
	t.Parallel()
	for _, class := range []string{"kubeadm", "rke2", "k3s"} {
		for name, lose := range map[string]func(*fakeGuests){
			"stopped":   func(g *fakeGuests) { g.off[101], g.off[102] = true, true },
			"destroyed": func(g *fakeGuests) { g.destroyed = map[int64]bool{101: true, 102: true} },
		} {
			t.Run(class+" "+name, func(t *testing.T) {
				t.Parallel()
				f := newRestoreFixture(t, func(c *clusterv1.Cluster) {
					c.Spec.Topology.ClassRef.Name = ClassPrefix + class
					askResetWhile("web-cp-2", QuorumLostCondition)(c)
				})
				f.r.poll = time.Millisecond
				f.apis.down = 1
				lose(f.guests)
				f.reconcile()

				want := []string{"103 ping", "103 stop", "103 reset", "103 start"}
				if got := f.guests.runs(); !slices.Equal(got, want) {
					t.Errorf("ran %v, want %v", got, want)
				}
				st := f.status()
				if st.Phase != RestoreSucceeded || !st.Reset || st.Machine != "web-cp-2" || !slices.Equal(st.Replaced, []string{"web-cp-0", "web-cp-1"}) {
					t.Errorf("status %+v", st)
				}
				if got := f.deleting(); !slices.Equal(got, []string{"web-cp-0", "web-cp-1"}) {
					t.Errorf("deleting %v", got)
				}
			})
		}
	}
}

// The reset cannot keep a machine whose VM is gone.
func TestControlPlaneResetAfterQuorumLossNeedsASurvivor(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t, askResetWhile("web-cp-0", QuorumLostCondition))
	f.apis.down = 1
	f.guests.destroyed = map[int64]bool{101: true, 102: true}
	f.reconcile()
	if st := f.status(); st.Phase != RestoreFailed || !strings.Contains(st.Message, "web-cp-0's VM does not run") {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); len(got) != 0 {
		t.Errorf("deleting %v", got)
	}
}

// A Talos reset after two of three nodes are lost keeps the survivor's own etcd, as the other
// distributions' does.
func TestTalosControlPlaneResetAfterQuorumLoss(t *testing.T) {
	t.Parallel()
	f, talos := newTalosRestoreFixture(t, askResetWhile("web-cp-2", QuorumLostCondition))
	f.apis.down = 1
	talos.down["10.0.0.11"], talos.down["10.0.0.12"] = true, true
	f.reconcile()
	want := []string{"10.0.0.13 snapshot", "10.0.0.13 wipe", "10.0.0.13 upload", "10.0.0.13 bootstrap"}
	if got := talos.restoreCalls(); !slices.Equal(got, want) {
		t.Errorf("called %v, want %v", got, want)
	}
	st := f.status()
	if st.Phase != RestoreSucceeded || !st.Reset || st.Machine != "web-cp-2" || !slices.Equal(st.Replaced, []string{"web-cp-0", "web-cp-1"}) {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); !slices.Equal(got, []string{"web-cp-0", "web-cp-1"}) {
		t.Errorf("deleting %v", got)
	}
}

// While the cluster's API answers, a Talos reset only replaces the other machines.
func TestTalosControlPlaneResetOnlyReplacesWhileTheAPIAnswers(t *testing.T) {
	t.Parallel()
	f, talos := newTalosRestoreFixture(t, askReset("web-cp-0"))
	f.reconcile()
	if calls := talos.restoreCalls(); len(calls) != 0 {
		t.Errorf("called %v", calls)
	}
	st := f.status()
	if st.Phase != RestoreSucceeded || !st.Answered || !strings.Contains(st.Message, "nothing was reset") {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); !slices.Equal(got, []string{"web-cp-1", "web-cp-2"}) {
		t.Errorf("deleting %v", got)
	}
}

// A Talos node that does not answer is not kept, and one that is kept has to.
func TestTalosControlPlaneResetLeavesOutWhatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	f, talos := newTalosRestoreFixture(t, askReset("web-cp-0"))
	f.apis.down = 1
	talos.down["10.0.0.13"] = true
	f.reconcile()
	for _, c := range talos.restoreCalls() {
		if strings.HasPrefix(c, "10.0.0.13 ") {
			t.Errorf("called %s on a node that does not answer", c)
		}
	}
	if st := f.status(); st.Phase != RestoreSucceeded || !slices.Contains(st.Replaced, "web-cp-2") {
		t.Errorf("status %+v", st)
	}

	f, talos = newTalosRestoreFixture(t, askReset("web-cp-1"))
	f.apis.down = 1
	talos.down["10.0.0.12"] = true
	f.reconcile()
	if calls := talos.restoreCalls(); len(calls) != 0 {
		t.Errorf("called %v", calls)
	}
	if st := f.status(); st.Phase != RestoreFailed {
		t.Errorf("status %+v", st)
	}
	if got := f.deleting(); len(got) != 0 {
		t.Errorf("deleting %v", got)
	}
}

func TestTalosControlPlaneResetRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, annotation, want string }{
		{"no such machine", `{"machine":"web-worker-0","at":"2026-10-21T11:00:00Z"}`, "not one of the cluster's control-plane machines"},
		{"a machine and a snapshot", `{"machine":"web-cp-0","snapshot":"x.db.age","target":"store","at":"2026-10-21T11:00:00Z"}`, "name one"},
		{"no time", `{"machine":"web-cp-0"}`, "names no time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, talos := newTalosRestoreFixture(t, func(c *clusterv1.Cluster) {
				metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdRestoreAnnotation, tc.annotation)
				hold(c, RollStalledCondition)
			})
			f.apis.down = 1
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
