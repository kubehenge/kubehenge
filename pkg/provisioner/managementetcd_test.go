package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestManagementFolders(t *testing.T) {
	t.Parallel()
	if got := ManagementEtcdFolder("kubehenge"); got != "kubehenge/management/etcd" {
		t.Errorf("folder %s", got)
	}
	if got := ManagementEtcdIdentity(""); got != "management/identity.age" {
		t.Errorf("identity %s", got)
	}
	// A cluster's folder holds a dot, so none is ever the management cluster's.
	if strings.HasPrefix(EtcdSnapshotPrefix("kubehenge", "management", "etcd"), ManagementEtcdFolder("kubehenge")) {
		t.Error("a cluster's folder reaches the management cluster's")
	}
}

func TestTalosEndpoints(t *testing.T) {
	t.Parallel()
	got, err := TalosEndpoints([]byte("context: m\ncontexts:\n  m:\n    endpoints: [10.0.0.21, 10.0.0.22]\n"))
	if err != nil || !slices.Equal(got, []string{"10.0.0.21", "10.0.0.22"}) {
		t.Errorf("%v, %v", got, err)
	}
	if _, err := TalosEndpoints([]byte("context: m\ncontexts:\n  m: {}\n")); err == nil {
		t.Error("a talosconfig without endpoints")
	}
}

// The management cluster's etcd is restored as a Talos cluster's: every node that answers is
// wiped, the first gets the snapshot and is bootstrapped from it. A node that does not answer is
// left out.
func TestManagementTalosRestore(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{down: map[string]bool{"10.0.0.23": true}}
	m := &ManagementTalos{Dial: f.dial, Poll: time.Millisecond, StepTimeout: 5 * time.Second}
	var log []string
	open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("etcd database")), nil }
	err := m.Restore(context.Background(), []byte("talosconfig of web"), []string{"10.0.0.21", "10.0.0.22", "10.0.0.23"}, open,
		func(line string) { log = append(log, line) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.21 wipe", "10.0.0.22 wipe", "10.0.0.21 upload", "10.0.0.21 bootstrap"}
	if got := f.restoreCalls(); !slices.Equal(got, want) {
		t.Errorf("calls %v\nwant %v", got, want)
	}
	if string(f.uploaded["10.0.0.21"]) != "etcd database" {
		t.Errorf("uploaded %q", f.uploaded["10.0.0.21"])
	}
	if !strings.HasPrefix(log[0], "Leaving 10.0.0.23 out") {
		t.Errorf("log %q", log)
	}
	f.down["10.0.0.21"], f.down["10.0.0.22"] = true, true
	if err := m.Restore(context.Background(), []byte("talosconfig of web"), []string{"10.0.0.21", "10.0.0.22", "10.0.0.23"}, open, func(string) {}); err == nil {
		t.Error("restored with no node answering")
	}
}

// restoreManagement restores the management cluster of nodes 10.0.0.21 and 10.0.0.22 through f, a
// step timing out after stepTimeout, and returns its log and error.
func restoreManagement(f *fakeTalos, stepTimeout time.Duration) ([]string, error) {
	m := &ManagementTalos{Dial: f.dial, Poll: time.Millisecond, StepTimeout: stepTimeout}
	var log []string
	open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("etcd database")), nil }
	err := m.Restore(context.Background(), []byte("talosconfig of web"), []string{"10.0.0.21", "10.0.0.22"}, open,
		func(line string) { log = append(log, line) })
	return log, err
}

