package provisioner

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Identity is one of the identities the interface and the cluster autoscaler reach inside a
// workload cluster with: a ServiceAccount there named kubehenge-<identity>, whose token is kept in
// a Secret of the management cluster. Each does one job, so that what serves one purpose cannot do
// another's whatever the holder does with it, and the workload cluster's audit log tells them
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
	// IdentityAutoscaler is the cluster autoscaler's: it reads what it simulates the scheduler over,
	// drains and taints the nodes it removes, and reports on itself.
	IdentityAutoscaler Identity = "autoscaler"
)

// IdentitySecretName is the Secret beside a Cluster holding one of its identities' tokens.
func IdentitySecretName(cluster string, id Identity) string { return cluster + "-" + string(id) }

// mountedSecretName names a mounted identity's Secret in the namespace of the pods that mount it,
// which hold those of every cluster.
func mountedSecretName(cluster client.ObjectKey, id Identity) string {
	return qualifiedName(cluster.Namespace, cluster.Name) + "-" + string(id)
}

// The keys of an identity's Secret: the workload cluster's API server, its CA and the token.
const (
	IdentityServerKey = "server"
	IdentityCAKey     = "ca.crt"
	IdentityTokenKey  = "token"
	// IdentityKubeconfigKey holds, in a mounted identity's Secret, a kubeconfig that reads the CA and
	// the token from the files beside it.
	IdentityKubeconfigKey = "kubeconfig"
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
	// clusterRules are granted with a ClusterRole, which rules on nodes and the API server's paths
	// need; rules with a Role in the namespace.
	clusterRules []rbacv1.PolicyRule
	rules        []rbacv1.PolicyRule
	// wanted says whether a cluster has the identity. One that no longer does loses its Secret;
	// what it is in the workload cluster goes with its namespace, or with what uses it there.
	wanted func(*clusterv1.Cluster) (bool, error)
	// mounted keeps the Secret for a pod in the management cluster to mount, in
	// WorkloadIdentities.MountNamespace, as a pod mounts Secrets of its own namespace alone. It
	// carries a kubeconfig too, which client-go reads the token through as the token changes.
	mounted bool
}

func (w *workloadIdentity) account() string { return "kubehenge-" + string(w.id) }

// workloadIdentities are the identities kept in every workload cluster that wants them.
var workloadIdentities = []workloadIdentity{
	{
		id: IdentityHealth, namespace: metav1.NamespaceSystem, wanted: always,
		clusterRules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes", "pods", "events", "persistentvolumeclaims"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: []string{"get", "list"}},
			{NonResourceURLs: []string{"/readyz", "/readyz/*"}, Verbs: []string{"get"}},
		},
	},
	{
		// The logs and no pod itself.
		id: IdentityLogs, namespace: metav1.NamespaceSystem, wanted: always,
		clusterRules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}}},
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
	{
		// The autoscaler beside the cluster in the management cluster mounts its token. One inside the
		// cluster runs as the same account, which the AutoscalerReconciler keeps there itself and
		// removes with the autoscaler.
		id: IdentityAutoscaler, namespace: metav1.NamespaceSystem, wanted: autoscalesBeside, mounted: true,
		clusterRules: autoscalerWorkloadRules, rules: autoscalerStatusRules,
	},
}

// identityOf is the identity kept as id.
func identityOf(id Identity) *workloadIdentity {
	for i := range workloadIdentities {
		if workloadIdentities[i].id == id {
			return &workloadIdentities[i]
		}
	}
	panic("no identity " + string(id))
}

func always(*clusterv1.Cluster) (bool, error) { return true, nil }

func backsUp(cluster *clusterv1.Cluster) (bool, error) {
	b, _, err := optionalVariable[Backups](cluster, backupsVariable)
	return b.Enabled, err
}

// autoscalesBeside says whether the cluster's autoscaler runs in the management cluster.
func autoscalesBeside(cluster *clusterv1.Cluster) (bool, error) {
	if !autoscaled(cluster) {
		return false, nil
	}
	s, err := autoscalerSettingsOf(cluster)
	return s.Placement == PlacementManagement, err
}

