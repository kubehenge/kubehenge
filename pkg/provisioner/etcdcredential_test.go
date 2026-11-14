package provisioner

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Snapshots need the store's keys: the cluster's own, or the ones for every cluster.
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
		// A change turning snapshots on is refused for the same cases.
		err = checkEtcdCredential(context.Background(), r.Client, etcdCluster(`{"enabled":false}`), c)
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: the change was refused with %v", tc.name, err)
		}
	}
}

// A cluster created without keys is not refused, since the interface stores them once it exists.
// One already on a target is not refused for an edit of the snapshots.
func TestEtcdKeysNotAskedOfCreatesAndEdits(t *testing.T) {
	t.Parallel()
	r, _ := newReconciler(t, &fakeCloud{})
	was := etcdCluster(`{"enabled":true,"target":"store"}`)
	if err := checkEtcdCredential(context.Background(), r.Client, nil, was); err != nil {
		t.Errorf("a create: %v", err)
	}
	if err := checkEtcdCredential(context.Background(), r.Client, was, etcdCluster(`{"enabled":true,"target":"store","keep":3}`)); err != nil {
		t.Errorf("an edit: %v", err)
	}
	if err := checkEtcdCredential(context.Background(), r.Client, was, etcdCluster(`{"enabled":true,"target":"aws"}`)); err == nil {
		t.Error("a move to another target without keys was allowed")
	}
}
