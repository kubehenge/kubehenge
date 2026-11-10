package provisioner

import (
	"reflect"
	"slices"

	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// controlPlaneEndpointVariable is the virtual IP address of the cluster's API.
const controlPlaneEndpointVariable = "controlPlaneEndpoint"

// fixedVariables are the class variables that say where a cluster's disks and API address are. The
// interface and admission both read this list, so that kubectl and GitOps hold to what the interface
// does.
var fixedVariables = []string{storageVariable, controlPlaneEndpointVariable}

// VariableFixed tells whether the variable cannot change once the cluster exists.
func VariableFixed(name string) bool { return slices.Contains(fixedVariables, name) }

// FixedVariableRefusal is what a change of a variable that cannot change is refused with.
func FixedVariableRefusal(name string) string {
	return name + " cannot change after the cluster is created"
}

// validateFixed refuses an update that changes a variable VariableFixed names. The values are
// compared as JSON, so a differently written value of the same meaning passes.
func validateFixed(old, c *clusterv1.Cluster) field.ErrorList {
	if old == nil {
		return nil
	}
	var errs field.ErrorList
	for _, name := range fixedVariables {
		was, wasSet, err1 := optionalVariable[any](old, name)
		is, isSet, err2 := optionalVariable[any](c, name)
		if err1 == nil && err2 == nil && wasSet == isSet && reflect.DeepEqual(was, is) {
			continue
		}
		errs = append(errs, field.Forbidden(field.NewPath("spec", "topology", "variables").Key(name), FixedVariableRefusal(name)))
	}
	return errs
}
