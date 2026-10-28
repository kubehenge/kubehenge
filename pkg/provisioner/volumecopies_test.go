package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/csiproxy"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

func TestCopyRefusal(t *testing.T) {
	t.Parallel()
	ok := csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-2"}
	with := func(change func(*csiproxy.Copy)) csiproxy.Copy {
		c := ok
		change(&c)
		return c
	}
	for name, tc := range map[string]struct {
		copy csiproxy.Copy
		want string
	}{
		"its own volume":             {ok, ""},
		"onto another node":          {with(func(c *csiproxy.Copy) { c.TargetNode = "pve2" }), ""},
		"on a directory storage":     {with(func(c *csiproxy.Copy) { c.Volume, c.Target = "4242/vm-4242-pvc-1.raw", "4242/vm-4242-snapshot-2.raw" }), ""},
		"another storage":            {with(func(c *csiproxy.Copy) { c.Storage = "local" }), "is not the cluster's storage local-zfs"},
		"another cluster's volume":   {with(func(c *csiproxy.Copy) { c.Volume = "vm-4243-pvc-1" }), "volume vm-4243-pvc-1 is not one of the cluster's own"},
		"a VM's disk":                {with(func(c *csiproxy.Copy) { c.Volume = "vm-100-disk-0" }), "is not one of the cluster's own"},
		"a directory of another VM":  {with(func(c *csiproxy.Copy) { c.Volume = "100/vm-4242-pvc-1.raw" }), "is not one of the cluster's own"},
		"a path out of the storage":  {with(func(c *csiproxy.Copy) { c.Volume = "../vm-4242-pvc-1" }), "is not one of the cluster's own"},
		"a target of another VM":     {with(func(c *csiproxy.Copy) { c.Target = "vm-100-disk-1" }), "target vm-100-disk-1 is not named after"},
		"a template's base disk":     {with(func(c *csiproxy.Copy) { c.Target = "base-4242-disk-0" }), "is not named after"},
		"onto itself":                {with(func(c *csiproxy.Copy) { c.Target = c.Volume }), "onto itself"},
		"a node that is no name":     {with(func(c *csiproxy.Copy) { c.Node = "pve1/../x" }), "not a Proxmox VE node's name"},
		"a target node that is none": {with(func(c *csiproxy.Copy) { c.TargetNode = "-x" }), "not a Proxmox VE node's name"},
	} {
		got := copyRefusal(&tc.copy, 4242, "local-zfs")
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// fakeRoot is Proxmox VE as root@pam sees it: the volumes on each node's storage, by node/storage,
// and the copies started, as "node storage volume target targetNode".
type fakeRoot struct {
	mu        sync.Mutex
	password  string
	challenge bool
	volumes   map[string][]string
	copies    []string
	exit      string
}

func (f *fakeRoot) SignIn(_ context.Context, username, password string) (*pve.Ticket, *pve.Challenge, error) {
	switch {
	case username != rootUser || password != f.password:
		return nil, nil, pve.ErrAuthentication
	case f.challenge:
		return nil, &pve.Challenge{Username: username, TOTP: true}, nil
	}
	return &pve.Ticket{Username: username, Ticket: "PVE:root@pam:1", Issued: time.Now()}, nil, nil
}

func (*fakeRoot) Renew(_ context.Context, t *pve.Ticket) (*pve.Ticket, error) {
	return &pve.Ticket{Username: t.Username, Ticket: t.Ticket, Issued: time.Now()}, nil
}

func (f *fakeRoot) StorageVolumes(_ context.Context, _ *pve.Ticket, node, storage string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.volumes[node+"/"+storage]), nil
}

func (f *fakeRoot) StartCopy(_ context.Context, _ *pve.Ticket, node, storage, volume, target, targetNode string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copies = append(f.copies, strings.Join([]string{node, storage, volume, target, targetNode}, " "))
	return fmt.Sprintf("UPID:%s:%04d:copy", node, len(f.copies)), nil
}

func (f *fakeRoot) TaskExit(context.Context, *pve.Ticket, string, string) (string, error) {
	return f.exit, nil
}

// copierFixture is a VolumeCopier serving web's copy requests, which are in workload.
type copierFixture struct {
	*VolumeCopier

	web      *clusterv1.Cluster
	workload client.Client
	recorder *events.FakeRecorder
}

// copierFor has web, which takes volume snapshots on local-zfs with volume owner 4242, ask for the
// given copies.
func copierFor(t *testing.T, root *fakeRoot, requests ...*corev1.ConfigMap) copierFixture {
	t.Helper()
	web := storedCluster("team-a", "web", "local-zfs", snapshotting)
	creds := Credentials{TokenID: webNames.TokenID(), TokenSecret: "s3cret", VolumeOwner: 4242, VolumeOwnerNode: "pve1"}
	_, kube := newReconciler(t, &fakeCloud{}, web, rootPasswordSecret("r00t\n"), credentialsSecret(web, creds, true))
	snapshotStoragesOf(t, kube, "kubehenge-rke2", "local-zfs")
	objs := make([]client.Object, len(requests))
	for i, cm := range requests {
		cm.Name = fmt.Sprintf("copy-%d", i)
		if cm.CreationTimestamp.IsZero() {
			cm.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Duration(i-len(requests)) * time.Second))
		}
		objs[i] = cm
	}
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(objs...).Build()
	recorder := events.NewFakeRecorder(10)
	return copierFixture{
		VolumeCopier: &VolumeCopier{
			Client: kube, ClassNamespace: "kubehenge-system", VolumeSnapshots: testVolumeSnapshots, Accounts: root,
			Remotes: &Remotes{byCluster: map[string]remote{remoteKey(web): {client: workload, until: time.Now().Add(time.Hour)}}},
			Events:  recorder, Interval: time.Second,
		},
		web: web, workload: workload, recorder: recorder,
	}
}

