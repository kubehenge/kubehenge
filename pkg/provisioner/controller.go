package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// Finalizer holds a Cluster until its Proxmox VE objects are removed.
const Finalizer = "kubehenge.dev/proxmox"

// ClassPrefix starts the name of every Kubehenge ClusterClass.
const ClassPrefix = "kubehenge-"

const (
	// concurrentReconciles bounds how many clusters reconcile at once.
	concurrentReconciles = 4
	// reconcileTimeout bounds one reconcile, so that one cluster whose Proxmox VE calls hang holds
	// up no other for long.
	reconcileTimeout = 30 * time.Minute
	// waitInterval is how often what waits for something looks again.
	waitInterval = 10 * time.Second
	// resync is how often a cluster's objects are checked, so that one removed behind the
	// controller's back comes back.
	resync = 10 * time.Minute
)

// Cloud creates and removes a cluster's Proxmox VE objects; *Proxmox is the real one.
type Cloud interface {
	Ensure(ctx context.Context, n Names, held Credentials, record func(Credentials) error) (Credentials, error)
	Remove(ctx context.Context, n Names, held Credentials) error
}

// Reconciler provisions the Proxmox VE side of Clusters built from Kubehenge's ClusterClasses.
type Reconciler struct {
	Client client.Client
	Cloud  Cloud
	// WorkloadPool is the pool the clusters' pools nest under.
	WorkloadPool string
	// ClassNamespace holds Kubehenge's ClusterClasses.
	ClassNamespace string
	Endpoint       Endpoint
	Charts         Charts
	// Registrar removes clusters from Rancher before Rancher itself is switched off.
	Registrar *Registrar
	// Remotes reaches the workload clusters, to take their workloads out of the mesh before Istio
	// is switched off.
	Remotes *Remotes
	// Events reports on a Cluster what the controller refuses to do for it; may be nil.
	Events events.EventRecorder
	// RancherKubernetes is what the Rancher release Kubehenge installs runs on; unset allows any.
	RancherKubernetes KubernetesMinors

	ids ciliumIDs
}

// distributionOf is the distribution of a Cluster built from one of Kubehenge's ClusterClasses.
func distributionOf(c *clusterv1.Cluster) templates.Distribution {
	return templates.Distribution(strings.TrimPrefix(c.Spec.Topology.ClassRef.Name, ClassPrefix))
}

// Owns reports whether a Cluster is built from one of Kubehenge's ClusterClasses.
func Owns(c *clusterv1.Cluster, classNamespace string) bool {
	ref := c.Spec.Topology.ClassRef
	ns := ref.Namespace
	if ns == "" {
		ns = c.Namespace
	}
	return strings.HasPrefix(ref.Name, ClassPrefix) && ns == classNamespace
}

