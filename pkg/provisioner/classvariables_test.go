package provisioner

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

// classReads makes h read ClusterClasses only through a cache that counts its reads. Any
// ClusterClass request to the live reader fails the test.
func classReads(t *testing.T, h *Hooks, failFirst int32) *atomic.Int32 {
	t.Helper()
	live, ok := h.Reader.(client.WithWatch)
	if !ok {
		t.Fatal("the hooks' reader cannot be wrapped")
	}
	var reads atomic.Int32
	h.Reader = interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*clusterv1.ClusterClass); ok {
				t.Errorf("live read of ClusterClass %s", key)
				return errors.New("live ClusterClass read")
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*clusterv1.ClusterClassList); ok {
				t.Error("live list of ClusterClasses")
				return errors.New("live ClusterClass list")
			}
			return c.List(ctx, list, opts...)
		},
	})
	h.Classes = interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if reads.Add(1) <= failFirst {
				return apierrors.NewNotFound(clusterv1.GroupVersion.WithResource("clusterclasses").GroupResource(), key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	return &reads
}

func TestAdmissionReadsTheClassOnceFromTheCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := hooksFor(t)
	reads := classReads(t, h, 0)
	a := &ClusterAdmission{Hooks: h}
	c := networkCluster("10.0.0.1", 24)
	if _, err := a.ValidateCreate(ctx, c); err != nil && strings.Contains(err.Error(), "ClusterClass") {
		t.Fatalf("create: %v", err)
	}
	if _, err := a.ValidateUpdate(ctx, c, c.DeepCopy()); err != nil && strings.Contains(err.Error(), "ClusterClass") {
		t.Fatalf("update: %v", err)
	}
	if got := reads.Load(); got != 2 {
		t.Errorf("cache reads for two requests: %d, want one each", got)
	}
}

func TestDefaulterReadsTheClassFromTheCache(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	reads := classReads(t, h, 0)
	c := withCNI(networkCluster("10.0.0.1", 24), "cilium")
	if err := (&ClusterDefaulter{Hooks: h}).Default(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("cache reads for one request: %d, want 1", got)
	}
	if got := rangesString(c); got != "10.42.0.0/16 10.43.0.0/16" {
		t.Errorf("ranges: %s", got)
	}
}

func TestAdmissionWaitsForAClassThatIsNotInTheCacheYet(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	h := &Hooks{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(variablesClass()).Build(), ClassNamespace: "kubehenge-system"}
	reads := classReads(t, h, 3)
	err := (&ClusterAdmission{Hooks: h}).checkVariables(context.Background(), nil, variablesCluster(map[string]string{"backups": `{"enabled":true}`}))
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "Backups need a target.") {
		t.Errorf("a class that arrives within the wait: %v", err)
	}
	if got := reads.Load(); got != 4 {
		t.Errorf("reads before the class arrived: %d, want 4", got)
	}
}

func TestAdmissionLetsAClusterOfAMissingClassThrough(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	h := &Hooks{Reader: fake.NewClientBuilder().WithScheme(scheme).Build(), ClassNamespace: "kubehenge-system"}
	classReads(t, h, 1<<20)
	start := time.Now()
	if _, err := (&ClusterAdmission{Hooks: h}).ValidateCreate(context.Background(), variablesCluster(nil)); err != nil {
		t.Errorf("a class that never arrives: %v", err)
	}
	if took := time.Since(start); took < classWait {
		t.Errorf("gave up after %s, want the wait of %s", took, classWait)
	}
}