// serveWeb serves web's requests once.
func (f copierFixture) serveWeb(t *testing.T) {
	t.Helper()
	if err := f.serve(context.Background(), f.web); err != nil {
		t.Fatal(err)
	}
}

func requestOf(t *testing.T, workload client.Client, name string) csiproxy.Request {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := workload.Get(context.Background(), client.ObjectKey{Namespace: csiproxy.Namespace, Name: name}, cm); err != nil {
		t.Fatal(err)
	}
	return csiproxy.ParseRequest(cm)
}

func TestVolumeCopierCopiesOnlyTheClustersOwnVolumes(t *testing.T) {
	t.Parallel()
	root := &fakeRoot{password: "r00t", exit: "OK", volumes: map[string][]string{
		"pve1/local-zfs": {"local-zfs:vm-4242-pvc-1", "local-zfs:vm-4242-snapshot-taken", "local-zfs:vm-100-disk-0"},
	}}
	f := copierFor(t, root,
		csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-1"}),
		csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-100-disk-0", Target: "vm-4242-snapshot-2"}),
		csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-taken"}),
		csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-gone", Target: "vm-4242-snapshot-3"}),
	)
	f.serveWeb(t)
	if want := []string{"pve1 local-zfs vm-4242-pvc-1 vm-4242-snapshot-1 "}; !slices.Equal(root.copies, want) {
		t.Errorf("copies %q, want %q", root.copies, want)
	}
	if r := requestOf(t, f.workload, "copy-0"); r.State != csiproxy.StateDone || r.Task != "UPID:pve1:0001:copy" {
		t.Errorf("the cluster's own copy: %+v", r)
	}
	for name, why := range map[string]string{
		"copy-1": "volume vm-100-disk-0 is not one of the cluster's own",
		"copy-2": "volume vm-4242-snapshot-taken is on storage local-zfs on node pve1 already",
		"copy-3": "volume vm-4242-pvc-gone is not on storage local-zfs",
	} {
		if r := requestOf(t, f.workload, name); r.State != csiproxy.StateFailed || !strings.Contains(r.Message, why) {
			t.Errorf("%s: %+v, want it failed with %q", name, r, why)
		}
	}
	if n := len(f.recorder.Events); n != 3 {
		t.Errorf("%d events, want one for each refusal", n)
	}
	if e := <-f.recorder.Events; !strings.Contains(e, "VolumeCopyFailed") {
		t.Errorf("event %q", e)
	}
}