// Talos leaving the management cluster's etcd Failed after the bootstrap gets the first node the
// snapshot again and bootstraps it once more.
func TestManagementTalosRestoreStartsAFailedEtcdAgain(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{starting: func(bootstraps int) (string, bool) {
		if bootstraps < 2 {
			return talosEtcdFailed, false
		}
		return talosEtcdRunning, true
	}}
	log, err := restoreManagement(f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.21 wipe", "10.0.0.22 wipe", "10.0.0.21 upload", "10.0.0.21 bootstrap", "10.0.0.21 upload", "10.0.0.21 bootstrap"}
	if got := f.restoreCalls(); !slices.Equal(got, want) {
		t.Errorf("calls %v\nwant %v", got, want)
	}
	if string(f.uploaded["10.0.0.21"]) != "etcd database" {
		t.Errorf("uploaded %q", f.uploaded["10.0.0.21"])
	}
	if !slices.Contains(log, "Starting etcd again on 10.0.0.21 after Talos left it Failed") {
		t.Errorf("log %q", log)
	}
}

// An etcd Talos left Failed before the upload, as when its first image pull after the wipe failed,
// takes the snapshot and the bootstrap as one waiting in Preparing does.
func TestManagementTalosRestoreUploadsToAFailedEtcd(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{pullFailed: map[string]bool{"10.0.0.21": true}}
	if _, err := restoreManagement(f, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.21 wipe", "10.0.0.22 wipe", "10.0.0.21 upload", "10.0.0.21 bootstrap"}
	if got := f.restoreCalls(); !slices.Equal(got, want) {
		t.Errorf("calls %v\nwant %v", got, want)
	}
}

// The management cluster's restore waits for each other node to join, and reboots one whose etcd
// Talos left Failed rather than bootstrapping it.
func TestManagementTalosRestoreRebootsAJoinerLeftFailed(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{pullFailed: map[string]bool{"10.0.0.22": true}}
	log, err := restoreManagement(f, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.rebooted, []string{"10.0.0.22"}) || slices.Contains(f.calls, "10.0.0.22 bootstrap") {
		t.Errorf("rebooted %v, calls %v", f.rebooted, f.calls)
	}
	if !slices.Contains(log, "Rebooting 10.0.0.22 after Talos left its etcd Failed") {
		t.Errorf("log %q", log)
	}
}

// An etcd that stays Failed is bootstrapped again at most once a poll, until the step's time is
// up. The error then says it stayed Failed, and why as Talos says.
func TestManagementTalosRestoreGivesUpOnAFailedEtcd(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{starting: func(int) (string, bool) { return talosEtcdFailed, false }}
	_, err := restoreManagement(f, 300*time.Millisecond)
	want := fmt.Sprintf("starting etcd on 10.0.0.21 did not finish: etcd stayed Failed after starting it again, Talos says %q", fakePullFailure)
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("error %v, want one ending on %q", err, want)
	}
	// Every read of etcd after the first bootstrap found it Failed, and each led to one bootstrap.
	reads, bootstraps := 0, 0
	for _, c := range f.calls {
		switch {
		case c == "10.0.0.21 bootstrap":
			bootstraps++
		case c == "10.0.0.21 etcd" && bootstraps > 0:
			reads++
		}
	}
	if bootstraps < 3 || bootstraps-1 > reads {
		t.Errorf("%d bootstraps for %d reads of a Failed etcd", bootstraps, reads)
	}
}

// A re-upload a node is not ready for is tried again on the next poll. Any other failure of it
// ends the step.
func TestManagementTalosRestoreRetriesStartingEtcdAgain(t *testing.T) {
	t.Parallel()
	failedOnce := func(bootstraps int) (string, bool) {
		if bootstraps < 2 {
			return talosEtcdFailed, false
		}
		return talosEtcdRunning, true
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"not ready", errTalosNotReady, ""},
		{"refused", errors.New("permission denied"), "starting etcd: 10.0.0.21: permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeTalos{starting: failedOnce}
			uploads := 0
			f.fail = func(_, call string) error {
				if call != "upload" {
					return nil
				}
				uploads++
				if uploads == 2 {
					return tc.err
				}
				return nil
			}
			_, err := restoreManagement(f, 5*time.Second)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || err.Error() != tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if tc.want == "" && uploads != 3 {
				t.Errorf("%d uploads, want the failed one tried again", uploads)
			}
		})
	}
}

var (
	firstFailed  = TalosService{State: talosEtcdFailed, Event: fakePullFailure}
	firstRunning = TalosService{State: talosEtcdRunning, Healthy: true}
)

// firstStart has StartFirstEtcd look once at the management nodes 10.0.0.21, the one bootstrapped,
// and 10.0.0.22 through f, with starts, and returns its log.
func firstStart(f *fakeTalos, starts map[string]EtcdStart, record func() error) []string {
	return firstStartIn(context.Background(), f, starts, record)
}

// firstStartIn is firstStart in ctx.
func firstStartIn(ctx context.Context, f *fakeTalos, starts map[string]EtcdStart, record func() error) []string {
	if record == nil {
		record = func() error { return nil }
	}
	var log []string
	m := &ManagementTalos{Dial: f.dial}
	m.StartFirstEtcd(ctx, []byte("talosconfig of web"), []string{"10.0.0.21", "10.0.0.22"}, starts, record,
		func(line string) { log = append(log, line) })
	return log
}

// The first node's etcd Talos left Failed before any etcd ran is bootstrapped again, counted
// before the bootstrap, and the log names the node and Talos's reason.
func TestStartFirstEtcdBootstrapsTheFirstNode(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": firstFailed, "10.0.0.22": {State: talosEtcdPreparing}}}
	starts := map[string]EtcdStart{}
	var counted []string
	log := firstStart(f, starts, func() error {
		counted = append(counted, fmt.Sprintf("%+v after %v", starts["10.0.0.21"], f.restoreCalls()))
		return nil
	})
	if got := f.restoreCalls(); !slices.Equal(got, []string{"10.0.0.21 plain bootstrap"}) || len(f.rebooted) != 0 {
		t.Errorf("calls %v, rebooted %v", got, f.rebooted)
	}
	if want := []string{"{Boot: Bootstrapped: Repairs:1 GaveUp:false Ran:false} after []"}; !slices.Equal(counted, want) {
		t.Errorf("recorded %q, want %q", counted, want)
	}
	if want := []string{"Talos left etcd on 10.0.0.21 Failed: " + fakePullFailure + ". Bootstrapped it again."}; !slices.Equal(log, want) {
		t.Errorf("log %q, want %q", log, want)
	}
}

