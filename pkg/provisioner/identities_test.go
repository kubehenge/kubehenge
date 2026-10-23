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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// identitiesFixture is a WorkloadIdentities with the management and workload clusters it acts on,
// and what issues the workload cluster's tokens.
type identitiesFixture struct {
	w        *WorkloadIdentities
	kube     client.Client
	workload client.Client
	issuer   *tokenIssuer
}

func newIdentities(t *testing.T, c *clusterv1.Cluster, clock *time.Time) identitiesFixture {
	t.Helper()
	r, kube := newReconciler(t, &fakeCloud{}, c)
	issuer := &tokenIssuer{now: func() time.Time { return *clock }}
	inside := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(&corev1.Namespace{Name: metav1.NamespaceSystem}).Build()
	workload := interceptor.NewClient(inside, interceptor.Funcs{SubResourceCreate: issuer.create})
	config := &rest.Config{Host: "https://10.0.0.20:6443", CAData: []byte("the cluster CA")}
	w := &WorkloadIdentities{
		Client: r.Client, ClassNamespace: r.ClassNamespace,
		Remotes: &Remotes{byCluster: map[string]remote{remoteKey(c): {config: config, client: workload, until: time.Now().Add(time.Hour)}}},
		now:     func() time.Time { return *clock },
	}
	return identitiesFixture{w: w, kube: kube, workload: workload, issuer: issuer}
}

func reconcileIdentities(t *testing.T, w *WorkloadIdentities, c *clusterv1.Cluster) ctrl.Result {
	t.Helper()
	result, err := w.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// secretsBeside says which of the identities' Secrets are beside the cluster.
func secretsBeside(t *testing.T, kube client.Client, c *clusterv1.Cluster, ids ...Identity) []Identity {
	t.Helper()
	var found []Identity
	for _, id := range ids {
		err := kube.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: IdentitySecretName(c.Name, id)}, &corev1.Secret{})
		switch {
		case err == nil:
			found = append(found, id)
		case !apierrors.IsNotFound(err):
			t.Fatal(err)
		}
	}
	return found
}

func TestIdentityTokensBesideTheCluster(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c := initialised(storageCluster())
	f := newIdentities(t, c, &clock)
	w, kube, issuer := f.w, f.kube, f.issuer

	if got := reconcileIdentities(t, w, c); got.RequeueAfter != tokenRenewal {
		t.Errorf("requeued after %v, want %v", got.RequeueAfter, tokenRenewal)
	}
	for _, name := range []string{"web-health", "web-health-logs"} {
		s := &corev1.Secret{}
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: name}, s); err != nil {
			t.Fatal(err)
		}
		if string(s.Data[IdentityServerKey]) != "https://10.0.0.20:6443" || string(s.Data[IdentityCAKey]) != "the cluster CA" || len(s.Data[IdentityTokenKey]) == 0 {
			t.Errorf("%s holds %q", name, s.Data)
		}
		if s.Annotations[IdentityExpiresAnnotation] != "2026-10-22T10:00:00Z" {
			t.Errorf("%s runs out at %q, want an hour from now", name, s.Annotations[IdentityExpiresAnnotation])
		}
		if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != c.Name {
			t.Errorf("%s is owned by %v, want the Cluster, so it goes with it", name, s.OwnerReferences)
		}
	}
	if issuer.count() != 2 {
		t.Fatalf("%d tokens made, want one per identity", issuer.count())
	}
	if got := secretsBeside(t, kube, c, IdentityBackups, IdentityRestores); len(got) != 0 {
		t.Errorf("%v beside a cluster that does not back up", got)
	}

	// Looked at again before the renewal is due, nothing is asked of the cluster.
	clock = clock.Add(5 * time.Minute)
	if got := reconcileIdentities(t, w, c); got.RequeueAfter != 15*time.Minute || issuer.count() != 2 {
		t.Errorf("after 5 minutes: requeued after %v with %d tokens, want 15m and no new ones", got.RequeueAfter, issuer.count())
	}
	clock = clock.Add(16 * time.Minute)
	reconcileIdentities(t, w, c)
	if issuer.count() != 4 {
		t.Errorf("after 21 minutes, %d tokens, want both renewed", issuer.count())
	}
}

