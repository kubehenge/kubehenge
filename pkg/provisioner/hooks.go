package provisioner

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Announcer has Cluster API rediscover the Runtime Extension's hooks once this controller serves
// them: Cluster API asks an extension which hooks it serves only when its ExtensionConfig changes,
// so hooks a newer controller adds would go unasked until then.
type Announcer struct {
	Client client.Client
	// ExtensionConfig names the extension's ExtensionConfig.
	ExtensionConfig string
	// Serving reports whether the extension server is up.
	Serving func() error
	// Version tells this controller apart from earlier ones, such as its pod's name.
	Version string
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*Announcer) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable.
func (a *Announcer) Start(ctx context.Context) error {
	for a.Serving() != nil {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	ec := &unstructured.Unstructured{}
	ec.SetAPIVersion("runtime.cluster.x-k8s.io/v1beta2")
	ec.SetKind("ExtensionConfig")
	ec.SetName(a.ExtensionConfig)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, "kubehenge.dev/served-by", a.Version)
	for {
		err := a.Client.Patch(ctx, ec, client.RawPatch(types.MergePatchType, []byte(patch)))
		if err == nil {
			return nil
		}
		ctrl.LoggerFrom(ctx).Error(err, "announcing the extension's hooks")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(waitInterval):
		}
	}
}

// Hooks are Cluster API lifecycle hooks, served as a Runtime Extension.
type Hooks struct {
	// Reader should read past any cache: the hook must see the Secret the moment it is written.
	Reader         client.Reader
	ClassNamespace string
	Registrar      *Registrar
	// RancherKubernetes is what the Rancher release Kubehenge installs runs on.
	RancherKubernetes KubernetesMinors
	// Nodes is asked which Proxmox VE nodes are online for a cluster the Placer has not marked yet.
	Nodes Nodes
}

// BeforeClusterUpgrade holds Rancher's cluster at the Kubernetes versions Rancher runs on.
func (h *Hooks) BeforeClusterUpgrade(ctx context.Context, req *runtimehooksv1.BeforeClusterUpgradeRequest, resp *runtimehooksv1.BeforeClusterUpgradeResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	if !Owns(&req.Cluster, h.ClassNamespace) {
		return
	}
	if err := h.checkRancher(ctx, &req.Cluster, req.ToKubernetesVersion); err != nil {
		// Retried rather than failed: a later Kubehenge release with a newer Rancher lets it through.
		resp.RetryAfterSeconds = 300
		resp.Message = err.Error()
	}
}

// BeforeClusterDelete removes the cluster from Rancher while its VMs still run, so that Rancher can
// clean up inside it; for Rancher's own cluster, every cluster registered with it first.
func (h *Hooks) BeforeClusterDelete(ctx context.Context, req *runtimehooksv1.BeforeClusterDeleteRequest, resp *runtimehooksv1.BeforeClusterDeleteResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	if !Owns(&req.Cluster, h.ClassNamespace) || h.Registrar == nil {
		return
	}
	done, err := h.Registrar.BeforeDelete(ctx, &req.Cluster)
	switch {
	case err != nil:
		ctrl.LoggerFrom(ctx).Error(err, "removing the cluster from Rancher", "cluster", client.ObjectKeyFromObject(&req.Cluster))
		resp.RetryAfterSeconds = 10
		resp.Message = "removing the cluster from Rancher: " + err.Error()
	case !done:
		resp.RetryAfterSeconds = 10
		resp.Message = "removing the cluster from Rancher"
	}
}

// BeforeClusterCreate holds a new cluster back until the Reconciler has created its Proxmox VE
// objects; capmox would otherwise clone its first VMs into a pool that does not exist yet. It
// refuses a cluster whose pod or service range overlaps its nodes' network.
func (h *Hooks) BeforeClusterCreate(ctx context.Context, req *runtimehooksv1.BeforeClusterCreateRequest, resp *runtimehooksv1.BeforeClusterCreateResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	cluster := &req.Cluster
	if !Owns(cluster, h.ClassNamespace) {
		return
	}
	class := &clusterv1.ClusterClass{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: h.ClassNamespace, Name: cluster.Spec.Topology.ClassRef.Name}, class); err != nil {
		resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
		return
	}
	if err := checkRanges(cluster, class); err != nil {
		resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
		return
	}
	if err := h.checkRancher(ctx, cluster, cluster.Spec.Topology.Version); err != nil {
		resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
		return
	}
	if err := checkRancherNodes(cluster, class); err != nil {
		resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
		return
	}
	if err := checkControlPlaneSize(nil, cluster, class); err != nil {
		resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
		return
	}
	if err := checkAddonRoom(nil, cluster, class); err != nil {
		resp.Status, resp.Message = runtimehooksv1.ResponseStatusFailure, err.Error()
		return
	}
	s := &corev1.Secret{}
	err := h.Reader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, s)
	switch {
	case apierrors.IsNotFound(err) || err == nil && !isReady(s):
		resp.RetryAfterSeconds = 5
		resp.Message = "waiting for the cluster's Proxmox VE pool, user and token"
	case err != nil:
		ctrl.LoggerFrom(ctx).Error(err, "reading the cluster's Proxmox VE credentials", "cluster", client.ObjectKeyFromObject(cluster))
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = err.Error()
	}
}

// checkRancher refuses Rancher on a Kubernetes version its release does not run on, and a second
// cluster with Rancher: every cluster registers with the one.
func (h *Hooks) checkRancher(ctx context.Context, cluster *clusterv1.Cluster, version string) error {
	v, err := rancherOf(cluster)
	if err != nil || !v.Install {
		return err
	}
	if !h.RancherKubernetes.Allows(version) {
		return fmt.Errorf("the Rancher release Kubehenge installs runs on Kubernetes 1.%d to 1.%d, not %s",
			h.RancherKubernetes.Min, h.RancherKubernetes.Max, version)
	}
	clusters := &clusterv1.ClusterList{}
	if err := h.Reader.List(ctx, clusters); err != nil {
		return fmt.Errorf("listing clusters: %w", err)
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if c.Namespace == cluster.Namespace && c.Name == cluster.Name || !Owns(c, h.ClassNamespace) {
			continue
		}
		if other, err := rancherOf(c); err == nil && other.Install {
			return fmt.Errorf("cluster %s/%s already runs Rancher, with which every cluster registers", c.Namespace, c.Name)
		}
	}
	return nil
}
