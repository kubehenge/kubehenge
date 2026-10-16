package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Rancher is a cluster's rancher variable.
type Rancher struct {
	// Install puts Rancher into this cluster; Kubehenge then registers its other clusters with it.
	Install  bool   `json:"install"`
	Hostname string `json:"hostname,omitempty"`
	// Register is nil when the variable leaves it out, which means yes.
	Register *bool `json:"register,omitempty"`
}

func (r Rancher) registers() bool { return r.Register == nil || *r.Register }

// KubernetesMinors is the range of Kubernetes minor versions a release supports, both included.
type KubernetesMinors struct{ Min, Max int }

// ParseKubernetesMinors reads <min>-<max>, such as 34-36.
func ParseKubernetesMinors(s string) (KubernetesMinors, error) {
	lo, hi, ok := strings.Cut(s, "-")
	m := KubernetesMinors{}
	var err1, err2 error
	m.Min, err1 = strconv.Atoi(lo)
	m.Max, err2 = strconv.Atoi(hi)
	if !ok || err1 != nil || err2 != nil || m.Min > m.Max {
		return KubernetesMinors{}, fmt.Errorf("%q is not <min>-<max> Kubernetes minor versions", s)
	}
	return m, nil
}

// Allows reports whether a Kubernetes version such as v1.36.5+rke2r1 is in the range.
func (m KubernetesMinors) Allows(version string) bool {
	major, minorOn, ok := strings.Cut(strings.TrimPrefix(version, "v"), ".")
	if !ok || major != "1" {
		return false
	}
	minorText, _, _ := strings.Cut(minorOn, ".")
	minor, err := strconv.Atoi(minorText)
	return err == nil && minor >= m.Min && minor <= m.Max
}

var (
	provisioningClusterGVK = schema.GroupVersionKind{Group: "provisioning.cattle.io", Version: "v1", Kind: "Cluster"}
	registrationTokenGVK   = schema.GroupVersionKind{Group: "management.cattle.io", Version: "v3", Kind: "ClusterRegistrationToken"}
)

const (
	// rancherWorkspace is where Rancher keeps the clusters it manages through Fleet.
	rancherWorkspace = "fleet-default"
	// rancherClusterAnnotation names the Kubehenge cluster a Rancher cluster object stands for.
	rancherClusterAnnotation = "kubehenge.dev/cluster"
	// fieldOwner applies the Rancher agent's manifest.
	fieldOwner = "kubehenge"
	// registerPoll is how soon a registration Rancher is working on is looked at again: Rancher
	// fills in a new cluster within seconds.
	registerPoll = 5 * time.Second
	// agentStart is how long Rancher's agent takes to connect once it is installed.
	agentStart = 30 * time.Second
	// remoteTimeout bounds each request to a workload cluster.
	remoteTimeout = 30 * time.Second
)

// rancherName names a cluster's object in Rancher: <namespace>-<name>, which alone would give a-b/c
// and a/b-c the same name, so a hash of the identity follows. Rancher treats c-xxxxx and local as
// clusters of its own, which this never matches.
func rancherName(cluster *clusterv1.Cluster) string {
	sum := sha256.Sum256([]byte(cluster.Namespace + "/" + cluster.Name))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	name := strings.ToLower(cluster.Namespace + "-" + cluster.Name)
	if len(name) > 63-len(suffix) {
		name = strings.TrimRight(name[:63-len(suffix)], "-")
	}
	return name + suffix
}

// remoteConfig reads the kubeconfig Cluster API keeps for a cluster, and whether it exists yet.
func remoteConfig(ctx context.Context, c client.Reader, cluster client.ObjectKey) (*rest.Config, bool, error) {
	s := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name + "-kubeconfig"}, s)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading the kubeconfig of cluster %s: %w", cluster, err)
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(s.Data["value"])
	if err != nil {
		return nil, false, fmt.Errorf("kubeconfig of cluster %s: %w", cluster, err)
	}
	config.Timeout = remoteTimeout
	return config, true, nil
}

// Registrar registers every cluster built from Kubehenge's ClusterClasses with the Rancher that
// Kubehenge installed into one of them, unless the cluster opts out, and removes it from Rancher
// before the cluster goes. It signs in to Rancher's cluster with the kubeconfig Cluster API keeps
// for it, so it needs no Rancher account.
type Registrar struct {
	Client         client.Client
	ClassNamespace string
	// Remotes reaches Rancher's cluster and the clusters it registers.
	Remotes *Remotes
}

