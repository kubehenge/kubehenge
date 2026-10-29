package provisioner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	// AutoscalerVariable is the cluster variable with the autoscaler's settings for the cluster.
	AutoscalerVariable = "autoscaler"
	// AutoscalerRole is the chart's ClusterRole of what an autoscaler may do in the management
	// cluster, bound for each in its cluster's namespace: read the cluster's machines and pools and
	// the machine templates their sizes come from, scale the pools and mark the machines to remove.
	AutoscalerRole = "kubehenge-autoscaler"
	// AutoscalerPolicy names the ConfigMap the chart's admission policy holds each autoscaler to: by
	// account, its cluster and the MachineDeployments it may scale.
	AutoscalerPolicy = "kubehenge-autoscaler"
	// ClusterNamespaceLabel names, on what Kubehenge keeps for a cluster outside the cluster's
	// namespace, such as its autoscaler, the cluster's namespace.
	ClusterNamespaceLabel = "kubehenge.dev/cluster-namespace"
	// ClusterNameLabel names, on what Kubehenge keeps for a cluster outside the cluster's namespace,
	// the cluster.
	ClusterNameLabel = "kubehenge.dev/cluster-name"
	// PlacementManagement runs a cluster's autoscaler in the management cluster, the default.
	PlacementManagement = "management"
	// PlacementWorkload runs a cluster's autoscaler on the cluster's own control planes.
	PlacementWorkload = "workload"
	// AutoscalerFinalizer holds a Cluster while its autoscaler has anything in the autoscalers'
	// namespace.
	AutoscalerFinalizer = "kubehenge.dev/autoscaler"
)

const (
	// autoscalerAccount names, in a workload cluster's kube-system, the autoscaler's identity there,
	// and with placement workload the autoscaler itself.
	autoscalerAccount = "kubehenge-" + string(IdentityAutoscaler)
	// autoscalerStatus is the ConfigMap the autoscaler reports on itself in, in kube-system.
	autoscalerStatus = "cluster-autoscaler-status"
	// autoscalerManagementSecret holds, inside a workload cluster, the autoscaler's way into the
	// management cluster.
	autoscalerManagementSecret = "kubehenge-autoscaler-management" //nolint:gosec // a Secret's name, not a credential
	// autoscalerApp is the autoscaler's app.kubernetes.io/name, and its container's name.
	autoscalerApp = "cluster-autoscaler"
	// managedByLabel is app.kubernetes.io/managed-by, the tool that applies an object.
	managedByLabel = "app.kubernetes.io/managed-by"
	// autoscalerFieldOwner applies the capacity annotations and the autoscaler's Deployment.
	autoscalerFieldOwner = "kubehenge-autoscaler"
	// autoscalerPlacementAnnotation records, on the autoscaler's account, where the autoscaler was
	// last placed.
	autoscalerPlacementAnnotation = "kubehenge.dev/autoscaler-placement"
	// scalingLook is how often a cluster whose pools are growing is looked at, so that one Proxmox VE
	// has no room for is reported while it waits.
	scalingLook = time.Minute
	// maxNodeProvisionTime is how long the autoscaler gives a new machine to become a node before it
	// gives up on it: the classes' health checks wait 20 minutes for a node to join, after the clone.
	maxNodeProvisionTime = "30m"
	// The autoscaler's tuning where the variable leaves it out: upstream's defaults.
	defaultExpander           = "least-waste"
	defaultScaleDownMinutes   = 10
	autoscalerHealthPort      = 8085
	workloadConfigDirectory   = "/etc/kubehenge/workload"
	managementConfigDirectory = "/etc/kubehenge/management"
	// autoscalerUser is the image's nonroot user and group.
	autoscalerUser = 65532
	// autoscalerLivenessSeconds is how long the autoscaler has to start, and how often it is asked
	// after: one loop of it can take seconds on a large cluster.
	autoscalerLivenessSeconds = 30
	// noRoomMessage starts what capmox reports on a ProxmoxMachine whose VM no allowed node has the
	// memory for.
	noRoomMessage = "cannot reserve "
)

// AutoscalerSettings is the autoscaler variable: where the autoscaler runs and how it scales.
type AutoscalerSettings struct {
	Placement                     string `json:"placement,omitempty"`
	Expander                      string `json:"expander,omitempty"`
	ScaleDownDelayAfterAddMinutes *int   `json:"scaleDownDelayAfterAddMinutes,omitempty"`
	ScaleDownUnneededMinutes      *int   `json:"scaleDownUnneededMinutes,omitempty"`
}

