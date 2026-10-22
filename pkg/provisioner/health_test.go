package provisioner

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// tokenIssuer stands in for a workload cluster's TokenRequest API: each token is new, and lasts as
// long as asked from the test's clock.
type tokenIssuer struct {
	mu     sync.Mutex
	now    func() time.Time
	issued []string
}

func (ti *tokenIssuer) create(_ context.Context, _ client.Client, sub string, obj, body client.Object, _ ...client.SubResourceCreateOption) error {
	req, ok := body.(*authenticationv1.TokenRequest)
	if sub != "token" || !ok {
		return fmt.Errorf("unexpected %s of %T", sub, body)
	}
	ti.mu.Lock()
	defer ti.mu.Unlock()
	ti.issued = append(ti.issued, obj.GetName())
	req.Status.Token = fmt.Sprintf("token-%d-%s", len(ti.issued), obj.GetName())
	req.Status.ExpirationTimestamp = metav1.NewTime(ti.now().Add(time.Duration(*req.Spec.ExpirationSeconds) * time.Second))
	return nil
}

func (ti *tokenIssuer) count() int {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	return len(ti.issued)
}

func initialised(c *clusterv1.Cluster) *clusterv1.Cluster {
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
		Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue, Reason: "Up",
	})
	return c
}

// healthFixture is a HealthIdentity with the management and workload clusters it acts on, and what
// issues the workload cluster's tokens.
type healthFixture struct {
	h        *HealthIdentity
	kube     client.Client
	workload client.Client
	issuer   *tokenIssuer
}

func newHealthIdentity(t *testing.T, c *clusterv1.Cluster, clock *time.Time) healthFixture {
	t.Helper()
	r, kube := newReconciler(t, &fakeCloud{}, c)
	issuer := &tokenIssuer{now: func() time.Time { return *clock }}
	workload := interceptor.NewClient(fake.NewClientBuilder().WithScheme(remoteScheme).Build(), interceptor.Funcs{SubResourceCreate: issuer.create})
	config := &rest.Config{Host: "https://10.0.0.20:6443", CAData: []byte("the cluster CA")}
	h := &HealthIdentity{
		Client: r.Client, ClassNamespace: r.ClassNamespace,
		Remotes: &Remotes{byCluster: map[string]remote{remoteKey(c): {config: config, client: workload, until: clock.Add(24 * time.Hour)}}},
		now:     func() time.Time { return *clock },
	}
	return healthFixture{h: h, kube: kube, workload: workload, issuer: issuer}
}

func reconcileHealth(t *testing.T, h *HealthIdentity, c *clusterv1.Cluster) ctrl.Result {
	t.Helper()
	result, err := h.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHealthTokensBesideTheCluster(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c := initialised(storageCluster())
	f := newHealthIdentity(t, c, &clock)
	h, kube, issuer := f.h, f.kube, f.issuer

	if got := reconcileHealth(t, h, c); got.RequeueAfter != healthRenewal {
		t.Errorf("requeued after %v, want %v", got.RequeueAfter, healthRenewal)
	}
	for _, name := range []string{HealthSecretName(c.Name), HealthLogsSecretName(c.Name)} {
		s := &corev1.Secret{}
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: name}, s); err != nil {
			t.Fatal(err)
		}
		if string(s.Data[HealthServerKey]) != "https://10.0.0.20:6443" || string(s.Data[HealthCAKey]) != "the cluster CA" || len(s.Data[HealthTokenKey]) == 0 {
			t.Errorf("%s holds %q", name, s.Data)
		}
		if s.Annotations[HealthExpiresAnnotation] != "2026-10-22T10:00:00Z" {
			t.Errorf("%s runs out at %q, want an hour from now", name, s.Annotations[HealthExpiresAnnotation])
		}
		if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != c.Name {
			t.Errorf("%s is owned by %v, want the Cluster, so it goes with it", name, s.OwnerReferences)
		}
	}
	if issuer.count() != 2 {
		t.Fatalf("%d tokens made, want one per identity", issuer.count())
	}

	// Looked at again before the renewal is due, nothing is asked of the cluster.
	clock = clock.Add(5 * time.Minute)
	if got := reconcileHealth(t, h, c); got.RequeueAfter != 15*time.Minute || issuer.count() != 2 {
		t.Errorf("after 5 minutes: requeued after %v with %d tokens, want 15m and no new ones", got.RequeueAfter, issuer.count())
	}
	clock = clock.Add(16 * time.Minute)
	reconcileHealth(t, h, c)
	if issuer.count() != 4 {
		t.Errorf("after 21 minutes, %d tokens, want both renewed", issuer.count())
	}
}

func TestHealthIdentityWaitsForTheControlPlane(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c := storageCluster()
	f := newHealthIdentity(t, c, &clock)
	h, kube, issuer := f.h, f.kube, f.issuer
	if got := reconcileHealth(t, h, c); got.RequeueAfter != waitInterval || issuer.count() != 0 {
		t.Errorf("requeued after %v with %d tokens, want %v and none", got.RequeueAfter, issuer.count(), waitInterval)
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: HealthSecretName(c.Name)}, &corev1.Secret{}); err == nil {
		t.Error("a health Secret before the control plane is up")
	}
}

// The health identity reads states and nothing they hold; the logs identity reads logs and nothing
// else. Neither writes.
func TestHealthIdentitiesReadOnly(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c := initialised(storageCluster())
	f := newHealthIdentity(t, c, &clock)
	h, workload := f.h, f.workload
	reconcileHealth(t, h, c)

	rules := func(name string) []rbacv1.PolicyRule {
		t.Helper()
		role := &rbacv1.ClusterRole{}
		if err := workload.Get(context.Background(), client.ObjectKey{Name: name}, role); err != nil {
			t.Fatal(err)
		}
		binding := &rbacv1.ClusterRoleBinding{}
		if err := workload.Get(context.Background(), client.ObjectKey{Name: name}, binding); err != nil {
			t.Fatal(err)
		}
		want := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: metav1.NamespaceSystem}}
		if binding.RoleRef.Name != name || !slices.Equal(binding.Subjects, want) {
			t.Errorf("%s binds %v to %v", name, binding.RoleRef, binding.Subjects)
		}
		sa := &corev1.ServiceAccount{}
		if err := workload.Get(context.Background(), client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: name}, sa); err != nil {
			t.Fatal(err)
		}
		if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
			t.Errorf("%s has its token mounted into pods", name)
		}
		return role.Rules
	}
	for _, r := range rules(healthAccount) {
		if slices.ContainsFunc(r.Verbs, func(v string) bool { return v != "get" && v != "list" }) {
			t.Errorf("the health identity may %v %v", r.Verbs, r.Resources)
		}
		if slices.Contains(r.Resources, "secrets") || slices.Contains(r.Resources, "pods/log") || slices.Contains(r.Resources, "pods/exec") || slices.Contains(r.Resources, "*") {
			t.Errorf("the health identity may read %v", r.Resources)
		}
	}
	if got := rules(healthLogsAccount); len(got) != 1 || !slices.Equal(got[0].Resources, []string{"pods/log"}) || !slices.Equal(got[0].Verbs, []string{"get"}) {
		t.Errorf("the logs identity may do %+v, want get pods/log alone", got)
	}
}
