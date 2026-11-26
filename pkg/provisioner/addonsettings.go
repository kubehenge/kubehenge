package provisioner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// namespaceLabelsVariable are labels for the namespaces the add-ons install into.
	namespaceLabelsVariable = "namespaceLabels"

	// namespaceLabelsAnnotation on a Cluster marks that its namespaces may carry labels from
	// namespaceLabelsVariable, until the controller has taken them off again.
	namespaceLabelsAnnotation = "kubehenge.dev/namespace-labels"
	// managedLabelsAnnotation on a Namespace lists the label keys the controller put there.
	managedLabelsAnnotation = "kubehenge.dev/managed-labels"

	// AddonConflictCondition on a Cluster says that an add-on is not installed because the cluster
	// already holds an install of the same custom resource definitions.
	AddonConflictCondition = "AddonConflict"
	// AddonConflictReason is the reason of AddonConflictCondition.
	AddonConflictReason = "CRDsExist"

	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
)

// retiredVariables are variables a cluster may still set that the ClusterClasses no longer define.
// Cluster API refuses every change to a Cluster that sets one, and stops reconciling its topology.
var retiredVariables = []string{"useExisting"}

// dropRetiredVariables takes the retired variables off the cluster.
func (r *Reconciler) dropRetiredVariables(ctx context.Context, cluster *clusterv1.Cluster) error {
	retired := func(v clusterv1.ClusterVariable) bool { return slices.Contains(retiredVariables, v.Name) }
	if !slices.ContainsFunc(cluster.Spec.Topology.Variables, retired) {
		return nil
	}
	patch := client.MergeFromWithOptions(cluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
	cluster.Spec.Topology.Variables = slices.DeleteFunc(cluster.Spec.Topology.Variables, retired)
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("taking retired variables off the cluster: %w", err)
	}
	return nil
}

// validateAddonSettings holds the namespace labels to Kubernetes' label syntax.
func validateAddonSettings(c *clusterv1.Cluster) field.ErrorList {
	variables := field.NewPath("spec", "topology", "variables")
	var errs field.ErrorList
	labels, _, err := optionalVariable[map[string]string](c, namespaceLabelsVariable)
	if err != nil {
		return field.ErrorList{field.Invalid(variables.Key(namespaceLabelsVariable), nil, err.Error())}
	}
	path := variables.Key(namespaceLabelsVariable)
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		for _, msg := range validation.IsQualifiedName(k) {
			errs = append(errs, field.Invalid(path.Key(k), k, "the label's key: "+msg))
		}
		for _, msg := range validation.IsValidLabelValue(labels[k]) {
			errs = append(errs, field.Invalid(path.Key(k), labels[k], "the label's value: "+msg))
		}
		if k == corev1.LabelMetadataName {
			errs = append(errs, field.Forbidden(path.Key(k), "Kubernetes sets "+k+" itself"))
		}
	}
	return errs
}

// crdOwner are the custom resource definitions an add-on's release installs.
type crdOwner struct {
	release, namespace string
	crds               []string
	// option says how to go on when the cluster holds these already.
	option string
}

// addonCRDs lists, by the suffix of the add-on's HelmChartProxy, a custom resource definition that
// is Kubehenge's alone to install. Each add-on's release owns its own, and Helm refuses to install
// over objects another release or tool owns.
var addonCRDs = map[string]crdOwner{
	certManagerAddon: {
		release: certManagerAddon, namespace: certManagerAddon, crds: []string{"certificates.cert-manager.io", "clusterissuers.cert-manager.io"},
		option: "Remove it",
	},
	kyvernoAddon: {
		release: kyvernoAddon, namespace: kyvernoAddon, crds: []string{"clusterpolicies.kyverno.io", "policyexceptions.kyverno.io"},
		option: "Remove it, or switch Kyverno off in the cluster's pod security",
	},
	monitoringAddon: {
		release: monitoringRelease, namespace: monitoringAddon, crds: []string{"prometheuses.monitoring.coreos.com", "servicemonitors.monitoring.coreos.com"},
		option: "Remove it, or switch monitoring off",
	},
}