// WorkloadIdentities keeps the identities in every workload cluster that wants them, and
// short-lived tokens for them in Secrets of the management cluster, which the interface reads and
// the autoscalers mount. A token is made only with the TokenRequest API, never stored in the
// workload cluster, and lasts an hour.
type WorkloadIdentities struct {
	Client         client.Client
	ClassNamespace string
	// MountNamespace holds the Secrets of the mounted identities, beside the pods that mount them:
	// the autoscalers' namespace.
	MountNamespace string
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
			return ctrl.Result{}, w.removeMounted(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	if !Owns(cluster, w.ClassNamespace) {
		return ctrl.Result{}, nil
	}
	// The other Secrets are the Cluster's and go with it.
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, w.removeMounted(ctx, req.NamespacedName)
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

// renew puts the identity into the workload cluster and a new token of it into its Secret. It
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
	token, expires, err := requestToken(ctx, workload.client, id.namespace, id.account())
	if err != nil {
		w.Remotes.forget(cluster)
		return false, err
	}
	return true, w.writeToken(ctx, cluster, workload, id, token, expires)
}

// secretKey is where the cluster's Secret of the identity is kept.
func (w *WorkloadIdentities) secretKey(cluster client.ObjectKey, id *workloadIdentity) client.ObjectKey {
	if id.mounted {
		return client.ObjectKey{Namespace: w.MountNamespace, Name: mountedSecretName(cluster, id.id)}
	}
	return client.ObjectKey{Namespace: cluster.Namespace, Name: IdentitySecretName(cluster.Name, id.id)}
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
		if err := w.removeSecret(ctx, w.secretKey(client.ObjectKeyFromObject(cluster), id)); err != nil {
			return nil, err
		}
	}
	return wanted, nil
}

// removeMounted deletes a cluster's Secrets of the mounted identities, which, outside the Cluster's
// namespace, it cannot own.
func (w *WorkloadIdentities) removeMounted(ctx context.Context, cluster client.ObjectKey) error {
	for i := range workloadIdentities {
		if id := &workloadIdentities[i]; id.mounted {
			if err := w.removeSecret(ctx, w.secretKey(cluster, id)); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeSecret deletes a Secret. It reads first, so that one never made costs no delete on every
// reconcile.
func (w *WorkloadIdentities) removeSecret(ctx context.Context, key client.ObjectKey) error {
	s := &corev1.Secret{}
	err := w.Client.Get(ctx, key, s)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading Secret %s: %w", key, err)
	}
	if err := client.IgnoreNotFound(w.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID})); err != nil {
		return fmt.Errorf("deleting Secret %s: %w", key, err)
	}
	return nil
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
		key := w.secretKey(client.ObjectKeyFromObject(cluster), id)
		s := &corev1.Secret{}
		if err := w.Client.Get(ctx, key, s); err != nil && !apierrors.IsNotFound(err) {
			return nil, 0, fmt.Errorf("reading Secret %s: %w", key, err)
		}
		left := tokenLeft(s, now)
		if left == 0 {
			due = append(due, id)
			continue
		}
		next = min(next, left)
	}
	return due, next, nil
}

// tokenLeft says how long a token's Secret stays fresh, or 0 once a new token is due: tokenRenewal
// after it was made, or at once for one that holds none to go by, such as one without the
// annotation, or with one Kubehenge did not write.
func tokenLeft(s *corev1.Secret, now time.Time) time.Duration {
	expires, err := time.Parse(time.RFC3339, s.Annotations[IdentityExpiresAnnotation])
	if err != nil || len(s.Data[IdentityTokenKey]) == 0 {
		return 0
	}
	return max(expires.Add(tokenRenewal-tokenLifetime).Sub(now), 0)
}

// writeToken keeps a token of the identity, with what reaches the cluster, in its Secret: beside
// the Cluster and owned by it, or, mounted, labelled with the cluster.
func (w *WorkloadIdentities) writeToken(ctx context.Context, cluster *clusterv1.Cluster, workload remote, id *workloadIdentity, token string, expires time.Time) error {
	key := client.ObjectKeyFromObject(cluster)
	at := w.secretKey(key, id)
	data := tokenData(workload.config.Host, workload.config.CAData, token)
	s := &corev1.Secret{Namespace: at.Namespace, Name: at.Name}
	if !id.mounted {
		return applyOwned(ctx, w.Client, cluster, s, func(got *corev1.Secret) { setToken(got, data, expires) })
	}
	if err := withKubeconfig(data); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, w.Client, s, func() error {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		maps.Copy(s.Labels, map[string]string{ClusterNamespaceLabel: key.Namespace, ClusterNameLabel: key.Name})
		setToken(s, data, expires)
		return nil
	})
	if err != nil {
		return fmt.Errorf("applying Secret %s: %w", at, err)
	}
	return nil
}

// tokenData is what a token's Secret holds: the token, the server it is for and its CA.
func tokenData(server string, ca []byte, token string) map[string][]byte {
	return map[string][]byte{IdentityServerKey: []byte(server), IdentityCAKey: ca, IdentityTokenKey: []byte(token)}
}