// SetupWithManager registers the registrar. Of a cluster's status it follows only whether the
// cluster is available, which registration waits for; when the cluster that installs Rancher
// changes, every other cluster is looked at again, as their registration depends on it.
func (r *Registrar) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
			availabilityChanged,
		))).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.othersOfRancher), builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			availabilityChanged,
		))).
		Named("rancher-registrar").
		Complete(r)
	if err != nil {
		return fmt.Errorf("setting up the Rancher registrar: %w", err)
	}
	return nil
}

// availabilityChanged passes a cluster's creation, deletion, and every update that flips its
// Available condition.
var availabilityChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		available := func(o client.Object) bool {
			c, ok := o.(*clusterv1.Cluster)
			return ok && meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.AvailableCondition)
		}
		return available(e.ObjectOld) != available(e.ObjectNew) || !e.ObjectNew.GetDeletionTimestamp().IsZero()
	},
}

// Reconcile implements reconcile.Reconciler.
func (r *Registrar) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	if !Owns(cluster, r.ClassNamespace) || !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	v, err := rancherOf(cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if v.Install {
		// Rancher manages its own cluster as "local".
		return ctrl.Result{}, nil
	}
	// With no Rancher yet, or before this cluster is available, there is nothing to do until one
	// of them changes, which the watches report. Nothing new registers with a Rancher being deleted.
	found, err := r.findRancher(ctx)
	if err != nil || found == nil || !found.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, err
	}
	rs, reached, err := r.connect(ctx, found)
	if err != nil || !reached {
		return ctrl.Result{}, err
	}
	if !v.registers() {
		return r.optOut(ctx, rs, cluster)
	}
	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, clusterv1.AvailableCondition) {
		return ctrl.Result{}, nil
	}
	res, err := r.register(ctx, rs, cluster)
	return res, r.failed(rs, err)
}

// BeforeDelete removes a cluster from Rancher before its VMs go, while Rancher can still clean up
// inside it; for Rancher's own cluster, every registered cluster first. Done when there is nothing
// to remove, or Rancher cannot be reached to remove it.
func (r *Registrar) BeforeDelete(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	// A rancher variable that cannot be read installs no Rancher, as RancherCluster reads it, but
	// the cluster may have registered while it could be read.
	v, err := rancherOf(cluster)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "removing the cluster from Rancher as one that does not install it")
		v = Rancher{}
	}
	// A Rancher being deleted still counts here, for its own clean-up.
	found, err := r.findRancher(ctx)
	if err != nil {
		return false, err
	}
	if found == nil {
		return true, nil
	}
	rs, reached, err := r.connect(ctx, found)
	if err != nil {
		return false, err
	}
	if !reached {
		return true, nil
	}
	if v.Install {
		if rs.cluster.Namespace != cluster.Namespace || rs.cluster.Name != cluster.Name {
			return true, nil
		}
		return rs.deregisterAll(ctx)
	}
	return rs.deregister(ctx, cluster)
}

// othersOfRancher asks for every other owned cluster when the one changing installs Rancher.
func (r *Registrar) othersOfRancher(ctx context.Context, o client.Object) []ctrl.Request {
	c, ok := o.(*clusterv1.Cluster)
	if !ok || !Owns(c, r.ClassNamespace) {
		return nil
	}
	if v, err := rancherOf(c); err != nil || !v.Install {
		return nil
	}
	clusters := &clusterv1.ClusterList{}
	if err := r.Client.List(ctx, clusters, client.UnsafeDisableDeepCopy); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing the clusters that register with Rancher")
		return nil
	}
	var reqs []ctrl.Request
	for i := range clusters.Items {
		other := &clusters.Items[i]
		if client.ObjectKeyFromObject(other) != client.ObjectKeyFromObject(c) && Owns(other, r.ClassNamespace) {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(other)})
		}
	}
	return reqs
}

// findRancher finds the cluster Kubehenge installed Rancher into; nil when there is none.
func (r *Registrar) findRancher(ctx context.Context) (*clusterv1.Cluster, error) {
	clusters := &clusterv1.ClusterList{}
	// Only read, the Rancher cluster included.
	if err := r.Client.List(ctx, clusters, client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}
	return RancherCluster(clusters.Items, r.ClassNamespace), nil
}

// connect reaches the cluster Rancher runs in, and reports false while it is not available.
func (r *Registrar) connect(ctx context.Context, found *clusterv1.Cluster) (*rancherSide, bool, error) {
	if !meta.IsStatusConditionTrue(found.Status.Conditions, clusterv1.AvailableCondition) {
		return nil, false, nil
	}
	reached, ok, err := r.Remotes.get(ctx, found)
	if err != nil || !ok {
		return nil, false, err
	}
	return &rancherSide{cluster: found, config: reached.config, client: reached.client}, true, nil
}

