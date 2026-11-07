package provisioner

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// NodeCustomisationVariable is the cluster variable that adds files, commands, arguments, an
	// audit policy and an audit webhook to a cluster's nodes.
	NodeCustomisationVariable = "nodeCustomisation"

	// auditPolicyConfigMapKey is the key of a ConfigMap that holds an audit policy.
	auditPolicyConfigMapKey = "policy.yaml"
	// auditPolicyMirrorKey is the key of the Secret Kubehenge's controller mirrors the policy into,
	// since a bootstrap template reads Secrets and no ConfigMap.
	auditPolicyMirrorKey = "audit-policy.yaml"
)

// NodeCustomisation is what a company adds to the nodes of a cluster. The class's patches append it
// after Kubehenge's own settings.
type NodeCustomisation struct {
	Files                 []NodeFile        `json:"files,omitempty"`
	Commands              []NodeCommand     `json:"commands,omitempty"`
	KubeletArgs           []NodeArg         `json:"kubeletArgs,omitempty"`
	APIServerArgs         []NodeArg         `json:"apiServerArgs,omitempty"`
	ControllerManagerArgs []NodeArg         `json:"controllerManagerArgs,omitempty"`
	SchedulerArgs         []NodeArg         `json:"schedulerArgs,omitempty"`
	AuditPolicy           *NodeAuditPolicy  `json:"auditPolicy,omitempty"`
	AuditWebhook          *NodeAuditWebhook `json:"auditWebhook,omitempty"`
}

// NodeFile is a file written to the nodes of its roles.
type NodeFile struct {
	Path        string         `json:"path"`
	Permissions string         `json:"permissions,omitempty"`
	Content     *string        `json:"content,omitempty"`
	Secret      *NodeSecretKey `json:"secret,omitempty"`
	Roles       []string       `json:"roles,omitempty"`
}

// NodeSecretKey is a key of a Secret in the cluster's namespace.
type NodeSecretKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// NodeCommand is a command that runs before the distribution starts.
type NodeCommand struct {
	Run   string   `json:"run"`
	Roles []string `json:"roles,omitempty"`
}

// NodeArg is one command-line argument of a component.
type NodeArg struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// NodeAuditPolicy is the API server's audit policy: a ConfigMap's, or one given in the variable.
type NodeAuditPolicy struct {
	ConfigMap string         `json:"configMap,omitempty"`
	Policy    map[string]any `json:"policy,omitempty"`
}

// NodeAuditWebhook is the Secret that holds the audit webhook's kubeconfig.
type NodeAuditWebhook struct {
	Secret string `json:"secret"`
	Key    string `json:"key,omitempty"`
}

// The components whose arguments the variable adds to.
const (
	componentKubelet           = "kubelet"
	componentAPIServer         = "apiServer"
	componentControllerManager = "controllerManager"
	componentScheduler         = "scheduler"
)

