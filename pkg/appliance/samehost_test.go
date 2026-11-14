package appliance

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// A backup volume on one host's own storage, or in a cluster of one node, is lost with that host,
// and the appliance's copy may be too. The summary says so until a bucket target is set, and never
// where the volume outlives a host.
func TestABackupVolumeThatSharesItsHostWarnsUntilABucketIsSet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		annotation string
		want       bool
	}{{"true", true}, {"false", false}} {
		a, _ := newAppliance(t)
		a.Talos = &fakeManagementTalos{}
		store := storeDeployment(0, "0")
		store.Annotations[sharesHost] = tc.annotation
		management := fake.NewClientset(store)
		a.ManagementClient = func([]byte) (kubernetes.Interface, error) { return management, nil }
		if err := a.Apply(settings("pve1"), passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if j := waitFor(t, a); j.State != StateSucceeded {
			t.Fatalf("apply: %+v", j)
		}
		st := a.EtcdBackups()
		if st.Where == nil || st.Where.SameHost != tc.want || st.Volume.SharesHost != tc.want {
			t.Fatalf("with %s %q the summary is %+v and the volume %+v, want the warning %v", sharesHost, tc.annotation, st.Where, st.Volume, tc.want)
		}
		if err := writeJSONFile(a.etcdBackupFile("settings.json"), EtcdBackupSettings{Target: "minio", EveryHours: 24, Keep: 2}); err != nil {
			t.Fatal(err)
		}
		if st := a.EtcdBackups(); st.Where.SameHost {
			t.Fatalf("with a bucket target set the summary still warns: %+v", st.Where)
		}

		// A cluster that does not answer keeps what was read before.
		if err := management.AppsV1().Deployments(store.Namespace).Delete(t.Context(), store.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := a.volumeSharesHost(t.Context(), nil, tc.want); got != tc.want {
			t.Fatalf("an unreadable store turned the warning to %v", got)
		}
	}
}
