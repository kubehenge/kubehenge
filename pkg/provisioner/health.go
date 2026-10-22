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

// The identities the interface's health page reads a workload cluster with, in its kube-system.
// The health identity lists nodes, pods, events and volume claims, which the interface sums up for
// anyone who may see the cluster; the logs identity reads pods' logs and nothing else, for those the
// interface lets see them. Two accounts rather than one, so that what serves every viewer cannot
// read a log whatever the interface does with it. Neither may write, read a Secret or exec.
const (
	healthAccount     = "kubehenge-health"
	healthLogsAccount = "kubehenge-health-logs"
	// healthTokenLifetime is how long one of their tokens is good for, so one that leaks is of use
	// for no longer.
	healthTokenLifetime = time.Hour
	// healthRenewal is how often their tokens are made again: a token stays good for 40 minutes after
	// the controller last reached the cluster.
	healthRenewal = 20 * time.Minute
)

// The keys of a health Secret: the workload cluster's API server, its CA and the token.
const (
	HealthServerKey = "server"
	HealthCAKey     = "ca.crt"
	HealthTokenKey  = "token"
	// HealthExpiresAnnotation is when a health Secret's token runs out, in RFC 3339.
	HealthExpiresAnnotation = "kubehenge.dev/token-expires"
)

// HealthSecretName is the Secret beside a Cluster holding the health identity's token.
func HealthSecretName(cluster string) string { return cluster + "-health" }

// HealthLogsSecretName is the Secret beside a Cluster holding the logs identity's token.
func HealthLogsSecretName(cluster string) string { return cluster + "-health-logs" }

// healthRules are what the health identity may read: the state of the cluster's nodes, pods and
// volume claims, their events and the API server's readiness.
var healthRules = []rbacv1.PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"nodes", "pods", "events", "persistentvolumeclaims"}, Verbs: []string{"get", "list"}},
	{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: []string{"get", "list"}},
	{NonResourceURLs: []string{"/readyz", "/readyz/*"}, Verbs: []string{"get"}},
}

// healthLogsRules let the logs identity read logs, and no pod itself.
var healthLogsRules = []rbacv1.PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
}

// HealthIdentity keeps the health and logs identities in every workload cluster, and short-lived
// tokens for them in Secrets beside the Cluster, which the interface reads. A token is made only
// with the TokenRequest API, never stored in the workload cluster, and lasts an hour.
type HealthIdentity struct {
	Client         client.Client
	ClassNamespace string
	// Remotes reaches the workload clusters.
	Remotes *Remotes

	now func() time.Time
}

