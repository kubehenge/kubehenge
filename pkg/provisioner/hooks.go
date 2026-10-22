package provisioner

import (
	"context"
	"fmt"
	"strings"
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
	// Releases has RKE2 and k3s nodes install the files recorded for their release.
	Releases *ReleaseChecksums
	// DatastoreAllocate is what lets a cluster hold Datastore.Allocate on its storage.
	DatastoreAllocate DatastoreAllocate
	// Client moves a cluster's node image time before an upgrade.
	Client client.Client
	// Soak is how long a template build waits before clusters are offered it.
	Soak time.Duration
	Now  func() time.Time
	// SysAuditGroup is the Proxmox VE group holding Sys.Audit on /; empty when no cluster may join it.
	SysAuditGroup string
	// VolumeSnapshots is what lets a cluster's CSI plugin take volume snapshots as root@pam.
	VolumeSnapshots VolumeSnapshots
}

// BeforeClusterUpgrade holds Rancher's cluster at the Kubernetes versions Rancher runs on, and has
// every upgrade clone its machines from the newest template builds offered: the upgrade replaces
// every machine anyway, and a refresh after it would replace them all again.
func (h *Hooks) BeforeClusterUpgrade(ctx context.Context, req *runtimehooksv1.BeforeClusterUpgradeRequest, resp *runtimehooksv1.BeforeClusterUpgradeResponse) {
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	if !Owns(&req.Cluster, h.ClassNamespace) {
		return
	}
	if err := h.checkRancher(ctx, &req.Cluster, req.ToKubernetesVersion); err != nil {
		// Retried rather than failed: a later Kubehenge release with a newer Rancher lets it through.
		resp.RetryAfterSeconds = 300
		resp.Message = err.Error()
		return
	}
	moved, err := h.newestNodeImages(ctx, &req.Cluster, req.ToKubernetesVersion)
	switch {
	case err != nil:
		resp.RetryAfterSeconds = 10
		resp.Message = "taking the newest node images: " + err.Error()
	case moved:
		// Held for a moment, so that the rollout starts from the templates of the new time rather
		// than rotating them again once it is under way.
		resp.RetryAfterSeconds = 5
		resp.Message = "taking the newest node images"
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
// refuses a cluster whose pod or service range overlaps its nodes' network, load-balancer
// addresses that clash with that network or another cluster's, logs without monitoring, backups
// Velero cannot take, Datastore.Allocate on a storage that is not the cluster's alone, volume
// snapshots where they cannot be taken, and Sys.Audit on / where no cluster may hold it.
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
	if err := h.refusal(ctx, cluster, class); err != nil {
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

// refusal is the first reason BeforeClusterCreate refuses the cluster for, or nil.
func (h *Hooks) refusal(ctx context.Context, cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	for _, check := range []func() error{
		func() error { return checkRanges(cluster, class) },
		func() error { return h.checkLoadBalancer(ctx, cluster) },
		func() error { return h.checkRancher(ctx, cluster, cluster.Spec.Topology.Version) },
		func() error { return checkRancherNodes(cluster, class) },
		func() error { return checkControlPlaneSize(nil, cluster, class) },
		func() error { return checkAddonRoom(nil, cluster, class) },
		func() error { return checkLogs(cluster) },
		func() error { return checkBackups(nil, cluster, class) },
		func() error { return h.checkDatastoreAllocate(ctx, cluster, class) },
		func() error { return h.checkVolumeSnapshots(ctx, cluster, class) },
		func() error { return h.checkSysAudit(cluster) },
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// newestNodeImages moves a cluster's node image time to now when that changes the build its
// machines at the version are cloned from, and tells whether it did.
func (h *Hooks) newestNodeImages(ctx context.Context, c *clusterv1.Cluster, version string) (bool, error) {
	if h.Client == nil || !RefreshesNodes(c) {
		return false, nil
	}
	builds, err := ClassBuilds(ctx, h.Reader, c)
	if err != nil {
		return false, err
	}
	t, err := NodeImageTime(c)
	if err != nil {
		return false, err
	}
	line, err := MachineLine(strings.TrimPrefix(c.Spec.Topology.ClassRef.Name, ClassPrefix), version)
	if err != nil {
		return false, err
	}
	now := h.now()
	if _, waiting := builds.Waiting(line, t, now, h.Soak); !waiting {
		return false, nil
	}
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, NodeImageAnnotation, now.Format(time.RFC3339))
	target := &clusterv1.Cluster{}
	target.Namespace, target.Name = c.Namespace, c.Name
	if err := h.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return false, fmt.Errorf("moving the node image time: %w", err)
	}
	return true, nil
}

func (h *Hooks) now() time.Time {
	if h.Now == nil {
		return time.Now().UTC().Truncate(time.Second)
	}
	return h.Now().UTC().Truncate(time.Second)
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