// A bootstrap Talos refuses, as for an etcd that has data, is a reboot instead. One the node is not
// ready for is no start, and is made on a later look.
func TestStartFirstEtcdOnARefusedBootstrap(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{"refused": errors.New("etcd data directory is not empty"), "not ready": errTalosNotReady} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": firstFailed, "10.0.0.22": {State: talosEtcdPreparing}}, fail: func(_, call string) error {
				if call == "plain bootstrap" {
					return refusal
				}
				return nil
			}}
			starts := map[string]EtcdStart{}
			log := firstStart(f, starts, nil)
			if errors.Is(refusal, errTalosNotReady) {
				if len(f.rebooted) != 0 || starts["10.0.0.21"] != (EtcdStart{}) || len(log) != 0 {
					t.Errorf("rebooted %v, counted %+v, log %q", f.rebooted, starts["10.0.0.21"], log)
				}
				return
			}
			if !slices.Equal(f.rebooted, []string{"10.0.0.21"}) || starts["10.0.0.21"].Repairs != 1 {
				t.Errorf("rebooted %v, counted %+v", f.rebooted, starts["10.0.0.21"])
			}
			if len(log) != 1 || !strings.Contains(log[0], ". Talos refused to bootstrap it again: etcd data directory is not empty. Rebooted the node to start etcd again.") {
				t.Errorf("log %q", log)
			}
		})
	}
}

// A joining node's etcd Talos left Failed is rebooted, counted before the reboot and its boot
// written down after it, never bootstrapped.
func TestStartFirstEtcdRebootsAFailedJoiner(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": firstFailed}}
	starts := map[string]EtcdStart{}
	var rebootedThen []int
	log := firstStart(f, starts, func() error {
		rebootedThen = append(rebootedThen, len(f.rebooted))
		return nil
	})
	if !slices.Equal(f.rebooted, []string{"10.0.0.22"}) || len(f.restoreCalls()) != 0 {
		t.Errorf("rebooted %v, calls %v", f.rebooted, f.restoreCalls())
	}
	if !slices.Equal(rebootedThen, []int{0, 1}) || starts["10.0.0.22"] != (EtcdStart{Boot: "0", Repairs: 1}) {
		t.Errorf("recorded with %v reboots made, %+v", rebootedThen, starts["10.0.0.22"])
	}
	if want := []string{"Talos left etcd on 10.0.0.22 Failed: " + fakePullFailure + ". Rebooted the node to start etcd again."}; !slices.Equal(log, want) {
		t.Errorf("log %q, want %q", log, want)
	}
}

// Once etcd runs on any node, now or on an earlier look, the first node is never bootstrapped, and
// a node whose etcd ran is left alone.
func TestStartFirstEtcdNeverBootstrapsBesideARunningEtcd(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": firstFailed, "10.0.0.22": firstRunning}}
	starts := map[string]EtcdStart{}
	firstStart(f, starts, nil)
	if len(f.restoreCalls()) != 0 || !slices.Equal(f.rebooted, []string{"10.0.0.21"}) || !starts["10.0.0.22"].Ran {
		t.Errorf("calls %v, rebooted %v, starts %+v", f.restoreCalls(), f.rebooted, starts)
	}
	f = &fakeTalos{service: map[string]TalosService{"10.0.0.21": firstFailed, "10.0.0.22": firstFailed}}
	starts = map[string]EtcdStart{"10.0.0.22": {Ran: true}}
	firstStart(f, starts, nil)
	if len(f.restoreCalls()) != 0 || !slices.Equal(f.rebooted, []string{"10.0.0.21"}) {
		t.Errorf("after etcd ran on a node: calls %v, rebooted %v", f.restoreCalls(), f.rebooted)
	}
}

// A node is started once per boot: a look in the same boot leaves it, one in the next starts it.
func TestStartFirstEtcdOncePerBoot(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.22": firstFailed}, boots: map[string]int{}}
	starts := map[string]EtcdStart{}
	firstStart(f, starts, nil)
	firstStart(f, starts, nil)
	if !slices.Equal(f.rebooted, []string{"10.0.0.22"}) {
		t.Fatalf("rebooted %v in one boot", f.rebooted)
	}
	f.boots["10.0.0.22"] = 1
	firstStart(f, starts, nil)
	if !slices.Equal(f.rebooted, []string{"10.0.0.22", "10.0.0.22"}) || starts["10.0.0.22"] != (EtcdStart{Boot: "1", Repairs: 2}) {
		t.Errorf("rebooted %v, counted %+v", f.rebooted, starts["10.0.0.22"])
	}
}

// After a bootstrap Talos took, a first node whose etcd fails once more in the same boot is
// bootstrapped again, three times in all, and then left Failed.
func TestStartFirstEtcdBootstrapsAgainInTheSameBoot(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": firstFailed, "10.0.0.22": {State: talosEtcdPreparing}}}
	starts := map[string]EtcdStart{}
	log := make([]string, 0, 4)
	for range 5 {
		log = append(log, firstStart(f, starts, nil)...)
	}
	bootstrap := "10.0.0.21 plain bootstrap"
	if got := f.restoreCalls(); !slices.Equal(got, []string{bootstrap, bootstrap, bootstrap}) || len(f.rebooted) != 0 {
		t.Errorf("calls %v, rebooted %v", got, f.rebooted)
	}
	if len(log) != 4 || !strings.HasSuffix(log[3], ". Left it Failed after 3 starts.") {
		t.Errorf("log %q", log)
	}
}

