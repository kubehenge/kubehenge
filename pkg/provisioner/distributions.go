package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	// ClassLabel names the ClusterClass a ConfigMap of templates belongs to, and marks the
	// templates applied from it.
	ClassLabel = "kubehenge.dev/class"
	// TemplatesAnnotation on a ClusterClass, and the label on its applied templates, is the hash
	// of the templates applied last. Cluster API does not watch a class's templates, so changing
	// the annotation has it look at the class again.
	TemplatesAnnotation = "kubehenge.dev/templates"
	// providerLabel marks the Deployments of Cluster API providers.
	providerLabel = "cluster.x-k8s.io/provider"
	// templatesKey holds a ConfigMap's templates, as YAML documents.
	templatesKey = "templates.yaml"
)

// Distributions runs the Cluster API providers of a ClusterClass's distribution only while a
// Cluster uses them, which on a management cluster with one distribution in use leaves the others'
// controllers idle. Without a Cluster their Deployments are scaled to none. For the first, they are
// scaled up and, once they serve, the class's control-plane and bootstrap templates, which their
// webhooks admit, are applied from the ConfigMap of the class's name; until then Cluster API holds
// the Cluster, as it does any Cluster whose class is not ready. Once the last Cluster is gone, the
// templates go first, while the webhooks still answer, then the providers.
type Distributions struct {
	Client client.Client
	// ClassNamespace holds Kubehenge's ClusterClasses and their ConfigMaps of templates.
	ClassNamespace string
	// Providers lists, by ClusterClass, the namespaces of the providers only its Clusters use.
	Providers map[string][]string
}

// ParseClassProviders reads <class>=<namespace>,<namespace> pairs.
func ParseClassProviders(pairs []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, p := range pairs {
		class, list, ok := strings.Cut(p, "=")
		namespaces := strings.Split(list, ",")
		if !ok || class == "" || slices.Contains(namespaces, "") {
			return nil, fmt.Errorf("%q: want <class>=<namespace>[,<namespace>...]", p)
		}
		if _, twice := out[class]; twice {
			return nil, fmt.Errorf("%s is given twice", class)
		}
		out[class] = namespaces
	}
	return out, nil
}

// Namespaces lists every namespace of the providers, for the cache of Deployments.
func (d *Distributions) Namespaces() []string {
	var all []string
	for _, ns := range d.Providers {
		all = append(all, ns...)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// SetupWithManager registers the reconciler, keyed by ClusterClass. Clusters wake it only when
// created, deleted, or moved to another class or control plane; Deployments as their pods come up.
func (d *Distributions) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		Named("distributions").
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(d.every), builder.WithPredicates(usageChanged)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(d.ofConfigMap)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(d.ofDeployment)).
		Complete(d)
	if err != nil {
		return fmt.Errorf("setting up the distributions controller: %w", err)
	}
	return nil
}

var usageChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		was, ok := e.ObjectOld.(*clusterv1.Cluster)
		is, ok2 := e.ObjectNew.(*clusterv1.Cluster)
		return !ok || !ok2 || was.Spec.Topology.ClassRef != is.Spec.Topology.ClassRef || was.Spec.ControlPlaneRef != is.Spec.ControlPlaneRef
	},
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// Reconcile scales one class's providers and applies or removes its templates.
func (d *Distributions) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	namespaces, ok := d.Providers[req.Name]
	if !ok || req.Namespace != d.ClassNamespace {
		return ctrl.Result{}, nil
	}
	templates, hash, err := d.templates(ctx, req.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	used, err := d.used(ctx, req.Name, templates)
	if err != nil {
		return ctrl.Result{}, err
	}
	var deployments []appsv1.Deployment
	for _, ns := range namespaces {
		list := &appsv1.DeploymentList{}
		if err := d.Client.List(ctx, list, client.InNamespace(ns), client.HasLabels{providerLabel}); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing the provider Deployments in %s: %w", ns, err)
		}
		deployments = append(deployments, list.Items...)
	}
	if !used {
		if err := d.removeTemplates(ctx, req.Name, templates, ""); err != nil {
			return ctrl.Result{}, err
		}
		if err := d.markClass(ctx, req.Name, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, d.scale(ctx, deployments, 0)
	}
	if err := d.scale(ctx, deployments, 1); err != nil {
		return ctrl.Result{}, err
	}
	for i := range deployments {
		// Their pods coming up update the Deployments, which brings this back.
		if !serving(&deployments[i]) {
			return ctrl.Result{}, nil
		}
	}
	for _, t := range templates {
		if err := d.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(t), client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying %s %s: %w", t.GetKind(), t.GetName(), err)
		}
	}
	if err := d.removeTemplates(ctx, req.Name, templates, hash); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, d.markClass(ctx, req.Name, hash)
}

