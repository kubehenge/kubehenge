package appliance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testPassphrase = "correct horse battery staple"

// leased is an appliance whose management cluster's lease is at generation cluster, 0 for none.
func leased(t *testing.T, cluster int) (*Appliance, *fake.Clientset) {
	t.Helper()
	a, _ := newAppliance(t)
	if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.workdir(), "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	management := fake.NewClientset()
	if cluster > 0 {
		setClusterGeneration(t, management, cluster)
	}
	a.ManagementClient = func([]byte) (kubernetes.Interface, error) { return management, nil }
	return a, management
}

func setClusterGeneration(t *testing.T, kube *fake.Clientset, g int) {
	t.Helper()
	cm := &corev1.ConfigMap{
		Name: leaseConfigMap, Namespace: interfaceNamespace,
		Data: map[string]string{"generation": strconv.Itoa(g)},
	}
	if _, err := kube.CoreV1().ConfigMaps(interfaceNamespace).Update(t.Context(), cm, metav1.UpdateOptions{}); apierrors.IsNotFound(err) {
		_, err = kube.CoreV1().ConfigMaps(interfaceNamespace).Create(t.Context(), cm, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func clusterGeneration(t *testing.T, kube kubernetes.Interface) int {
	t.Helper()
	g, _, err := clusterLease(t.Context(), kube)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func applied(t *testing.T, a *Appliance) *JobView {
	t.Helper()
	if err := a.Apply(settings("pve1"), testPassphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return waitFor(t, a)
}

// Each apply counts the generation up, on the appliance and in the management cluster.
func TestApplyClaimsTheLease(t *testing.T) {
	t.Parallel()
	a, management := leased(t, 0)
	for want := 1; want <= 2; want++ {
		if j := applied(t, a); j.State != StateSucceeded {
			t.Fatalf("apply %d: %s: %s", want, j.State, j.Error)
		}
		if local, cluster := a.localGeneration(), clusterGeneration(t, management); local != want || cluster != want {
			t.Errorf("after apply %d: generation %d here and %d in the cluster", want, local, cluster)
		}
	}
}

// A snapshot rollback, an older backup or a second appliance leaves this state behind the
// cluster's. The apply is refused before OpenTofu changes anything, until an administrator takes
// the management cluster over.
func TestApplyRefusesAnOlderStateUntilTakenOver(t *testing.T) {
	t.Parallel()
	a, management := leased(t, 3)
	if err := a.setLocalGeneration(2); err != nil {
		t.Fatal(err)
	}
	j := applied(t, a)
	if j.State != StateFailed || j.Stale == nil || j.Stale.Local != 2 || j.Stale.Cluster != 3 || !strings.Contains(j.Error, "Take it over") {
		t.Fatalf("apply on an older state: %+v", j)
	}
	if strings.Contains(strings.Join(j.Log, "\n"), "Applying the management cluster") {
		t.Error("OpenTofu applied an older state")
	}
	a.LoadLast()
	if j = a.Current(); j.Stale == nil || j.Stale.Cluster != 3 {
		t.Errorf("after a restart: %+v", j)
	}

	if err := a.TakeOver(t.Context(), "the wrong passphrase entirely"); !errors.Is(err, ErrPassphrase) {
		t.Errorf("take over with the wrong passphrase: %v", err)
	}
	if err := a.TakeOver(t.Context(), testPassphrase); err != nil {
		t.Fatal(err)
	}
	if j = a.Current(); j.Stale != nil {
		t.Errorf("still shown as stale once taken over: %+v", j.Stale)
	}
	if j = applied(t, a); j.State != StateSucceeded {
		t.Fatalf("apply once taken over: %s: %s", j.State, j.Error)
	}
	if local, cluster := a.localGeneration(), clusterGeneration(t, management); local != 4 || cluster != 4 {
		t.Errorf("generation %d here and %d in the cluster, want 4", local, cluster)
	}
}

// Two appliances that read the same generation cannot both claim the next: the second update
// conflicts, and the appliance's own count goes back.
func TestApplyRefusesAClaimAtTheSameTime(t *testing.T) {
	t.Parallel()
	a, management := leased(t, 1)
	if err := a.setLocalGeneration(1); err != nil {
		t.Fatal(err)
	}
	management.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, leaseConfigMap, errors.New("changed"))
	})
	if j := applied(t, a); j.State != StateFailed || !strings.Contains(j.Error, ErrLeaseTaken.Error()) {
		t.Fatalf("apply with a claim at the same time: %+v", j)
	}
	if local := a.localGeneration(); local != 1 {
		t.Errorf("generation %d here after losing the claim, want 1", local)
	}
}

// A management cluster that does not answer may need the very apply that repairs it. An
// administrator's goes ahead and says so. An automatic one is refused.
func TestLeaseOfAClusterOutOfReach(t *testing.T) {
	t.Parallel()
	a, _ := leased(t, 0)
	a.ManagementClient = func([]byte) (kubernetes.Interface, error) {
		kube := fake.NewClientset()
		kube.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("connection refused")
		})
		return kube, nil
	}
	j := applied(t, a)
	if j.State != StateSucceeded || !strings.Contains(strings.Join(j.Log, "\n"), "goes ahead unchecked: reading the management cluster's lease: connection refused") {
		t.Errorf("administrator's apply: %+v", j)
	}

	env := []string{"PATH=" + os.Getenv("PATH"), "TF_ENCRYPTION=" + encryption(testPassphrase)}
	err := a.claimLease(t.Context(), &Job{Automatic: true}, env)
	if err == nil || !strings.Contains(err.Error(), "not known whether a newer state changed it") {
		t.Errorf("automatic apply: %v", err)
	}
}

// A removal by an older state would leave what the newer apply made.
func TestRemoveRefusesAnOlderState(t *testing.T) {
	t.Parallel()
	a, _ := leased(t, 2)
	var removed atomic.Bool
	if err := a.Remove(testPassphrase, ticket(), func(context.Context, *Job) error { removed.Store(true); return nil }, func() {}); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateFailed || j.Stale == nil || removed.Load() {
		t.Errorf("removal by an older state: %+v, removed %v", j, removed.Load())
	}
}
