//go:build integration

package provisioner

import (
	"context"
	"net/http"
	"strings"
	"testing"

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
