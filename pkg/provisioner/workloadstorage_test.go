package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDefaultStorageClassSetsTheDriveOptions(t *testing.T) {
	t.Parallel()
	_, csi := proxmoxValues("local-lvm", &Addons{})
	class := lookup[map[string]any](t, csi, "storageClass", 0)
	extra := lookup[map[string]any](t, class, "extraParameters")
	if extra["discard"] != "on" || extra["ssd"] != "true" {
		t.Errorf("the default StorageClass's drive options are %v; want discard on and ssd true", extra)
	}
	if _, set := extra["backup"]; set {
		t.Errorf("backup is set on %v; the plugin's own default is off", extra)
	}
	if lookup[string](t, class, "storage") != "local-lvm" || lookup[string](t, class, "name") != defaultStorageClass {
		t.Errorf("the StorageClass is %v", class)
	}
}

func storageClassOf(parameters map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{Name: defaultStorageClass, UID: "u1", Parameters: parameters}
}

func TestStaleStorageClassIsDeletedForTheChartToCreateAgain(t *testing.T) {
	t.Parallel()
	storage := clusterv1.ClusterVariable{Name: "storage", Value: jsonValue(`"local-lvm"`)}
	for name, tc := range map[string]struct {
		parameters map[string]string
		deleted    bool
	}{
		"created before discard was set": {map[string]string{"storage": "local-lvm", "csi.storage.k8s.io/fstype": "ext4"}, true},
		"on another storage":             {map[string]string{"storage": "ceph", "discard": "on", "ssd": "true"}, true},
		"current":                        {map[string]string{"storage": "local-lvm", "discard": "on", "ssd": "true"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := cluster(storage)
			workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(storageClassOf(tc.parameters)).Build()
			rec := events.NewFakeRecorder(5)
			r := &Reconciler{Events: rec, Remotes: &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}}
			waiting, err := r.retireStaleStorageClass(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			gone := apierrors.IsNotFound(workload.Get(context.Background(), client.ObjectKey{Name: defaultStorageClass}, &storagev1.StorageClass{}))
			if gone != tc.deleted || waiting != tc.deleted {
				t.Errorf("deleted %v, waiting %v; want %v", gone, waiting, tc.deleted)
			}
			if tc.deleted && len(rec.Events) != 1 {
				t.Errorf("%d events; want one saying the class is created again", len(rec.Events))
			}
		})
	}
}

func TestStorageClassNotThereYetIsLeftAlone(t *testing.T) {
	t.Parallel()
	c := cluster(clusterv1.ClusterVariable{Name: "storage", Value: jsonValue(`"local-lvm"`)})
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).Build()
	r := &Reconciler{Remotes: &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}}
	if waiting, err := r.retireStaleStorageClass(context.Background(), c); err != nil || waiting {
		t.Errorf("waiting %v, %v; want nothing to do", waiting, err)
	}
}

func TestVolumeOwnerIsProtectedWhenCreatedAndWhenFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	created := newFakePVE("a", nil)
	created.destroyed, created.token, created.clusterName = true, true, "lab"
	created.nextIDs = []int{4242}
	p := &Proxmox{Client: serve(t, created), Settings: Settings{Role: "kubehenge-cluster"}}
	if _, err := p.Ensure(ctx, webNames, Access{}, Credentials{TokenID: webNames.TokenID(), TokenSecret: "s3cret"}, func(Credentials) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if created.config["protection"] != "1" {
		t.Errorf("a new owner VM has the configuration %v; want protection 1", created.config)
	}
	if !strings.Contains(created.config["description"], "Do not delete this VM") {
		t.Errorf("the description %q does not warn against deleting the VM", created.config["description"])
	}

	existing := newFakePVE("a", nil)
	existing.token, existing.clusterName = true, "lab"
	p = &Proxmox{Client: serve(t, existing), Settings: Settings{Role: "kubehenge-cluster"}}
	held := Credentials{TokenID: webNames.TokenID(), TokenSecret: "s3cret", VolumeOwner: 4242, VolumeOwnerNode: "a"}
	if _, err := p.Ensure(ctx, webNames, Access{}, held, func(Credentials) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if existing.config["protection"] != "1" {
		t.Errorf("an owner VM made before has the configuration %v; want protection 1 once seen", existing.config)
	}
}

func TestRemoveLiftsProtectionBeforeDestroyingTheOwner(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{"a/local": {"local:4242/vm-4242-pvc-1.raw"}})
	f.config["protection"] = "1"
	p := &Proxmox{Client: serve(t, f)}
	if err := p.Remove(context.Background(), webNames, Credentials{}, "local"); err != nil {
		t.Fatal(err)
	}
	if f.destroys != 1 || !f.destroyed {
		t.Errorf("%d destroys, destroyed %v; Proxmox VE refuses a protected VM", f.destroys, f.destroyed)
	}
	if !slices.Equal(f.protections, []string{"0"}) {
		t.Errorf("protection written %v; want it lifted once", f.protections)
	}
}

