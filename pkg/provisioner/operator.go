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
// spec and each provider's Deployments have rolled out, writing what it still waits for to
// progress whenever that changes. Neither Helm's own wait nor the providers' Ready condition can
// tell: Helm sees only the provider objects, and Ready mirrors the Deployments' availability,
// which their old pods keep up until the operator replaces them.
func WaitForProviders(ctx context.Context, c client.Reader, interval time.Duration, progress io.Writer) error {
	var last []string
	for {
		pending, err := ProvidersPending(ctx, c)
		if err == nil && len(pending) == 0 {
			return nil
		}
		if err != nil {
			pending = []string{err.Error()}
		}
		if !slices.Equal(pending, last) {
			// Progress is only for the log; failing to write it does not change the wait.
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

// ProvidersPending lists what keeps the Cluster API operator's providers from running as their
// specs say: a provider whose current spec the operator has not applied, a provider namespace
// without a provider Deployment, and a provider Deployment that has not rolled out.
func ProvidersPending(ctx context.Context, c client.Reader) ([]string, error) {
	var pending []string
	namespaces := map[string]bool{}
	for _, kind := range providerKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(operatorAPI.WithKind(kind + "List"))
		if err := c.List(ctx, list); err != nil {
			return nil, fmt.Errorf("listing %ss: %w", kind, err)
		}
		for i := range list.Items {
			p := &list.Items[i]
			namespaces[p.GetNamespace()] = true
			if why := notApplied(p); why != "" {
				pending = append(pending, fmt.Sprintf("%s %s/%s: %s", kind, p.GetNamespace(), p.GetName(), why))
			}
		}
	}
	for _, ns := range slices.Sorted(maps.Keys(namespaces)) {
		deployments := &appsv1.DeploymentList{}
		if err := c.List(ctx, deployments, client.InNamespace(ns), client.HasLabels{providerLabel}); err != nil {
			return nil, fmt.Errorf("listing the provider Deployments in %s: %w", ns, err)
		}
		if len(deployments.Items) == 0 {
			pending = append(pending, fmt.Sprintf("namespace %s: no provider Deployment", ns))
		}
		for i := range deployments.Items {
			if dep := &deployments.Items[i]; !rolledOut(dep) {
				pending = append(pending, fmt.Sprintf("Deployment %s/%s: rolling out", ns, dep.Name))
			}
		}
	}
	return pending, nil
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
