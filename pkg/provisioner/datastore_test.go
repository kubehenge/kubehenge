package provisioner

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var testDatastoreAllocate = DatastoreAllocate{Role: "kubehenge-cluster-datastore", Management: []string{"local-lvm", "local"}}

// allocating asks for Datastore.Allocate on the cluster's storage.
var allocating = variableOf(csiVariable, `{"datastoreAllocate":true}`)

// storedCluster is a cluster of the RKE2 class on storage, with more variables.
func storedCluster(namespace, name, storage string, variables ...clusterv1.ClusterVariable) *clusterv1.Cluster {
	c := cluster(append([]clusterv1.ClusterVariable{variableOf("storage", `"`+storage+`"`)}, variables...)...)
	c.Namespace, c.Name = namespace, name
	return c
}

func TestDatastoreAllocateRefusal(t *testing.T) {
	t.Parallel()
	class := &clusterv1.ClusterClass{Name: "kubehenge-rke2", Namespace: "kubehenge-system"}
	class.Annotations = map[string]string{TemplateStoragesAnnotation: "nvme,ssd"}
	web := storedCluster("team-a", "web", "ceph", allocating)
	foreign := storedCluster("team-c", "other", "ceph", allocating)
	foreign.Spec.Topology.ClassRef.Namespace = "elsewhere"
	for name, tc := range map[string]struct {
		d        DatastoreAllocate
		cluster  *clusterv1.Cluster
		clusters []clusterv1.Cluster
		want     string
	}{
		"alone on its storage":         {testDatastoreAllocate, web, []clusterv1.Cluster{*web}, ""},
		"not asking":                   {testDatastoreAllocate, storedCluster("team-a", "web", "local-lvm"), nil, ""},
		"no role":                      {DatastoreAllocate{}, web, nil, "lets no cluster hold Datastore.Allocate"},
		"the management cluster's":     {testDatastoreAllocate, storedCluster("team-a", "web", "local", allocating), nil, "the management cluster uses storage local"},
		"the templates'":               {testDatastoreAllocate, storedCluster("team-a", "web", "ssd", allocating), nil, "VM templates have disks on storage ssd"},
		"shared with another cluster":  {testDatastoreAllocate, web, []clusterv1.Cluster{*storedCluster("team-b", "api", "ceph")}, "cluster team-b/api uses storage ceph too"},
		"another cluster on its own":   {testDatastoreAllocate, web, []clusterv1.Cluster{*storedCluster("team-b", "api", "nvme2")}, ""},
		"onto a storage another holds": {testDatastoreAllocate, storedCluster("team-b", "api", "ceph"), []clusterv1.Cluster{*web}, "cluster team-a/web holds Datastore.Allocate on storage ceph"},
		"beside another class's":       {testDatastoreAllocate, web, []clusterv1.Cluster{*foreign}, ""},
	} {
		got, err := tc.d.Refusal(tc.cluster, class, tc.clusters, "kubehenge-system")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refused with %q, want %q", name, got, tc.want)
		}
	}
}

func TestDatastoreAllocateRefusedAtAdmissionAndCreation(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "ceph", allocating)
	h, _ := hooksFor(t, web)
	h.DatastoreAllocate = testDatastoreAllocate
	api := storedCluster("team-b", "api", "ceph")
	if _, err := (&ClusterValidator{Hooks: h}).ValidateCreate(context.Background(), api); err == nil || !strings.Contains(err.Error(), "holds Datastore.Allocate on storage ceph") {
		t.Errorf("the validator let a second cluster onto web's storage: %v", err)
	}
	created := networkCluster("192.168.1.1", 24)
	created.Spec.Topology.Variables = append(created.Spec.Topology.Variables, variableOf("storage", `"local"`), variableOf(csiVariable, `{"datastoreAllocate":true}`))
	if resp := callBeforeClusterCreate(h, created); !strings.Contains(resp.Message, "the management cluster uses storage local") {
		t.Errorf("BeforeClusterCreate let Datastore.Allocate onto the management cluster's storage: %s %q", resp.Status, resp.Message)
	}

	// An unrelated change to a cluster that asks for it is not checked again.
	again := web.DeepCopy()
	again.Spec.Topology.Variables = append(again.Spec.Topology.Variables, variableOf("headlamp", `{"enabled":true}`))
	other := storedCluster("team-b", "api", "ceph")
	h2, _ := hooksFor(t, web, other)
	h2.DatastoreAllocate = testDatastoreAllocate
	if _, err := (&ClusterValidator{Hooks: h2}).ValidateUpdate(context.Background(), web, again); err != nil {
		t.Errorf("an unrelated change was refused: %v", err)
	}
	switched := storedCluster("team-b", "api", "ceph", allocating)
	if _, err := (&ClusterValidator{Hooks: h2}).ValidateUpdate(context.Background(), other, switched); err == nil || !strings.Contains(err.Error(), "uses storage ceph too") {
		t.Errorf("switching it on beside another cluster was let through: %v", err)
	}
}

