package provisioner

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// operatorAPI is the Cluster API operator's, whose objects each stand for a provider it installs.
var operatorAPI = schema.GroupVersion{Group: "operator.cluster.x-k8s.io", Version: "v1alpha2"}

var providerKinds = []string{"CoreProvider", "BootstrapProvider", "ControlPlaneProvider", "InfrastructureProvider", "IPAMProvider", "AddonProvider"}

// appliedSpecHashAnnotation is where the operator records the hash of what it applied, on a
// provider and on the Secret it caches the provider's manifests in.
const appliedSpecHashAnnotation = "operator.cluster.x-k8s.io/applied-spec-hash"

// ProvidersSettle is how long the providers must stay unchanged before they count as installed.
// Right after an install, the operator can install a provider again from a stale read, which
// replaces its Deployments for about 15 seconds.
const ProvidersSettle = 30 * time.Second

// WaitForOperator returns once the Cluster API operator's webhooks admit a provider, which they
// cannot while the operator's pod or its Service's endpoints are still being replaced: after a node
// reboots, its old pod lingers as Completed for minutes, and the Service still points at it. Each
// attempt creates an AddonProvider in namespace as a dry run, so nothing is kept.
func WaitForOperator(ctx context.Context, c client.Client, namespace string, interval time.Duration) error {
	for {
		probe := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
		probe.SetGroupVersionKind(operatorAPI.WithKind("AddonProvider"))
		probe.SetNamespace(namespace)
		probe.SetName("webhook-probe")
		err := c.Create(ctx, probe, client.DryRunAll)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the Cluster API operator's webhooks did not admit a provider: %w", err)
		case <-time.After(interval):
		}
	}
}

// WaitForProviders returns once the Cluster API operator has applied every provider's current
// spec, each provider's Deployments have rolled out, and none of that has changed for settle. It
// writes what it still waits for to progress whenever that changes. Neither Helm's own wait nor
// the providers' Ready condition can tell: Helm sees only the provider objects, and Ready mirrors
// the Deployments' availability, which their old pods keep up until the operator replaces them.
func WaitForProviders(ctx context.Context, c client.Reader, interval, settle time.Duration, progress io.Writer) error {
	var (
		last    []string
		settled map[string]string
		since   time.Time
	)
	for {
		pending, applied, err := providersPending(ctx, c)
		switch {
		case err != nil:
			pending, since = []string{err.Error()}, time.Time{}
		case len(pending) > 0:
			since = time.Time{}
		case since.IsZero() || !maps.Equal(applied, settled):
			settled, since = applied, time.Now()
			pending = []string{fmt.Sprintf("the providers to stay unchanged for %s", settle)}
		case time.Since(since) >= settle:
			return nil
		default:
			pending = last
		}
		if !slices.Equal(pending, last) {
			// Progress is only for the log. Failing to write it does not change the wait.
			_, _ = fmt.Fprintf(progress, "waiting for:\n  %s\n", strings.Join(pending, "\n  "))
			last = pending
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the Cluster API providers did not all run: %s", strings.Join(last, "; "))
		case <-time.After(interval):
		}
	}
}

// providersPending lists what keeps the Cluster API operator's providers from running as their
// specs say: a provider whose current spec the operator has not applied or is still applying, a
// provider namespace without a provider Deployment, and a provider Deployment that has not rolled
// out. With it comes what the operator has applied: each provider's spec hash, and each provider
// Deployment's UID and generation, which an install or upgrade of the provider replaces.
func providersPending(ctx context.Context, c client.Reader) ([]string, map[string]string, error) {
	var pending []string
	applied := map[string]string{}
	namespaces := map[string]bool{}
	for _, kind := range providerKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(operatorAPI.WithKind(kind + "List"))
		if err := c.List(ctx, list); err != nil {
			return nil, nil, fmt.Errorf("listing %ss: %w", kind, err)
		}
		for i := range list.Items {
			p := &list.Items[i]
			namespaces[p.GetNamespace()] = true
			name := fmt.Sprintf("%s %s/%s", kind, p.GetNamespace(), p.GetName())
			why, err := providerPending(ctx, c, kind, p)
			if err != nil {
				return nil, nil, err
			}
			if why != "" {
				pending = append(pending, fmt.Sprintf("%s: %s", name, why))
			}
			applied[name] = p.GetAnnotations()[appliedSpecHashAnnotation]
		}
	}
	for _, ns := range slices.Sorted(maps.Keys(namespaces)) {
		waiting, err := deploymentsPending(ctx, c, ns, applied)
		if err != nil {
			return nil, nil, err
		}
		pending = append(pending, waiting...)
	}
	return pending, applied, nil
}

