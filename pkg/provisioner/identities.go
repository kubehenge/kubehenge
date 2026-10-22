package provisioner

import (
	"context"
	"fmt"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Identity is one of the identities the interface reaches inside a workload cluster with: a
// ServiceAccount there named kubehenge-<identity>, whose token is kept beside the Cluster in the
// Secret <cluster>-<identity>. Each does one job, so that what serves one purpose cannot do
// another's whatever the interface does with it, and the workload cluster's audit log tells them
// apart. None may read a Secret or exec.
type Identity string

const (
	// IdentityHealth reads the state of nodes, pods, events and volume claims, which the interface
	// sums up for anyone who may see the cluster.
	IdentityHealth Identity = "health"
	// IdentityLogs reads pods' logs and nothing else, for those the interface lets see them.
	IdentityLogs Identity = "health-logs"
	// IdentityBackups lists Velero's backups and restores and starts a backup, for those who may
	// see or change a cluster that backs up.
	IdentityBackups Identity = "backups"
	// IdentityRestores starts a restore, for those the interface lets restore.
	IdentityRestores Identity = "restores"
)

// IdentitySecretName is the Secret beside a Cluster holding one of its identities' tokens.
func IdentitySecretName(cluster string, id Identity) string { return cluster + "-" + string(id) }

// The keys of an identity's Secret: the workload cluster's API server, its CA and the token.
const (
	IdentityServerKey = "server"
	IdentityCAKey     = "ca.crt"
	IdentityTokenKey  = "token"
	// IdentityExpiresAnnotation is when an identity's token runs out, in RFC 3339.
	IdentityExpiresAnnotation = "kubehenge.dev/token-expires"
)

const (
	// tokenLifetime is how long an identity's token is good for, so one that leaks is of use for
	// no longer.
	tokenLifetime = time.Hour
	// tokenRenewal is how often each token is made again: a token stays good for 40 minutes after
	// the controller last reached the cluster.
	tokenRenewal = 20 * time.Minute
)

// workloadIdentity is how one identity is kept in the workload clusters.
type workloadIdentity struct {
	id Identity
	// namespace holds the ServiceAccount; the identity waits while it does not exist.
	namespace string
	// clusterWide grants the rules with a ClusterRole, which rules on nodes and the API server's
	// paths need; otherwise with a Role in the namespace.
	clusterWide bool
	rules       []rbacv1.PolicyRule
	// wanted says whether a cluster has the identity. One that no longer does loses its Secret;
	// what it is in the workload cluster goes with its namespace.
	wanted func(*clusterv1.Cluster) (bool, error)
}

func (w *workloadIdentity) account() string { return "kubehenge-" + string(w.id) }

// workloadIdentities are the identities kept in every workload cluster that wants them.
var workloadIdentities = []workloadIdentity{
	{
		id: IdentityHealth, namespace: metav1.NamespaceSystem, clusterWide: true, wanted: always,
		rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes", "pods", "events", "persistentvolumeclaims"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: []string{"get", "list"}},
			{NonResourceURLs: []string{"/readyz", "/readyz/*"}, Verbs: []string{"get"}},
		},
	},
	{
		// The logs and no pod itself.
		id: IdentityLogs, namespace: metav1.NamespaceSystem, clusterWide: true, wanted: always,
		rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}}},
	},
	{
		// The provisioner makes the velero namespace before Velero's chart, and removes it with
		// Velero.
		id: IdentityBackups, namespace: VeleroNamespace, wanted: backsUp,
		rules: []rbacv1.PolicyRule{
			{APIGroups: []string{veleroAPIGroup}, Resources: []string{"backups", "restores", "schedules", "backupstoragelocations", "podvolumebackups", "podvolumerestores"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{veleroAPIGroup}, Resources: []string{"backups"}, Verbs: []string{"create"}},
		},
	},
	{
		// A restore of a backup it can read.
		id: IdentityRestores, namespace: VeleroNamespace, wanted: backsUp,
		rules: []rbacv1.PolicyRule{
			{APIGroups: []string{veleroAPIGroup}, Resources: []string{"backups", "restores"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{veleroAPIGroup}, Resources: []string{"restores"}, Verbs: []string{"create"}},
		},
	},
}

func always(*clusterv1.Cluster) (bool, error) { return true, nil }

func backsUp(cluster *clusterv1.Cluster) (bool, error) {
	b, _, err := optionalVariable[Backups](cluster, backupsVariable)
	return b.Enabled, err
}

// WorkloadIdentities keeps the interface's identities in every workload cluster that wants them,
// and short-lived tokens for them in Secrets beside the Cluster, which the interface reads. A token
// is made only with the TokenRequest API, never stored in the workload cluster, and lasts an hour.
type WorkloadIdentities struct {
	Client         client.Client
	ClassNamespace string
	// Remotes reaches the workload clusters.
	Remotes *Remotes

	now func() time.Time
}

// SetupWithManager registers the reconciler for Clusters. Its own requeue renews the tokens; a new
// generation or annotation only has it look sooner, such as once the control plane is up or an
// add-on is switched on or off.
func (w *WorkloadIdentities) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Named("workload-identities").
		Complete(w)
	if err != nil {
		return fmt.Errorf("setting up the workload identities: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler.
func (w *WorkloadIdentities) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := w.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	if !Owns(cluster, w.ClassNamespace) || !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	wanted, err := w.removeUnwanted(ctx, cluster)
	if err != nil || len(wanted) == 0 {
		return ctrl.Result{}, err
	}
	// Until then the API server may not answer, and a node or pod to read is yet to come.
	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	due, next, err := w.renewalDue(ctx, cluster, wanted, w.clock())
	if err != nil || len(due) == 0 {
		return ctrl.Result{RequeueAfter: next}, err
	}
	workload, found, err := w.Remotes.get(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	for _, id := range due {
		renewed, err := w.renew(ctx, cluster, workload, id)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !renewed {
			next = min(next, waitInterval)
		}
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

// renew puts the identity into the workload cluster and a new token of it beside the Cluster. It
// reports false while the identity's namespace does not exist.
func (w *WorkloadIdentities) renew(ctx context.Context, cluster *clusterv1.Cluster, workload remote, id *workloadIdentity) (bool, error) {
	ready, err := id.ensure(ctx, workload.client)
	if err != nil {
		w.Remotes.forget(cluster)
		return false, err
	}
	if !ready {
		return false, nil
	}
	token, expires, err := id.requestToken(ctx, workload.client)
	if err != nil {
		w.Remotes.forget(cluster)
		return false, err
	}
	return true, w.writeToken(ctx, cluster, workload, id, token, expires)
}

// removeUnwanted deletes the Secrets of the identities the cluster no longer wants, and returns
// those it does.
func (w *WorkloadIdentities) removeUnwanted(ctx context.Context, cluster *clusterv1.Cluster) ([]*workloadIdentity, error) {
	var wanted []*workloadIdentity
	for i := range workloadIdentities {
		id := &workloadIdentities[i]
		on, err := id.wanted(cluster)
		if err != nil {
			return nil, err
		}
		if on {
			wanted = append(wanted, id)
			continue
		}
		// Read first, so that a cluster that never had the identity costs no delete on every reconcile.
		name := IdentitySecretName(cluster.Name, id.id)
		s := &corev1.Secret{}
		err = w.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, s)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading Secret %s: %w", name, err)
		}
		if err := client.IgnoreNotFound(w.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID})); err != nil {
			return nil, fmt.Errorf("deleting Secret %s: %w", name, err)
		}
	}
	return wanted, nil
}

func (w *WorkloadIdentities) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// renewalDue picks the identities whose token is due now, missing or made more than tokenRenewal
// ago, and says how long the others' stay fresh: tokenRenewal at most.
func (w *WorkloadIdentities) renewalDue(ctx context.Context, cluster *clusterv1.Cluster, ids []*workloadIdentity, now time.Time) ([]*workloadIdentity, time.Duration, error) {
	var due []*workloadIdentity
	next := tokenRenewal
	for _, id := range ids {
		name := IdentitySecretName(cluster.Name, id.id)
		s := &corev1.Secret{}
		err := w.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, s)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, 0, fmt.Errorf("reading Secret %s: %w", name, err)
		}
		expires, ok := tokenExpiry(s)
		left := expires.Add(tokenRenewal - tokenLifetime).Sub(now)
		if err != nil || !ok || left <= 0 {
			due = append(due, id)
			continue
		}
		next = min(next, left)
	}
	return due, next, nil
}