// SetupWithManager registers the reconciler. Status updates do not trigger it: only a new
// generation, deletion, or the requeue every resync. Several clusters reconcile at once and each
// reconcile is bounded, so one cluster whose Proxmox VE calls hang holds up no other.
// Concurrent reconciles may race for the same VMID; the loser's creation fails and is retried,
// and an owner is only ever taken by its name and pool.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.meshPeersOf), builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Named("proxmox-provisioner").
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles, ReconciliationTimeout: reconcileTimeout}).
		Complete(r)
	if err != nil {
		return fmt.Errorf("setting up the provisioner: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	if !Owns(cluster, r.ClassNamespace) {
		return ctrl.Result{}, nil
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster)
	}
	if err := r.addFinalizer(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	creds, err := r.ensureCloud(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err = r.ensureEncryption(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	addons, rancherRefused, err := r.addons(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	addonWaiting, err := r.applyAddons(ctx, cluster, creds, &addons)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Marked ready last: BeforeClusterCreate lets the cluster be built once it is.
	if err = r.keep(ctx, cluster, creds, true); err != nil {
		return ctrl.Result{}, err
	}
	othersWaiting, err := r.afterTheRest(ctx, cluster, &addons, rancherRefused)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Nothing the waits are on changes the Cluster, so only a requeue looks again.
	if addonWaiting || othersWaiting {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	return ctrl.Result{RequeueAfter: resync}, nil
}

// afterTheRest does what waits on other clusters or on the workload cluster's own API, however long
// that takes, so that nothing else of this cluster waits with it: removing Rancher, which waits on
// every cluster registered with it, removing Istio, which waits on the cluster's workloads, and
// linking the cluster's Cluster Mesh peers. It reports whether any of it waits.
func (r *Reconciler) afterTheRest(ctx context.Context, cluster *clusterv1.Cluster, addons *Addons, rancherRefused string) (bool, error) {
	rancherWaiting := false
	if !addons.Rancher.Install && rancherRefused == "" {
		var with []string
		if !addons.Monitoring.Enabled {
			with = append(with, certManagerProxy(cluster))
		}
		var err error
		if rancherWaiting, err = r.removeRancherWhenFree(ctx, cluster, with...); err != nil {
			return false, err
		}
	}
	istioWaiting := false
	if !addons.Istio.Enabled {
		var err error
		if istioWaiting, err = r.removeIstio(ctx, cluster); err != nil {
			return false, err
		}
	}
	meshWaiting, err := r.meshCluster(ctx, cluster, addons)
	return rancherWaiting || istioWaiting || meshWaiting, err
}

// removeRancherWhenFree removes Rancher from a cluster that switched it off, or reports that it
// waits for Rancher to let go of the clusters registered with it first.
func (r *Reconciler) removeRancherWhenFree(ctx context.Context, cluster *clusterv1.Cluster, with ...string) (bool, error) {
	waiting, err := r.removeRancher(ctx, cluster, with...)
	if err != nil || !waiting {
		return false, err
	}
	ctrl.LoggerFrom(ctx).Info("waiting for Rancher to let go of the clusters registered with it before removing it")
	if r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeNormal, "RemovingRancher", "RemoveRancher",
			"waiting for Rancher to let go of the clusters registered with it")
	}
	return true, nil
}

// addFinalizer holds the cluster until its Proxmox VE objects are removed.
func (r *Reconciler) addFinalizer(ctx context.Context, cluster *clusterv1.Cluster) error {
	if controllerutil.ContainsFinalizer(cluster, Finalizer) {
		return nil
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	controllerutil.AddFinalizer(cluster, Finalizer)
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("adding the finalizer: %w", err)
	}
	return nil
}

// cloudObjects names the cluster's Proxmox VE objects and reads what was recorded of them.
func (r *Reconciler) cloudObjects(ctx context.Context, cluster *clusterv1.Cluster) (Names, Credentials, error) {
	names, err := NamesFor(r.WorkloadPool, cluster.Namespace, cluster.Name)
	if err != nil {
		return Names{}, Credentials{}, err
	}
	held, err := r.held(ctx, cluster)
	if err != nil {
		return names, Credentials{}, err
	}
	return names, held, nil
}

// ensureCloud creates the cluster's Proxmox VE objects, keeping what it creates as it goes.
func (r *Reconciler) ensureCloud(ctx context.Context, cluster *clusterv1.Cluster) (Credentials, error) {
	names, held, err := r.cloudObjects(ctx, cluster)
	if err != nil {
		return held, err
	}
	record := func(c Credentials) error { return r.keep(ctx, cluster, c, false) }
	creds, err := r.Cloud.Ensure(ctx, names, held, record)
	if err != nil {
		return creds, fmt.Errorf("creating the cluster's Proxmox VE objects: %w", err)
	}
	return creds, nil
}

// reconcileDelete removes a deleted cluster's Proxmox VE objects once its machines are gone, then
// lets the Cluster go.
func (r *Reconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, Finalizer) {
		return ctrl.Result{}, nil
	}
	log := ctrl.LoggerFrom(ctx)
	machines := &clusterv1.MachineList{}
	err := r.Client.List(ctx, machines, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing the cluster's machines: %w", err)
	}
	if len(machines.Items) > 0 {
		log.Info("waiting for the cluster's machines to be deleted", "machines", len(machines.Items))
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	names, held, err := r.cloudObjects(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Cloud.Remove(ctx, names, held); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing the cluster's Proxmox VE objects: %w", err)
	}
	log.Info("removed the cluster's Proxmox VE objects", "pool", names.Pool)
	patch := client.MergeFrom(cluster.DeepCopy())
	controllerutil.RemoveFinalizer(cluster, Finalizer)
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing the finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// addons reads the add-ons the cluster switches on, with Rancher switched off when it is refused,
// and why it is refused, or "". A Rancher refused here is neither installed nor removed: what the
// cluster already runs stays.
func (r *Reconciler) addons(ctx context.Context, cluster *clusterv1.Cluster) (Addons, string, error) {
	addons, err := addonsOf(cluster)
	if err != nil {
		return addons, "", err
	}
	refused, err := r.refuseRancher(ctx, cluster, addons.Rancher)
	if err != nil {
		return addons, "", err
	}
	if refused != "" {
		ctrl.LoggerFrom(ctx).Info("not installing Rancher: " + refused)
		if r.Events != nil {
			r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, "RancherRefused", "InstallRancher", "%s", refused)
		}
		addons.Rancher.Install = false
	}
	return addons, refused, nil
}

// removeSwitchedOff removes the proxies of Headlamp, monitoring, the load balancer and Kyverno when
// the cluster switches them off, and kube-vip's cloud provider when the load balancer is MetalLB;
// Rancher's and Istio's removals wait on other clusters, apart from these. It reports whether a
// removal waits on another.
func (r *Reconciler) removeSwitchedOff(ctx context.Context, cluster *clusterv1.Cluster, addons *Addons) (bool, error) {
	var off []string
	if !addons.Headlamp.Enabled {
		off = append(off, headlampProxy(cluster))
	}
	if !addons.Monitoring.Enabled {
		off = append(off, monitoringProxy(cluster))
	}
	if !addons.LoadBalancer.Enabled {
		off = append(off, loadBalancerProxy(cluster))
	}
	if !addons.LoadBalancer.Enabled || addons.LoadBalancer.implementation() != kubeVip {
		off = append(off, kubeVipProviderProxy(cluster))
	}
	if err := r.removeProxies(ctx, cluster, off...); err != nil {
		return false, err
	}
	if !addons.PodSecurity.kyvernoOn() {
		return r.removeKyverno(ctx, cluster)
	}
	return false, nil
}

// applyAddons applies what the cluster gets installed: the Secrets of its ClusterResourceSet, the
// set itself and the HelmChartProxies of its charts, and removes those switched off. It reports
// whether an add-on's installation or removal waits on another.
func (r *Reconciler) applyAddons(ctx context.Context, cluster *clusterv1.Cluster, creds Credentials, addons *Addons) (bool, error) {
	storage, err := variable[string](cluster, "storage")
	if err != nil {
		return false, err
	}
	resources, err := resourceSetSecret(cluster, r.Endpoint, creds)
	if err != nil {
		return false, err
	}
	cni, err := r.addonCNI(ctx, cluster)
	if err != nil {
		return false, err
	}
	if runsCilium(cluster) {
		if cni.ClusterID, err = r.ciliumClusterID(ctx, cluster); err != nil {
			return false, err
		}
	}
	proxies, err := helmChartProxies(cluster, &r.Charts, distributionOf(cluster), storage, cni, addons)
	if err != nil {
		return false, err
	}
	waiting, err := r.removeSwitchedOff(ctx, cluster, addons)
	if err != nil {
		return false, err
	}
	secrets, err := resourceSetSecrets(cluster, resources, cni, addons)
	if err != nil {
		return false, err
	}
	for _, s := range secrets {
		desired := s.DeepCopy()
		mutate := func(got *corev1.Secret) { got.Type, got.Data = desired.Type, desired.Data }
		if err := applyOwned(ctx, r.Client, cluster, s, mutate); err != nil {
			return false, err
		}
	}
	crs := clusterResourceSet(cluster, secrets)
	desired := crs.Spec.DeepCopy()
	if err := applyOwned(ctx, r.Client, cluster, crs, func(got *addonsv1.ClusterResourceSet) { got.Spec = *desired }); err != nil {
		return false, err
	}
	if installing, err := r.applyProxies(ctx, cluster, proxies); err != nil || installing {
		return installing, err
	}
	if addons.LoadBalancer.Enabled && distributionOf(cluster) == templates.K3s {
		if err := r.removeServiceLB(ctx, cluster); err != nil {
			return false, err
		}
	}
	return waiting, nil
}

// resourceSetSecrets are the Secrets of the cluster's ClusterResourceSet: its Proxmox VE Secret,
// Calico's resources when Kubehenge installs Calico, the configuration of the distribution's own
// Cilium when it runs that, and MetalLB's address pool when MetalLB is its load balancer.
func resourceSetSecrets(cluster *clusterv1.Cluster, resources *corev1.Secret, cni CNI, addons *Addons) ([]*corev1.Secret, error) {
	secrets := []*corev1.Secret{resources}
	switch {
	case cni.Name == cniCalico:
		calico, err := calicoResources(cluster, cni)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, calico)
	case cni.Name == "" && runsCilium(cluster):
		cilium, err := rke2CiliumConfig(cluster, cni.ClusterID, addons)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, cilium)
	}
	if lb := addons.LoadBalancer; lb.Enabled && lb.implementation() == metalLB {
		pool, err := metalLBPool(cluster, lb)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, pool)
	}
	return secrets, nil
}