func TestIdentitiesWaitForTheControlPlane(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c := storageCluster()
	f := newIdentities(t, c, &clock)
	w, kube, issuer := f.w, f.kube, f.issuer
	if got := reconcileIdentities(t, w, c); got.RequeueAfter != waitInterval || issuer.count() != 0 {
		t.Errorf("requeued after %v with %d tokens, want %v and none", got.RequeueAfter, issuer.count(), waitInterval)
	}
	if got := secretsBeside(t, kube, c, IdentityHealth, IdentityLogs); len(got) != 0 {
		t.Errorf("%v before the control plane is up", got)
	}
}

// clusterRights reads the rules an identity in kube-system is granted cluster-wide.
func clusterRights(t *testing.T, workload client.Client, name string) []rbacv1.PolicyRule {
	t.Helper()
	role, binding := &rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}
	for _, o := range []client.Object{role, binding} {
		if err := workload.Get(context.Background(), client.ObjectKey{Name: name}, o); err != nil {
			t.Fatal(err)
		}
	}
	checkBinding(t, workload, metav1.NamespaceSystem, name, binding.RoleRef, binding.Subjects)
	return role.Rules
}

// namespacedRights reads the rules an identity is granted in its own namespace.
func namespacedRights(t *testing.T, workload client.Client, namespace, name string) []rbacv1.PolicyRule {
	t.Helper()
	role, binding := &rbacv1.Role{}, &rbacv1.RoleBinding{}
	for _, o := range []client.Object{role, binding} {
		if err := workload.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, o); err != nil {
			t.Fatal(err)
		}
	}
	checkBinding(t, workload, namespace, name, binding.RoleRef, binding.Subjects)
	return role.Rules
}

// checkBinding checks that an identity's role is bound to its ServiceAccount alone, whose token is
// mounted into no pod.
func checkBinding(t *testing.T, workload client.Client, namespace, name string, ref rbacv1.RoleRef, subjects []rbacv1.Subject) {
	t.Helper()
	want := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: namespace}}
	if ref.Name != name || !slices.Equal(subjects, want) {
		t.Errorf("%s binds %v to %v", name, ref, subjects)
	}
	sa := &corev1.ServiceAccount{}
	if err := workload.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, sa); err != nil {
		t.Fatal(err)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Errorf("%s has its token mounted into pods", name)
	}
}

// The health identity reads states and nothing they hold; the logs identity reads logs and nothing
// else. Neither writes.
func TestHealthIdentitiesReadOnly(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c := initialised(storageCluster())
	f := newIdentities(t, c, &clock)
	reconcileIdentities(t, f.w, c)

	for _, r := range clusterRights(t, f.workload, "kubehenge-health") {
		if slices.ContainsFunc(r.Verbs, func(v string) bool { return v != "get" && v != "list" }) {
			t.Errorf("the health identity may %v %v", r.Verbs, r.Resources)
		}
		if slices.Contains(r.Resources, "secrets") || slices.Contains(r.Resources, "pods/log") || slices.Contains(r.Resources, "pods/exec") || slices.Contains(r.Resources, "*") {
			t.Errorf("the health identity may read %v", r.Resources)
		}
	}
	if got := clusterRights(t, f.workload, "kubehenge-health-logs"); len(got) != 1 || !slices.Equal(got[0].Resources, []string{"pods/log"}) || !slices.Equal(got[0].Verbs, []string{"get"}) {
		t.Errorf("the logs identity may do %+v, want get pods/log alone", got)
	}
}

