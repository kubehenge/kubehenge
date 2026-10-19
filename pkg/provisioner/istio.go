package provisioner

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// Istio is a cluster's istio variable.
type Istio struct {
	Enabled bool `json:"enabled"`
}

const (
	istioAddon     = "istio"
	istioNamespace = "istio-system"
	// istioNodeNamespace holds istio-cni and ztunnel apart from istiod: both reach into the nodes'
	// network namespaces, which only Pod Security's privileged level allows, and istiod needs none
	// of it.
	istioNodeNamespace = "istio-node"
	// istioDataplaneMode enrols a namespace or pod in the mesh; istioRedirection is istio-cni's
	// mark on a pod whose traffic it sends through ztunnel.
	istioDataplaneMode = "istio.io/dataplane-mode"
	istioRedirection   = "ambient.istio.io/redirection"
)

// The releases of Istio's four charts, each also its proxy's suffix: the chart of the istio flag
// is istiod, and the others are found beside it at the same version.
const (
	istioBase    = "istio-base"
	istiod       = "istiod"
	istioCNI     = "istio-cni"
	istioZtunnel = "ztunnel"
)

// istioProxies names the cluster's Istio proxies, which go together when Istio is switched off.
func istioProxies(cluster *clusterv1.Cluster) []string {
	return []string{cluster.Name + "-" + istioBase, cluster.Name + "-" + istiod, cluster.Name + "-" + istioCNI, cluster.Name + "-" + istioZtunnel}
}

// beside is the chart of the same repository and version under another name.
func (c Chart) beside(name string) Chart {
	c.Name = name
	return c
}

// istioCharts are Istio in ambient mode, without sidecars: ztunnel on every node carries the
// enrolled pods' traffic over mutual TLS, istio-cni redirects it there, and istiod gives ztunnel
// its workloads' certificates. Its CRDs come with the base chart, and none of the charts renders
// a resource of them, so the four install side by side. istiod runs at Pod Security's restricted
// level, as do waypoints it deploys; on k3s with its own flannel, istio-cni finds the CNI
// configuration where k3s keeps it, and everywhere else in the standard directories.
func istioCharts(istio Chart, distribution templates.Distribution, cni CNI) []chartProxy {
	restricted := map[string]any{"type": string(corev1.SeccompProfileTypeRuntimeDefault)}
	ambient := func(values map[string]any) map[string]any {
		values["profile"] = "ambient"
		return values
	}
	cniValues := ambient(map[string]any{})
	if distribution == templates.K3s && cni.Name == "" {
		cniValues["global"] = map[string]any{"platform": "k3s"}
	}
	return []chartProxy{
		{istioBase, istioBase, istioNamespace, istio.beside("base"), map[string]any{}},
		{istiod, istiod, istioNamespace, istio, ambient(map[string]any{
			"pilot": map[string]any{
				"trustedZtunnelNamespace": istioNodeNamespace,
				seccompProfileKey:         restricted,
				// The chart asks for 2 GiB, room for a mesh of thousands of pods.
				"resources": map[string]any{requestsKey: map[string]any{"cpu": "100m", memoryKey: "256Mi"}},
			},
			"gateways": map[string]any{seccompProfileKey: restricted},
		})},
		{istioCNI, istioCNI, istioNodeNamespace, istio.beside("cni"), cniValues},
		{istioZtunnel, istioZtunnel, istioNodeNamespace, istio.beside("ztunnel"), ambient(map[string]any{})},
	}
}

// removeIstio takes every workload out of the mesh, then uninstalls Istio: a pod still enrolled
// once ztunnel has gone keeps its traffic redirected to a proxy that no longer runs, and reaches
// nothing. Enrolment is a label, which outlives the charts, so it goes too; istio-cni takes the
// running pods' redirection away once it has. It reports whether the removal waits, as it does
// while a pod is still redirected or the cluster cannot be reached.
func (r *Reconciler) removeIstio(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	proxies := istioProxies(cluster)
	installed, err := r.anyProxy(ctx, cluster, proxies...)
	if err != nil || !installed {
		return false, err
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return true, err
	}
	redirected, err := unenrol(ctx, workload.client)
	if err != nil {
		r.Remotes.forget(cluster)
		return false, err
	}
	if redirected {
		return true, nil
	}
	return false, r.removeProxies(ctx, cluster, proxies...)
}

// unenrol removes the mesh's label from every namespace and pod that has it, and reports whether a
// pod is redirected still.
func unenrol(ctx context.Context, workload client.Client) (bool, error) {
	unlabel := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"`+istioDataplaneMode+`":null}}}`))
	for _, kind := range []string{"Namespace", "Pod"} {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(kind + "List"))
		if err := workload.List(ctx, list, client.HasLabels{istioDataplaneMode}); err != nil {
			return false, fmt.Errorf("listing what is enrolled in the mesh: %w", err)
		}
		for i := range list.Items {
			o := &list.Items[i]
			o.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(kind))
			if err := client.IgnoreNotFound(workload.Patch(ctx, o, unlabel)); err != nil {
				return false, fmt.Errorf("taking %s %s out of the mesh: %w", kind, client.ObjectKeyFromObject(o), err)
			}
		}
	}
	pods := &metav1.PartialObjectMetadataList{}
	pods.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("PodList"))
	if err := workload.List(ctx, pods); err != nil {
		return false, fmt.Errorf("listing the pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Annotations[istioRedirection] == "enabled" {
			return true, nil
		}
	}
	return false, nil
}
