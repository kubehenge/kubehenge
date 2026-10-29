package ui

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestRunsListWhatRunsAndWhatFailedLately(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	stamp := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	one := cluster("default", "one")
	one.Annotations = map[string]string{
		provisioner.EtcdRestoreAnnotation: "20261012T060000Z-v1.36.5-rke2r1.db.age",
		provisioner.EtcdRestoreStatusAnnotation: stamp(provisioner.EtcdRestoreStatus{
			Snapshot: "20261012T060000Z-v1.36.5-rke2r1.db.age", By: "dev@pve", RequestedAt: now.Add(-3 * time.Minute),
			Started: now.Add(-2 * time.Minute), Phase: provisioner.RestoreFetching,
		}),
	}
	one.Status.Conditions = []metav1.Condition{{
		Type: provisioner.EtcdSnapshotCondition, Status: metav1.ConditionFalse, Reason: "UploadFailed",
		Message: "the target refused the upload", LastTransitionTime: metav1.NewTime(now.Add(-2 * time.Hour)),
	}}
	finished := now.Add(-30 * time.Minute)
	two := cluster("default", "two")
	two.Annotations = map[string]string{
		// A snapshot of three hours ago is long done, and leaves the list.
		provisioner.EtcdSnapshotAnnotation: stamp(provisioner.EtcdSnapshot{Name: "old.db.age", Time: now.Add(-3 * time.Hour)}),
		provisioner.EtcdRestoreStatusAnnotation: stamp(provisioner.EtcdRestoreStatus{
			Snapshot: "x.db.age", RequestedAt: now.Add(-40 * time.Minute), Started: now.Add(-40 * time.Minute),
			Phase: provisioner.RestoreSucceeded, Finished: &finished,
		}),
	}
	h := newHarness(t, one, two)
	spool := t.TempDir()
	h.server.Upgrades = &Upgrades{Spool: appliance.Spool(spool), Current: "0.1.0"}
	failed := appliance.UpdateStatus{State: "failed", From: "0.1.0", To: "0.1.1", Time: now.Add(-time.Hour), Error: "the release's checksum does not match"}
	if err := os.WriteFile(appliance.Spool(spool).StatusPath(), []byte(stamp(failed)), 0o600); err != nil {
		t.Fatal(err)
	}
	runsOf := func(user string) []runView {
		t.Helper()
		b := h.browser()
		b.signIn(user)
		rec := b.do("GET", "/api/runs", "")
		expect(t, rec, http.StatusOK)
		var v []runView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	ids := func(list []runView) []string {
		out := make([]string, 0, len(list))
		for _, v := range list {
			out = append(out, v.ID+" "+v.State)
		}
		return out
	}

	admin := runsOf("admin@pve")
	want := []string{
		"etcd-restore/default/one running",
		"appliance-upgrade failed",
		"etcd-snapshot/default/one failed",
		"etcd-restore/default/two succeeded",
	}
	if got := ids(admin); !slices.Equal(got, want) {
		t.Fatalf("admin's runs = %q, want %q", got, want)
	}
	if r := admin[0]; r.Phase != "Fetching" || r.By != "dev@pve" || r.Cluster != "one" || !r.Started.Equal(now.Add(-2*time.Minute)) {
		t.Errorf("restore %+v", r)
	}
	if got := stepStates(&admin[0]); !slices.Equal(got, []string{
		"Preparing done ", "Fetching running ", "Stopping waiting ", "Restoring waiting ", "Starting waiting ", "Replacing waiting ",
	}) {
		t.Errorf("restore steps %q", got)
	}
	if r := admin[1]; r.Error != failed.Error || r.What != "Upgrade the appliance to 0.1.1" {
		t.Errorf("upgrade %+v", r)
	}
	if r := admin[2]; r.Error != "the target refused the upload" {
		t.Errorf("snapshot %+v", r)
	}

	// The appliance's work is for those who may set it up, and other clusters' for those who see them.
	if got := ids(runsOf("viewer@pve")); !slices.Equal(got, []string{"etcd-restore/default/one running", "etcd-snapshot/default/one failed"}) {
		t.Errorf("viewer's runs = %q", got)
	}

	// A day later the failure has waited long enough.
	h.clock = now.Add(26 * time.Hour)
	if got := ids(runsOf("admin@pve")); !slices.Equal(got, []string{"etcd-restore/default/one running"}) {
		t.Errorf("a day later = %q", got)
	}
}

func TestVeleroStates(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		phase, failure string
		errs           int64
		state, problem string
	}{
		{"InProgress", "", 0, runRunning, ""},
		{"New", "", 0, runRunning, ""},
		{"Completed", "", 0, runSucceeded, ""},
		{"Failed", "the location is unavailable", 0, runFailed, "the location is unavailable"},
		{"PartiallyFailed", "", 3, runFailed, "3 errors; Velero's log of it has them"},
		{"FailedValidation", "", 0, runFailed, "FailedValidation"},
		{"Deleting", "", 0, "", ""},
	} {
		if state, problem := veleroState(c.phase, c.failure, c.errs); state != c.state || problem != c.problem {
			t.Errorf("%s: %q %q, want %q %q", c.phase, state, problem, c.state, c.problem)
		}
	}
}

func TestAFailedRestoreShowsThePhaseItFailedIn(t *testing.T) {
	t.Parallel()
	st := provisioner.EtcdRestoreStatus{Phase: provisioner.RestoreFailed, FailedIn: provisioner.RestoreRestoring}
	v := runView{Steps: restoreSteps(&st, runFailed)}
	want := []string{"Preparing done ", "Fetching done ", "Stopping done ", "Restoring failed ", "Starting waiting ", "Replacing waiting "}
	if got := stepStates(&v); !slices.Equal(got, want) {
		t.Errorf("steps = %q, want %q", got, want)
	}
}

func TestARestoreNamesTheSnapshotOfEtcdAsItWas(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status provisioner.EtcdRestoreStatus
		want   string
	}{
		{provisioner.EtcdRestoreStatus{Phase: provisioner.RestoreFetching, Before: "20261021T120000Z-v1.37.1-rke2r1.db.age"}, "Preparing done etcd as it was saved as 20261021T120000Z-v1.37.1-rke2r1.db.age"},
		{provisioner.EtcdRestoreStatus{Phase: provisioner.RestoreFetching, NoBefore: "reaching etcd: no leader"}, "Preparing done no snapshot of etcd as it was: reaching etcd: no leader"},
		{provisioner.EtcdRestoreStatus{Phase: provisioner.RestorePreparing}, "Preparing running "},
	} {
		v := runView{Steps: restoreSteps(&c.status, runRunning)}
		if got := stepStates(&v)[0]; got != c.want {
			t.Errorf("first step %q, want %q", got, c.want)
		}
	}
}