// The backup identities come once the velero namespace is there, may do nothing outside Velero's
// resources, and go when backups are switched off.
func TestBackupIdentitiesGoWithBackups(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c := initialised(backupsCluster(`{"enabled":true,"target":"store"}`))
	f := newIdentities(t, c, &clock)
	ctx := context.Background()

	if got := reconcileIdentities(t, f.w, c); got.RequeueAfter != waitInterval || f.issuer.count() != 2 {
		t.Errorf("before Velero's namespace: requeued after %v with %d tokens, want %v and the health identities'", got.RequeueAfter, f.issuer.count(), waitInterval)
	}
	if got := secretsBeside(t, f.kube, c, IdentityBackups, IdentityRestores); len(got) != 0 {
		t.Errorf("%v before Velero's namespace", got)
	}
	if err := f.workload.Create(ctx, &corev1.Namespace{Name: VeleroNamespace}); err != nil {
		t.Fatal(err)
	}
	if got := reconcileIdentities(t, f.w, c); got.RequeueAfter != tokenRenewal || f.issuer.count() != 4 {
		t.Errorf("requeued after %v with %d tokens, want %v and the backup identities' besides", got.RequeueAfter, f.issuer.count(), tokenRenewal)
	}
	if got := secretsBeside(t, f.kube, c, IdentityBackups, IdentityRestores); len(got) != 2 {
		t.Fatalf("only %v beside the cluster", got)
	}

	creates := func(name string) []string {
		t.Helper()
		var made []string
		for _, r := range namespacedRights(t, f.workload, VeleroNamespace, name) {
			if !slices.Equal(r.APIGroups, []string{veleroAPIGroup}) || len(r.NonResourceURLs) > 0 || len(r.ResourceNames) > 0 {
				t.Errorf("%s may reach beyond Velero's resources: %+v", name, r)
			}
			for _, v := range r.Verbs {
				switch v {
				case "get", "list":
				case "create":
					made = append(made, r.Resources...)
				default:
					t.Errorf("%s may %s %v", name, v, r.Resources)
				}
			}
		}
		return made
	}
	if got := creates("kubehenge-backups"); !slices.Equal(got, []string{"backups"}) {
		t.Errorf("the backups identity may create %v, want backups alone", got)
	}
	if got := creates("kubehenge-restores"); !slices.Equal(got, []string{"restores"}) {
		t.Errorf("the restores identity may create %v, want restores alone", got)
	}

	current := &clusterv1.Cluster{}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	for i := range current.Spec.Topology.Variables {
		if current.Spec.Topology.Variables[i].Name == backupsVariable {
			current.Spec.Topology.Variables[i].Value.Raw = []byte(`{"enabled":false}`)
		}
	}
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileIdentities(t, f.w, current)
	if got := secretsBeside(t, f.kube, c, IdentityBackups, IdentityRestores); len(got) != 0 {
		t.Errorf("%v kept with backups off", got)
	}
	if got := secretsBeside(t, f.kube, c, IdentityHealth, IdentityLogs); len(got) != 2 {
		t.Errorf("only %v kept with backups off, want the health identities'", got)
	}
}

// Each token is renewed on its own once it is due, so that one identity that waits leaves the
// others' alone.
func TestIdentityRenewal(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := backupsCluster(`{"enabled":true,"target":"store"}`)
	token := func(id Identity, expires time.Time) client.Object {
		return &corev1.Secret{
			Namespace: c.Namespace, Name: IdentitySecretName(c.Name, id),
			Annotations: map[string]string{IdentityExpiresAnnotation: expires.UTC().Format(time.RFC3339)},
			Data:        map[string][]byte{IdentityTokenKey: []byte("token")},
		}
	}
	var ids []*workloadIdentity
	for i := range workloadIdentities {
		if id := &workloadIdentities[i]; id.id == IdentityBackups || id.id == IdentityRestores {
			ids = append(ids, id)
		}
	}
	for _, tc := range []struct {
		name    string
		secrets []client.Object
		due     []Identity
		next    time.Duration
	}{
		{"none yet", nil, []Identity{IdentityBackups, IdentityRestores}, tokenRenewal},
		{"one missing", []client.Object{token(IdentityBackups, now.Add(time.Hour))}, []Identity{IdentityRestores}, tokenRenewal},
		{"fresh", []client.Object{token(IdentityBackups, now.Add(time.Hour)), token(IdentityRestores, now.Add(time.Hour))}, nil, tokenRenewal},
		{"one older", []client.Object{token(IdentityBackups, now.Add(50*time.Minute)), token(IdentityRestores, now.Add(time.Hour))}, nil, 10 * time.Minute},
		{"one due", []client.Object{token(IdentityBackups, now.Add(30*time.Minute)), token(IdentityRestores, now.Add(time.Hour))}, []Identity{IdentityBackups}, tokenRenewal},
	} {
		_, kube := newReconciler(t, &fakeCloud{}, tc.secrets...)
		w := &WorkloadIdentities{Client: kube}
		due, next, err := w.renewalDue(context.Background(), c, ids, now)
		var got []Identity
		for _, id := range due {
			got = append(got, id.id)
		}
		if err != nil || !slices.Equal(got, tc.due) || next != tc.next {
			t.Errorf("%s: %v due, the rest in %v, %v; want %v and %v", tc.name, got, next, err, tc.due, tc.next)
		}
	}
}