func TestReclaimLiftsProtectionAroundTheDeletionAndRestoresIt(t *testing.T) {
	t.Parallel()
	f := newFakePVE("a", map[string][]string{"a/local": {"local:4242/vm-4242-pvc-1.raw"}})
	f.config["protection"] = "1"
	pve := serve(t, f)
	o := &ownerVM{pve: pve, mover: moved{pve}, vmid: 4242, created: "a"}
	if err := o.deleteVolume(context.Background(), volume{node: "a", storage: "local", volid: "local:4242/vm-4242-pvc-1.raw", owner: 4242}); err != nil {
		t.Fatal(err)
	}
	if len(f.volumes["a/local"]) != 0 {
		t.Errorf("volumes left %v", f.volumes)
	}
	if f.config["protection"] != "1" || !slices.Equal(f.protections, []string{"0", "1"}) {
		t.Errorf("protection is %q after writes %v; want it lifted and put back", f.config["protection"], f.protections)
	}
}

// lostOwnerCloud creates a new volume owner VM in place of one that is gone.
type lostOwnerCloud struct{ fakeCloud }

func (*lostOwnerCloud) Ensure(_ context.Context, _ Names, _ Access, held Credentials, _ func(Credentials) error) (Credentials, error) {
	held.VolumeOwner++
	return held, nil
}

func TestMissingVolumeOwnerRaisesAnEventNamingTheOldVMID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ready bool
		owner int
		event bool
	}{
		"the cluster was ready":      {true, 77, true},
		"the cluster never was":      {false, 77, false},
		"no owner was ever recorded": {true, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := cluster()
			rec := events.NewFakeRecorder(5)
			secret := credentialsSecret(c, Credentials{TokenID: "t", TokenSecret: "s", VolumeOwner: tc.owner}, tc.ready)
			kube := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(secret).Build()
			r := &Reconciler{Client: kube, Cloud: &lostOwnerCloud{}, WorkloadPool: "kubehenge", Events: rec}
			if _, err := r.ensureCloud(context.Background(), c, Access{}); err != nil {
				t.Fatal(err)
			}
			var got string
			select {
			case got = <-rec.Events:
			default:
			}
			if (got != "") != tc.event {
				t.Errorf("event %q, want one: %v", got, tc.event)
			}
			if tc.event && !strings.Contains(got, "volume owner VM 77 is gone") {
				t.Errorf("event %q does not name the old VMID", got)
			}
		})
	}
}

// storages for the removal tests: the cluster's own storage is "local" on nodes a and b.
func removalFixture(storages []map[string]any, volumes map[string][]string) *fakePVE {
	f := newFakePVE("a", volumes)
	f.storageList = storages
	return f
}