// autoscalerSettingsOf reads the cluster's autoscaler variable, with upstream's defaults for what it
// leaves out.
func autoscalerSettingsOf(c *clusterv1.Cluster) (AutoscalerSettings, error) {
	s, _, err := optionalVariable[AutoscalerSettings](c, AutoscalerVariable)
	if err != nil {
		return s, err
	}
	s.Placement = cmp.Or(s.Placement, PlacementManagement)
	s.Expander = cmp.Or(s.Expander, defaultExpander)
	if s.ScaleDownDelayAfterAddMinutes == nil {
		s.ScaleDownDelayAfterAddMinutes = new(defaultScaleDownMinutes)
	}
	if s.ScaleDownUnneededMinutes == nil {
		s.ScaleDownUnneededMinutes = new(defaultScaleDownMinutes)
	}
	return s, nil
}

// AutoscalerImages are the cluster autoscaler's releases, one for each Kubernetes minor version:
// each carries the scheduler of its minor, which it simulates where pods would fit.
type AutoscalerImages []autoscalerImage

type autoscalerImage struct {
	minor int
	ref   string
}

var autoscalerTag = regexp.MustCompile(`:v1\.(\d+)\.\d+@sha256:[0-9a-f]{64}$`)

// ParseAutoscalerImages reads the autoscaler's images, as <registry>/<repository>:v1.<minor>.<patch>
// @sha256:<digest>, one for each minor.
func ParseAutoscalerImages(refs []string) (AutoscalerImages, error) {
	images := make(AutoscalerImages, 0, len(refs))
	for _, ref := range refs {
		m := autoscalerTag.FindStringSubmatch(ref)
		if m == nil {
			return nil, fmt.Errorf("%q is not an image as <registry>/<repository>:v1.<minor>.<patch>@sha256:<digest>", ref)
		}
		minor, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("%q: %w", ref, err)
		}
		if slices.ContainsFunc(images, func(i autoscalerImage) bool { return i.minor == minor }) {
			return nil, fmt.Errorf("two images for Kubernetes 1.%d", minor)
		}
		images = append(images, autoscalerImage{minor, ref})
	}
	if len(images) == 0 {
		return nil, errors.New("no images")
	}
	slices.SortFunc(images, func(a, b autoscalerImage) int { return a.minor - b.minor })
	return images, nil
}

// For is the image for a cluster at version: the release of its minor, else the newest one before
// it, as for a minor the autoscaler has no release for yet. A cluster older than every release
// listed gets the oldest.
func (images AutoscalerImages) For(version string) string {
	if len(images) == 0 {
		return ""
	}
	minor, ok := kubernetesMinor(version)
	if !ok {
		return images[len(images)-1].ref
	}
	chosen := images[0].ref
	for _, i := range images {
		if i.minor <= minor {
			chosen = i.ref
		}
	}
	return chosen
}

// ManagementAPI is how a workload cluster's autoscaler reaches the management cluster where it runs
// inside its cluster.
type ManagementAPI struct {
	Server string
	CA     []byte
}

// AutoscalerReconciler runs a cluster autoscaler for every cluster with an autoscaled pool, while
// it has one. Each signs in to its cluster as the autoscaler identity, which reads what the
// autoscaler simulates and evicts pods, never with Cluster API's administrator kubeconfig: beside
// the cluster with the identity's token that WorkloadIdentities keeps, inside it as the identity's
// account itself. In the
// management cluster its account may read the machines of its cluster's namespace and scale only
// its own cluster's autoscaled pools: a Role is namespace-wide and the interface's clusters share
// one, so the chart's admission policy holds each autoscaler to the names recorded for it here.
// It also tells the autoscaler what a node of each autoscaled pool holds, so that a pool grows from
// none, and reports on the Cluster a new worker Proxmox VE has no room for.
type AutoscalerReconciler struct {
	Client         client.Client
	ClassNamespace string
	// Namespace holds the autoscalers that run in the management cluster, their accounts and the
	// admission policy's ConfigMap.
	Namespace string
	// Remotes reaches the workload clusters.
	Remotes *Remotes
	Images  AutoscalerImages
	// Management is how an autoscaler inside its cluster reaches the management cluster; with no
	// server, no cluster can have one there.
	Management ManagementAPI
	// Events reports on a Cluster what keeps its autoscaler from working; may be nil.
	Events events.EventRecorder

	now func() time.Time
}