// SetupWithManager registers the reconciler for Clusters. Its own requeue renews the tokens; a new
// generation or annotation only has it look sooner, such as once the control plane is up.
func (h *HealthIdentity) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Named("health-identity").
		Complete(h)
	if err != nil {
		return fmt.Errorf("setting up the health identity: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler.
func (h *HealthIdentity) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := h.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	if !Owns(cluster, h.ClassNamespace) || !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	// Until then the API server may not answer, and a node or pod to read is yet to come.
	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	now := h.clock()
	renew, err := h.renewalDue(ctx, cluster, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if renew > 0 {
		return ctrl.Result{RequeueAfter: renew}, nil
	}
	workload, found, err := h.Remotes.get(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	if err := ensureHealthAccess(ctx, workload.client); err != nil {
		h.Remotes.forget(cluster)
		return ctrl.Result{}, err
	}
	for _, account := range []struct{ name, secret string }{
		{healthAccount, HealthSecretName(cluster.Name)},
		{healthLogsAccount, HealthLogsSecretName(cluster.Name)},
	} {
		token, expires, err := requestToken(ctx, workload.client, account.name)
		if err != nil {
			h.Remotes.forget(cluster)
			return ctrl.Result{}, err
		}
		s := &corev1.Secret{Namespace: cluster.Namespace, Name: account.secret}
		err = applyOwned(ctx, h.Client, cluster, s, func(got *corev1.Secret) {
			got.Type = corev1.SecretTypeOpaque
			got.Data = map[string][]byte{
				HealthServerKey: []byte(workload.config.Host),
				HealthCAKey:     workload.config.CAData,
				HealthTokenKey:  []byte(token),
			}
			if got.Annotations == nil {
				got.Annotations = map[string]string{}
			}
			got.Annotations[HealthExpiresAnnotation] = expires.UTC().Format(time.RFC3339)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: healthRenewal}, nil
}

func (h *HealthIdentity) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// renewalDue says how long both tokens of a cluster are still fresh, or 0 when they are due now:
// one is missing, or was made more than healthRenewal ago.
func (h *HealthIdentity) renewalDue(ctx context.Context, cluster *clusterv1.Cluster, now time.Time) (time.Duration, error) {
	due := healthRenewal
	for _, name := range []string{HealthSecretName(cluster.Name), HealthLogsSecretName(cluster.Name)} {
		s := &corev1.Secret{}
		err := h.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, s)
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("reading Secret %s: %w", name, err)
		}
		expires, ok := tokenExpiry(s)
		if !ok {
			return 0, nil
		}
		left := expires.Add(healthRenewal - healthTokenLifetime).Sub(now)
		if left <= 0 {
			return 0, nil
		}
		due = min(due, left)
	}
	return due, nil
}

// tokenExpiry is when a health Secret's token runs out, or false when it holds none to go by: one
// without the annotation, or with one Kubehenge did not write, is made again.
func tokenExpiry(s *corev1.Secret) (time.Time, bool) {
	expires, err := time.Parse(time.RFC3339, s.Annotations[HealthExpiresAnnotation])
	return expires, err == nil && len(s.Data[HealthTokenKey]) > 0
}

// ensureHealthAccess creates the two identities in the workload cluster with their roles, and puts
// back what was changed of them.
func ensureHealthAccess(ctx context.Context, workload client.Client) error {
	for _, a := range []struct {
		name  string
		rules []rbacv1.PolicyRule
	}{{healthAccount, healthRules}, {healthLogsAccount, healthLogsRules}} {
		sa := &corev1.ServiceAccount{Namespace: metav1.NamespaceSystem, Name: a.name}
		_, err := controllerutil.CreateOrUpdate(ctx, workload, sa, func() error {
			sa.AutomountServiceAccountToken = new(false)
			return nil
		})
		if err != nil {
			return fmt.Errorf("applying ServiceAccount %s: %w", a.name, err)
		}
		role := &rbacv1.ClusterRole{Name: a.name}
		_, err = controllerutil.CreateOrUpdate(ctx, workload, role, func() error {
			role.Rules = a.rules
			return nil
		})
		if err != nil {
			return fmt.Errorf("applying ClusterRole %s: %w", a.name, err)
		}
		binding := &rbacv1.ClusterRoleBinding{Name: a.name}
		_, err = controllerutil.CreateOrUpdate(ctx, workload, binding, func() error {
			binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: a.name}
			binding.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: a.name, Namespace: metav1.NamespaceSystem}}
			return nil
		})
		if err != nil {
			return fmt.Errorf("applying ClusterRoleBinding %s: %w", a.name, err)
		}
	}
	return nil
}

// requestToken asks the workload cluster for a new token of one of its ServiceAccounts in
// kube-system, and when it runs out.
func requestToken(ctx context.Context, workload client.Client, account string) (string, time.Time, error) {
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		ExpirationSeconds: new(int64(healthTokenLifetime / time.Second)),
	}}
	sa := &corev1.ServiceAccount{Namespace: metav1.NamespaceSystem, Name: account}
	if err := workload.SubResource("token").Create(ctx, sa, req); err != nil {
		return "", time.Time{}, fmt.Errorf("requesting a token for %s: %w", account, err)
	}
	if req.Status.Token == "" {
		return "", time.Time{}, fmt.Errorf("the cluster made no token for %s", account)
	}
	return req.Status.Token, req.Status.ExpirationTimestamp.Time, nil
}
