package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// TalosVersionVariable is the Talos release a Talos cluster's nodes run.
	TalosVersionVariable = "talosVersion"
	// TalosImageAnnotation is the Talos release a Talos cluster's new machines boot first, whose
	// template they are cloned from. The controller then upgrades them in place to the cluster's
	// talosVersion. It moves to talosVersion only when Cluster API replaces every machine anyway, at
	// a Kubernetes upgrade, since a machine template that selects another template rolls them all.
	TalosImageAnnotation = "kubehenge.dev/talos-image"
	// TalosUpgradingAnnotation names the node the controller is upgrading in place, and to which
	// release: "<node> to <release> since <time>". It is gone once no node is being upgraded.
	TalosUpgradingAnnotation = "kubehenge.dev/talos-upgrading"
	// TalosUpgradedAnnotation records the last node upgrade that finished: "<node> to <release> at
	// <time>".
	TalosUpgradedAnnotation = "kubehenge.dev/talos-upgraded"
)

// isTalos tells whether a cluster is built from the Talos ClusterClass.
func isTalos(c *clusterv1.Cluster) bool {
	return distributionOf(c) == templates.Talos
}

// TalosVersion is a Talos cluster's talosVersion variable.
func TalosVersion(c *clusterv1.Cluster) (string, error) {
	v, set, err := optionalVariable[string](c, TalosVersionVariable)
	if err != nil {
		return "", err
	}
	if !set || v == "" {
		return "", fmt.Errorf("cluster %s/%s: no %s", c.Namespace, c.Name, TalosVersionVariable)
	}
	return v, nil
}

// TalosImage is the Talos release a Talos cluster's new machines boot: TalosImageAnnotation, or
// before the cluster has it, talosVersion.
func TalosImage(c *clusterv1.Cluster) (string, error) {
	if v, ok := c.Annotations[TalosImageAnnotation]; ok {
		return v, nil
	}
	return TalosVersion(c)
}

// TalosLine is the template a Talos machine booting a release is cloned from, by its tags.
func TalosLine(version string) ([]string, error) {
	t, err := templates.Plan(&templates.Request{Distribution: templates.Talos, TalosVersion: version})
	if err != nil {
		return nil, fmt.Errorf("planning the Talos %s template: %w", version, err)
	}
	return t.Tags, nil
}

// ClassTalosTemplates are the Talos releases TalosTemplatesAnnotation records templates for.
func ClassTalosTemplates(class *clusterv1.ClusterClass) []string {
	recorded := class.Annotations[TalosTemplatesAnnotation]
	if recorded == "" {
		return nil
	}
	return strings.Split(recorded, ",")
}

// checkTalos refuses a Talos cluster whose Talos release does not run its Kubernetes version, and
// a change of release Talos does not upgrade along. While nodes may still run the release before a
// change, the Kubernetes version has to run on both.
func checkTalos(old, c *clusterv1.Cluster) error {
	if !isTalos(c) {
		return nil
	}
	talos, err := TalosVersion(c)
	if err != nil {
		return err
	}
	// The templates package's errors name the releases, and the caller's field path the variable.
	if err = templates.TalosRunsKubernetes(talos, c.Spec.Topology.Version); err != nil {
		return err //nolint:wrapcheck // as above
	}
	if old == nil || !isTalos(old) {
		return nil
	}
	was, err := TalosVersion(old)
	if err != nil || was == talos {
		return err
	}
	if err = templates.TalosUpgrade(was, talos); err != nil {
		return err //nolint:wrapcheck // as above
	}
	if err = templates.TalosRunsKubernetes(was, c.Spec.Topology.Version); err != nil {
		return fmt.Errorf("the nodes still on %s while they upgrade: %w", was, err)
	}
	return nil
}