// tokenExpiry is when an identity's Secret's token runs out, or false when it holds none to go by:
// one without the annotation, or with one Kubehenge did not write, is made again.
func tokenExpiry(s *corev1.Secret) (time.Time, bool) {
	expires, err := time.Parse(time.RFC3339, s.Annotations[IdentityExpiresAnnotation])
	return expires, err == nil && len(s.Data[IdentityTokenKey]) > 0
}

// writeToken keeps a token of the identity, with what reaches the cluster, in its Secret beside the
// Cluster and owned by it.
func (w *WorkloadIdentities) writeToken(ctx context.Context, cluster *clusterv1.Cluster, workload remote, id *workloadIdentity, token string, expires time.Time) error {
	s := &corev1.Secret{Namespace: cluster.Namespace, Name: IdentitySecretName(cluster.Name, id.id)}
	return applyOwned(ctx, w.Client, cluster, s, func(got *corev1.Secret) {
		got.Type = corev1.SecretTypeOpaque
		got.Data = map[string][]byte{
			IdentityServerKey: []byte(workload.config.Host),
			IdentityCAKey:     workload.config.CAData,
			IdentityTokenKey:  []byte(token),
		}
		metav1.SetMetaDataAnnotation(&got.ObjectMeta, IdentityExpiresAnnotation, expires.UTC().Format(time.RFC3339))
	})
}