func set(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

var (
	servingFlags    = []string{"tls-cert-file", "tls-private-key-file", "tls-cipher-suites", "tls-min-version", "client-ca-file", "secure-port", "bind-address", "profiling"}
	kubeconfigFlags = []string{"kubeconfig", "authentication-kubeconfig", "authorization-kubeconfig", "authorization-always-allow-paths"} // spellchecker:disable-line

	// protectedFlags are the arguments Kubehenge's classes set for a cluster's identity or security,
	// by component. A test holds them to every argument the rendered classes set.
	protectedFlags = map[string]map[string]bool{
		componentKubelet: set("cloud-provider", "provider-id", "node-ip", "hostname-override", "node-labels", "register-with-taints",
			"register-node", "kubeconfig", "bootstrap-kubeconfig", "config", "cert-dir", "tls-cert-file", "tls-private-key-file",
			"tls-cipher-suites", "tls-min-version", "rotate-server-certificates", "client-ca-file", "anonymous-auth",
			"authorization-mode", "authentication-token-webhook", "read-only-port", "protect-kernel-defaults", "seccomp-default", // spellchecker:disable-line
			"streaming-connection-idle-timeout", "allowed-unsafe-sysctls", "pod-manifest-path", "container-runtime-endpoint"),
		componentAPIServer: set("kubelet-certificate-authority", "admission-control-config-file", "enable-admission-plugins",
			"disable-admission-plugins", "authentication-config", "encryption-provider-config", "audit-policy-file", "audit-log-path",
			"audit-webhook-config-file", "service-account-extend-token-expiration", "profiling", "tls-cipher-suites", "tls-min-version",
			"tls-cert-file", "tls-private-key-file", "client-ca-file", "anonymous-auth", "authorization-mode", "authorization-config", // spellchecker:disable-line
			"authorization-webhook-config-file", "etcd-servers", "etcd-cafile", "etcd-certfile", "etcd-keyfile", // spellchecker:disable-line
			"kubelet-client-certificate", "kubelet-client-key", "kubelet-preferred-address-types", "service-account-issuer",
			"service-account-key-file", "service-account-signing-key-file", "requestheader-client-ca-file", "requestheader-allowed-names",
			"requestheader-extra-headers-prefix", "requestheader-group-headers", "requestheader-username-headers",
			"proxy-client-cert-file", "proxy-client-key-file", "secure-port", "advertise-address", "service-cluster-ip-range",
			"token-auth-file", "insecure-port", "authorization-always-allow-paths"),
		componentControllerManager: set(slices.Concat(servingFlags, kubeconfigFlags, []string{
			"leader-elect", "cloud-provider", "use-service-account-credentials", "root-ca-file", "service-account-private-key-file",
			"cluster-signing-cert-file", "cluster-signing-key-file", "cluster-cidr", "service-cluster-ip-range", "allocate-node-cidrs",
		})...),
		componentScheduler: set(slices.Concat(servingFlags, kubeconfigFlags, []string{"leader-elect", "config"})...),
	}

	// protectedPrefixes are the argument families Kubehenge sets whole: the leader election of the
	// controller manager and the scheduler, and the OIDC arguments of the API server, which the
	// authentication configuration carries.
	protectedPrefixes = map[string][]string{
		componentAPIServer:         {"oidc-", "authorization-webhook-"}, // spellchecker:disable-line
		componentControllerManager: {"leader-elect-"},
		componentScheduler:         {"leader-elect-"},
	}

	// auditRotationFlags are Kubehenge's log rotation, which an audit policy or webhook of the
	// company's may replace.
	auditRotationFlags = set("audit-log-maxage", "audit-log-maxbackup", "audit-log-maxsize")

	// ownAuditFlags have a field of their own.
	ownAuditFlags = map[string]string{
		"audit-policy-file": "auditPolicy", "audit-log-path": "auditPolicy", "audit-webhook-config-file": "auditWebhook",
	}

	// protectedPathPrefixes are the directories and files Kubehenge writes on the nodes of every
	// distribution, and /usr, which is read-only.
	protectedPathPrefixes = []string{
		"/etc/kubernetes", "/etc/kubehenge", "/etc/rancher", "/var/lib/rancher", "/var/lib/kubelet", "/etc/sysconfig/kubelet",
		"/etc/sysctl.d/90-kubehenge.conf", "/etc/systemd/system/rke2-server.service.d", "/etc/systemd/system/k3s.service.d",
		"/etc/systemd/system/kubelet.service.d", "/var/log/kubernetes", "/usr",
	}

	// talosPathPrefixes are where Talos writes machine files, and talosProtectedPrefixes what is
	// Talos's own beneath them.
	talosPathPrefixes      = []string{"/var/", "/etc/cri/conf.d/"}
	talosProtectedPrefixes = []string{"/var/lib/etcd", "/var/lib/kubelet", "/var/log/audit", "/var/run", "/var/lib/rancher", "/var/log/kubernetes"}

	nodeRoles = set(roleControlPlane, roleWorker)
)

// ProtectedNodeFlag reports whether the variable refuses an argument of a component (kubelet,
// apiServer, controllerManager or scheduler) in any case. The audit log rotation is refused only
// where no audit field is used (see checkNodeArgs).
func ProtectedNodeFlag(component, name string) bool {
	if protectedFlags[component][name] || (component == componentAPIServer && auditRotationFlags[name]) {
		return true
	}
	return slices.ContainsFunc(protectedPrefixes[component], func(p string) bool { return strings.HasPrefix(name, p) })
}

// protectedNodePath says what Kubehenge keeps at a path on the nodes, or "" where the path is free.
func protectedNodePath(distribution templates.Distribution, p string) string {
	if distribution == templates.Talos {
		return talosPathProblem(p)
	}
	for _, prefix := range protectedPathPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			if prefix == "/usr" {
				return "the nodes' /usr is read-only"
			}
			return "Kubehenge writes under " + prefix
		}
	}
	return ""
}

