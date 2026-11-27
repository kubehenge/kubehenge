package provisioner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/version"
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
	// AddonConflictReason is the reason of AddonConflictCondition while the cluster holds an add-on's
	// custom resource definitions.
	AddonConflictReason = "CRDsExist"
	// AddonConflictDeploymentReason is its reason while the only conflict is a cert-manager
	// controller's Deployment, installed without its definitions.
	AddonConflictDeploymentReason = "DeploymentExists"
	// AddonConflictSettingsReason is its reason while the cluster's own cert-manager is used and
	// certificates' settings that only Kubehenge's carries are not applied.
	AddonConflictSettingsReason = "SettingsNotApplied"

	// addonLabel on the objects of an add-on's release names the add-on. cert-manager's release
	// carries it, since a cert-manager installed the usual Helm way has its release name and
	// namespace too.
	addonLabel = "kubehenge.dev/addon"

	// certManagerMinimum is the oldest cert-manager of the cluster's own that its users get: Rancher's
	// chart requires it, and the other add-ons' Certificates and Issuers use nothing newer.
	certManagerMinimum = "v1.15.0"
	// certManagerController and certManagerWebhook are the component labels of cert-manager's
	// Deployments.
	certManagerController = "controller"
	certManagerWebhook    = "webhook"

	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
)

// retiredVariables are variables a cluster may still set that the ClusterClasses no longer define.
// Cluster API refuses every change to a Cluster that sets one, and stops reconciling its topology.
var retiredVariables = []string{"useExisting"}

// retired tells whether v is one of retiredVariables.
func retired(v clusterv1.ClusterVariable) bool { return slices.Contains(retiredVariables, v.Name) }

// dropRetiredVariables takes the retired variables off the cluster.
func (r *Reconciler) dropRetiredVariables(ctx context.Context, cluster *clusterv1.Cluster) error {
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
	// labelled is set where the release puts addonLabel on its objects, which then tells them apart.
	labelled bool
}

// owns tells whether o is of the add-on's release.
func (o *crdOwner) owns(obj metav1.Object, addon string) bool {
	if o.labelled {
		return obj.GetLabels()[addonLabel] == addon
	}
	return obj.GetAnnotations()[helmReleaseNameAnnotation] == o.release && obj.GetAnnotations()[helmReleaseNamespaceAnnotation] == o.namespace
}

// addonConflict is why an add-on is not installed: the reason of AddonConflictCondition and its
// line. clusterOwn is set instead where the cluster's own install works and is used, and unread
// where the cluster could not be read in full for one.
type addonConflict struct {
	reason, message string
	clusterOwn      bool
	unread          bool
}