// SetupWithManager registers the reconciler for Clusters, their MachineDeployments and the
// autoscalers' Deployments. A pool's MachineDeployment is new or renamed after a change of the
// Cluster, and its replicas change as the autoscaler scales it.
func (a *AutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	changed := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{})
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(changed)).
		Watches(&clusterv1.MachineDeployment{}, handler.EnqueueRequestsFromMapFunc(clusterOfLabelled(clusterv1.ClusterNameLabel, "")), builder.WithPredicates(changed)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(clusterOfLabelled(ClusterNameLabel, ClusterNamespaceLabel)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("cluster-autoscaler").
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		Complete(a)
	if err != nil {
		return fmt.Errorf("setting up the cluster autoscalers: %w", err)
	}
	return nil
}

// clusterOfLabelled maps an object to the Cluster its labels name, in the namespace a label names
// or else its own.
func clusterOfLabelled(nameLabel, namespaceLabel string) handler.MapFunc {
	return func(_ context.Context, o client.Object) []ctrl.Request {
		name := o.GetLabels()[nameLabel]
		if name == "" {
			return nil
		}
		namespace := o.GetNamespace()
		if namespaceLabel != "" {
			namespace = o.GetLabels()[namespaceLabel]
		}
		return []ctrl.Request{{Namespace: namespace, Name: name}}
	}
}

// autoscalerName names a cluster's autoscaler in the management cluster, and its account there.
func autoscalerName(key client.ObjectKey) string { return qualifiedName(key.Namespace, key.Name) }

// Reconcile implements reconcile.Reconciler.
func (a *AutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := a.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, a.removeFromManagement(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	if !Owns(cluster, a.ClassNamespace) {
		return ctrl.Result{}, nil
	}
	// The autoscaler goes as soon as the cluster does; what it has inside the cluster goes with it.
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, a.removeDeleted(ctx, cluster)
	}
	deployments := &clusterv1.MachineDeploymentList{}
	if err := a.Client.List(ctx, deployments, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing the MachineDeployments of cluster %s: %w", req.NamespacedName, err)
	}
	if err := a.applyCapacity(ctx, cluster, deployments.Items); err != nil {
		return ctrl.Result{}, err
	}
	if !autoscaled(cluster) {
		return ctrl.Result{}, a.switchOff(ctx, cluster)
	}
	settings, err := autoscalerSettingsOf(cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if settings.Placement == PlacementWorkload && a.Management.Server == "" {
		// Installing Kubehenge with the address restarts the controller, which looks at every cluster.
		a.event(cluster, corev1.EventTypeWarning, "AutoscalerNotPlaced", "PlaceAutoscaler",
			"the autoscaler cannot run inside the cluster: Kubehenge was installed without the management cluster's address (managementEndpoint)")
		return ctrl.Result{}, nil
	}
	// Until then the API server may not answer, and the autoscaler has nothing to scale.
	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	next, err := a.switchOn(ctx, cluster, settings, deployments.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	if growing(cluster, deployments.Items) {
		if err := a.reportNoRoom(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		if next == 0 || next > scalingLook {
			next = scalingLook
		}
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

func (a *AutoscalerReconciler) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *AutoscalerReconciler) event(cluster *clusterv1.Cluster, kind, reason, action, note string) {
	if a.Events != nil {
		a.Events.Eventf(cluster, nil, kind, reason, action, "%s", note)
	}
}

// poolOf is the pool of the cluster's topology a MachineDeployment is made from, or nil.
func poolOf(cluster *clusterv1.Cluster, md *clusterv1.MachineDeployment) *clusterv1.MachineDeploymentTopology {
	name := md.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel]
	for i := range cluster.Spec.Topology.Workers.MachineDeployments {
		if p := &cluster.Spec.Topology.Workers.MachineDeployments[i]; p.Name == name {
			return p
		}
	}
	return nil
}

func autoscaledPool(p *clusterv1.MachineDeploymentTopology) bool {
	if p == nil {
		return false
	}
	_, ok, err := poolScale(p)
	return ok && err == nil
}

// growing tells whether an autoscaled pool waits for workers it was scaled up to.
func growing(cluster *clusterv1.Cluster, deployments []clusterv1.MachineDeployment) bool {
	return slices.ContainsFunc(deployments, func(md clusterv1.MachineDeployment) bool {
		return autoscaledPool(poolOf(cluster, &md)) && md.Spec.Replicas != nil &&
			*md.Spec.Replicas > ptrValue(md.Status.ReadyReplicas)
	})
}

func ptrValue[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// applyCapacity keeps the capacity annotations on the MachineDeployment of every autoscaled pool,
// following a change of its machines' size, and takes them off a pool no longer autoscaled. Server
// side apply under a field owner of its own leaves the topology controller's annotations alone.
func (a *AutoscalerReconciler) applyCapacity(ctx context.Context, cluster *clusterv1.Cluster, deployments []clusterv1.MachineDeployment) error {
	var class *clusterv1.ClusterClass
	for i := range deployments {
		md := &deployments[i]
		want := map[string]string{}
		if pool := poolOf(cluster, md); autoscaledPool(pool) {
			if class == nil {
				class = &clusterv1.ClusterClass{}
				if err := a.Client.Get(ctx, client.ObjectKey{Namespace: a.ClassNamespace, Name: cluster.Spec.Topology.ClassRef.Name}, class); err != nil {
					return fmt.Errorf("reading the ClusterClass of cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
				}
			}
			size, err := nodeShape(cluster, class, "workerMachine", pool.Variables.Overrides)
			if err != nil {
				return err
			}
			want = capacityAnnotations(size, md.Spec.Template.Labels)
		}
		if capacityCurrent(md.Annotations, want) {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(clusterv1.GroupVersion.WithKind("MachineDeployment"))
		u.SetNamespace(md.Namespace)
		u.SetName(md.Name)
		if len(want) > 0 {
			u.SetAnnotations(want)
		}
		if err := a.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(autoscalerFieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("telling the autoscaler what a node of MachineDeployment %s holds: %w", md.Name, err)
		}
	}
	return nil
}

// capacityCurrent tells whether a MachineDeployment's capacity annotations are those wanted.
func capacityCurrent(have, want map[string]string) bool {
	for k, v := range have {
		if strings.HasPrefix(k, capacityPrefix) && want[k] != v {
			return false
		}
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// switchOn sets up the cluster's autoscaler where the settings place it, and says how soon it is to
// be looked at again, or 0 for once something changes. Its rights come before it does, and the
// admission policy's record of what it may scale before the rights to scale.
func (a *AutoscalerReconciler) switchOn(ctx context.Context, cluster *clusterv1.Cluster, s AutoscalerSettings, deployments []clusterv1.MachineDeployment) (time.Duration, error) {
	key := client.ObjectKeyFromObject(cluster)
	if err := a.hold(ctx, cluster); err != nil {
		return 0, err
	}
	sa, err := a.ensureAccount(ctx, key)
	if err != nil {
		return 0, err
	}
	if err = a.recordPools(ctx, cluster, deployments); err != nil {
		return 0, err
	}
	if err = a.bindRole(ctx, cluster); err != nil {
		return 0, err
	}
	was := sa.Annotations[autoscalerPlacementAnnotation]
	var workload remote
	if s.Placement == PlacementWorkload || was == PlacementWorkload {
		var found bool
		if workload, found, err = a.Remotes.get(ctx, cluster); err != nil || !found {
			return waitInterval, err
		}
	}
	if was != "" && was != s.Placement {
		if err = a.removePlaced(ctx, key, was, workload.client); err != nil {
			a.Remotes.forget(cluster)
			return 0, err
		}
	}
	image := a.Images.For(cluster.Spec.Topology.Version)
	var next time.Duration
	if s.Placement == PlacementWorkload {
		if next, err = a.placeInside(ctx, cluster, s, image, workload.client); err != nil {
			a.Remotes.forget(cluster)
			return 0, err
		}
	} else if err = applyServerSide(ctx, a.Client, a.managementAutoscaler(cluster, s, image), appsv1.SchemeGroupVersion.WithKind("Deployment")); err != nil {
		return 0, fmt.Errorf("applying the autoscaler of cluster %s: %w", key, err)
	}
	if was != s.Placement {
		if err = a.recordPlacement(ctx, sa, s.Placement); err != nil {
			return 0, err
		}
	}
	return next, nil
}

// placeInside runs the cluster's autoscaler on its control planes, as the autoscaler identity, with
// a token of its management account in the cluster renewed as an identity's is. It says when the
// token is due.
func (a *AutoscalerReconciler) placeInside(ctx context.Context, cluster *clusterv1.Cluster, s AutoscalerSettings, image string, workload client.Client) (time.Duration, error) {
	if _, err := identityOf(IdentityAutoscaler).ensure(ctx, workload); err != nil {
		return 0, err
	}
	next, err := a.managementToken(ctx, client.ObjectKeyFromObject(cluster), workload)
	if err != nil {
		return 0, err
	}
	if err = applyServerSide(ctx, workload, workloadAutoscaler(cluster, s, image), appsv1.SchemeGroupVersion.WithKind("Deployment")); err != nil {
		return 0, fmt.Errorf("applying the autoscaler inside cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return next, nil
}

// managementToken keeps a token of the autoscaler's management account, with the kubeconfig that
// reaches the management cluster, in its Secret inside the cluster, and says when it is due again.
func (a *AutoscalerReconciler) managementToken(ctx context.Context, key client.ObjectKey, workload client.Client) (time.Duration, error) {
	s := &corev1.Secret{Namespace: metav1.NamespaceSystem, Name: autoscalerManagementSecret}
	if err := workload.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil && !apierrors.IsNotFound(err) {
		return 0, fmt.Errorf("reading the autoscaler's Secret %s: %w", s.Name, err)
	}
	if left := tokenLeft(s, a.clock()); left > 0 {
		return left, nil
	}
	token, expires, err := requestToken(ctx, a.Client, a.Namespace, autoscalerName(key))
	if err != nil {
		return 0, err
	}
	data := tokenData(a.Management.Server, a.Management.CA, token)
	if err = withKubeconfig(data); err != nil {
		return 0, err
	}
	s = &corev1.Secret{Namespace: metav1.NamespaceSystem, Name: autoscalerManagementSecret}
	if _, err = controllerutil.CreateOrUpdate(ctx, workload, s, func() error { setToken(s, data, expires); return nil }); err != nil {
		return 0, fmt.Errorf("applying the autoscaler's Secret %s: %w", s.Name, err)
	}
	return tokenRenewal, nil
}

// recordPlacement records on the autoscaler's account where it now runs, once what ran elsewhere
// is gone.
func (a *AutoscalerReconciler) recordPlacement(ctx context.Context, sa *corev1.ServiceAccount, placement string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{autoscalerPlacementAnnotation: placement}}})
	if err != nil {
		return fmt.Errorf("recording where the autoscaler runs: %w", err)
	}
	if err = a.Client.Patch(ctx, sa, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("recording where the autoscaler runs on ServiceAccount %s: %w", sa.Name, err)
	}
	return nil
}

// clusterLabels name the cluster on what runs its autoscaler in the management cluster.
func clusterLabels(key client.ObjectKey) map[string]string {
	return map[string]string{
		appName:               autoscalerApp,
		managedByLabel:        "kubehenge",
		ClusterNamespaceLabel: key.Namespace,
		ClusterNameLabel:      key.Name,
	}
}

// ensureAccount creates the autoscaler's account in the management cluster, and returns it with the
// record of its last token.
func (a *AutoscalerReconciler) ensureAccount(ctx context.Context, key client.ObjectKey) (*corev1.ServiceAccount, error) {
	sa := &corev1.ServiceAccount{Namespace: a.Namespace, Name: autoscalerName(key)}
	_, err := controllerutil.CreateOrUpdate(ctx, a.Client, sa, func() error {
		if sa.Labels == nil {
			sa.Labels = map[string]string{}
		}
		maps.Copy(sa.Labels, clusterLabels(key))
		// Only the autoscaler's pod in the management cluster mounts a token of its own.
		sa.AutomountServiceAccountToken = new(false)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("applying the autoscaler's ServiceAccount of cluster %s: %w", key, err)
	}
	return sa, nil
}

// recordPools writes, for the admission policy, the cluster and the MachineDeployments of its
// autoscaled pools the cluster's autoscaler may scale: <namespace>/<cluster>/<name>,<name> under its
// account's name.
func (a *AutoscalerReconciler) recordPools(ctx context.Context, cluster *clusterv1.Cluster, deployments []clusterv1.MachineDeployment) error {
	var names []string
	for i := range deployments {
		if md := &deployments[i]; autoscaledPool(poolOf(cluster, md)) && md.DeletionTimestamp.IsZero() {
			names = append(names, md.Name)
		}
	}
	slices.Sort(names)
	value := cluster.Namespace + "/" + cluster.Name + "/" + strings.Join(names, ",")
	return a.setPolicyEntry(ctx, autoscalerName(client.ObjectKeyFromObject(cluster)), &value)
}

// setPolicyEntry sets an autoscaler's entry in the admission policy's ConfigMap, or removes it for
// nil. A merge patch touches that entry alone, so clusters reconciled at once do not conflict.
func (a *AutoscalerReconciler) setPolicyEntry(ctx context.Context, account string, value *string) error {
	patch, err := json.Marshal(map[string]any{"data": map[string]*string{account: value}})
	if err != nil {
		return fmt.Errorf("the admission policy's entry for %s: %w", account, err)
	}
	cm := &corev1.ConfigMap{Namespace: a.Namespace, Name: AutoscalerPolicy}
	for range 2 {
		err = a.Client.Patch(ctx, cm, client.RawPatch(types.MergePatchType, patch))
		if !apierrors.IsNotFound(err) || value == nil {
			break
		}
		created := &corev1.ConfigMap{Namespace: a.Namespace, Name: AutoscalerPolicy, Data: map[string]string{account: *value}}
		if err = a.Client.Create(ctx, created); !apierrors.IsAlreadyExists(err) {
			break
		}
	}
	if err != nil && (value != nil || !apierrors.IsNotFound(err)) {
		return fmt.Errorf("recording what the autoscaler %s may scale: %w", account, err)
	}
	return nil
}

// bindRole gives the cluster's autoscaler the chart's role in the cluster's namespace. The binding
// is the cluster's, so it goes with it.
func (a *AutoscalerReconciler) bindRole(ctx context.Context, cluster *clusterv1.Cluster) error {
	b := &rbacv1.RoleBinding{Namespace: cluster.Namespace, Name: autoscalerBinding(cluster.Name)}
	return applyOwned(ctx, a.Client, cluster, b, func(got *rbacv1.RoleBinding) {
		got.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: AutoscalerRole}
		got.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: autoscalerName(client.ObjectKeyFromObject(cluster)), Namespace: a.Namespace}}
	})
}

func autoscalerBinding(cluster string) string { return AutoscalerRole + "-" + cluster }

// applyServerSide applies a typed object with server-side apply as the autoscaler's field owner, so
// that an unchanged one costs no write and fields the API server defaults are left to it.
func applyServerSide(ctx context.Context, c client.Client, obj client.Object, gvk schema.GroupVersionKind) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Errorf("%s %s: %w", gvk.Kind, obj.GetName(), err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(gvk)
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "spec", "template", "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "status")
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(autoscalerFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying %s %s: %w", gvk.Kind, obj.GetName(), err)
	}
	return nil
}

// autoscalerArgs are the autoscaler's flags for the cluster, wherever it runs. Proxmox VE's cloud
// controller manager puts each node in its Proxmox VE node's zone, which would keep pools that are
// alike from being balanced. Volumes on Proxmox VE storage are no local storage, which upstream's
// default would keep almost every node from being removed for.
func autoscalerArgs(cluster *clusterv1.Cluster, s AutoscalerSettings) []string {
	return []string{
		"--cloud-provider=clusterapi",
		fmt.Sprintf("--node-group-auto-discovery=clusterapi:namespace=%s,clusterName=%s", cluster.Namespace, cluster.Name),
		// One replica runs, replaced only once it has stopped.
		"--leader-elect=false",
		"--expander=" + s.Expander,
		"--balance-similar-node-groups",
		"--balancing-ignore-label=topology.kubernetes.io/zone",
		"--balancing-ignore-label=topology.kubernetes.io/region",
		"--skip-nodes-with-local-storage=false",
		"--enforce-node-group-min-size",
		"--max-node-provision-time=" + maxNodeProvisionTime,
		fmt.Sprintf("--scale-down-delay-after-add=%dm", *s.ScaleDownDelayAfterAddMinutes),
		fmt.Sprintf("--scale-down-unneeded-time=%dm", *s.ScaleDownUnneededMinutes),
	}
}

// autoscalerContainer is the autoscaler's container with the configuration directory it reads its
// kubeconfig from.
func autoscalerContainer(image string, args []string, directory string) corev1.Container {
	return corev1.Container{
		Name:    autoscalerApp,
		Image:   image,
		Command: []string{"/cluster-autoscaler"},
		Args:    args,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		},
		LivenessProbe: &corev1.Probe{
			HTTPGet:             &corev1.HTTPGetAction{Path: "/health-check", Port: intstr.FromInt32(autoscalerHealthPort)},
			InitialDelaySeconds: autoscalerLivenessSeconds,
			PeriodSeconds:       autoscalerLivenessSeconds,
		},
		SecurityContext: restrictedSecurity(),
		VolumeMounts:    []corev1.VolumeMount{{Name: "config", MountPath: directory, ReadOnly: true}},
	}
}

// managementAutoscaler is the cluster's autoscaler in the management cluster: it reaches the
// machines with its own account there and the cluster with the autoscaler identity's kubeconfig.
func (a *AutoscalerReconciler) managementAutoscaler(cluster *clusterv1.Cluster, s AutoscalerSettings, image string) *appsv1.Deployment {
	key := client.ObjectKeyFromObject(cluster)
	name := autoscalerName(key)
	labels := clusterLabels(key)
	args := append(autoscalerArgs(cluster, s), "--kubeconfig="+workloadConfigDirectory+"/"+IdentityKubeconfigKey, "--clusterapi-cloud-config-authoritative")
	return &appsv1.Deployment{
		Namespace: a.Namespace, Name: name, Labels: labels,
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				ClusterNamespaceLabel: key.Namespace, ClusterNameLabel: key.Name,
			}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           name,
					AutomountServiceAccountToken: new(true),
					EnableServiceLinks:           new(false),
					SecurityContext:              restrictedPod(autoscalerUser),
					Containers:                   []corev1.Container{autoscalerContainer(image, args, workloadConfigDirectory)},
					Volumes: []corev1.Volume{{Name: "config", Secret: &corev1.SecretVolumeSource{
						SecretName: mountedSecretName(key, IdentityAutoscaler),
					}}},
				},
			},
		},
	}
}

// workloadAutoscaler is the cluster's autoscaler inside it, on its control planes, which it never
// removes: it reaches the cluster as the autoscaler identity and the management cluster with the
// kubeconfig in its Secret.
func workloadAutoscaler(cluster *clusterv1.Cluster, s AutoscalerSettings, image string) *appsv1.Deployment {
	labels := map[string]string{appName: autoscalerApp, managedByLabel: "kubehenge"}
	args := append(autoscalerArgs(cluster, s), "--cloud-config="+managementConfigDirectory+"/"+IdentityKubeconfigKey)
	return &appsv1.Deployment{
		Namespace: metav1.NamespaceSystem, Name: autoscalerAccount, Labels: labels,
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           autoscalerAccount,
					AutomountServiceAccountToken: new(true),
					EnableServiceLinks:           new(false),
					PriorityClassName:            "system-cluster-critical",
					SecurityContext:              restrictedPod(autoscalerUser),
					Tolerations:                  []corev1.Toleration{{Key: controlPlaneRole, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
					Affinity:                     &controlPlaneAffinity,
					Containers:                   []corev1.Container{autoscalerContainer(image, args, managementConfigDirectory)},
					Volumes:                      []corev1.Volume{{Name: "config", Secret: &corev1.SecretVolumeSource{SecretName: autoscalerManagementSecret}}},
				},
			},
		},
	}
}