// optOut removes a cluster that opted out from Rancher, looking again until Rancher has let go of
// it.
func (r *Registrar) optOut(ctx context.Context, rs *rancherSide, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	done, err := rs.deregister(ctx, cluster)
	if err != nil || !done {
		return ctrl.Result{RequeueAfter: waitInterval}, r.failed(rs, err)
	}
	return ctrl.Result{}, nil
}

// failed passes err on, first dropping the client to Rancher's cluster it may have come from.
func (r *Registrar) failed(rs *rancherSide, err error) error {
	if err != nil {
		r.Remotes.forget(rs.cluster)
	}
	return err
}

// register makes the cluster's object in Rancher, then installs into the cluster the agent Rancher
// makes for it.
func (r *Registrar) register(ctx context.Context, rs *rancherSide, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("rancher", client.ObjectKeyFromObject(rs.cluster))
	ctx = ctrl.LoggerInto(ctx, log)
	rc := &unstructured.Unstructured{}
	rc.SetGroupVersionKind(provisioningClusterGVK)
	key := client.ObjectKey{Namespace: rancherWorkspace, Name: rancherName(cluster)}
	err := rs.client.Get(ctx, key, rc)
	switch {
	case meta.IsNoMatchError(err):
		log.Info("waiting for Rancher to serve its API")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	case apierrors.IsNotFound(err):
		return rs.create(ctx, key, cluster)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("reading cluster %s in Rancher: %w", key, err)
	}
	if owner := rc.GetAnnotations()[rancherClusterAnnotation]; owner != cluster.Namespace+"/"+cluster.Name {
		return ctrl.Result{}, fmt.Errorf("cluster %s in Rancher stands for %q, not this cluster", key, owner)
	}
	if ready, _, _ := unstructured.NestedBool(rc.Object, "status", "ready"); ready {
		return ctrl.Result{RequeueAfter: resync}, nil
	}
	return r.installAgent(ctx, rs, cluster, rc)
}

// installAgent installs Rancher's agent into the cluster once Rancher has made it.
func (r *Registrar) installAgent(ctx context.Context, rs *rancherSide, cluster *clusterv1.Cluster, rc *unstructured.Unstructured) (ctrl.Result, error) {
	internal, _, _ := unstructured.NestedString(rc.Object, "status", "clusterName")
	if internal == "" {
		return ctrl.Result{RequeueAfter: registerPoll}, nil
	}
	manifest, err := rs.agentManifest(ctx, internal)
	if err != nil || manifest == nil {
		return ctrl.Result{RequeueAfter: waitInterval}, err
	}
	config, found, err := remoteConfig(ctx, r.Client, client.ObjectKeyFromObject(cluster))
	if err != nil || !found {
		return ctrl.Result{RequeueAfter: waitInterval}, err
	}
	if err := applyManifest(ctx, config, manifest); err != nil {
		return ctrl.Result{}, fmt.Errorf("installing Rancher's agent: %w", err)
	}
	ctrl.LoggerFrom(ctx).Info("installed Rancher's agent", "name", rc.GetName())
	return ctrl.Result{RequeueAfter: agentStart}, nil
}

// rancherSide is the cluster Rancher runs in, as the registrar reaches it.
type rancherSide struct {
	cluster *clusterv1.Cluster
	config  *rest.Config
	client  client.Client
}

// create makes the cluster's object in Rancher. With no rkeConfig, Rancher imports the cluster
// rather than provisioning it.
func (rs *rancherSide) create(ctx context.Context, key client.ObjectKey, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	rc := &unstructured.Unstructured{}
	rc.SetGroupVersionKind(provisioningClusterGVK)
	rc.SetNamespace(key.Namespace)
	rc.SetName(key.Name)
	rc.SetAnnotations(map[string]string{rancherClusterAnnotation: cluster.Namespace + "/" + cluster.Name})
	rc.Object["spec"] = map[string]any{}
	if err := rs.client.Create(ctx, rc); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating cluster %s in Rancher: %w", key, err)
	}
	ctrl.LoggerFrom(ctx).Info("registered the cluster with Rancher", "name", key.Name)
	return ctrl.Result{RequeueAfter: registerPoll}, nil
}

