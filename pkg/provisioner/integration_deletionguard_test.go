//go:build integration

package provisioner

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/test/integration"
)

// whileAway answers as a controller that is away while away is set: it drops the connection, so
// that the API server fails to call the webhook.
func whileAway(away *atomic.Bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !away.Load() {
			next.ServeHTTP(w, r)
			return
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
}

// deleteCluster deletes a Kubehenge cluster as the interface and the CLI do: it annotates the
// cluster first, which the chart's deletion policy asks for.
func deleteCluster(ctx context.Context, c client.Client, cluster client.Object) error {
	allow := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+AllowDeletionAnnotation+`":"true"}}}`))
	if err := c.Patch(ctx, cluster, allow); err != nil {
		return err
	}
	return c.Delete(ctx, cluster)
}

// TestIntegrationDeletionGuard holds the chart's deletion policy on a real API server, with Cluster
// API's conversion: a delete of a Kubehenge cluster through v1beta1, alone or as a collection, is
// refused without the annotation, and the v1beta2 delete with it goes through. The annotation, a
// metadata-only patch, goes through while Kubehenge's cluster webhook cannot be reached, which a
// spec change does not.
func TestIntegrationDeletionGuard(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	var away atomic.Bool
	objects, webhooks := apiObjects(t, "testdata/clusterapi-values.yaml")
	api := integration.Start(t, &integration.Options{
		Objects: objects, Namespaces: apiNamespaces(), Webhooks: webhooks, ClusterAPI: true,
		Serve: func(c client.Client) map[string]http.Handler {
			hooks := &Hooks{Reader: c, ClassNamespace: classNamespace, RancherKubernetes: KubernetesMinors{Min: 34, Max: 37}}
			handlers := AdmissionWebhooks(c.Scheme(), hooks)
			for path, h := range handlers {
				handlers[path] = whileAway(&away, h)
			}
			return handlers
		},
	})
	c := api.Client
	ctx := context.Background()
	for i, name := range []string{"web", "fleet"} {
		if err := c.Create(ctx, clusterOf("kubehenge-k3s", name, i+1, nil)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	there := func(name string) bool {
		got := &clusterv1.Cluster{}
		err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, got)
		return err == nil && got.DeletionTimestamp.IsZero()
	}
	refused := func(what string, err error) {
		t.Helper()
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "is protected from deletion: annotate it with "+AllowDeletionAnnotation) {
			t.Errorf("%s: %v, want the deletion guard's refusal", what, err)
		}
	}

	old := &unstructured.Unstructured{}
	old.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	old.SetKind("Cluster")
	old.SetNamespace("team-a")
	old.SetName("web")
	refused("a delete through v1beta1", c.Delete(ctx, old))
	all := &unstructured.Unstructured{}
	all.SetAPIVersion("cluster.x-k8s.io/v1beta1")
	all.SetKind("Cluster")
	refused("a delete collection through v1beta1", c.DeleteAllOf(ctx, all, client.InNamespace("team-a")))
	if !there("web") || !there("fleet") {
		t.Fatal("a cluster went without the annotation")
	}

	away.Store(true)
	web := &clusterv1.Cluster{Namespace: "team-a", Name: "web"}
	scale := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"topology":{"controlPlane":{"replicas":3}}}}`))
	if err := c.Patch(ctx, web, scale); err == nil || !strings.Contains(err.Error(), "failed calling webhook") {
		t.Errorf("a spec change while the webhook cannot be reached: %v, want it refused", err)
	}
	allow := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"team":"a"},"annotations":{"`+AllowDeletionAnnotation+`":"true"}}}`))
	if err := c.Patch(ctx, web, allow); err != nil {
		t.Fatalf("the annotation while the webhook cannot be reached: %v", err)
	}
	if err := c.Delete(ctx, &clusterv1.Cluster{Namespace: "team-a", Name: "web"}); err != nil {
		t.Errorf("the annotated cluster's delete: %v", err)
	}
	if there("web") {
		t.Error("the annotated cluster is not being deleted")
	}
}