// addonCRDs lists, by the suffix of the add-on's HelmChartProxy, a custom resource definition that
// is Kubehenge's alone to install. Each add-on's release owns its own, and Helm refuses to install
// over objects another release or tool owns.
var addonCRDs = map[string]crdOwner{
	certManagerAddon: {
		release: certManagerAddon, namespace: certManagerAddon, crds: []string{"certificates.cert-manager.io", "clusterissuers.cert-manager.io"},
		option: "Remove it, or make it work", labelled: true,
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
// already owns its own. No conflict has an empty message. A cert-manager of the cluster's own is a
// conflict only where it does not work (certManagerConflictOf).
func (r *Reconciler) conflictOf(ctx context.Context, cluster *clusterv1.Cluster, p *unstructured.Unstructured) (addonConflict, error) {
	suffix := strings.TrimPrefix(p.GetName(), cluster.Name+"-")
	owner, ok := addonCRDs[suffix]
	if !ok {
		return addonConflict{}, nil
	}
	installed := &unstructured.Unstructured{}
	installed.SetGroupVersionKind(helmChartProxyGVK)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(p), installed)
	if err == nil {
		return addonConflict{}, nil
	}
	if !apierrors.IsNotFound(err) {
		return addonConflict{}, fmt.Errorf("reading add-on %s: %w", p.GetName(), err)
	}
	if suffix == certManagerAddon {
		return r.certManagerConflictOf(ctx, cluster), nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return addonConflict{}, err
	}
	for _, name := range owner.crds {
		crd := crdMetadata()
		switch err = workload.client.Get(ctx, client.ObjectKey{Name: name}, crd); {
		case apierrors.IsNotFound(err):
			continue
		case unreachable(err):
			// A cluster whose API does not answer holds nothing to collide with yet, or Helm says so.
			r.Remotes.forget(cluster)
			return addonConflict{}, nil
		case err != nil:
			return addonConflict{}, fmt.Errorf("reading the cluster's custom resource definition %s: %w", name, err)
		}
		if owner.owns(crd, suffix) {
			continue
		}
		return addonConflict{reason: AddonConflictReason, message: fmt.Sprintf("%s is not installed: the cluster already holds its custom resource definition %s, which no Kubehenge release owns. %s", suffix, name, owner.option)}, nil
	}
	return addonConflict{}, nil
}

// crdMetadata is a custom resource definition to read the metadata of.
func crdMetadata() *metav1.PartialObjectMetadata {
	crd := &metav1.PartialObjectMetadata{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	return crd
}

// certManagerConflictOf decides on Kubehenge's cert-manager where it is not installed yet. A read
// of the cluster that fails or cannot be made yet leaves it unread, to be read again on the next
// reconcile: Kubehenge's release would upgrade or replace any install of the same name, so it is
// made only once a complete read found none.
func (r *Reconciler) certManagerConflictOf(ctx context.Context, cluster *clusterv1.Cluster) addonConflict {
	conflict, err := r.readCertManager(ctx, cluster)
	if err != nil {
		if unreachable(err) {
			r.Remotes.forget(cluster)
		}
		ctrl.LoggerFrom(ctx).Info("cert-manager waits for a complete read of the cluster", "error", err.Error())
		return addonConflict{unread: true}
	}
	return conflict
}

// readCertManager reads the cluster for a cert-manager of its own: one that works is used, one
// that does not is a conflict that says why.
func (r *Reconciler) readCertManager(ctx context.Context, cluster *clusterv1.Cluster) (addonConflict, error) {
	owner := addonCRDs[certManagerAddon]
	workload, found, err := r.Remotes.get(ctx, cluster)
	switch {
	case err != nil:
		return addonConflict{}, err
	case !found:
		return addonConflict{unread: true}, nil
	}
	for _, name := range owner.crds {
		crd := crdMetadata()
		switch err = workload.client.Get(ctx, client.ObjectKey{Name: name}, crd); {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return addonConflict{}, fmt.Errorf("reading the cluster's custom resource definition %s: %w", name, err)
		}
		if owner.owns(crd, certManagerAddon) {
			continue
		}
		problem, problemErr := certManagerProblem(ctx, workload.client)
		switch {
		case problemErr != nil:
			return addonConflict{}, problemErr
		case problem == "":
			return addonConflict{clusterOwn: true}, nil
		}
		return addonConflict{reason: AddonConflictReason, message: fmt.Sprintf("%s is not installed: the cluster already holds its custom resource definition %s, which no Kubehenge release owns. %s. %s",
			certManagerAddon, name, problem, owner.option)}, nil
	}
	deployment, err := foreignCertManager(ctx, workload.client)
	if err != nil || deployment == "" {
		return addonConflict{}, err
	}
	return addonConflict{reason: AddonConflictDeploymentReason, message: fmt.Sprintf("%s is not installed: the cluster already runs its Deployment %s, which no Kubehenge release owns. It has no custom resource definitions. %s", certManagerAddon, deployment, owner.option)}, nil
}

// certManagerProblem says why the cluster's cert-manager cannot serve Kubehenge's add-ons, or
// nothing where it can: it serves cert-manager.io/v1, its controller runs in the cert-manager
// namespace, where Kubehenge's add-ons put their issuers' Secrets, and keeps the cluster issuers'
// Secrets there, its controller and webhook are Available, and its version reads as
// certManagerMinimum or newer.
func certManagerProblem(ctx context.Context, workload client.Client) (string, error) {
	for _, name := range addonCRDs[certManagerAddon].crds {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := workload.Get(ctx, client.ObjectKey{Name: name}, crd)
		if err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("reading the cluster's custom resource definition %s: %w", name, err)
		}
		if !slices.ContainsFunc(crd.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool { return v.Name == "v1" && v.Served }) {
			return "It does not serve " + name + " at v1", nil
		}
	}
	controllers, err := certManagerDeployments(ctx, workload, client.MatchingLabels{appName: certManagerAddon, appComponent: certManagerController})
	if err != nil {
		return "", err
	}
	if len(controllers) == 0 {
		return "It has no controller Deployment, as after an uninstall", nil
	}
	controller := &controllers[0]
	if controller.Namespace != certManagerAddon {
		return "It runs in " + controller.Namespace + ", not in " + certManagerAddon, nil
	}
	if !available(controller) {
		return "Its controller " + controller.Namespace + "/" + controller.Name + " is not Available", nil
	}
	webhooks, err := certManagerDeployments(ctx, workload, client.InNamespace(controller.Namespace), client.MatchingLabels{appComponent: certManagerWebhook})
	if err != nil {
		return "", err
	}
	switch {
	case len(webhooks) == 0:
		return "It has no webhook Deployment in " + controller.Namespace, nil
	case !available(&webhooks[0]):
		return "Its webhook " + webhooks[0].Namespace + "/" + webhooks[0].Name + " is not Available", nil
	}
	switch v := certManagerVersion(controller); {
	case v == nil:
		return "Its version cannot be read", nil
	case v.LessThan(version.MustParseSemantic(certManagerMinimum)):
		return fmt.Sprintf("It is v%s, older than %s", v, certManagerMinimum), nil
	}
	if ns := clusterResourceNamespace(controller); ns != certManagerAddon {
		return "It keeps the cluster issuers' Secrets in " + ns + ", not in " + certManagerAddon, nil
	}
	return "", nil
}

// certManagerDeployments are the cert-manager Deployments that opts select.
func certManagerDeployments(ctx context.Context, workload client.Client, opts ...client.ListOption) ([]appsv1.Deployment, error) {
	deployments := &appsv1.DeploymentList{}
	if err := workload.List(ctx, deployments, opts...); err != nil {
		return nil, fmt.Errorf("listing the cluster's cert-manager Deployments: %w", err)
	}
	return deployments.Items, nil
}

// available tells whether the Deployment has its condition Available.
func available(d *appsv1.Deployment) bool {
	return slices.ContainsFunc(d.Status.Conditions, func(c appsv1.DeploymentCondition) bool {
		return c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue
	})
}

// certManagerVersion is the controller's version, from its version label or its image's tag, or nil
// where neither gives one.
func certManagerVersion(d *appsv1.Deployment) *version.Version {
	tag := d.Labels["app.kubernetes.io/version"]
	if containers := d.Spec.Template.Spec.Containers; tag == "" && len(containers) > 0 {
		image, _, _ := strings.Cut(containers[0].Image, "@")
		if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
			tag = image[i+1:]
		}
	}
	v, err := version.ParseSemantic(tag)
	if err != nil {
		return nil
	}
	return v
}