// autoscalerWorkloadRules are what the autoscaler identity may do in its cluster, upstream's
// workload role without the leader election it does not take part in: read what it simulates the
// scheduler over, evict the pods of a node it removes and taint that node first, and report.
var autoscalerWorkloadRules = []rbacv1.PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"namespaces", "persistentvolumeclaims", "persistentvolumes", "pods", "replicationcontrollers", "services"}, Verbs: []string{"get", "list", "watch"}},
	{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch", verbUpdate}},
	{APIGroups: []string{""}, Resources: []string{"pods/eviction"}, Verbs: []string{"create"}},
	{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
	{APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"}, Verbs: []string{"list", "watch"}},
	{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"csinodes", "storageclasses", "csidrivers", "csistoragecapacities", "volumeattachments"}, Verbs: []string{"get", "list", "watch"}},
	{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"list", "watch"}},
	{APIGroups: []string{"apps"}, Resources: []string{"daemonsets", "replicasets", "statefulsets"}, Verbs: []string{"list", "watch"}},
	{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceslices", "deviceclasses", "resourceclaims"}, Verbs: []string{"get", "list", "watch"}},
}

// autoscalerStatusRules let it keep its status ConfigMap in kube-system, and no other; a create
// cannot be held to a name.
var autoscalerStatusRules = []rbacv1.PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create"}},
	{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{autoscalerStatus}, Verbs: []string{"get", verbUpdate, "delete"}},
}

