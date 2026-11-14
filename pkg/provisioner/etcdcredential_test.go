package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Snapshots need the store's keys: the cluster's own, or the ones for every cluster. A cluster with
// neither is accepted, and its snapshots wait for a key.
func TestEtcdSnapshotsNeedKeys(t *testing.T) {
	t.Parallel()
	const on = `{"enabled":true,"target":"store"}`
	own := storeKeys("web.backup", "web", backupKeysData)
	shared := storeKeys("kubehenge-backup", "", backupKeysData)
	for _, tc := range []struct {
		name string
		raw  string
		objs []client.Object
		want string
	}{
		{"shared key", on, []client.Object{shared}, ""},
		{"own key", on, []client.Object{own}, ""},
		{"neither", on, nil, "no credential for the backup target"},
		{"another cluster's only", on, []client.Object{storeKeys("db.backup", "db", backupKeysData)}, "no credential for the backup target"},
		{"snapshots off", `{"enabled":false,"target":"store"}`, nil, ""},
	} {
		r, _ := newReconciler(t, &fakeCloud{}, tc.objs...)
		c := etcdCluster(tc.raw)
		missing, err := etcdCredentialMissing(context.Background(), r.Client, c)
		if err != nil || (tc.want == "") != (missing == "") || !strings.Contains(missing, tc.want) {
			t.Errorf("%s: missing %q, %v", tc.name, missing, err)
		}
		// Whatever the keys, the cluster is accepted.
		if err = checkEtcdSnapshots(nil, c, targetClass()); err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
	}
}

func credentialScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

// A cluster with snapshots on and no key shows why none are taken.
func TestEtcdSnapshotsWaitForAKey(t *testing.T) {
	t.Parallel()
	c := etcdCluster(`{"enabled":true,"target":"store"}`)
	c.Namespace = "team-a"
	kube := fake.NewClientBuilder().WithScheme(credentialScheme(t)).WithObjects(c).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	s := &EtcdSnapshotter{Client: kube, Reader: kube, ClassNamespace: "kubehenge-system"}
	res, err := s.Reconcile(context.Background(), ctrl.Request{Namespace: "team-a", Name: "web"})
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("reconciled to %+v, %v", res, err)
	}
	got := &clusterv1.Cluster{}
	if err = kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, EtcdSnapshotCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != EtcdSnapshotWaitingForKey {
		t.Errorf("condition %+v", cond)
	}
}

// A credential stored ahead of its cluster gets the cluster as its owner once it exists, and goes
// when the cluster does not come.
func TestCredentialOrphans(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	ahead := func(cluster string, age time.Duration) *corev1.Secret {
		s := storeKeys(cluster+".backup", cluster, backupKeysData)
		s.Namespace = "team-a"
		s.Type = CredentialType
		s.CreationTimestamp = metav1.NewTime(now.Add(-age))
		return s
	}
	web := etcdCluster(`{"enabled":false}`)
	web.Namespace = "team-a"
	owned := ahead("web", time.Hour)
	owned.Name = "owned.backup"
	owned.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "u"}}
	kube := fake.NewClientBuilder().WithScheme(credentialScheme(t)).WithObjects(
		web, ahead("web", time.Minute), ahead("gone", time.Hour), ahead("late", time.Minute), owned,
	).Build()
	o := &CredentialOrphans{Client: kube, Grace: 10 * time.Minute, Now: func() time.Time { return now }}
	if err := o.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	read := func(name string) (*corev1.Secret, error) {
		s := &corev1.Secret{}
		return s, kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, s)
	}
	if s, err := read("web.backup"); err != nil || len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != web.UID {
		t.Errorf("the cluster's key: %+v, %v", s, err)
	}
	if _, err := read("gone.backup"); err == nil {
		t.Error("the key of a cluster that never came was kept")
	}
	if _, err := read("late.backup"); err != nil {
		t.Errorf("a key within its grace: %v", err)
	}
	if s, err := read("owned.backup"); err != nil || s.OwnerReferences[0].Name != "x" {
		t.Errorf("an owned key: %+v, %v", s, err)
	}
}