// applyProxies applies the cluster's HelmChartProxies, in order, and reports whether one waits on
// another.
func (r *Reconciler) applyProxies(ctx context.Context, cluster *clusterv1.Cluster, proxies []*unstructured.Unstructured) (bool, error) {
	waiting := false
	for _, p := range proxies {
		if p.GetName() == kyvernoPoliciesProxy(cluster) {
			wait, err := r.policiesWait(ctx, cluster)
			if err != nil {
				return false, err
			}
			if wait {
				waiting = true
				continue
			}
		}
		desired := p.Object["spec"]
		if err := applyOwned(ctx, r.Client, cluster, p, func(got *unstructured.Unstructured) { got.Object["spec"] = desired }); err != nil {
			return false, err
		}
	}
	return waiting, nil
}

// held reads what was recorded for the cluster; nothing yet, or no longer once a foreground
// deletion has removed the cluster's dependents.
func (r *Reconciler) held(ctx context.Context, cluster *clusterv1.Cluster) (Credentials, error) {
	stored := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, stored)
	if apierrors.IsNotFound(err) {
		return Credentials{}, nil
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("reading the cluster's credentials: %w", err)
	}
	return credentialsFrom(stored)
}

// keep stores the cluster's credentials, ready once everything they are for exists.
func (r *Reconciler) keep(ctx context.Context, cluster *clusterv1.Cluster, c Credentials, ready bool) error {
	s := credentialsSecret(cluster, c, ready)
	data := maps.Clone(s.Data)
	return applyOwned(ctx, r.Client, cluster, s, func(got *corev1.Secret) { got.Data = data })
}

