package provisioner

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ClusterValidator answers the API server for Kubehenge's clusters as they are created and changed,
// so that what BeforeClusterCreate would refuse later is refused at once, and a change made after
// creation is held to the same: Rancher switched on beside another, or on nodes too small for it,
// and a Rancher cluster's nodes made smaller than it needs. A cluster that already fell short before
// a change is not held to it for unrelated edits.
type ClusterValidator struct {
	Hooks *Hooks
}

var _ admission.Validator[*clusterv1.Cluster] = &ClusterValidator{}

// ValidateCreate implements admission.Validator.
func (v *ClusterValidator) ValidateCreate(ctx context.Context, c *clusterv1.Cluster) (admission.Warnings, error) {
	return nil, v.validate(ctx, nil, c)
}

// ValidateUpdate implements admission.Validator.
func (v *ClusterValidator) ValidateUpdate(ctx context.Context, old, c *clusterv1.Cluster) (admission.Warnings, error) {
	return nil, v.validate(ctx, old, c)
}

// ValidateDelete implements admission.Validator.
func (*ClusterValidator) ValidateDelete(context.Context, *clusterv1.Cluster) (admission.Warnings, error) {
	return nil, nil
}

func (v *ClusterValidator) validate(ctx context.Context, old, c *clusterv1.Cluster) error {
	if !Owns(c, v.Hooks.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
		return nil
	}
	r, err := rancherOf(c)
	if err != nil || !r.Install {
		return err
	}
	class := &clusterv1.ClusterClass{}
	if err := v.Hooks.Reader.Get(ctx, client.ObjectKey{Namespace: v.Hooks.ClassNamespace, Name: c.Spec.Topology.ClassRef.Name}, class); err != nil {
		if apierrors.IsNotFound(err) {
			// Cluster API refuses a cluster of a class that does not exist.
			return nil
		}
		return fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	switchedOn := old == nil
	if old != nil {
		was, err := rancherOf(old)
		switchedOn = err == nil && !was.Install
	}
	var errs field.ErrorList
	topology := field.NewPath("spec", "topology")
	if switchedOn {
		if err := v.Hooks.checkRancher(ctx, c, c.Spec.Topology.Version); err != nil {
			errs = append(errs, field.Forbidden(topology.Child("variables"), err.Error()))
		}
	}
	if err := checkRancherNodes(c, class); err != nil && (switchedOn || checkRancherNodes(old, class) == nil) {
		errs = append(errs, field.Forbidden(topology, err.Error()))
	}
	if len(errs) == 0 {
		return nil
	}
	// Invalid, not the plain denial a validator's error becomes, so that the interface shows the
	// reason as it does the API server's own validation.
	return apierrors.NewInvalid(clusterv1.GroupVersion.WithKind("Cluster").GroupKind(), c.Name, errs) //nolint:wrapcheck // made here, not passed on
}