func TestVolumeCopierRecordsAFailedCopy(t *testing.T) {
	t.Parallel()
	root := &fakeRoot{password: "r00t", exit: "storage full", volumes: map[string][]string{"pve1/local-zfs": {"local-zfs:vm-4242-pvc-1"}}}
	f := copierFor(t, root,
		csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-1"}))
	f.serveWeb(t)
	if r := requestOf(t, f.workload, "copy-0"); r.State != csiproxy.StateFailed || !strings.Contains(r.Message, "storage full") {
		t.Errorf("request %+v", r)
	}
}

func TestVolumeCopierResumesACopyUnderWay(t *testing.T) {
	t.Parallel()
	root := &fakeRoot{password: "r00t", exit: "OK"}
	underWay := csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-1"})
	csiproxy.Record(underWay, csiproxy.StateCopying, "UPID:pve1:0007:copy", "")
	elsewhere := csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-2"})
	csiproxy.Record(elsewhere, csiproxy.StateCopying, "UPID:pve2:0008:copy", "")
	f := copierFor(t, root, underWay, elsewhere)
	f.serveWeb(t)
	if len(root.copies) != 0 {
		t.Errorf("copies started again: %q", root.copies)
	}
	if r := requestOf(t, f.workload, "copy-0"); r.State != csiproxy.StateDone || r.Task != "UPID:pve1:0007:copy" {
		t.Errorf("the copy under way: %+v", r)
	}
	if r := requestOf(t, f.workload, "copy-1"); r.State != csiproxy.StateFailed || !strings.Contains(r.Message, "names no copy task of its volume's node") {
		t.Errorf("a task on another node: %+v", r)
	}
}

func TestVolumeCopierRefusesWithoutRootAtPam(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		root *fakeRoot
		why  string
	}{
		"a wrong password": {&fakeRoot{password: "other"}, "refuses the root@pam password"},
		"a second factor":  {&fakeRoot{password: "r00t", challenge: true}, "asks for a second factor"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tc.root.volumes = map[string][]string{"pve1/local-zfs": {"local-zfs:vm-4242-pvc-1"}}
			f := copierFor(t, tc.root,
				csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-1"}))
			f.serveWeb(t)
			if r := requestOf(t, f.workload, "copy-0"); r.State != csiproxy.StateFailed || !strings.Contains(r.Message, tc.why) || len(tc.root.copies) != 0 {
				t.Errorf("request %+v, copies %q", r, tc.root.copies)
			}
		})
	}
}

func TestVolumeCopierRefusesWhenSnapshotsAreRefused(t *testing.T) {
	t.Parallel()
	root := &fakeRoot{password: "r00t", exit: "OK", volumes: map[string][]string{"pve1/local-zfs": {"local-zfs:vm-4242-pvc-1"}}}
	f := copierFor(t, root,
		csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-1"}))
	snapshotStoragesOf(t, f.Client, "kubehenge-rke2", "local")
	f.serveWeb(t)
	if r := requestOf(t, f.workload, "copy-0"); r.State != csiproxy.StateFailed || len(root.copies) != 0 {
		t.Errorf("a copy on a storage volume snapshots are refused on: %+v, copies %q", r, root.copies)
	}
}

func TestVolumeCopierDeletesFinishedRequestsOnceNoneWaits(t *testing.T) {
	t.Parallel()
	old := csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-1"})
	csiproxy.Record(old, csiproxy.StateDone, "UPID:pve1:0001:copy", "")
	recent := csiproxy.NewRequest(&csiproxy.Copy{Node: "pve1", Storage: "local-zfs", Volume: "vm-4242-pvc-1", Target: "vm-4242-snapshot-2"})
	csiproxy.Record(recent, csiproxy.StateFailed, "", "refused")
	old.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	f := copierFor(t, &fakeRoot{password: "r00t"}, old, recent)
	f.serveWeb(t)
	if err := f.workload.Get(context.Background(), client.ObjectKeyFromObject(old), &corev1.ConfigMap{}); err == nil {
		t.Error("a request finished long ago is kept")
	}
	if r := requestOf(t, f.workload, recent.Name); r.State != csiproxy.StateFailed || r.Message != "refused" {
		t.Errorf("a request just finished: %+v", r)
	}
}

func TestVolumeCopierLimitsEachClustersCopies(t *testing.T) {
	t.Parallel()
	v := &VolumeCopier{}
	web, api := storedCluster("team-a", "web", "local-zfs"), storedCluster("team-a", "api", "local-zfs")
	for i := range copiesPerHour {
		if !v.allow(web) {
			t.Fatalf("copy %d refused", i+1)
		}
	}
	if v.allow(web) {
		t.Error("one copy more than the hour allows")
	}
	if !v.allow(api) {
		t.Error("another cluster's copy counted against web's")
	}
	v.started[remoteKey(web)][0] = time.Now().Add(-time.Hour)
	if !v.allow(web) {
		t.Error("a copy over an hour old still counts")
	}
}