// removePlaced removes the autoscaler from where it ran before its placement changed, and inside
// the cluster its way into the management cluster. The autoscaler identity stays in the cluster for
// the new placement; its token beside the cluster goes with WorkloadIdentities once unwanted.
func (a *AutoscalerReconciler) removePlaced(ctx context.Context, key client.ObjectKey, placement string, workload client.Client) error {
	if placement == PlacementWorkload {
		return deleteAll(ctx, workload,
			&appsv1.Deployment{Namespace: metav1.NamespaceSystem, Name: autoscalerAccount},
			&corev1.Secret{Namespace: metav1.NamespaceSystem, Name: autoscalerManagementSecret})
	}
	return deleteAll(ctx, a.Client, &appsv1.Deployment{Namespace: a.Namespace, Name: autoscalerName(key)})
}

// deleteAll deletes objects, those already gone too.
func deleteAll(ctx context.Context, c client.Client, objects ...client.Object) error {
	for _, o := range objects {
		if err := c.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting %T %s: %w", o, o.GetName(), err)
		}
	}
	return nil
}

// switchOff removes the autoscaler of a cluster without an autoscaled pool, and everything it had:
// the autoscaler stops first, then loses its rights, and its account in the management cluster,
// which says that there is something to remove, goes last. The identity's token beside the cluster
// goes with WorkloadIdentities.
func (a *AutoscalerReconciler) switchOff(ctx context.Context, cluster *clusterv1.Cluster) error {
	key := client.ObjectKeyFromObject(cluster)
	err := a.Client.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: autoscalerName(key)}, &corev1.ServiceAccount{})
	if apierrors.IsNotFound(err) {
		return a.release(ctx, cluster)
	}
	if err != nil {
		return fmt.Errorf("reading the autoscaler's ServiceAccount of cluster %s: %w", key, err)
	}
	name := autoscalerName(key)
	err = deleteAll(ctx, a.Client,
		&appsv1.Deployment{Namespace: a.Namespace, Name: name},
		&rbacv1.RoleBinding{Namespace: cluster.Namespace, Name: autoscalerBinding(cluster.Name)},
	)
	if err != nil {
		return err
	}
	if err = a.setPolicyEntry(ctx, name, nil); err != nil {
		return err
	}
	workload, found, err := a.Remotes.get(ctx, cluster)
	if err != nil {
		return err
	}
	if found {
		err = deleteAll(ctx, workload.client,
			&appsv1.Deployment{Namespace: metav1.NamespaceSystem, Name: autoscalerAccount},
			&corev1.Secret{Namespace: metav1.NamespaceSystem, Name: autoscalerManagementSecret},
			&corev1.ConfigMap{Namespace: metav1.NamespaceSystem, Name: autoscalerStatus},
		)
		if err == nil {
			err = identityOf(IdentityAutoscaler).remove(ctx, workload.client)
		}
		if err != nil {
			a.Remotes.forget(cluster)
			return err
		}
	}
	if err = deleteAll(ctx, a.Client, &corev1.ServiceAccount{Namespace: a.Namespace, Name: name}); err != nil {
		return err
	}
	return a.release(ctx, cluster)
}