// ensure creates the identity in the workload cluster with its role, and puts back what was changed
// of them. It reports false, with nothing done, while the identity's namespace does not exist.
func (w *workloadIdentity) ensure(ctx context.Context, workload client.Client) (bool, error) {
	if err := workload.Get(ctx, client.ObjectKey{Name: w.namespace}, &corev1.Namespace{}); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading namespace %s: %w", w.namespace, err)
	}
	account := w.account()
	subjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: account, Namespace: w.namespace}}
	type applied struct {
		kind   string
		obj    client.Object
		mutate func()
	}
	sa := &corev1.ServiceAccount{Namespace: w.namespace, Name: account}
	objects := []applied{{"ServiceAccount", sa, func() { sa.AutomountServiceAccountToken = new(false) }}}
	if w.clusterWide {
		role := &rbacv1.ClusterRole{Name: account}
		binding := &rbacv1.ClusterRoleBinding{Name: account}
		ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: account}
		objects = append(objects,
			applied{"ClusterRole", role, func() { role.Rules = w.rules }},
			applied{"ClusterRoleBinding", binding, func() { binding.RoleRef, binding.Subjects = ref, subjects }})
	} else {
		role := &rbacv1.Role{Namespace: w.namespace, Name: account}
		binding := &rbacv1.RoleBinding{Namespace: w.namespace, Name: account}
		ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: account}
		objects = append(objects,
			applied{"Role", role, func() { role.Rules = w.rules }},
			applied{"RoleBinding", binding, func() { binding.RoleRef, binding.Subjects = ref, subjects }})
	}
	for _, o := range objects {
		if _, err := controllerutil.CreateOrUpdate(ctx, workload, o.obj, func() error { o.mutate(); return nil }); err != nil {
			return false, fmt.Errorf("applying %s %s: %w", o.kind, account, err)
		}
	}
	return true, nil
}

// requestToken asks the workload cluster for a new token of the identity, and when it runs out.
func (w *workloadIdentity) requestToken(ctx context.Context, workload client.Client) (string, time.Time, error) {
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		ExpirationSeconds: new(int64(tokenLifetime / time.Second)),
	}}
	account := w.account()
	sa := &corev1.ServiceAccount{Namespace: w.namespace, Name: account}
	if err := workload.SubResource("token").Create(ctx, sa, req); err != nil {
		return "", time.Time{}, fmt.Errorf("requesting a token for %s: %w", account, err)
	}
	if req.Status.Token == "" {
		return "", time.Time{}, fmt.Errorf("the cluster made no token for %s", account)
	}
	return req.Status.Token, req.Status.ExpirationTimestamp.Time, nil
}