// A start cut off before Talos answered, by a stop or by the call running out of time, is no
// refusal: nothing else is tried, no boot is written down, a stop logs nothing, and the next look
// starts the node again in the same boot.
func TestStartFirstEtcdOnACutOffStart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		node string
		stop bool
	}{
		"stop during a bootstrap":       {node: "10.0.0.21", stop: true},
		"bootstrap out of time":         {node: "10.0.0.21"},
		"stop during a joiner's reboot": {node: "10.0.0.22", stop: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cut := func() error {
				if tc.stop {
					cancel()
					return status.Error(codes.Canceled, context.Canceled.Error())
				}
				return status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error())
			}
			f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": {State: talosEtcdPreparing}}}
			f.service[tc.node] = firstFailed
			if tc.node == "10.0.0.22" {
				f.rebootFails = cut
			}
			f.fail = func(_, call string) error {
				if call == "plain bootstrap" {
					return cut()
				}
				return nil
			}
			starts := map[string]EtcdStart{}
			log := firstStartIn(ctx, f, starts, nil)
			if len(f.rebooted) != 0 || starts[tc.node] != (EtcdStart{Repairs: 1}) {
				t.Errorf("rebooted %v, counted %+v", f.rebooted, starts[tc.node])
			}
			lines := 1
			if tc.stop {
				lines = 0
			}
			if len(log) != lines || lines == 1 && strings.Contains(log[0], "eboot") {
				t.Errorf("log %q", log)
			}
			f.rebootFails, f.fail = nil, nil
			firstStart(f, starts, nil)
			started := len(f.rebooted) == 1
			if tc.node == "10.0.0.21" {
				started = slices.Equal(f.restoreCalls(), []string{"10.0.0.21 plain bootstrap", "10.0.0.21 plain bootstrap"})
			}
			if !started || starts[tc.node].Repairs != 2 {
				t.Errorf("in the same boot: calls %v, rebooted %v, counted %+v", f.restoreCalls(), f.rebooted, starts[tc.node])
			}
		})
	}
}

// After three starts the node is left Failed, which the log says once.
func TestStartFirstEtcdGivesUpAfterThreeStarts(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.22": firstFailed}, boots: map[string]int{"10.0.0.22": 3}}
	starts := map[string]EtcdStart{"10.0.0.22": {Boot: "2", Repairs: 3}}
	log := firstStart(f, starts, nil)
	log = append(log, firstStart(f, starts, nil)...)
	if len(f.rebooted) != 0 || !starts["10.0.0.22"].GaveUp {
		t.Errorf("rebooted %v, starts %+v", f.rebooted, starts["10.0.0.22"])
	}
	if want := []string{"Talos left etcd on 10.0.0.22 Failed: " + fakePullFailure + ". Left it Failed after 3 starts."}; !slices.Equal(log, want) {
		t.Errorf("log %q, want %q", log, want)
	}
}

// A node whose Talos API does not answer is passed by, and started once it answers.
func TestStartFirstEtcdPassesByANodeThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": firstFailed}, down: map[string]bool{"10.0.0.22": true}}
	starts := map[string]EtcdStart{}
	if log := firstStart(f, starts, nil); len(log) != 0 || len(f.rebooted) != 0 {
		t.Fatalf("log %q, rebooted %v", log, f.rebooted)
	}
	f.down["10.0.0.22"] = false
	firstStart(f, starts, nil)
	if !slices.Equal(f.rebooted, []string{"10.0.0.22"}) {
		t.Errorf("rebooted %v", f.rebooted)
	}
}

// A reboot whose boot cannot be written down after it is still logged, before the failure.
func TestStartFirstEtcdLogsARebootItCouldNotRecord(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": firstFailed}}
	records := 0
	log := firstStart(f, map[string]EtcdStart{}, func() error {
		records++
		if records > 1 {
			return errors.New("disk full")
		}
		return nil
	})
	want := []string{
		"Talos left etcd on 10.0.0.22 Failed: " + fakePullFailure + ". Rebooted the node to start etcd again.",
		"Starting etcd again on 10.0.0.22 failed: disk full",
	}
	if !slices.Equal(f.rebooted, []string{"10.0.0.22"}) || !slices.Equal(log, want) {
		t.Errorf("rebooted %v, log %q", f.rebooted, log)
	}
}

// afterReboot is a first node back in a newer boot than its reboot's with etcd waiting, which is
// bootstrapped once, and a joiner waiting beside it.
func afterReboot() *fakeTalos {
	return &fakeTalos{
		service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": {State: talosEtcdPreparing}},
		boots:   map[string]int{"10.0.0.21": 1},
	}
}

// A bootstrap after the reboot that Talos refuses is logged and left, since the boot is written
// down and no later look in it bootstraps again. One cut off before Talos answered writes the boot
// off, says why when the run goes on, and is made again on the next look.
func TestStartFirstEtcdOnABootstrapAfterARebootThatFails(t *testing.T) {
	t.Parallel()
	before := EtcdStart{Boot: "0", Repairs: 1}
	for name, tc := range map[string]struct {
		err      error
		stop     bool
		wantLog  []string
		wantSave EtcdStart
		again    int
	}{
		"refused": {
			err:      errors.New("etcd data directory is not empty"),
			wantLog:  []string{"Talos refused to bootstrap etcd on 10.0.0.21 after its reboot: etcd data directory is not empty"},
			wantSave: EtcdStart{Boot: "0", Bootstrapped: "1", Repairs: 1},
			again:    1,
		},
		"out of time": {
			err:      status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error()),
			wantLog:  []string{"Starting etcd again on 10.0.0.21 failed: rpc error: code = DeadlineExceeded desc = context deadline exceeded"},
			wantSave: before,
			again:    2,
		},
		"stopped": {
			err:      status.Error(codes.Canceled, context.Canceled.Error()),
			stop:     true,
			wantSave: before,
			again:    2,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := afterReboot()
			f.fail = func(_, call string) error {
				if call != "plain bootstrap" {
					return nil
				}
				if tc.stop {
					cancel()
				}
				return tc.err
			}
			starts := map[string]EtcdStart{"10.0.0.21": before}
			var saved []EtcdStart
			record := func() error {
				saved = append(saved, starts["10.0.0.21"])
				return nil
			}
			log := firstStartIn(ctx, f, starts, record)
			if !slices.Equal(log, tc.wantLog) || starts["10.0.0.21"] != tc.wantSave || len(f.rebooted) != 0 {
				t.Errorf("log %q, counted %+v, rebooted %v", log, starts["10.0.0.21"], f.rebooted)
			}
			marked := EtcdStart{Boot: "0", Bootstrapped: "1", Repairs: 1}
			wantSaved := []EtcdStart{marked}
			if tc.wantSave != marked {
				wantSaved = append(wantSaved, tc.wantSave)
			}
			if !slices.Equal(saved, wantSaved) {
				t.Errorf("saved %+v, want %+v", saved, wantSaved)
			}
			f.fail = nil
			firstStart(f, starts, nil)
			if got := len(f.restoreCalls()); got != tc.again {
				t.Errorf("%d bootstraps over two looks, want %d: %v", got, tc.again, f.restoreCalls())
			}
		})
	}
}

