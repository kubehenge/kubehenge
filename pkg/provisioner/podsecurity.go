package provisioner

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodSecurity is a cluster's podSecurity variable: the Pod Security Standard level Pod Security
// Admission enforces, which the ClusterClass's API server configuration reads, and the one Kyverno
// enforces, which this controller installs it for.
type PodSecurity struct {
	Admission string `json:"admission,omitempty"`
	Kyverno   string `json:"kyverno,omitempty"`
}

const (
	podSecurityVariable = "podSecurity"
	kyvernoAddon        = "kyverno"
	kyvernoPolicies     = "kyverno-policies"
	// kyvernoOff is the Kyverno level that leaves Kyverno out of the cluster.
	kyvernoOff = "off"
)

// kyvernoOn reports whether the cluster has Kyverno enforce a level.
func (p PodSecurity) kyvernoOn() bool {
	return p.Kyverno != "" && p.Kyverno != kyvernoOff
}

func kyvernoProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-" + kyvernoAddon
}

func kyvernoPoliciesProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-" + kyvernoPolicies
}

// kyvernoValues let a workload past the policies only through a PolicyException in Kyverno's own
// namespace, which those who may write there grant, rather than anyone who may create one in a
// namespace of their own.
func kyvernoValues() map[string]any {
	return map[string]any{"features": map[string]any{
		"policyExceptions": map[string]any{"enabled": true, "namespace": kyvernoAddon},
	}}
}

// kyvernoPolicyValues have Kyverno's Pod Security Standards policies deny a pod short of level,
// outside the namespaces Pod Security Admission exempts too. A policy Kyverno cannot reach denies
// as well, rather than let the pod through.
func kyvernoPolicyValues(level string, exempt []string) map[string]any {
	return map[string]any{
		"podSecurityStandard":     level,
		"validationFailureAction": "Enforce",
		"failurePolicy":           "Fail",
		"vpolExclude":             map[string]any{"excludeNamespaces": exempt},
	}
}

// policiesWait reports whether Kyverno's policies wait for Kyverno, as they do until Kyverno's
// release is deployed for its current spec: before, their kinds are unknown to the cluster, and the
// Add-on Provider for Helm puts each install after a failed one further off, past what a cluster
// whose API server is slow to take Kyverno's custom resource definitions waits. Policies already
// installed stay.
func (r *Reconciler) policiesWait(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: kyvernoPoliciesProxy(cluster)}, p)
	switch {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("reading add-on %s: %w", kyvernoPoliciesProxy(cluster), err)
	}
	ready, err := r.proxyReady(ctx, cluster, kyvernoProxy(cluster))
	return !ready, err
}

// removeKyverno uninstalls Kyverno's policies, and Kyverno once they are gone: their release
// cannot be uninstalled after Kyverno's custom resource definitions have gone with Kyverno's.
// It reports whether Kyverno waits on its policies.
func (r *Reconciler) removeKyverno(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	if err := r.removeProxies(ctx, cluster, kyvernoPoliciesProxy(cluster)); err != nil {
		return false, err
	}
	policies, err := r.anyProxy(ctx, cluster, kyvernoPoliciesProxy(cluster))
	if err != nil || policies {
		return policies, err
	}
	return false, r.removeProxies(ctx, cluster, kyvernoProxy(cluster))
}