// applyOwned creates or updates obj, owned by the cluster so that it goes when the cluster does.
// obj is first overwritten with the stored object, so mutate must not take what is wanted from obj.
func applyOwned[T client.Object](ctx context.Context, c client.Client, cluster *clusterv1.Cluster, obj T, mutate func(T)) error {
	_, err := controllerutil.CreateOrUpdate(ctx, c, obj, func() error {
		mutate(obj)
		return controllerutil.SetOwnerReference(cluster, obj, c.Scheme())
	})
	if err != nil {
		return fmt.Errorf("applying %T %s: %w", obj, obj.GetName(), err)
	}
	return nil
}

// removeProxies deletes HelmChartProxies of add-ons switched off; the Add-on Provider for Helm
// uninstalls a proxy's releases when the proxy goes.
func (r *Reconciler) removeProxies(ctx context.Context, cluster *clusterv1.Cluster, names ...string) error {
	for _, name := range names {
		p := &unstructured.Unstructured{}
		p.SetGroupVersionKind(helmChartProxyGVK)
		p.SetNamespace(cluster.Namespace)
		p.SetName(name)
		if err := client.IgnoreNotFound(r.Client.Delete(ctx, p)); err != nil {
			return fmt.Errorf("removing add-on %s: %w", name, err)
		}
	}
	return nil
}

// anyProxy reports whether any of the cluster's HelmChartProxies named is still there.
func (r *Reconciler) anyProxy(ctx context.Context, cluster *clusterv1.Cluster, names ...string) (bool, error) {
	for _, name := range names {
		p := &unstructured.Unstructured{}
		p.SetGroupVersionKind(helmChartProxyGVK)
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, p)
		if err == nil {
			return true, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("reading add-on %s: %w", name, err)
		}
	}
	return false, nil
}

