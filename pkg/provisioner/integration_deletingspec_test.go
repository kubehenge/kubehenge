//go:build integration

package provisioner

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/test/integration"
)

// TestIntegrationDeletingClusterTakesNoSpecChange holds, on a real API server with the chart's
// webhook calling the controller, that a spec change to a cluster being deleted is refused, and that
// what a deletion writes still goes through until the cluster is gone.
func TestIntegrationDeletingClusterTakesNoSpecChange(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	objects, webhooks := apiObjects(t, "testdata/clusterapi-values.yaml")
	api := integration.Start(t, &integration.Options{
		Objects: objects, Namespaces: apiNamespaces(), Webhooks: webhooks, ClusterAPI: true,
		Serve: func(c client.Client) map[string]http.Handler {
			hooks := &Hooks{Reader: c, ClassNamespace: classNamespace, RancherKubernetes: KubernetesMinors{Min: 34, Max: 37}}
			return AdmissionWebhooks(c.Scheme(), hooks)
		},
	})
	c := api.Client
	ctx := context.Background()
	if err := c.Create(ctx, clusterOf("kubehenge-k3s", "web", 1, nil)); err != nil {
		t.Fatal(err)
	}
	web := &clusterv1.Cluster{Namespace: "team-a", Name: "web"}
	patch := func(body string) error {
		return c.Patch(ctx, web, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	if err := patch(`{"metadata":{"finalizers":["example.org/hold"],"annotations":{"` + AllowDeletionAnnotation + `":"true"}}}`); err != nil {
		t.Fatalf("holding the cluster and allowing its deletion: %v", err)
	}
	if err := c.Delete(ctx, web); err != nil {
		t.Fatalf("deleting the cluster: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(web), web); err != nil || web.DeletionTimestamp.IsZero() {
		t.Fatalf("the cluster is not being deleted: %v", err)
	}

	err := patch(`{"spec":{"topology":{"controlPlane":{"replicas":3}}}}`)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), ClusterBeingDeleted) {
		t.Errorf("a spec change to a cluster being deleted: %v, want %q", err, ClusterBeingDeleted)
	}
	if err = patch(`{"metadata":{"annotations":{"` + DeletionStatusAnnotation + `":"{}"}}}`); err != nil {
		t.Errorf("the deletion's status: %v", err)
	}
	if err = patch(`{"metadata":{"finalizers":null}}`); err != nil {
		t.Errorf("the last finalizer: %v", err)
	}
	if err = c.Get(ctx, client.ObjectKeyFromObject(web), web); !apierrors.IsNotFound(err) {
		t.Errorf("the cluster after its finalizers went: %v, want it gone", err)
	}
}

// TestIntegrationDeletingClusterLosesARetiredVariable holds, on a real API server with Cluster API's
// webhooks and the chart's, that a cluster being deleted while it sets a variable its class no
// longer defines has the variable taken off, and then deletes once its finalizers go.
func TestIntegrationDeletingClusterLosesARetiredVariable(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	api, _ := startClusterAPI(t, "testdata/clusterapi-values.yaml")
	c := api.Client
	ctx := context.Background()
	const class = "kubehenge-k3s"
	integration.ApplyClassTemplates(t, c, classNamespace, class, templatesKey)
	integration.WaitReconciled(t, c, classNamespace, class)
	redefineUseExisting(t, c, class, useExistingDefinition)
	web := clusterOf(class, "web", 1, map[string]string{"useExisting": `{"certManager":true}`})
	web.Finalizers = []string{"example.org/hold"}
	web.Annotations = map[string]string{AllowDeletionAnnotation: "true"}
	if err := c.Create(ctx, web); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, web); err != nil {
		t.Fatalf("deleting the cluster: %v", err)
	}
	redefineUseExisting(t, c, class)

	if err := c.Get(ctx, client.ObjectKeyFromObject(web), web); err != nil || web.DeletionTimestamp.IsZero() {
		t.Fatalf("the cluster is not being deleted: %v", err)
	}
	if err := (&Reconciler{Client: c}).dropRetiredVariables(ctx, web); err != nil {
		t.Fatalf("taking the retired variable off a cluster being deleted: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(web), web); err != nil || storedVariable(web, "useExisting") != "" {
		t.Fatalf("the retired variable after it was taken off: %q, %v", storedVariable(web, "useExisting"), err)
	}
	if err := c.Patch(ctx, web, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))); err != nil {
		t.Errorf("the last finalizer: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(web), web); !apierrors.IsNotFound(err) {
		t.Errorf("the cluster after its finalizers went: %v, want it gone", err)
	}
}

// useExistingDefinition is how the classes defined the retired variable useExisting.
var useExistingDefinition = clusterv1.ClusterClassVariable{Name: "useExisting", Required: new(false), Schema: clusterv1.VariableSchema{
	OpenAPIV3Schema: clusterv1.JSONSchemaProps{Type: "object", Properties: map[string]clusterv1.JSONSchemaProps{
		"certManager": {Type: "boolean", Default: &apiextensionsv1.JSON{Raw: []byte("false")}},
	}},
}}

// redefineUseExisting takes useExisting out of the class and puts the definitions given in, then
// waits for Cluster API to pass the class.
func redefineUseExisting(t *testing.T, c client.Client, class string, definitions ...clusterv1.ClusterClassVariable) {
	t.Helper()
	cc := &clusterv1.ClusterClass{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: classNamespace, Name: class}, cc); err != nil {
		t.Fatal(err)
	}
	cc.Spec.Variables = slices.DeleteFunc(cc.Spec.Variables, func(v clusterv1.ClusterClassVariable) bool { return v.Name == "useExisting" })
	cc.Spec.Variables = append(cc.Spec.Variables, definitions...)
	if err := c.Update(context.Background(), cc); err != nil {
		t.Fatalf("updating the class: %v", err)
	}
	integration.WaitReconciled(t, c, classNamespace, class)
}
