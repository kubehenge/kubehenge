package provisioner

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
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

// An etcd that stays Failed is bootstrapped again at most once a poll, until the step's time is
// up. The error then says it stayed Failed.
func TestManagementTalosRestoreGivesUpOnAFailedEtcd(t *testing.T) {
	t.Parallel()
	f := &fakeTalos{starting: func(int) (string, bool) { return talosEtcdFailed, false }}
	_, err := restoreManagement(f, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "starting etcd on 10.0.0.21 did not finish") || !strings.Contains(err.Error(), "etcd stayed Failed") {
		t.Fatalf("error %v, want one saying etcd stayed Failed", err)
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