// every asks for each class: a Cluster may use any, by its class or by its control plane.
func (d *Distributions) every(context.Context, client.Object) []ctrl.Request {
	out := make([]ctrl.Request, 0, len(d.Providers))
	for class := range d.Providers {
		out = append(out, ctrl.Request{Namespace: d.ClassNamespace, Name: class})
	}
	return out
}

func (d *Distributions) ofConfigMap(_ context.Context, o client.Object) []ctrl.Request {
	if _, ok := d.Providers[o.GetName()]; !ok || o.GetNamespace() != d.ClassNamespace {
		return nil
	}
	return []ctrl.Request{{Namespace: d.ClassNamespace, Name: o.GetName()}}
}

func (d *Distributions) ofDeployment(_ context.Context, o client.Object) []ctrl.Request {
	var out []ctrl.Request
	for class, namespaces := range d.Providers {
		if slices.Contains(namespaces, o.GetNamespace()) {
			out = append(out, ctrl.Request{Namespace: d.ClassNamespace, Name: class})
		}
	}
	return out
}

const (
	// templatesHashBytes of the templates' SHA-256 make the hash in their label: unique enough
	// among one class's versions, and short.
	templatesHashBytes = 8
	// decoderLookahead is how far the decoder reads to tell JSON from YAML.
	decoderLookahead = 4096
)

// templates reads a class's templates from its ConfigMap, labelled for it and with the hash of the
// lot; none if it has no ConfigMap.
func (d *Distributions) templates(ctx context.Context, class string) ([]*unstructured.Unstructured, string, error) {
	cm := &corev1.ConfigMap{}
	if err := d.Client.Get(ctx, client.ObjectKey{Namespace: d.ClassNamespace, Name: class}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("reading the templates of %s: %w", class, err)
	}
	raw := cm.Data[templatesKey]
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:templatesHashBytes])
	var out []*unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(raw), decoderLookahead)
	for {
		u := &unstructured.Unstructured{}
		if err := decoder.Decode(&u.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, "", fmt.Errorf("ConfigMap %s/%s: %w", d.ClassNamespace, class, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		if u.GetKind() == "" || u.GetName() == "" {
			return nil, "", fmt.Errorf("ConfigMap %s/%s: a template without kind or name", d.ClassNamespace, class)
		}
		u.SetNamespace(d.ClassNamespace)
		l := u.GetLabels()
		if l == nil {
			l = map[string]string{}
		}
		l[ClassLabel], l[TemplatesAnnotation] = class, hash
		u.SetLabels(l)
		out = append(out, u)
	}
	return out, hash, nil
}

// used reports whether a Cluster is built from the class, or runs a control plane of its kind
// without it.
func (d *Distributions) used(ctx context.Context, class string, templates []*unstructured.Unstructured) (bool, error) {
	var controlPlanes []string
	for _, t := range templates {
		gvk := t.GroupVersionKind()
		if kind, ok := strings.CutSuffix(gvk.Kind, "Template"); ok && gvk.Group == clusterv1.GroupVersionControlPlane.Group {
			controlPlanes = append(controlPlanes, kind)
		}
	}
	clusters := &clusterv1.ClusterList{}
	if err := d.Client.List(ctx, clusters, client.UnsafeDisableDeepCopy); err != nil {
		return false, fmt.Errorf("listing clusters: %w", err)
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		ref := c.Spec.Topology.ClassRef
		ns := ref.Namespace
		if ns == "" {
			ns = c.Namespace
		}
		cp := c.Spec.ControlPlaneRef
		if ref.Name == class && ns == d.ClassNamespace ||
			cp.APIGroup == clusterv1.GroupVersionControlPlane.Group && slices.Contains(controlPlanes, cp.Kind) {
			return true, nil
		}
	}
	return false, nil
}

// removeTemplates deletes a class's applied templates, all of them or those of another hash than
// keep.
func (d *Distributions) removeTemplates(ctx context.Context, class string, templates []*unstructured.Unstructured, keep string) error {
	selector := labels.Set{ClassLabel: class}.AsSelector()
	if keep != "" {
		other, err := labels.NewRequirement(TemplatesAnnotation, selection.NotEquals, []string{keep})
		if err != nil {
			return fmt.Errorf("selecting the templates of other versions than %s: %w", keep, err)
		}
		selector = selector.Add(*other)
	}
	done := map[schema.GroupVersionKind]bool{}
	for _, t := range templates {
		gvk := t.GroupVersionKind()
		if done[gvk] {
			continue
		}
		done[gvk] = true
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		err := d.Client.DeleteAllOf(ctx, u, client.InNamespace(d.ClassNamespace), client.MatchingLabelsSelector{Selector: selector})
		if err != nil && !meta.IsNoMatchError(err) && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing %s templates of %s: %w", gvk.Kind, class, err)
		}
	}
	return nil
}