// A boot that cannot be written down before the bootstrap after a reboot stops it, and so does a
// failure to write it off after a bootstrap cut off. The log says each once.
func TestStartFirstEtcdOnABootstrapAfterARebootItCouldNotRecord(t *testing.T) {
	t.Parallel()
	t.Run("before the bootstrap", func(t *testing.T) {
		t.Parallel()
		f := afterReboot()
		log := firstStart(f, map[string]EtcdStart{"10.0.0.21": {Boot: "0", Repairs: 1}}, func() error { return errors.New("disk full") })
		if want := []string{"Starting etcd again on 10.0.0.21 failed: disk full"}; !slices.Equal(log, want) || len(f.restoreCalls()) != 0 {
			t.Errorf("log %q, calls %v", log, f.restoreCalls())
		}
	})
	t.Run("after a cut off bootstrap", func(t *testing.T) {
		t.Parallel()
		f := afterReboot()
		f.fail = func(_, call string) error {
			if call == "plain bootstrap" {
				return status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error())
			}
			return nil
		}
		records := 0
		log := firstStart(f, map[string]EtcdStart{"10.0.0.21": {Boot: "0", Repairs: 1}}, func() error {
			records++
			if records > 1 {
				return errors.New("disk full")
			}
			return nil
		})
		if want := []string{"Starting etcd again on 10.0.0.21 failed: disk full"}; !slices.Equal(log, want) || records != 2 || !slices.Equal(f.restoreCalls(), []string{"10.0.0.21 plain bootstrap"}) {
			t.Errorf("log %q, records %d, calls %v", log, records, f.restoreCalls())
		}
	})
}

// A reboot of the first node whose boot cannot be written down after it is logged before the
// failure, as a joiner's is, with the count written down once before the reboot.
func TestStartFirstEtcdLogsAFirstNodeRebootItCouldNotRecord(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{
		service: map[string]TalosService{"10.0.0.21": firstFailed, "10.0.0.22": {State: talosEtcdPreparing}},
		kubelet: map[string]TalosService{"10.0.0.21": kubeletFailed},
	}
	starts := map[string]EtcdStart{}
	var saved []EtcdStart
	log := firstStart(f, starts, func() error {
		saved = append(saved, starts["10.0.0.21"])
		if len(saved) > 1 {
			return errors.New("disk full")
		}
		return nil
	})
	want := []string{
		"Talos left etcd and the kubelet on 10.0.0.21 Failed: " + fakePullFailure + ". Rebooted the node to start etcd again.",
		"Starting etcd again on 10.0.0.21 failed: disk full",
	}
	if !slices.Equal(f.rebooted, []string{"10.0.0.21"}) || len(f.restoreCalls()) != 0 || !slices.Equal(log, want) {
		t.Errorf("rebooted %v, calls %v, log %q", f.rebooted, f.restoreCalls(), log)
	}
	if wantSaved := []EtcdStart{{Repairs: 1}, {Boot: "0", Repairs: 1}}; !slices.Equal(saved, wantSaved) {
		t.Errorf("saved %+v, want %+v", saved, wantSaved)
	}
}

// A count that cannot be written stops the start, which the log says.
func TestStartFirstEtcdStartsNothingUncounted(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": firstFailed}}
	log := firstStart(f, map[string]EtcdStart{}, func() error { return errors.New("disk full") })
	if len(f.rebooted) != 0 || !slices.Equal(log, []string{"Starting etcd again on 10.0.0.22 failed: disk full"}) {
		t.Errorf("rebooted %v, log %q", f.rebooted, log)
	}
}

var kubeletFailed = TalosService{State: talosEtcdFailed, Event: "Failed to run pre stage: failed to pull image \"registry.k8s.io/kubelet\": signature not found"}