func TestRemoveIgnoresAnOfflineNodeThatHoldsNothingOfTheCluster(t *testing.T) {
	t.Parallel()
	f := removalFixture([]map[string]any{
		{"storage": "local", "content": "images", "shared": 0, "nodes": "a"},
		{"storage": "scratch", "content": "images", "shared": 0, "nodes": "b"},
	}, map[string][]string{"a/local": {"local:4242/vm-4242-pvc-1.raw"}})
	f.offline["b"] = true
	p := &Proxmox{Client: serve(t, f)}
	if err := p.Remove(context.Background(), webNames, Credentials{}, "local"); err != nil {
		t.Fatalf("an offline node without the cluster's storage blocked the removal: %v", err)
	}
	if !f.destroyed {
		t.Error("the owner VM is still there")
	}
}

func TestRemoveIgnoresAnUnrelatedStorageThatCannotBeListed(t *testing.T) {
	t.Parallel()
	f := removalFixture([]map[string]any{
		{"storage": "local", "content": "images", "shared": 0},
		{"storage": "old-pool", "content": "images", "shared": 0},
	}, map[string][]string{"a/local": {"local:4242/vm-4242-pvc-1.raw"}})
	f.inactive["a/old-pool"], f.inactive["b/old-pool"] = true, true
	p := &Proxmox{Client: serve(t, f)}
	if err := p.Remove(context.Background(), webNames, Credentials{}, "local"); err != nil {
		t.Fatalf("a disabled storage of another use blocked the removal: %v", err)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "old-pool") {
			t.Errorf("asked for %s, a storage the cluster never used", r)
		}
	}
}

func TestRemoveSkipsTheClustersStorageWhereItIsNotActive(t *testing.T) {
	t.Parallel()
	f := removalFixture([]map[string]any{{"storage": "local", "content": "images", "shared": 0}},
		map[string][]string{"a/local": {"local:4242/vm-4242-pvc-1.raw"}})
	f.inactive["b/local"] = true
	p := &Proxmox{Client: serve(t, f)}
	if err := p.Remove(context.Background(), webNames, Credentials{}, "local"); err != nil {
		t.Fatal(err)
	}
	if !f.destroyed || len(f.volumes["a/local"]) != 0 {
		t.Errorf("destroyed %v, volumes %v", f.destroyed, f.volumes)
	}
}

func TestRemoveBlocksOnAnOfflineNodeWithTheClustersLocalStorage(t *testing.T) {
	t.Parallel()
	f := removalFixture([]map[string]any{{"storage": "local", "content": "images", "shared": 0}}, nil)
	f.offline["b"] = true
	p := &Proxmox{Client: serve(t, f)}
	err := p.Remove(context.Background(), webNames, Credentials{}, "local")
	if err == nil || !strings.Contains(err.Error(), "node b is offline and holds storage local") {
		t.Fatalf("got %v; want the node and the storage named", err)
	}
	if f.destroyed {
		t.Error("destroyed the owner VM while a node with its volumes is offline")
	}
}

func TestRemoveListsASharedStorageOnce(t *testing.T) {
	t.Parallel()
	f := removalFixture([]map[string]any{{"storage": "ceph", "content": "images", "shared": 1}},
		map[string][]string{"*/ceph": {"ceph:vm-4242-pvc-3"}})
	f.offline["a"] = true
	p := &Proxmox{Client: serve(t, f)}
	if err := p.Remove(context.Background(), webNames, Credentials{}, "ceph"); err != nil {
		t.Fatalf("an offline node blocked a shared storage another node reaches: %v", err)
	}
	listed := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, "GET /nodes/") && strings.HasSuffix(r, "/storage/ceph/content") {
			listed++
		}
	}
	if listed != 1 {
		t.Errorf("the shared storage was listed %d times; want once", listed)
	}
}

func TestAbandonLeavesSentence(t *testing.T) {
	t.Parallel()
	if got := abandonLeaves(nil); got != "" {
		t.Errorf("got %q for nothing left behind", got)
	}
	if got := abandonLeaves([]string{"a", "b"}); got != ". Abandoning leaves behind: a, b" {
		t.Errorf("got %q", got)
	}
}