func talosPathProblem(p string) string {
	if !slices.ContainsFunc(talosPathPrefixes, func(prefix string) bool { return strings.HasPrefix(p, prefix) }) {
		return "Talos writes machine files under /var and /etc/cri/conf.d only"
	}
	for _, prefix := range talosProtectedPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return "Talos keeps " + prefix + " for itself"
		}
	}
	return ""
}

// checkNodeCustomisation holds the variable to what the distribution can carry and to the settings
// Kubehenge keeps for itself.
func checkNodeCustomisation(distribution templates.Distribution, n *NodeCustomisation) field.ErrorList {
	base := field.NewPath("spec", "topology", "variables").Key(NodeCustomisationVariable)
	var errs field.ErrorList
	talos := distribution == templates.Talos
	for i, f := range n.Files {
		at := base.Child("files").Index(i)
		errs = append(errs, checkNodeFile(distribution, f, at)...)
	}
	if talos && len(n.Commands) > 0 {
		errs = append(errs, field.Forbidden(base.Child("commands"), "Talos has no shell, so commands cannot run on its nodes. Write a file or use a Talos extension instead"))
	}
	for i, c := range n.Commands {
		for _, r := range c.Roles {
			if !nodeRoles[r] {
				errs = append(errs, field.NotSupported(base.Child("commands").Index(i).Child("roles"), r, []string{roleControlPlane, roleWorker}))
			}
		}
	}
	errs = append(errs, checkNodeArgs(n, base)...)
	if p := n.AuditPolicy; p != nil && talos && p.ConfigMap != "" {
		errs = append(errs, field.Forbidden(base.Child("auditPolicy", "configMap"), "Talos reads no ConfigMap into its machine configuration. Give the policy in auditPolicy.policy"))
	}
	if talos && n.AuditWebhook != nil {
		errs = append(errs, field.Forbidden(base.Child("auditWebhook"), "Talos reads no Secret into its machine configuration, so its audit webhook cannot be set here"))
	}
	return errs
}

func checkNodeFile(distribution templates.Distribution, f NodeFile, at *field.Path) field.ErrorList {
	var errs field.ErrorList
	if clean := path.Clean(f.Path); clean != f.Path || !path.IsAbs(f.Path) {
		errs = append(errs, field.Invalid(at.Child("path"), f.Path, "give an absolute path without . or .. parts, double slashes or a closing slash"))
	} else if why := protectedNodePath(distribution, f.Path); why != "" {
		errs = append(errs, field.Forbidden(at.Child("path"), fmt.Sprintf("%s is a path Kubehenge keeps: %s", f.Path, why)))
	}
	if distribution == templates.Talos && f.Secret != nil {
		errs = append(errs, field.Forbidden(at.Child("secret"), "Talos reads no Secret into its machine configuration. Give the file's content"))
	}
	for _, r := range f.Roles {
		if !nodeRoles[r] {
			errs = append(errs, field.NotSupported(at.Child("roles"), r, []string{roleControlPlane, roleWorker}))
		}
	}
	return errs
}

