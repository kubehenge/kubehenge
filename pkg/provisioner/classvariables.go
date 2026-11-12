package provisioner

import (
	"context"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
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
		DefaultClusterPath:  admission.WithDefaulter(scheme, &ClusterDefaulter{Hooks: hooks}),
		ValidateClusterPath: admission.WithValidator(scheme, &ClusterAdmission{Hooks: hooks}),
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
	return a.admit(ctx, nil, c)
}

// ValidateUpdate implements admission.Validator.
func (a *ClusterAdmission) ValidateUpdate(ctx context.Context, old, c *clusterv1.Cluster) (admission.Warnings, error) {
	return a.admit(ctx, old, c)
}

// ValidateDelete implements admission.Validator.
func (*ClusterAdmission) ValidateDelete(context.Context, *clusterv1.Cluster) (admission.Warnings, error) {
	return nil, nil
}

// admit reads the cluster's class once for both of its checks.
func (a *ClusterAdmission) admit(ctx context.Context, old, c *clusterv1.Cluster) (admission.Warnings, error) {
	classOf := a.Hooks.classOf(c)
	if err := a.checkVariablesOf(ctx, old, c, classOf); err != nil {
		return nil, err
	}
	return (&ClusterValidator{Hooks: a.Hooks}).validateOf(ctx, old, c, classOf)
}

func (a *ClusterAdmission) checkVariables(ctx context.Context, old, c *clusterv1.Cluster) error {
	return a.checkVariablesOf(ctx, old, c, a.Hooks.classOf(c))
}

func (a *ClusterAdmission) checkVariablesOf(ctx context.Context, old, c *clusterv1.Cluster, classOf classReader) error {
	if !Owns(c, a.Hooks.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
		return nil
	}
	class, err := classOf(ctx)
	if err != nil || class == nil {
		return err
	}
	if errs := checkClassVariables(ctx, old, c, class); len(errs) > 0 {
		// Invalid, so that the interface shows the reason as it does the API server's own validation.
		return apierrors.NewInvalid(clusterv1.GroupVersion.WithKind("Cluster").GroupKind(), c.Name, errs) //nolint:wrapcheck // made here, not passed on
	}
	return nil
}

// classReader gives a cluster's ClusterClass, or nil where there is none. Cluster API refuses a
// cluster of a class that does not exist, so the checks that need the class let such a cluster pass.
type classReader func(ctx context.Context) (*clusterv1.ClusterClass, error)

// Cluster API's own webhook retries a class that is missing from its cache for this long, since a
// class made moments before its first cluster may not have reached the cache yet.
const (
	classWait         = 2 * time.Second
	classPollInterval = 200 * time.Millisecond
)

// classOf reads c's class on the first call and gives that answer on every later one, so that one
// admission request reads it once.
func (h *Hooks) classOf(c *clusterv1.Cluster) classReader {
	var (
		class *clusterv1.ClusterClass
		err   error
		read  bool
	)
	return func(ctx context.Context) (*clusterv1.ClusterClass, error) {
		if !read {
			class, err = h.readClass(ctx, c)
			read = true
		}
		return class, err
	}
}

func (h *Hooks) readClass(ctx context.Context, c *clusterv1.Cluster) (*clusterv1.ClusterClass, error) {
	key := client.ObjectKey{Namespace: h.ClassNamespace, Name: c.Spec.Topology.ClassRef.Name}
	class := &clusterv1.ClusterClass{}
	var err error
	if h.Classes == nil {
		err = h.Reader.Get(ctx, key, class)
	} else {
		_ = wait.PollUntilContextTimeout(ctx, classPollInterval, classWait, true, func(ctx context.Context) (bool, error) {
			err = h.Classes.Get(ctx, key, class)
			return !apierrors.IsNotFound(err), nil
		})
	}
	if apierrors.IsNotFound(err) {
		return nil, nil //nolint:nilnil // a missing class is an answer, which classReader documents
	}
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	return class, nil
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