// removeFromManagement removes what ran a cluster's autoscaler in the management cluster once the
// cluster goes, its identity's token beside it among them: what it had inside the cluster goes with
// the cluster's machines.
func (a *AutoscalerReconciler) removeFromManagement(ctx context.Context, key client.ObjectKey) error {
	name := autoscalerName(key)
	err := deleteAll(ctx, a.Client,
		&appsv1.Deployment{Namespace: a.Namespace, Name: name},
		&rbacv1.RoleBinding{Namespace: key.Namespace, Name: autoscalerBinding(key.Name)},
		&corev1.Secret{Namespace: a.Namespace, Name: mountedSecretName(key, IdentityAutoscaler)},
	)
	if err != nil {
		return err
	}
	if err = a.setPolicyEntry(ctx, name, nil); err != nil {
		return err
	}
	return deleteAll(ctx, a.Client, &corev1.ServiceAccount{Namespace: a.Namespace, Name: name})
}

// removeDeleted removes what ran a deleted cluster's autoscaler beside it, then lets the cluster go.
func (a *AutoscalerReconciler) removeDeleted(ctx context.Context, cluster *clusterv1.Cluster) error {
	if err := a.removeFromManagement(ctx, client.ObjectKeyFromObject(cluster)); err != nil {
		return err
	}
	return a.release(ctx, cluster)
}