// agentManifest fetches the manifest of Rancher's agent for a cluster, nil until Rancher has made
// it. It goes through the Rancher cluster's API server to Rancher's service, so the management
// cluster needs neither Rancher's hostname nor its CA. The registration token travels in the
// path, as with every way Rancher offers the manifest; whoever can read that cluster's audit log
// can read the token's Secret too.
func (rs *rancherSide) agentManifest(ctx context.Context, internal string) ([]byte, error) {
	token := &unstructured.Unstructured{}
	token.SetGroupVersionKind(registrationTokenGVK)
	err := rs.client.Get(ctx, client.ObjectKey{Namespace: internal, Name: "default-token"}, token)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Rancher's registration token: %w", err)
	}
	manifestURL, _, _ := unstructured.NestedString(token.Object, "status", "manifestUrl")
	secretName, _, _ := unstructured.NestedString(token.Object, "status", "tokenSecretName")
	if manifestURL == "" || secretName == "" {
		// Rancher makes no URL while its server-url setting is empty.
		return nil, nil
	}
	secret := &corev1.Secret{}
	err = rs.client.Get(ctx, client.ObjectKey{Namespace: internal, Name: secretName}, secret)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Rancher's registration token Secret %s: %w", secretName, err)
	}
	u, err := url.Parse(manifestURL)
	if err != nil {
		return nil, fmt.Errorf("manifest URL of Rancher's agent: %w", err)
	}
	path := strings.Replace(u.Path, "{token}", string(secret.Data["token"]), 1)
	cs, err := kubernetes.NewForConfig(rs.config)
	if err != nil {
		return nil, fmt.Errorf("reaching Rancher's cluster: %w", err)
	}
	body, err := cs.CoreV1().Services(rancherNamespace).ProxyGet("https", "rancher", "443", path, nil).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching Rancher's agent manifest: %w", err)
	}
	return body, nil
}

// deregister removes a cluster from Rancher, which first runs its clean-up in the cluster through
// the agent; done once Rancher has let go of it.
func (rs *rancherSide) deregister(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	rc := &unstructured.Unstructured{}
	rc.SetGroupVersionKind(provisioningClusterGVK)
	err := rs.client.Get(ctx, client.ObjectKey{Namespace: rancherWorkspace, Name: rancherName(cluster)}, rc)
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading cluster %s in Rancher: %w", rancherName(cluster), err)
	}
	if rc.GetAnnotations()[rancherClusterAnnotation] != cluster.Namespace+"/"+cluster.Name {
		return true, nil
	}
	if rc.GetDeletionTimestamp() == nil {
		if err := client.IgnoreNotFound(rs.client.Delete(ctx, rc)); err != nil {
			return false, fmt.Errorf("removing cluster %s from Rancher: %w", rc.GetName(), err)
		}
		ctrl.LoggerFrom(ctx).Info("removing the cluster from Rancher", "name", rc.GetName())
	}
	return false, nil
}

// deregisterAll removes every cluster Kubehenge registered from Rancher, as Rancher's own cluster
// goes; done once none is left.
func (rs *rancherSide) deregisterAll(ctx context.Context) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(provisioningClusterGVK.GroupVersion().WithKind("ClusterList"))
	err := rs.client.List(ctx, list, client.InNamespace(rancherWorkspace))
	if meta.IsNoMatchError(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("listing the clusters in Rancher: %w", err)
	}
	done := true
	for i := range list.Items {
		rc := &list.Items[i]
		if _, ours := rc.GetAnnotations()[rancherClusterAnnotation]; !ours {
			continue
		}
		done = false
		if rc.GetDeletionTimestamp() == nil {
			if err := client.IgnoreNotFound(rs.client.Delete(ctx, rc)); err != nil {
				return false, fmt.Errorf("removing cluster %s from Rancher: %w", rc.GetName(), err)
			}
		}
	}
	return done, nil
}

// RancherCluster is the cluster whose Rancher every other registers with: of the clusters built
// from the ClusterClasses in classNamespace that install it, the one created first; nil if none
// does. Only one may install it, which the hooks and the interface enforce. Should a client set it
// on a second one anyway, the first keeps the role and the second gets no Rancher, rather than
// every cluster's registration and deletion stopping on the conflict.
func RancherCluster(clusters []clusterv1.Cluster, classNamespace string) *clusterv1.Cluster {
	var found *clusterv1.Cluster
	for i := range clusters {
		c := &clusters[i]
		if !Owns(c, classNamespace) {
			continue
		}
		if v, err := rancherOf(c); err != nil || !v.Install {
			continue
		}
		if found == nil || createdBefore(c, found) {
			found = c
		}
	}
	return found
}

// createdBefore orders clusters by creation, by name within the same second.
func createdBefore(a, b *clusterv1.Cluster) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
}

// applyManifest applies every object of a YAML stream to a cluster.
func applyManifest(ctx context.Context, config *rest.Config, manifest []byte) error {
	c, err := client.New(config, client.Options{})
	if err != nil {
		return fmt.Errorf("reaching the cluster: %w", err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), decoderLookahead)
	for {
		u := &unstructured.Unstructured{}
		if err := decoder.Decode(&u.Object); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("reading the manifest: %w", err)
		}
		if len(u.Object) == 0 {
			continue
		}
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("%s %s: %w", u.GetKind(), u.GetName(), err)
		}
	}
}
