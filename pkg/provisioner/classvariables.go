package provisioner

import (
	"context"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiadmission "sigs.k8s.io/cluster-api/core/webhooks/admission"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Paths of the controller's admission webhooks, which the chart's webhook configurations call.
const (
	DefaultClusterPath  = "/default-cluster"
	ValidateClusterPath = "/validate-cluster"
)

// AdmissionWebhooks are the controller's admission webhooks by path: ClusterDefaulter, then
// ClusterAdmission, as the API server calls them for every Cluster.
func AdmissionWebhooks(scheme *runtime.Scheme, hooks *Hooks) map[string]http.Handler {
	return map[string]http.Handler{
		DefaultClusterPath:  admission.WithDefaulter[*clusterv1.Cluster](scheme, &ClusterDefaulter{Hooks: hooks}),
		ValidateClusterPath: admission.WithValidator[*clusterv1.Cluster](scheme, &ClusterAdmission{Hooks: hooks}),
	}
}

// ClusterAdmission is what the API server asks about Kubehenge's clusters as they are created and
// changed: their variables against their class's schemas and CEL rules, then ClusterValidator's
// checks.
type ClusterAdmission struct {
	Hooks *Hooks
}

var _ admission.Validator[*clusterv1.Cluster] = &ClusterAdmission{}

// ValidateCreate implements admission.Validator.
func (a *ClusterAdmission) ValidateCreate(ctx context.Context, c *clusterv1.Cluster) (admission.Warnings, error) {
	if err := a.checkVariables(ctx, nil, c); err != nil {
		return nil, err
	}
	return (&ClusterValidator{Hooks: a.Hooks}).ValidateCreate(ctx, c)
}

// ValidateUpdate implements admission.Validator.
func (a *ClusterAdmission) ValidateUpdate(ctx context.Context, old, c *clusterv1.Cluster) (admission.Warnings, error) {
	if err := a.checkVariables(ctx, old, c); err != nil {
		return nil, err
	}
	return (&ClusterValidator{Hooks: a.Hooks}).ValidateUpdate(ctx, old, c)
}

// ValidateDelete implements admission.Validator.
func (*ClusterAdmission) ValidateDelete(context.Context, *clusterv1.Cluster) (admission.Warnings, error) {
	return nil, nil
}

func (a *ClusterAdmission) checkVariables(ctx context.Context, old, c *clusterv1.Cluster) error {
	if !Owns(c, a.Hooks.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
		return nil
	}
	class := &clusterv1.ClusterClass{}
	if err := a.Hooks.Reader.Get(ctx, client.ObjectKey{Namespace: a.Hooks.ClassNamespace, Name: c.Spec.Topology.ClassRef.Name}, class); err != nil {
		if apierrors.IsNotFound(err) {
			// Cluster API refuses a cluster of a class that does not exist.
			return nil
		}
		return fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	if errs := checkClassVariables(ctx, old, c, class); len(errs) > 0 {
		// Invalid, so that the interface shows the reason as it does the API server's own validation.
		return apierrors.NewInvalid(clusterv1.GroupVersion.WithKind("Cluster").GroupKind(), c.Name, errs) //nolint:wrapcheck // made here, not passed on
	}
	return nil
}

// checkClassVariables holds a cluster's variables to its class's schemas and CEL rules with Cluster
// API's own code. Cluster API checks them only once its ClusterClass controller has passed the
// class, which needs every template the class names, and Kubehenge applies a class's templates only
// once its first cluster has started the class's providers: until then Cluster API lets every
// cluster of the class through unchecked, the first of each distribution included. Unset variables
// take their defaults on both sides first, so that a rule on changes compares a value with the
// default the cluster ran with, also where Cluster API never stored it.
func checkClassVariables(ctx context.Context, old, c *clusterv1.Cluster, class *clusterv1.ClusterClass) field.ErrorList {
	inline := class.DeepCopy()
	inline.Status.Variables = make([]clusterv1.ClusterClassStatusVariable, 0, len(class.Spec.Variables))
	for i := range class.Spec.Variables {
		v := &class.Spec.Variables[i]
		inline.Status.Variables = append(inline.Status.Variables, clusterv1.ClusterClassStatusVariable{
			Name: v.Name,
			Definitions: []clusterv1.ClusterClassStatusVariableDefinition{{
				From: clusterv1.VariableDefinitionFromInline, Required: v.Required, Schema: v.Schema,
			}},
		})
	}
	var was *clusterv1.Cluster
	if old != nil {
		was = old.DeepCopy()
		// A default the old value no longer meets is for the check of the new one to find.
		_ = capiadmission.DefaultVariables(was, inline)
	}
	return (&capiadmission.Cluster{}).DefaultAndValidateVariables(ctx, c.DeepCopy(), was, inline)
}