// withKubeconfig adds to a token's data, for a pod to mount, a kubeconfig that reads the CA and the
// token from the files beside it. client-go resolves a kubeconfig's relative paths against its own
// directory and reads a token file again as it changes, so a pod takes each new token without a
// restart.
func withKubeconfig(data map[string][]byte) error {
	server := string(data[IdentityServerKey])
	config := clientcmdapi.NewConfig()
	config.Clusters["cluster"] = &clientcmdapi.Cluster{Server: server, CertificateAuthority: IdentityCAKey}
	config.AuthInfos["identity"] = &clientcmdapi.AuthInfo{TokenFile: IdentityTokenKey}
	config.Contexts["cluster"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "identity"}
	config.CurrentContext = "cluster"
	written, err := clientcmd.Write(*config)
	if err != nil {
		return fmt.Errorf("writing a kubeconfig for %s: %w", server, err)
	}
	data[IdentityKubeconfigKey] = written
	return nil
}

// setToken makes a Secret hold a token's data and when the token runs out.
func setToken(s *corev1.Secret, data map[string][]byte, expires time.Time) {
	s.Type = corev1.SecretTypeOpaque
	s.Data = data
	metav1.SetMetaDataAnnotation(&s.ObjectMeta, IdentityExpiresAnnotation, expires.UTC().Format(time.RFC3339))
}

// identityObject is one of what an identity is in a workload cluster, with what puts it right.
type identityObject struct {
	kind   string
	obj    client.Object
	mutate func()
}

// objects are what the identity is in a workload cluster: its ServiceAccount, then each role with
// its binding.
func (w *workloadIdentity) objects() []identityObject {
	account := w.account()
	subjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: account, Namespace: w.namespace}}
	sa := &corev1.ServiceAccount{Namespace: w.namespace, Name: account}
	objects := []identityObject{{"ServiceAccount", sa, func() { sa.AutomountServiceAccountToken = new(false) }}}
	if len(w.clusterRules) > 0 {
		role := &rbacv1.ClusterRole{Name: account}
		binding := &rbacv1.ClusterRoleBinding{Name: account}
		ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: account}
		objects = append(objects,
			identityObject{"ClusterRole", role, func() { role.Rules = w.clusterRules }},
			identityObject{"ClusterRoleBinding", binding, func() { binding.RoleRef, binding.Subjects = ref, subjects }})
	}
	if len(w.rules) > 0 {
		role := &rbacv1.Role{Namespace: w.namespace, Name: account}
		binding := &rbacv1.RoleBinding{Namespace: w.namespace, Name: account}
		ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: account}
		objects = append(objects,
			identityObject{"Role", role, func() { role.Rules = w.rules }},
			identityObject{"RoleBinding", binding, func() { binding.RoleRef, binding.Subjects = ref, subjects }})
	}
	return objects
}

// ensure creates the identity in the workload cluster with its roles, and puts back what was
// changed of them. It reports false, with nothing done, while the identity's namespace does not
// exist.
func (w *workloadIdentity) ensure(ctx context.Context, workload client.Client) (bool, error) {
	if err := workload.Get(ctx, client.ObjectKey{Name: w.namespace}, &corev1.Namespace{}); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading namespace %s: %w", w.namespace, err)
	}
	for _, o := range w.objects() {
		if _, err := controllerutil.CreateOrUpdate(ctx, workload, o.obj, func() error { o.mutate(); return nil }); err != nil {
			return false, fmt.Errorf("applying %s %s: %w", o.kind, w.account(), err)
		}
	}
	return true, nil
}

// remove deletes the identity from the workload cluster: its rights first, its account last.
func (w *workloadIdentity) remove(ctx context.Context, workload client.Client) error {
	for _, o := range slices.Backward(w.objects()) {
		if err := client.IgnoreNotFound(workload.Delete(ctx, o.obj)); err != nil {
			return fmt.Errorf("deleting %s %s: %w", o.kind, w.account(), err)
		}
	}
	return nil
}

// requestToken asks a cluster for a new token of a ServiceAccount, good for tokenLifetime, and when
// it runs out.
func requestToken(ctx context.Context, c client.Client, namespace, account string) (string, time.Time, error) {
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		ExpirationSeconds: new(int64(tokenLifetime / time.Second)),
	}}
	sa := &corev1.ServiceAccount{Namespace: namespace, Name: account}
	if err := c.SubResource("token").Create(ctx, sa, req); err != nil {
		return "", time.Time{}, fmt.Errorf("requesting a token for %s: %w", account, err)
	}
	if req.Status.Token == "" {
		return "", time.Time{}, fmt.Errorf("the cluster made no token for %s", account)
	}
	return req.Status.Token, req.Status.ExpirationTimestamp.Time, nil
}
