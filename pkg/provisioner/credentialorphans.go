package provisioner

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CredentialOrphans looks after the credentials of a cluster that were stored ahead of it, which
// have no owner until the Cluster exists. It makes the Cluster their owner once it does, and deletes
// those whose Cluster has not appeared within Grace, such as when its creation failed.
type CredentialOrphans struct {
	Client   client.Client
	Interval time.Duration
	Grace    time.Duration
	Now      func() time.Time
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*CredentialOrphans) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (o *CredentialOrphans) Start(ctx context.Context) error {
	tick := time.NewTicker(o.Interval)
	defer tick.Stop()
	for {
		if err := o.RunOnce(ctx); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).WithName("credential-orphans").Error(err, "looking after the credentials stored ahead of their clusters")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// RunOnce adopts or deletes every credential of a cluster that has no owner.
func (o *CredentialOrphans) RunOnce(ctx context.Context) error {
	secrets := &corev1.SecretList{}
	if err := o.Client.List(ctx, secrets, client.HasLabels{CredentialClusterLabel}); err != nil {
		return fmt.Errorf("listing the clusters' credentials: %w", err)
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Type != CredentialType || len(s.OwnerReferences) > 0 {
			continue
		}
		cluster := &clusterv1.Cluster{}
		err := o.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Labels[CredentialClusterLabel]}, cluster)
		switch {
		case err == nil:
			s.OwnerReferences = []metav1.OwnerReference{CredentialOwner(cluster)}
			if err = o.Client.Update(ctx, s); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("making cluster %s the owner of %s: %w", cluster.Name, s.Name, err)
			}
		case apierrors.IsNotFound(err):
			if now().Sub(s.CreationTimestamp.Time) < o.Grace {
				continue
			}
			if err = o.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting %s, whose cluster never came: %w", s.Name, err)
			}
		default:
			return fmt.Errorf("reading cluster %s: %w", s.Labels[CredentialClusterLabel], err)
		}
	}
	return nil
}

// CredentialOwner is the owner reference that has a cluster's credentials go with it.
func CredentialOwner(c *clusterv1.Cluster) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: c.Name, UID: c.UID}
}