// hold puts AutoscalerFinalizer on the cluster before anything of its autoscaler is made outside
// its namespace, where owner references cannot reach: a cluster deleted while the controller is
// down then waits for it to remove them.
func (a *AutoscalerReconciler) hold(ctx context.Context, cluster *clusterv1.Cluster) error {
	if controllerutil.ContainsFinalizer(cluster, AutoscalerFinalizer) {
		return nil
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	controllerutil.AddFinalizer(cluster, AutoscalerFinalizer)
	if err := a.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("adding the autoscaler's finalizer to cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return nil
}

// release takes AutoscalerFinalizer off the cluster once nothing of its autoscaler is left outside
// its namespace.
func (a *AutoscalerReconciler) release(ctx context.Context, cluster *clusterv1.Cluster) error {
	if !controllerutil.ContainsFinalizer(cluster, AutoscalerFinalizer) {
		return nil
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	controllerutil.RemoveFinalizer(cluster, AutoscalerFinalizer)
	if err := a.Client.Patch(ctx, cluster, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing the autoscaler's finalizer from cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return nil
}

// reportNoRoom reports on the Cluster each new worker of an autoscaled pool that capmox found no
// Proxmox VE node with the memory for. The autoscaler knows nothing of Proxmox VE: it waits for the
// machine, gives up after maxNodeProvisionTime and backs off, and the event says why.
func (a *AutoscalerReconciler) reportNoRoom(ctx context.Context, cluster *clusterv1.Cluster) error {
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: proxmoxMachineVersion, Kind: "ProxmoxMachineList"})
	if err := a.Client.List(ctx, machines, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return fmt.Errorf("listing the ProxmoxMachines of cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	for i := range machines.Items {
		pm := &machines.Items[i]
		pool := pm.GetLabels()[clusterv1.ClusterTopologyMachineDeploymentNameLabel]
		if pool == "" || !slices.ContainsFunc(cluster.Spec.Topology.Workers.MachineDeployments, func(p clusterv1.MachineDeploymentTopology) bool {
			return p.Name == pool && autoscaledPool(&p)
		}) {
			continue
		}
		if message, ok := noRoom(pm); ok {
			a.event(cluster, corev1.EventTypeWarning, "AutoscalerNoRoom", "AddWorker",
				fmt.Sprintf("Proxmox VE has no node with the memory for a new worker of pool %s: %s", pool, message))
		}
	}
	return nil
}

// noRoom is capmox's message on a ProxmoxMachine whose VM no allowed node has the memory for.
func noRoom(pm *unstructured.Unstructured) (string, bool) {
	conditions, _, _ := unstructured.NestedSlice(pm.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok || cond["type"] != "VirtualMachineProvisioned" || cond["status"] != string(metav1.ConditionFalse) || cond["reason"] != "VMProvisionFailed" {
			continue
		}
		if message, isText := cond["message"].(string); isText && strings.HasPrefix(message, noRoomMessage) {
			return message, true
		}
	}
	return "", false
}