func csiChartVersion(t *testing.T, kube client.Client) string {
	t.Helper()
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-proxmox-csi"}, hcp); err != nil {
		t.Fatal(err)
	}
	v, _, err := unstructured.NestedString(hcp.Object, "spec", "version")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReconcileGrantsDatastoreAllocateOnlyOnAStorageOfItsOwn(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "ceph", allocating)
	cloud := &fakeCloud{}
	r, kube := newReconciler(t, cloud, web)
	r.DatastoreAllocate = testDatastoreAllocate
	recorder := events.NewFakeRecorder(10)
	r.Events = recorder
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cloud.access.DatastoreAllocate, []string{"ceph"}) {
		t.Errorf("granted Datastore.Allocate on %v, want ceph", cloud.access.DatastoreAllocate)
	}
	if got := csiChartVersion(t, kube); !strings.HasPrefix(got, testCharts.CSINewest.Version+"@") {
		t.Errorf("the CSI plugin's chart is %s, want the newest release", got)
	}

	// Another cluster on the storage, let in before web asked: web loses the privilege and
	// goes back to the release that needs none.
	if err := kube.Create(context.Background(), storedCluster("team-b", "api", "ceph")); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if len(cloud.access.DatastoreAllocate) != 0 {
		t.Errorf("still granted Datastore.Allocate on %v", cloud.access.DatastoreAllocate)
	}
	if got := csiChartVersion(t, kube); !strings.HasPrefix(got, testCharts.CSI.Version+"@") {
		t.Errorf("the CSI plugin's chart is %s, want the held release", got)
	}
	select {
	case e := <-recorder.Events:
		if !strings.Contains(e, "DatastoreAllocateRefused") || !strings.Contains(e, "team-b/api") {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("no event says why")
	}
}

func TestGrantOnTheStoragesOfAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakePVE("a", nil)
	p := &Proxmox{Client: serve(t, f), Settings: Settings{Role: "kubehenge-cluster", DatastoreRole: "kubehenge-cluster-datastore"}}
	n := Names{Pool: "kubehenge/team-a.web", User: "kubehenge.team-a.web@pve"}
	// Another user's grant on the storage stays as it is.
	f.acl = []map[string]any{{"path": "/storage/ceph", "type": "user", "ugid": "admin@pve", "roleid": "Administrator", "propagate": 1}}
	for range 2 {
		if err := p.ensureGrant(ctx, n, Access{DatastoreAllocate: []string{"ceph"}}); err != nil {
			t.Fatal(err)
		}
	}
	want := []map[string]any{
		{"path": "/storage/ceph", "type": "user", "ugid": "admin@pve", "roleid": "Administrator", "propagate": 1},
		{"path": "/pool/kubehenge/team-a.web", "type": "user", "ugid": n.User, "roleid": "kubehenge-cluster", "propagate": float64(1)},
		{"path": "/storage/ceph", "type": "user", "ugid": n.User, "roleid": "kubehenge-cluster-datastore", "propagate": float64(0)},
	}
	if f.aclWrites != 2 || !slices.EqualFunc(f.acl, want, maps.Equal) {
		t.Fatalf("%d writes, ACL %v", f.aclWrites, f.acl)
	}

	if err := p.ensureGrant(ctx, n, Access{}); err != nil {
		t.Fatal(err)
	}
	if f.aclWrites != 3 || !slices.EqualFunc(f.acl, want[:2], maps.Equal) {
		t.Errorf("after the storage went from access: %d writes, ACL %v", f.aclWrites, f.acl)
	}

	p.Settings.DatastoreRole = ""
	if err := p.ensureGrant(ctx, n, Access{DatastoreAllocate: []string{"ceph"}}); err == nil {
		t.Error("granted a storage without a role for it")
	}
}
