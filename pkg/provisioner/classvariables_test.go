package provisioner

import (
	"context"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// variablesClass is a class whose variables carry CEL rules, with no status: a class Cluster API
// has not passed.
func variablesClass() *clusterv1.ClusterClass {
	schema := func(s clusterv1.JSONSchemaProps) clusterv1.VariableSchema {
		return clusterv1.VariableSchema{OpenAPIV3Schema: s}
	}
	return &clusterv1.ClusterClass{
		Namespace: "kubehenge-system", Name: "kubehenge-k3s",
		Spec: clusterv1.ClusterClassSpec{Variables: []clusterv1.ClusterClassVariable{
			{Name: "cni", Schema: schema(clusterv1.JSONSchemaProps{
				Type: "string", Enum: []apiextensionsv1.JSON{{Raw: []byte(`"flannel"`)}, {Raw: []byte(`"cilium"`)}},
				Default:      &apiextensionsv1.JSON{Raw: []byte(`"flannel"`)},
				XValidations: []clusterv1.ValidationRule{{Rule: "self == oldSelf", Message: "the CNI is chosen when the cluster is created"}},
			})},
			{Name: "backups", Schema: schema(clusterv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]clusterv1.JSONSchemaProps{
					"enabled": {Type: "boolean"},
					"target":  {Type: "string"},
				},
				XValidations: []clusterv1.ValidationRule{{Rule: "!self.enabled || has(self.target)", Message: "Backups need a target."}},
			})},
		}},
	}
}

func variablesCluster(vars map[string]string) *clusterv1.Cluster {
	c := &clusterv1.Cluster{
		Namespace: "team-a", Name: "web",
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-k3s", Namespace: "kubehenge-system"},
			Version:  "v1.36.5+k3s1",
		}},
	}
	return withAddons(c, vars)
}

func TestClusterAdmissionHoldsVariablesToTheirClass(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(variablesClass()).Build()
	a := &ClusterAdmission{Hooks: &Hooks{Reader: reader, ClassNamespace: "kubehenge-system"}}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"a rule broken", map[string]string{"backups": `{"enabled":true}`}, "Backups need a target."},
		{"a value the schema has not", map[string]string{"cni": `"weave"`}, `supported values: "flannel", "cilium"`},
		{"a variable the class has not", map[string]string{"istio": `{}`}, "variable is not defined"},
	} {
		if err := a.checkVariables(ctx, nil, variablesCluster(tc.vars)); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	made := variablesCluster(map[string]string{"backups": `{"enabled":true,"target":"store"}`})
	if err := a.checkVariables(ctx, nil, made); err != nil {
		t.Fatalf("a cluster that meets its class: %v", err)
	}
	// The cluster was made without the CNI, so with the default the rule on changes holds.
	if err := a.checkVariables(ctx, made, withAddons(made.DeepCopy(), map[string]string{"cni": `"cilium"`})); err == nil ||
		!strings.Contains(err.Error(), "the CNI is chosen when the cluster is created") {
		t.Errorf("the CNI changed from its default: %v", err)
	}
	if err := a.checkVariables(ctx, made, withAddons(made.DeepCopy(), map[string]string{"cni": `"flannel"`})); err != nil {
		t.Errorf("the CNI given as its default: %v", err)
	}
	other := variablesCluster(map[string]string{"backups": `{"enabled":true}`})
	other.Spec.Topology.ClassRef.Namespace = "elsewhere"
	if err := a.checkVariables(ctx, nil, other); err != nil {
		t.Errorf("a cluster of another's class: %v", err)
	}
}