// The first node whose etcd and kubelet Talos both left Failed is rebooted rather than
// bootstrapped, since the kubelet starts again only with a boot.
func TestStartFirstEtcdRebootsTheFirstNodeWithAFailedKubelet(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{
		service: map[string]TalosService{"10.0.0.21": firstFailed, "10.0.0.22": {State: talosEtcdPreparing}},
		kubelet: map[string]TalosService{"10.0.0.21": kubeletFailed},
	}
	starts := map[string]EtcdStart{}
	log := firstStart(f, starts, nil)
	if !slices.Equal(f.rebooted, []string{"10.0.0.21"}) || len(f.restoreCalls()) != 0 || starts["10.0.0.21"] != (EtcdStart{Boot: "0", Repairs: 1}) {
		t.Errorf("rebooted %v, calls %v, counted %+v", f.rebooted, f.restoreCalls(), starts["10.0.0.21"])
	}
	if want := []string{"Talos left etcd and the kubelet on 10.0.0.21 Failed: " + fakePullFailure + ". Rebooted the node to start etcd again."}; !slices.Equal(log, want) {
		t.Errorf("log %q, want %q", log, want)
	}
}

// The first node, back in a newer boot than its reboot's with etcd waiting and etcd running on no
// node, is bootstrapped once. A node nothing rebooted, or one beside a running etcd, is not.
func TestStartFirstEtcdBootstrapsTheFirstNodeOnceAfterItsReboot(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{
		service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": {State: talosEtcdPreparing}},
		boots:   map[string]int{"10.0.0.21": 1},
	}
	starts := map[string]EtcdStart{"10.0.0.21": {Boot: "0", Repairs: 1}}
	log := firstStart(f, starts, nil)
	firstStart(f, starts, nil)
	if got := f.restoreCalls(); !slices.Equal(got, []string{"10.0.0.21 plain bootstrap"}) || len(f.rebooted) != 0 {
		t.Errorf("calls %v, rebooted %v", got, f.rebooted)
	}
	if want := []string{"Bootstrapped etcd on 10.0.0.21 after its reboot."}; !slices.Equal(log, want) || starts["10.0.0.21"] != (EtcdStart{Boot: "0", Bootstrapped: "1", Repairs: 1}) {
		t.Errorf("log %q, counted %+v", log, starts["10.0.0.21"])
	}

	same := &fakeTalos{service: f.service}
	firstStart(same, map[string]EtcdStart{"10.0.0.21": {Boot: "0", Repairs: 1}}, nil)
	firstStart(same, map[string]EtcdStart{}, nil)
	beside := &fakeTalos{service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": firstRunning}, boots: f.boots}
	firstStart(beside, map[string]EtcdStart{"10.0.0.21": {Boot: "0", Repairs: 1}}, nil)
	if len(same.restoreCalls()) != 0 || len(beside.restoreCalls()) != 0 {
		t.Errorf("bootstrapped in the boot of the reboot or never rebooted: %v, beside a running etcd: %v", same.restoreCalls(), beside.restoreCalls())
	}
}

// A node whose kubelet is Failed and whose etcd waits is rebooted once per boot.
func TestStartFirstEtcdRebootsAFailedKubeletOncePerBoot(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{
		service: map[string]TalosService{"10.0.0.22": {State: talosEtcdPreparing}},
		kubelet: map[string]TalosService{"10.0.0.22": kubeletFailed},
		boots:   map[string]int{},
	}
	starts := map[string]EtcdStart{}
	log := firstStart(f, starts, nil)
	log = append(log, firstStart(f, starts, nil)...)
	if !slices.Equal(f.rebooted, []string{"10.0.0.22"}) || len(f.restoreCalls()) != 0 {
		t.Errorf("rebooted %v in one boot, calls %v", f.rebooted, f.restoreCalls())
	}
	if want := []string{"Talos left the kubelet on 10.0.0.22 Failed: " + kubeletFailed.Event + ". Rebooted the node to start etcd again."}; !slices.Equal(log, want) {
		t.Errorf("log %q, want %q", log, want)
	}
	f.boots["10.0.0.22"] = 1
	firstStart(f, starts, nil)
	if !slices.Equal(f.rebooted, []string{"10.0.0.22", "10.0.0.22"}) {
		t.Errorf("rebooted %v after the next boot", f.rebooted)
	}
}

// A Failed kubelet is rebooted at most maxEtcdRepairs times, across looks and across a watch that
// starts from the saved counts.
func TestStartFirstEtcdCapsTheRebootsOfAFailedKubelet(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{
		service: map[string]TalosService{"10.0.0.22": {State: talosEtcdPreparing}},
		kubelet: map[string]TalosService{"10.0.0.22": kubeletFailed},
		boots:   map[string]int{},
	}
	starts := map[string]EtcdStart{}
	log := make([]string, 0, maxEtcdRepairs+2)
	for boot := range maxEtcdRepairs + 2 {
		f.boots["10.0.0.22"] = boot
		saved, err := json.Marshal(starts)
		if err != nil {
			t.Fatal(err)
		}
		starts = map[string]EtcdStart{}
		if err = json.Unmarshal(saved, &starts); err != nil {
			t.Fatal(err)
		}
		log = append(log, firstStart(f, starts, nil)...)
	}
	if len(f.rebooted) != maxEtcdRepairs || !starts["10.0.0.22"].GaveUp {
		t.Errorf("rebooted %v, counted %+v", f.rebooted, starts["10.0.0.22"])
	}
	if len(log) != maxEtcdRepairs+1 || !strings.HasSuffix(log[maxEtcdRepairs], ". Left it Failed after 3 starts.") {
		t.Errorf("log %q", log)
	}
}

// freshStarts keeps of each node what the appliance keeps for its next run: the boots and whether
// etcd ran, never the count.
func freshStarts(starts map[string]EtcdStart) {
	for address, s := range starts {
		starts[address] = EtcdStart{Boot: s.Boot, Bootstrapped: s.Bootstrapped, Ran: s.Ran}
	}
}

// The first node whose pulls fail again in the boot the watch bootstrapped it in is rebooted in
// that boot, and the watch of a later apply reboots it once more and bootstraps it in its next boot.
func TestStartFirstEtcdRebootsTheFirstNodeAgainAfterItsBootstrap(t *testing.T) {
	t.Parallel()
	nodes := []string{"10.0.0.21", "10.0.0.22"}
	f := &fakeTalos{service: map[string]TalosService{}, kubelet: map[string]TalosService{}, boots: map[string]int{}}
	pulls := func(boot int, failed bool) {
		for _, address := range nodes {
			f.boots[address] = boot
			f.service[address], f.kubelet[address] = TalosService{State: talosEtcdPreparing}, firstRunning
			if failed {
				f.service[address], f.kubelet[address] = firstFailed, kubeletFailed
			}
		}
	}
	starts := map[string]EtcdStart{}
	pulls(0, true)
	firstStart(f, starts, nil)
	pulls(1, false)
	firstStart(f, starts, nil)
	if got := f.restoreCalls(); !slices.Equal(got, []string{"10.0.0.21 plain bootstrap"}) {
		t.Fatalf("calls %v after the first reboot", got)
	}
	pulls(1, true)
	firstStart(f, starts, nil)
	if !slices.Equal(f.rebooted, []string{"10.0.0.21", "10.0.0.22", "10.0.0.21", "10.0.0.22"}) {
		t.Fatalf("rebooted %v, counted %+v", f.rebooted, starts)
	}
	if want := (EtcdStart{Boot: "1", Bootstrapped: "1", Repairs: 2}); starts["10.0.0.21"] != want {
		t.Errorf("counted %+v, want %+v", starts["10.0.0.21"], want)
	}

	// The apply ends, the pulls fail again in the next boot, and the next apply sets the site CA.
	pulls(2, true)
	freshStarts(starts)
	f.rebooted = nil
	firstStart(f, starts, nil)
	firstStart(f, starts, nil)
	pulls(3, false)
	log := firstStart(f, starts, nil)
	firstStart(f, starts, nil)
	if !slices.Equal(f.rebooted, []string{"10.0.0.21", "10.0.0.22"}) {
		t.Errorf("rebooted %v in the apply with the site CA", f.rebooted)
	}
	if got := f.restoreCalls(); !slices.Equal(got, []string{"10.0.0.21 plain bootstrap", "10.0.0.21 plain bootstrap"}) {
		t.Errorf("calls %v", got)
	}
	if want := []string{"Bootstrapped etcd on 10.0.0.21 after its reboot."}; !slices.Equal(log, want) {
		t.Errorf("log %q, want %q", log, want)
	}
}

// The first node whose etcd runs and whose kubelet Talos left Failed is rebooted, since without the
// kubelet kube-apiserver never runs and a reboot keeps etcd's data: once per boot, at most
// maxEtcdRepairs times in a run, and again in the next run.
func TestStartFirstEtcdRebootsARunningEtcdForItsKubelet(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{
		service: map[string]TalosService{"10.0.0.21": firstRunning, "10.0.0.22": {State: talosEtcdPreparing}},
		kubelet: map[string]TalosService{"10.0.0.21": kubeletFailed},
		boots:   map[string]int{},
	}
	starts := map[string]EtcdStart{}
	log := firstStart(f, starts, nil)
	log = append(log, firstStart(f, starts, nil)...)
	if !slices.Equal(f.rebooted, []string{"10.0.0.21"}) || starts["10.0.0.21"] != (EtcdStart{Boot: "0", Repairs: 1, Ran: true}) {
		t.Fatalf("rebooted %v in one boot, counted %+v", f.rebooted, starts["10.0.0.21"])
	}
	if want := []string{"Talos left the kubelet on 10.0.0.21 Failed: " + kubeletFailed.Event + ". Rebooted the node to start the kubelet again."}; !slices.Equal(log, want) {
		t.Errorf("log %q, want %q", log, want)
	}
	for boot := 1; boot <= maxEtcdRepairs; boot++ {
		f.boots["10.0.0.21"] = boot
		firstStart(f, starts, nil)
	}
	if len(f.rebooted) != maxEtcdRepairs || !starts["10.0.0.21"].GaveUp {
		t.Errorf("rebooted %v, counted %+v", f.rebooted, starts["10.0.0.21"])
	}
	freshStarts(starts)
	f.boots["10.0.0.21"]++
	firstStart(f, starts, nil)
	if len(f.rebooted) != maxEtcdRepairs+1 || len(f.restoreCalls()) != 0 {
		t.Errorf("in the next run: rebooted %v, calls %v", f.rebooted, f.restoreCalls())
	}
}

// A node whose etcd and kubelet both run is never rebooted, in any boot.
func TestStartFirstEtcdNeverRebootsARunningNode(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": firstRunning, "10.0.0.22": firstRunning}, boots: map[string]int{}}
	starts := map[string]EtcdStart{"10.0.0.21": {Boot: "0", Repairs: 1}}
	for boot := range maxEtcdRepairs + 2 {
		f.boots["10.0.0.21"], f.boots["10.0.0.22"] = boot, boot
		if log := firstStart(f, starts, nil); len(log) != 0 {
			t.Errorf("log %q", log)
		}
	}
	if len(f.rebooted) != 0 || len(f.restoreCalls()) != 0 {
		t.Errorf("rebooted %v, calls %v", f.rebooted, f.restoreCalls())
	}
}

// A reboot whose boot was never written down leads to no bootstrap of its own: the first node back
// waiting is left to the module, and one whose etcd reads Failed is bootstrapped as any other.
func TestStartFirstEtcdBootstrapsAfterARebootOfNoKnownBootAsBefore(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{
		service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": {State: talosEtcdPreparing}},
		boots:   map[string]int{"10.0.0.21": 1},
	}
	starts := map[string]EtcdStart{"10.0.0.21": {Repairs: 1}}
	firstStart(f, starts, nil)
	if len(f.restoreCalls()) != 0 || len(f.rebooted) != 0 {
		t.Fatalf("calls %v, rebooted %v while it waits", f.restoreCalls(), f.rebooted)
	}
	f.service["10.0.0.21"] = firstFailed
	log := firstStart(f, starts, nil)
	if !slices.Equal(f.restoreCalls(), []string{"10.0.0.21 plain bootstrap"}) || len(f.rebooted) != 0 || starts["10.0.0.21"] != (EtcdStart{Repairs: 2}) {
		t.Errorf("calls %v, rebooted %v, counted %+v", f.restoreCalls(), f.rebooted, starts["10.0.0.21"])
	}
	if want := []string{"Talos left etcd on 10.0.0.21 Failed: " + fakePullFailure + ". Bootstrapped it again."}; !slices.Equal(log, want) {
		t.Errorf("log %q, want %q", log, want)
	}
}

// A bootstrap after a reboot that Talos is not ready for says nothing and is made on the next look.
func TestStartFirstEtcdBootstrapsAfterItsRebootOnceTalosIsReady(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{
		service: map[string]TalosService{"10.0.0.21": {State: talosEtcdPreparing}, "10.0.0.22": {State: talosEtcdPreparing}},
		boots:   map[string]int{"10.0.0.21": 1},
	}
	f.fail = func(_, call string) error {
		if call == "plain bootstrap" {
			return fmt.Errorf("%w: bootstrap is not available yet", errTalosNotReady)
		}
		return nil
	}
	starts := map[string]EtcdStart{"10.0.0.21": {Boot: "0", Repairs: 1}}
	if log := firstStart(f, starts, nil); len(log) != 0 || starts["10.0.0.21"] != (EtcdStart{Boot: "0", Repairs: 1}) {
		t.Fatalf("log %q, counted %+v", log, starts["10.0.0.21"])
	}
	f.fail = nil
	log := firstStart(f, starts, nil)
	if want := []string{"Bootstrapped etcd on 10.0.0.21 after its reboot."}; !slices.Equal(log, want) || len(f.restoreCalls()) != 2 {
		t.Errorf("log %q, calls %v", log, f.restoreCalls())
	}
}

// A node the operator rebooted since the look began is not rebooted for the failure it had before,
// nor in the look after, once its services read fresh.
func TestStartFirstEtcdLeavesANodeTheOperatorRebooted(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{
		service: map[string]TalosService{"10.0.0.22": firstFailed},
		kubelet: map[string]TalosService{"10.0.0.22": kubeletFailed},
		boots:   map[string]int{},
	}
	f.fail = func(address, call string) error {
		if address == "10.0.0.22" && call == "kubelet" {
			f.boots[address]++ // the operator reboots it between the reads
		}
		return nil
	}
	starts := map[string]EtcdStart{}
	firstStart(f, starts, nil)
	if len(f.rebooted) != 0 || starts["10.0.0.22"] != (EtcdStart{}) {
		t.Errorf("rebooted %v for the old boot's failure, counted %+v", f.rebooted, starts["10.0.0.22"])
	}
	f.fail = nil
	f.service["10.0.0.22"], f.kubelet = TalosService{State: talosEtcdPreparing}, nil
	firstStart(f, starts, nil)
	if len(f.rebooted) != 0 {
		t.Errorf("rebooted %v after the operator's reboot", f.rebooted)
	}
}

// A node whose kubelet cannot be read is passed by, however etcd reads, and started once it can.
func TestStartFirstEtcdPassesByANodeWhoseKubeletDoesNotAnswer(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{service: map[string]TalosService{"10.0.0.21": firstFailed, "10.0.0.22": firstFailed}}
	f.fail = func(_, call string) error {
		if call == "kubelet" {
			return errors.New("connection refused")
		}
		return nil
	}
	starts := map[string]EtcdStart{}
	if log := firstStart(f, starts, nil); len(log) != 0 || len(f.rebooted) != 0 || len(f.restoreCalls()) != 0 || len(starts) != 0 {
		t.Fatalf("log %q, rebooted %v, calls %v, counted %+v", log, f.rebooted, f.restoreCalls(), starts)
	}
	f.fail = nil
	firstStart(f, starts, nil)
	if !slices.Equal(f.restoreCalls(), []string{"10.0.0.21 plain bootstrap"}) || !slices.Equal(f.rebooted, []string{"10.0.0.22"}) {
		t.Errorf("calls %v, rebooted %v", f.restoreCalls(), f.rebooted)
	}
}