// AddonCRDs are the custom resource definitions each add-on installs that a cluster's own install of
// the same software would collide with, by the add-on's name.
func AddonCRDs() map[string][]string {
	out := map[string][]string{}
	for name, o := range addonCRDs {
		out[name] = slices.Clone(o.crds)
	}
	return out
}

// conflictOf reports why the add-on of proxy p is not installed: the cluster holds custom resource
// definitions of the same software that its release does not own. An add-on that is installed
// already owns its own.
func (r *Reconciler) conflictOf(ctx context.Context, cluster *clusterv1.Cluster, p *unstructured.Unstructured, addons *Addons) (string, error) {
	suffix := strings.TrimPrefix(p.GetName(), cluster.Name+"-")
	owner, ok := addonCRDs[suffix]
	if !ok {
		return "", nil
	}
	installed := &unstructured.Unstructured{}
	installed.SetGroupVersionKind(helmChartProxyGVK)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(p), installed)
	if err == nil {
		return "", nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("reading add-on %s: %w", p.GetName(), err)
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return "", err
	}
	for _, name := range owner.crds {
		crd := &metav1.PartialObjectMetadata{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		switch err := workload.client.Get(ctx, client.ObjectKey{Name: name}, crd); {
		case apierrors.IsNotFound(err):
			continue
		case unreachable(err):
			// A cluster whose API does not answer holds nothing to collide with yet, or Helm says so.
			r.Remotes.forget(cluster)
			return "", nil
		case err != nil:
			return "", fmt.Errorf("reading the cluster's custom resource definition %s: %w", name, err)
		}
		if crd.Annotations[helmReleaseNameAnnotation] == owner.release && crd.Annotations[helmReleaseNamespaceAnnotation] == owner.namespace {
			continue
		}
		return fmt.Sprintf("%s is not installed: the cluster already holds its custom resource definition %s, which no Kubehenge release owns. %s", suffix, name, owner.option), nil
	}
	if suffix != certManagerAddon {
		return "", nil
	}
	deployment, err := foreignCertManager(ctx, workload.client)
	switch {
	case unreachable(err):
		r.Remotes.forget(cluster)
		return "", nil
	case err != nil || deployment == "":
		return "", err
	}
	return fmt.Sprintf("%s is not installed: the cluster already runs its Deployment %s, which no Kubehenge release owns. %s", suffix, deployment, owner.option), nil
}

// foreignCertManager names a cert-manager controller's Deployment in the workload cluster that
// Kubehenge's release does not own, as namespace/name, or nothing. A cert-manager installed without
// its custom resource definitions holds them by none, and would still fight Kubehenge's.
func foreignCertManager(ctx context.Context, workload client.Client) (string, error) {
	deployments := &metav1.PartialObjectMetadataList{}
	deployments.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DeploymentList"})
	err := workload.List(ctx, deployments, client.MatchingLabels{"app.kubernetes.io/name": certManagerAddon, "app.kubernetes.io/component": "controller"})
	if err != nil {
		return "", fmt.Errorf("listing the cluster's cert-manager Deployments: %w", err)
	}
	owner := addonCRDs[certManagerAddon]
	for _, d := range deployments.Items {
		if d.Annotations[helmReleaseNameAnnotation] != owner.release || d.Annotations[helmReleaseNamespaceAnnotation] != owner.namespace {
			return d.Namespace + "/" + d.Name, nil
		}
	}
	return "", nil
}

// unreachable reports whether err is a failure to reach a cluster's API, rather than its answer.
func unreachable(err error) bool {
	var status apierrors.APIStatus
	return err != nil && !errors.As(err, &status)
}

// recordAddonConflicts sets the cluster's AddonConflict condition to the conflicts found, or takes
// it off where there are none, and says so in an event when a conflict is new.
func (r *Reconciler) recordAddonConflicts(ctx context.Context, cluster *clusterv1.Cluster, conflicts []string) error {
	found := meta.FindStatusCondition(cluster.Status.Conditions, AddonConflictCondition)
	if len(conflicts) == 0 {
		if found == nil {
			return nil
		}
		_, err := markCluster(ctx, r.Client, r.Reader, client.ObjectKeyFromObject(cluster), AddonConflictCondition, nil)
		return err
	}
	cond := metav1.Condition{Type: AddonConflictCondition, Status: metav1.ConditionTrue, Reason: AddonConflictReason, Message: strings.Join(conflicts, ". ")}
	if found != nil && found.Message == shorten(cond.Message, watchMessageMax) {
		return nil
	}
	ctrl.LoggerFrom(ctx).Info("add-on not installed over an existing install", "conflicts", conflicts)
	if r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, AddonConflictCondition, "InstallAddon", "%s", clip(cond.Message, eventNoteMax))
	}
	_, err := markCluster(ctx, r.Client, r.Reader, client.ObjectKeyFromObject(cluster), AddonConflictCondition, &cond)
	return err
}