// markClass records the hash of the class's applied templates on it, or takes it off with none.
func (d *Distributions) markClass(ctx context.Context, class, hash string) error {
	cc := &clusterv1.ClusterClass{}
	if err := d.Client.Get(ctx, client.ObjectKey{Namespace: d.ClassNamespace, Name: class}, cc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading ClusterClass %s: %w", class, err)
	}
	if cc.Annotations[TemplatesAnnotation] == hash {
		return nil
	}
	value := "null"
	if hash != "" {
		value = fmt.Sprintf("%q", hash)
	}
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%s}}}`, TemplatesAnnotation, value)
	if err := d.Client.Patch(ctx, cc, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("marking ClusterClass %s: %w", class, err)
	}
	return nil
}

// scale sets each Deployment's replicas through the scale subresource, from none to one or
// to none: a count raised by hand is left alone while in use.
func (d *Distributions) scale(ctx context.Context, deployments []appsv1.Deployment, replicas int32) error {
	for i := range deployments {
		dep := &deployments[i]
		current := int32(1)
		if dep.Spec.Replicas != nil {
			current = *dep.Spec.Replicas
		}
		if (replicas == 0) == (current == 0) {
			continue
		}
		// The cache can still hold a count written a moment ago: scaling one provider wakes this
		// for the next before the cache has seen the next one's scale. The subresource is read
		// from the API server, and its resourceVersion makes the write conditional.
		scale := &autoscalingv1.Scale{}
		if err := d.Client.SubResource("scale").Get(ctx, dep, scale); err != nil {
			return fmt.Errorf("reading the scale of %s/%s: %w", dep.Namespace, dep.Name, err)
		}
		if (replicas == 0) == (scale.Spec.Replicas == 0) {
			continue
		}
		scale.Spec.Replicas = replicas
		if err := d.Client.SubResource("scale").Update(ctx, dep, client.WithSubResourceBody(scale)); err != nil {
			return fmt.Errorf("scaling %s/%s to %d: %w", dep.Namespace, dep.Name, replicas, err)
		}
		ctrl.LoggerFrom(ctx).Info("scaled a distribution's provider", "deployment", dep.Namespace+"/"+dep.Name, "replicas", replicas)
	}
	return nil
}

// serving reports whether a Deployment runs and its current pods are all available.
func serving(dep *appsv1.Deployment) bool {
	return wantedReplicas(dep) > 0 && rolledOut(dep)
}

// rolledOut reports whether a Deployment's pods all run its current spec and are available, as
// `kubectl rollout status` does; one scaled to none has rolled out once its pods are gone.
func rolledOut(dep *appsv1.Deployment) bool {
	want := wantedReplicas(dep)
	s := dep.Status
	return s.ObservedGeneration >= dep.Generation && s.UpdatedReplicas >= want && s.AvailableReplicas >= want && s.Replicas == s.UpdatedReplicas
}

func wantedReplicas(dep *appsv1.Deployment) int32 {
	if dep.Spec.Replicas != nil {
		return *dep.Spec.Replicas
	}
	return 1
}