// clusterResourceNamespace is where the controller reads its cluster issuers' Secrets: the value of
// its --cluster-resource-namespace, its own namespace by default.
func clusterResourceNamespace(d *appsv1.Deployment) string {
	const flag = "--cluster-resource-namespace="
	for i := range d.Spec.Template.Spec.Containers {
		for _, arg := range d.Spec.Template.Spec.Containers[i].Args {
			if ns, ok := strings.CutPrefix(arg, flag); ok && ns != "$(POD_NAMESPACE)" {
				return ns
			}
		}
	}
	return d.Namespace
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
	for i := range deployments.Items {
		d := &deployments.Items[i]
		if !owner.owns(d, certManagerAddon) {
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

// recordAddonConflicts sets the cluster's AddonConflict condition to the conflicts found, with the
// first one's reason, or takes it off where there are none, and says so in an event when a conflict
// is new.
func (r *Reconciler) recordAddonConflicts(ctx context.Context, cluster *clusterv1.Cluster, conflicts []addonConflict) error {
	found := meta.FindStatusCondition(cluster.Status.Conditions, AddonConflictCondition)
	if len(conflicts) == 0 {
		if found == nil {
			return nil
		}
		_, err := markCluster(ctx, r.Client, r.Reader, client.ObjectKeyFromObject(cluster), AddonConflictCondition, nil)
		return err
	}
	lines := make([]string, len(conflicts))
	for i, c := range conflicts {
		lines[i] = c.message
	}
	cond := metav1.Condition{Type: AddonConflictCondition, Status: metav1.ConditionTrue, Reason: conflicts[0].reason, Message: strings.Join(lines, ". ")}
	if found != nil && found.Reason == cond.Reason && found.Message == shorten(cond.Message, watchMessageMax) {
		return nil
	}
	ctrl.LoggerFrom(ctx).Info("add-on not installed over an existing install", "conflicts", lines)
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