// proxyNamespaces are the namespaces the proxies install into, kube-system and default aside.
func proxyNamespaces(proxies []*unstructured.Unstructured) []string {
	var out []string
	for _, p := range proxies {
		ns := specText(p.Object["spec"], "namespace")
		if ns != "" && ns != metav1.NamespaceSystem && ns != metav1.NamespaceDefault && !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

// applyNamespaceLabels puts the cluster's namespaceLabels on the namespaces the add-ons install
// into, creating a namespace that is not there yet with them so that a policy requiring labels
// admits it, and takes off labels that were dropped. It reports whether it waits for the cluster's
// API.
func (r *Reconciler) applyNamespaceLabels(ctx context.Context, cluster *clusterv1.Cluster, namespaces []string, labels map[string]string) (bool, error) {
	_, marked := cluster.Annotations[namespaceLabelsAnnotation]
	if len(labels) == 0 && !marked {
		return false, nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return len(labels) > 0, err
	}
	if len(labels) > 0 && !marked {
		if err := r.setClusterAnnotation(ctx, cluster, namespaceLabelsAnnotation, new("true")); err != nil {
			return false, err
		}
	}
	for _, ns := range namespaces {
		if err := labelNamespace(ctx, workload.client, ns, labels); err != nil {
			if unreachable(err) {
				r.Remotes.forget(cluster)
				return true, nil
			}
			return false, err
		}
	}
	if len(labels) == 0 {
		return false, r.setClusterAnnotation(ctx, cluster, namespaceLabelsAnnotation, nil)
	}
	return false, nil
}

// labelNamespace makes the namespace's labels those the controller manages: want, and what else it
// carries.
func labelNamespace(ctx context.Context, workload client.Client, name string, want map[string]string) error {
	ns := &corev1.Namespace{}
	err := workload.Get(ctx, client.ObjectKey{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		if len(want) == 0 {
			return nil
		}
		ns = &corev1.Namespace{Name: name}
		setManagedLabels(ns, want)
		if createErr := workload.Create(ctx, ns); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return fmt.Errorf("creating namespace %s: %w", name, createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading namespace %s: %w", name, err)
	}
	before := ns.DeepCopy()
	setManagedLabels(ns, want)
	if maps.Equal(before.Labels, ns.Labels) && maps.Equal(before.Annotations, ns.Annotations) {
		return nil
	}
	if err := workload.Patch(ctx, ns, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("labelling namespace %s: %w", name, err)
	}
	return nil
}

// setManagedLabels drops the labels the controller put on ns earlier, and puts want there.
func setManagedLabels(ns *corev1.Namespace, want map[string]string) {
	for k := range strings.SplitSeq(ns.Annotations[managedLabelsAnnotation], ",") {
		delete(ns.Labels, k)
	}
	delete(ns.Annotations, managedLabelsAnnotation)
	if len(want) == 0 {
		return
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	maps.Copy(ns.Labels, want)
	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	ns.Annotations[managedLabelsAnnotation] = strings.Join(slices.Sorted(maps.Keys(want)), ",")
}

// setClusterAnnotation sets an annotation of the cluster, or takes it off where value is nil.
func (r *Reconciler) setClusterAnnotation(ctx context.Context, cluster *clusterv1.Cluster, key string, value *string) error {
	patch := client.MergeFrom(cluster.DeepCopy())
	if value == nil {
		delete(cluster.Annotations, key)
	} else {
		if cluster.Annotations == nil {
			cluster.Annotations = map[string]string{}
		}
		cluster.Annotations[key] = *value
	}
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("setting the cluster's annotation %s: %w", key, err)
	}
	return nil
}
