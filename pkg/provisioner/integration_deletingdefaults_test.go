//go:build integration

package provisioner

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/test/integration"
)

func startDeletionAPI(t *testing.T) client.Client {
	t.Helper()
	objects, webhooks := apiObjects(t, "testdata/clusterapi-values.yaml")
	api := integration.Start(t, &integration.Options{
		Objects: objects, Namespaces: apiNamespaces(), Webhooks: webhooks, ClusterAPI: true,
		Serve: func(c client.Client) map[string]http.Handler {
			hooks := &Hooks{Reader: c, ClassNamespace: classNamespace, RancherKubernetes: KubernetesMinors{Min: 34, Max: 37}}
			return AdmissionWebhooks(c.Scheme(), hooks)
		},
	})
	return api.Client
}

// holdAndDelete makes web hold a finalizer, allows its deletion and deletes it.
func holdAndDelete(ctx context.Context, t *testing.T, c client.Client) *clusterv1.Cluster {
	t.Helper()
	web := &clusterv1.Cluster{Namespace: "team-a", Name: "web"}
	body := `{"metadata":{"finalizers":["example.org/hold"],"annotations":{"` + AllowDeletionAnnotation + `":"true"}}}`
	if err := c.Patch(ctx, web, client.RawPatch(types.MergePatchType, []byte(body))); err != nil {
		t.Fatalf("holding the cluster and allowing its deletion: %v", err)
	}
	if err := c.Delete(ctx, web); err != nil {
		t.Fatalf("deleting the cluster: %v", err)
	}
	return web
}

// finishDeletion makes the writes a deletion needs, one by one, and ends with the cluster gone. A
// person's change of the spec in the middle is refused.
func finishDeletion(ctx context.Context, t *testing.T, c client.Client, web *clusterv1.Cluster) {
	t.Helper()
	patch := func(body string) error {
		return c.Patch(ctx, web, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	for what, body := range map[string]string{
		"the deletion status": `{"metadata":{"annotations":{"` + DeletionStatusAnnotation + `":"{}"}}}`,
		"ok-to-delete":        `{"metadata":{"annotations":{"runtime.cluster.x-k8s.io/ok-to-delete":""}}}`,
		"abandon-deletion":    `{"metadata":{"annotations":{"` + AbandonAnnotation + `":"web"}}}`,
		"a label of a person": `{"metadata":{"labels":{"team":"a"}}}`,
	} {
		if err := patch(body); err != nil {
			t.Errorf("%s on a cluster being deleted: %v", what, err)
		}
	}
	err := patch(`{"spec":{"topology":{"controlPlane":{"replicas":3}}}}`)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), ClusterBeingDeleted) {
		t.Errorf("a person's spec change to a cluster being deleted: %v, want %q", err, ClusterBeingDeleted)
	}
	if err = patch(`{"metadata":{"finalizers":null}}`); err != nil {
		t.Errorf("the last finalizer: %v", err)
	}
	if err = c.Get(ctx, client.ObjectKeyFromObject(web), web); !apierrors.IsNotFound(err) {
		t.Errorf("the cluster after its finalizers went: %v, want it gone", err)
	}
}

// TestIntegrationDeletionEndsWhereTheClassGainsADefault holds that a class change which Cluster API
// answers by defaulting a new variable into the cluster does not hold the deletion up.
func TestIntegrationDeletionEndsWhereTheClassGainsADefault(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	c := startDeletionAPI(t)
	ctx := context.Background()
	integration.ApplyClassTemplates(t, c, classNamespace, "kubehenge-k3s", templatesKey)
	integration.WaitReconciled(t, c, classNamespace, "kubehenge-k3s")
	if err := c.Create(ctx, clusterOf("kubehenge-k3s", "web", 1, nil)); err != nil {
		t.Fatal(err)
	}
	web := holdAndDelete(ctx, t, c)

	// The harness's manager registers no Cluster index by class, so Cluster API's ClusterClass
	// webhook would fail every update of a class in use. Cluster API's own manager has the index.
	var configurations admissionregistrationv1.ValidatingWebhookConfigurationList
	if err := c.List(ctx, &configurations); err != nil {
		t.Fatal(err)
	}
	for i := range configurations.Items {
		w := &configurations.Items[i]
		n := len(w.Webhooks)
		w.Webhooks = slices.DeleteFunc(w.Webhooks, func(h admissionregistrationv1.ValidatingWebhook) bool {
			return h.Name == "validation.clusterclass.cluster.x-k8s.io"
		})
		if len(w.Webhooks) != n {
			if err := c.Update(ctx, w); err != nil {
				t.Fatal(err)
			}
		}
	}
	class := &clusterv1.ClusterClass{Namespace: classNamespace, Name: "kubehenge-k3s"}
	add := `[{"op":"add","path":"/spec/variables/-","value":{"name":"addedLater","required":false,"schema":{"openAPIV3Schema":{"type":"integer","default":7}}}}]`
	if err := c.Patch(ctx, class, client.RawPatch(types.JSONPatchType, []byte(add))); err != nil {
		t.Fatalf("adding a defaulted variable to the class: %v", err)
	}
	integration.WaitReconciled(t, c, classNamespace, "kubehenge-k3s")
	finishDeletion(ctx, t, c, web)
}

// TestIntegrationDeletionEndsOfAClusterMadeBeforeItsClassWasPassed holds the same for a cluster
// whose defaults were never stored, since Cluster API had not passed its class when it was made.
func TestIntegrationDeletionEndsOfAClusterMadeBeforeItsClassWasPassed(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	c := startDeletionAPI(t)
	ctx := context.Background()
	if err := c.Create(ctx, clusterOf("kubehenge-k3s", "web", 1, nil)); err != nil {
		t.Fatal(err)
	}
	web := holdAndDelete(ctx, t, c)
	integration.ApplyClassTemplates(t, c, classNamespace, "kubehenge-k3s", templatesKey)
	integration.WaitReconciled(t, c, classNamespace, "kubehenge-k3s")
	finishDeletion(ctx, t, c, web)
}