// checkNodeArgs refuses the arguments Kubehenge sets, and the audit ones that have a field of their
// own. Kubehenge's audit log rotation may be replaced where the company uses an audit field.
func checkNodeArgs(n *NodeCustomisation, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	auditOwn := n.AuditPolicy != nil || n.AuditWebhook != nil
	for _, c := range []struct {
		component, key string
		args           []NodeArg
	}{
		{componentKubelet, "kubeletArgs", n.KubeletArgs},
		{componentAPIServer, "apiServerArgs", n.APIServerArgs},
		{componentControllerManager, "controllerManagerArgs", n.ControllerManagerArgs},
		{componentScheduler, "schedulerArgs", n.SchedulerArgs},
	} {
		for i, a := range c.args {
			at := base.Child(c.key).Index(i).Child("name")
			name := strings.TrimLeft(a.Name, "-")
			switch own, has := ownAuditFlags[name]; {
			case c.component == componentAPIServer && has:
				errs = append(errs, field.Forbidden(at, fmt.Sprintf("%s is set by Kubehenge. Use %s to change it", name, own)))
			case c.component == componentAPIServer && auditRotationFlags[name] && auditOwn:
			case ProtectedNodeFlag(c.component, name):
				errs = append(errs, field.Forbidden(at, fmt.Sprintf("%s is an argument Kubehenge sets for the %s, so it cannot be changed here", name, componentName(c.component))))
			}
		}
	}
	return errs
}

func componentName(component string) string {
	switch component {
	case componentAPIServer:
		return "API server"
	case componentControllerManager:
		return "controller manager"
	default:
		return component
	}
}

// validateNodeCustomisation holds the variable of a new or changed cluster to checkNodeCustomisation,
// and to Secrets and a ConfigMap that exist in the cluster's namespace. Cluster API reads them there
// when a node bootstraps.
func (v *ClusterValidator) validateNodeCustomisation(ctx context.Context, old, c *clusterv1.Cluster) field.ErrorList {
	n, given, err := optionalVariable[NodeCustomisation](c, NodeCustomisationVariable)
	base := field.NewPath("spec", "topology", "variables").Key(NodeCustomisationVariable)
	if err != nil {
		return field.ErrorList{field.Invalid(base, NodeCustomisationVariable, err.Error())}
	}
	if !given || (old != nil && !variableChanged(old, c, NodeCustomisationVariable)) {
		return nil
	}
	errs := checkNodeCustomisation(distributionOf(c), &n)
	if len(errs) > 0 || v.Hooks == nil || v.Hooks.Reader == nil {
		return errs
	}
	return append(errs, checkNodeReferences(ctx, v.Hooks.Reader, c.Namespace, &n, base)...)
}

// ValidateNodeCustomisation is the webhook's check of the variable alone, for a client with no
// webhook behind it. r reads the Secrets and the ConfigMap the variable names.
func ValidateNodeCustomisation(ctx context.Context, r client.Reader, old, c *clusterv1.Cluster) error {
	errs := (&ClusterValidator{Hooks: &Hooks{Reader: r}}).validateNodeCustomisation(ctx, old, c)
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(clusterv1.GroupVersion.WithKind("Cluster").GroupKind(), c.Name, errs) //nolint:wrapcheck // made here, not passed on
}

// checkNodeReferences reads what the variable names in the cluster's namespace.
func checkNodeReferences(ctx context.Context, r client.Reader, namespace string, n *NodeCustomisation, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	secret := func(at *field.Path, name, key string) {
		if err := secretKeyProblem(ctx, r, namespace, name, key); err != nil {
			errs = append(errs, field.Invalid(at, name, err.Error()))
		}
	}
	for i, f := range n.Files {
		if f.Secret != nil {
			secret(base.Child("files").Index(i).Child("secret"), f.Secret.Name, f.Secret.Key)
		}
	}
	if w := n.AuditWebhook; w != nil {
		key := w.Key
		if key == "" {
			key = IdentityKubeconfigKey
		}
		secret(base.Child("auditWebhook", "secret"), w.Secret, key)
	}
	if p := n.AuditPolicy; p != nil && p.ConfigMap != "" {
		if err := auditPolicyProblem(ctx, r, namespace, p.ConfigMap); err != nil {
			errs = append(errs, field.Invalid(base.Child("auditPolicy", "configMap"), p.ConfigMap, err.Error()))
		}
	}
	return errs
}