// talosServesControlPlaneMetrics tells whether every controller manager and scheduler of a Talos
// cluster is passed a serving certificate, which Talos then issues from the cluster CA as it does
// the API server's, so that the metrics proxies on every control plane can verify them. Talos
// releases before that pass none, and the components sign their own. A cluster that cannot be read
// yet counts as one without. The next reconcile looks again.
func (r *Reconciler) talosServesControlPlaneMetrics(ctx context.Context, c *clusterv1.Cluster) bool {
	workload, found, err := r.Remotes.get(ctx, c)
	if err != nil || !found {
		return false
	}
	for _, component := range []string{"kube-controller-manager", "kube-scheduler"} {
		var pods corev1.PodList
		if err := workload.client.List(ctx, &pods, client.InNamespace(metav1.NamespaceSystem), client.MatchingLabels{"component": component}); err != nil {
			r.Remotes.forget(c)
			return false
		}
		if len(pods.Items) == 0 {
			return false
		}
		for i := range pods.Items {
			if servesOwnCertificate(&pods.Items[i]) {
				return false
			}
		}
	}
	return true
}

// servesOwnCertificate tells whether a control-plane pod runs without a serving certificate passed
// to it.
func servesOwnCertificate(p *corev1.Pod) bool {
	passed := func(arg string) bool { return strings.HasPrefix(arg, "--tls-cert-file=") }
	for i := range p.Spec.Containers {
		if slices.ContainsFunc(p.Spec.Containers[i].Command, passed) || slices.ContainsFunc(p.Spec.Containers[i].Args, passed) {
			return false
		}
	}
	return true
}

// talosImageReady records the release a new Talos cluster's machines boot first, and says what the
// cluster waits for, and how many seconds until it is asked again: Cluster API's own copy of the
// cluster to carry the record, and a template of the release in the template pool.
func (h *Hooks) talosImageReady(ctx context.Context, c *clusterv1.Cluster, class *clusterv1.ClusterClass) (string, int32, error) {
	image, err := TalosImage(c)
	if err != nil {
		return "", 0, err
	}
	if _, recorded := c.Annotations[TalosImageAnnotation]; !recorded && h.Client != nil {
		if err := h.setTalosImage(ctx, c, image); err != nil {
			return "", 0, err
		}
		// Cluster API patches the Cluster as it creates the topology, from the copy this hook was
		// given, and that patch can clear every annotation the copy lacks: the record goes too
		// unless Cluster API has read it back before it creates anything.
		return "recording the Talos release the cluster's machines boot first", 1, nil
	}
	if !slices.Contains(ClassTalosTemplates(class), image) {
		const templateWait = 10
		return fmt.Sprintf("waiting for a Talos %s template in the template pool; build it on the Administration page or with kubehenge template create talos --talos-version %s", image, image), templateWait, nil
	}
	return "", 0, nil
}

// newestTalosImage has the machines of an upgrade boot the cluster's talosVersion, once its
// template is there, and tells whether it moved them.
func (h *Hooks) newestTalosImage(ctx context.Context, c *clusterv1.Cluster) (bool, error) {
	image, err := TalosImage(c)
	if err != nil {
		return false, err
	}
	talos, err := TalosVersion(c)
	if err != nil || image == talos {
		return false, err
	}
	class := &clusterv1.ClusterClass{}
	ref := c.Spec.Topology.ClassRef
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: cmpOr(ref.Namespace, c.Namespace), Name: ref.Name}, class); err != nil {
		return false, fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	if !slices.Contains(ClassTalosTemplates(class), talos) {
		return false, nil
	}
	return true, h.setTalosImage(ctx, c, talos)
}

func (h *Hooks) setTalosImage(ctx context.Context, c *clusterv1.Cluster, image string) error {
	if h.Client == nil {
		return nil
	}
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, TalosImageAnnotation, image)
	target := &clusterv1.Cluster{}
	target.Namespace, target.Name = c.Namespace, c.Name
	if err := h.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("recording the Talos release the cluster's machines boot: %w", err)
	}
	return nil
}