// deploymentsPending lists what keeps the provider Deployments in a namespace from running, and
// records each one's UID and generation in applied.
func deploymentsPending(ctx context.Context, c client.Reader, ns string, applied map[string]string) ([]string, error) {
	deployments := &appsv1.DeploymentList{}
	if err := c.List(ctx, deployments, client.InNamespace(ns), client.HasLabels{providerLabel}); err != nil {
		return nil, fmt.Errorf("listing the provider Deployments in %s: %w", ns, err)
	}
	if len(deployments.Items) == 0 {
		return []string{fmt.Sprintf("namespace %s: no provider Deployment", ns)}, nil
	}
	var pending []string
	for i := range deployments.Items {
		dep := &deployments.Items[i]
		if !rolledOut(dep) {
			pending = append(pending, fmt.Sprintf("Deployment %s/%s: rolling out", ns, dep.Name))
		}
		applied["Deployment "+ns+"/"+dep.Name] = fmt.Sprintf("%s/%d", dep.UID, dep.Generation)
	}
	return pending, nil
}

// providerPending says why the operator has not finished applying a provider, or returns "".
func providerPending(ctx context.Context, c client.Reader, kind string, p *unstructured.Unstructured) (string, error) {
	if why := notApplied(p); why != "" {
		return why, nil
	}
	return stillApplying(ctx, c, kind, p)
}

// stillApplying says why the operator may still be applying a provider whose conditions say it is
// done, or returns "" if it is not. The operator records the hash of what it applied on the Secret
// it caches the provider's manifests in, then on the provider, so the two differ until a pass ends.
func stillApplying(ctx context.Context, c client.Reader, kind string, p *unstructured.Unstructured) (string, error) {
	hash := p.GetAnnotations()[appliedSpecHashAnnotation]
	if hash == "" {
		return "no applied spec recorded yet", nil
	}
	version, _, _ := unstructured.NestedString(p.Object, "spec", "version")
	cache := &metav1.PartialObjectMetadata{}
	cache.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	key := client.ObjectKey{Namespace: p.GetNamespace(), Name: providerCacheName(kind, p.GetName(), version)}
	switch err := c.Get(ctx, key, cache); {
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("its manifests for %s not cached yet", version), nil
	case err != nil:
		return "", fmt.Errorf("reading the cache Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	if cache.GetAnnotations()[appliedSpecHashAnnotation] != hash {
		return "still being applied", nil
	}
	return "", nil
}

// providerCacheName is the Secret the operator caches a provider's manifests in, as its own
// ProviderCacheName names it.
func providerCacheName(kind, name, version string) string {
	return fmt.Sprintf("%s-%s-%s-cache", providerType(kind), name, version)
}

// providerType is the operator's own name for a kind of provider, as its GetType gives it.
func providerType(kind string) string {
	if kind == "ControlPlaneProvider" {
		return "control-plane"
	}
	return strings.ToLower(strings.TrimSuffix(kind, "Provider"))
}

// notApplied says why the operator has not applied a provider's current generation, or returns
// "" if it has. The operator records a change of version on ProviderUpgraded alone and leaves
// ProviderInstalled at the generation it last installed, so either condition, true at the current
// generation, means the operator has applied it.
func notApplied(p *unstructured.Unstructured) string {
	var status struct {
		Conditions []metav1.Condition `json:"conditions"`
	}
	if raw, ok := p.Object["status"].(map[string]any); ok {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &status); err != nil {
			return fmt.Sprintf("unreadable status: %v", err)
		}
	}
	generation := p.GetGeneration()
	for _, kind := range []string{"ProviderInstalled", "ProviderUpgraded"} {
		if c := meta.FindStatusCondition(status.Conditions, kind); c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation {
			return ""
		}
	}
	for _, c := range status.Conditions {
		if c.Status == metav1.ConditionFalse && c.ObservedGeneration == generation {
			return fmt.Sprintf("%s: %s", c.Type, c.Message)
		}
	}
	return fmt.Sprintf("generation %d not applied yet", generation)
}