func secretKeyProblem(ctx context.Context, r client.Reader, namespace, name, key string) error {
	var s corev1.Secret
	switch err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s); {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("Secret %s is not in namespace %s. A Secret must sit in the cluster's namespace", name, namespace) //nolint:staticcheck // names a Secret
	case err != nil:
		return fmt.Errorf("reading Secret %s: %w", name, err)
	}
	if _, ok := s.Data[key]; !ok {
		return fmt.Errorf("Secret %s has no key %s", name, key) //nolint:staticcheck // names a Secret
	}
	return nil
}

// auditPolicyProblem reads a ConfigMap's policy and says what is wrong with it.
func auditPolicyProblem(ctx context.Context, r client.Reader, namespace, name string) error {
	var cm corev1.ConfigMap
	switch err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cm); {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("ConfigMap %s is not in namespace %s. A ConfigMap must sit in the cluster's namespace", name, namespace)
	case err != nil:
		return fmt.Errorf("reading ConfigMap %s: %w", name, err)
	}
	text, ok := cm.Data[auditPolicyConfigMapKey]
	if !ok {
		return fmt.Errorf("ConfigMap %s has no key %s", name, auditPolicyConfigMapKey)
	}
	return checkAuditPolicy([]byte(text))
}

// checkAuditPolicy refuses text that is not an audit Policy with rules, which the API server would
// refuse to start with.
func checkAuditPolicy(text []byte) error {
	var p struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Rules      []any  `json:"rules"`
	}
	if err := yaml.Unmarshal(text, &p); err != nil {
		return fmt.Errorf("the audit policy is not YAML: %w", err)
	}
	if !strings.HasPrefix(p.APIVersion, "audit.k8s.io/") || p.Kind != "Policy" {
		return errors.New("the audit policy has to be a Policy of audit.k8s.io/v1")
	}
	if len(p.Rules) == 0 {
		return errors.New("the audit policy has no rules, so the API server would log nothing")
	}
	return nil
}

// auditPolicySecretName is the Secret beside the cluster that mirrors its audit policy ConfigMap.
func auditPolicySecretName(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-audit-policy"
}

// ensureAuditPolicy mirrors the ConfigMap that holds the cluster's audit policy into the Secret
// its bootstrap templates read, and keeps it in step. A node reads the Secret once, as it
// bootstraps. A changed policy replaces the nodes through nodeCustomisationRevision.
func (r *Reconciler) ensureAuditPolicy(ctx context.Context, cluster *clusterv1.Cluster) error {
	n, _, err := optionalVariable[NodeCustomisation](cluster, NodeCustomisationVariable)
	if err != nil || n.AuditPolicy == nil || n.AuditPolicy.ConfigMap == "" || distributionOf(cluster) == templates.Talos {
		return err
	}
	if err := auditPolicyProblem(ctx, r.Reader, cluster.Namespace, n.AuditPolicy.ConfigMap); err != nil {
		return fmt.Errorf("audit policy of cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	var cm corev1.ConfigMap
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: n.AuditPolicy.ConfigMap}, &cm); err != nil {
		return fmt.Errorf("reading ConfigMap %s: %w", n.AuditPolicy.ConfigMap, err)
	}
	s := &corev1.Secret{Namespace: cluster.Namespace, Name: auditPolicySecretName(cluster)}
	return applyOwned(ctx, r.Client, cluster, s, func(got *corev1.Secret) {
		got.Type = corev1.SecretTypeOpaque
		got.Data = map[string][]byte{auditPolicyMirrorKey: []byte(cm.Data[auditPolicyConfigMapKey])}
	})
}
