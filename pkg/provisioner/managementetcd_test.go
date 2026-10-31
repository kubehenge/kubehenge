package provisioner

import (
	"context"
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