// refuseRancher says why Rancher may not be installed into the cluster, or "": another cluster runs
// it, or the cluster's Kubernetes is one Rancher does not run on. The hooks and the interface refuse
// both up front; this holds for any client that set the variable past them. One already installed
// stays where it is.
func (r *Reconciler) refuseRancher(ctx context.Context, cluster *clusterv1.Cluster, rancher Rancher) (string, error) {
	if !rancher.Install {
		return "", nil
	}
	clusters := &clusterv1.ClusterList{}
	if err := r.Client.List(ctx, clusters, client.UnsafeDisableDeepCopy); err != nil {
		return "", fmt.Errorf("listing clusters: %w", err)
	}
	if rc := RancherCluster(clusters.Items, r.ClassNamespace); rc != nil && (rc.Namespace != cluster.Namespace || rc.Name != cluster.Name) {
		return fmt.Sprintf("cluster %s/%s already runs Rancher, with which every cluster registers", rc.Namespace, rc.Name), nil
	}
	if r.RancherKubernetes == (KubernetesMinors{}) || r.RancherKubernetes.Allows(cluster.Spec.Topology.Version) {
		return "", nil
	}
	installed := &unstructured.Unstructured{}
	installed.SetGroupVersionKind(helmChartProxyGVK)
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: rancherProxy(cluster)}, installed)
	if err == nil {
		return "", nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("reading the Rancher proxy: %w", err)
	}
	return fmt.Sprintf("the Rancher release Kubehenge installs runs on Kubernetes 1.%d to 1.%d, not %s",
		r.RancherKubernetes.Min, r.RancherKubernetes.Max, cluster.Spec.Topology.Version), nil
}

// removeRancher uninstalls Rancher from a cluster that switched it off, once every cluster
// registered with it is removed from it, so that Rancher cleans up inside them first, and the
// proxies named in with along with it, or at once when Rancher was never there. It reports
// whether it is still waiting.
func (r *Reconciler) removeRancher(ctx context.Context, cluster *clusterv1.Cluster, with ...string) (bool, error) {
	proxies := append([]string{rancherProxy(cluster)}, with...)
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: proxies[0]}, p)
	if apierrors.IsNotFound(err) {
		return false, r.removeProxies(ctx, cluster, proxies[1:]...)
	}
	if err != nil {
		return false, fmt.Errorf("reading the Rancher proxy: %w", err)
	}
	if r.Registrar != nil {
		rs, reached, err := r.Registrar.connect(ctx, cluster)
		if err != nil {
			return true, err
		}
		if reached {
			done, err := rs.deregisterAll(ctx)
			if err != nil || !done {
				return true, err
			}
		}
	}
	return false, r.removeProxies(ctx, cluster, proxies...)
}

// addonCNI is the CNI Kubehenge installs into the cluster, or none when its distribution brings
// the one chosen.
func (r *Reconciler) addonCNI(ctx context.Context, cluster *clusterv1.Cluster) (CNI, error) {
	class := &clusterv1.ClusterClass{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: r.ClassNamespace, Name: cluster.Spec.Topology.ClassRef.Name}, class); err != nil {
		return CNI{}, fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	name, _, err := optionalVariable[string](cluster, "cni")
	if err != nil || name == "" || !slices.Contains(strings.Split(class.Annotations[CNIAnnotation], ","), name) {
		return CNI{}, err
	}
	pods := class.Annotations[DefaultPodsAnnotation]
	if pods == "" {
		return CNI{}, fmt.Errorf("ClusterClass %s names no default pod range (%s)", class.Name, DefaultPodsAnnotation)
	}
	return CNI{Name: name, Pods: pods, ContainerForwarding: class.Annotations[CNIForwardingAnnotation] == strconv.FormatBool(true)}, nil
}

// variable reads a topology variable of the Cluster.
func variable[T any](c *clusterv1.Cluster, name string) (T, error) {
	v, set, err := optionalVariable[T](c, name)
	if err == nil && !set {
		err = fmt.Errorf("cluster %s/%s: variable %s is not set", c.Namespace, c.Name, name)
	}
	return v, err
}

// optionalVariable reads a topology variable of the Cluster, and whether it is set.
func optionalVariable[T any](c *clusterv1.Cluster, name string) (T, bool, error) {
	var v T
	for _, cv := range c.Spec.Topology.Variables {
		if cv.Name == name {
			if err := json.Unmarshal(cv.Value.Raw, &v); err != nil {
				return v, true, fmt.Errorf("cluster %s/%s: variable %s: %w", c.Namespace, c.Name, name, err)
			}
			return v, true, nil
		}
	}
	return v, false, nil
}
