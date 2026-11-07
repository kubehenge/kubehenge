package provisioner

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
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

	// matchLabelsKey is a label selector's key in the objects and values written as maps.
	matchLabelsKey = "matchLabels"

	kyvernoAdmissionReplicas = 2
	// kyvernoWorkerPreference is the highest weight a preferred node affinity takes.
	kyvernoWorkerPreference = 100
)

// PodSecurityExempt are the namespaces whose pods neither Pod Security Admission nor Kyverno holds
// to a cluster's level, by what puts components that need the host there. Kyverno exempts only
// those of what the cluster runs, so that nobody can create one of the others and run privileged
// pods in it. Pod Security Admission exempts them all: its exemptions are a file on the
// control-plane nodes, and following the add-ons would replace those nodes whenever one is switched.
type PodSecurityExempt struct {
	// Always holds what every cluster runs.
	Always []string `json:"always"`
	// Calico holds Calico's, for a cluster whose CNI it is.
	Calico     []string `json:"calico"`
	Monitoring []string `json:"monitoring"`
	Logs       []string `json:"logs"`
	Backups    []string `json:"backups"`
	Istio      []string `json:"istio"`
	// MetalLB holds MetalLB's, for a load balancer that is MetalLB.
	MetalLB []string `json:"metallb"`
	// Rancher holds those of Rancher and its agents in Rancher's own cluster, RancherAgent those of
	// the agents in a cluster that registers with it.
	Rancher      []string `json:"rancher"`
	RancherAgent []string `json:"rancherAgent"`
}

// For is the namespaces a cluster running CNI cni, whether Kubehenge or its distribution installs
// it, and addons exempts.
func (e *PodSecurityExempt) For(cni string, addons *Addons) []string {
	out := slices.Clone(e.Always)
	for _, by := range []struct {
		on         bool
		namespaces []string
	}{
		{cni == cniCalico, e.Calico},
		{addons.Monitoring.Enabled, e.Monitoring},
		{addons.logsOn(), e.Logs},
		{addons.Backups.Enabled, e.Backups},
		{addons.Istio.Enabled, e.Istio},
		{addons.LoadBalancer.Enabled && addons.LoadBalancer.implementation() == metalLB, e.MetalLB},
		{addons.Rancher.Install, e.Rancher},
		{!addons.Rancher.Install && addons.Rancher.registers(), e.RancherAgent},
	} {
		if !by.on {
			continue
		}
		for _, ns := range by.namespaces {
			if !slices.Contains(out, ns) {
				out = append(out, ns)
			}
		}
	}
	return out
}

// all is every namespace any cluster may exempt.
func (e *PodSecurityExempt) all() []string {
	var out []string
	for _, namespaces := range [][]string{e.Always, e.Calico, e.Monitoring, e.Logs, e.Backups, e.Istio, e.MetalLB, e.Rancher, e.RancherAgent} {
		for _, ns := range namespaces {
			if !slices.Contains(out, ns) {
				out = append(out, ns)
			}
		}
	}
	return out
}

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
//
// Each controller holds the leases the other add-ons do. Kyverno takes only the retry period and
// derives the rest from it, a lease of six periods renewed within five.
//
// The admission controller runs two replicas, which the chart then keeps one of through a
// disruption budget. Its leader shuts down when it cannot renew its lease, and with the policies
// failing closed a lone replica would refuse every pod until it is back. The other goes on
// answering. A third would not fit the least worker beside every other add-on.
//
// The two must stand on different nodes, or a drain of the node holding both waits on the budget
// for ever: the first one evicted has nowhere to go, and the second may not leave. With one worker
// the second therefore runs on a control plane, as the other cluster-critical add-ons may. Where
// workers are enough, they are preferred.
func kyvernoValues() map[string]any {
	lease := func() map[string]any { return map[string]any{"leaderElectionRetryPeriod": leaseRetry} }
	admission := map[string]any{appComponent: "admission-controller"}
	return map[string]any{
		"features": map[string]any{
			"policyExceptions": map[string]any{"enabled": true, "namespace": kyvernoAddon},
		},
		"admissionController": map[string]any{
			"replicas":  kyvernoAdmissionReplicas,
			"container": map[string]any{extraArgsKey: lease()},
			"tolerations": []any{map[string]any{
				"key": controlPlaneTaint.Key, "operator": string(corev1.TolerationOpExists), "effect": string(controlPlaneTaint.Effect),
			}},
			"topologySpreadConstraints": []any{map[string]any{
				"maxSkew": 1, "topologyKey": corev1.LabelHostname,
				"whenUnsatisfiable": string(corev1.DoNotSchedule), "labelSelector": map[string]any{matchLabelsKey: admission},
			}},
			"nodeAffinity": map[string]any{"preferredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
				"weight": kyvernoWorkerPreference,
				"preference": map[string]any{"matchExpressions": []any{map[string]any{
					"key": controlPlaneRole, "operator": string(corev1.NodeSelectorOpDoesNotExist),
				}}},
			}}},
		},
		"backgroundController": map[string]any{extraArgsKey: lease()},
		"cleanupController":    map[string]any{extraArgsKey: lease()},
		"reportsController":    map[string]any{extraArgsKey: lease()},
	}
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
