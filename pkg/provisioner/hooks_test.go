package provisioner

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestBeforeClusterDeleteActsOnlyOnAClusterBeingDeleted(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	rancher := clusterWithRancher("rancher", true)
	gone := cluster()
	gone.Name = "gone"
	gone.Finalizers = []string{Finalizer}
	gone.DeletionTimestamp = new(metav1.Now())
	// Every removal from Rancher starts by looking for Rancher's cluster.
	var looked atomic.Int32
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rancher, gone).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			looked.Add(1)
			return c.List(ctx, list, opts...)
		},
	}).Build()
	h := &Hooks{Reader: kube, ClassNamespace: "kubehenge-system", Registrar: &Registrar{Client: kube, ClassNamespace: "kubehenge-system"}}
	call := func(c *clusterv1.Cluster) *runtimehooksv1.BeforeClusterDeleteResponse {
		resp := &runtimehooksv1.BeforeClusterDeleteResponse{}
		h.BeforeClusterDelete(t.Context(), &runtimehooksv1.BeforeClusterDeleteRequest{Cluster: *c}, resp)
		return resp
	}

	// Rancher's cluster would take every cluster out of Rancher with it.
	forged := rancher.DeepCopy()
	forged.DeletionTimestamp = new(metav1.Now())
	if resp := call(forged); resp.Status != runtimehooksv1.ResponseStatusSuccess || looked.Load() != 0 {
		t.Errorf("a cluster the API does not hold as being deleted was removed from Rancher: %+v", resp)
	}
	missing := cluster()
	missing.Name = "missing"
	if resp := call(missing); resp.Status != runtimehooksv1.ResponseStatusSuccess || looked.Load() != 0 {
		t.Errorf("a cluster the API does not hold was removed from Rancher: %+v", resp)
	}
	sent := gone.DeepCopy()
	sent.DeletionTimestamp = nil
	if resp := call(sent); resp.Status != runtimehooksv1.ResponseStatusSuccess || resp.RetryAfterSeconds != 0 || looked.Load() != 1 {
		t.Errorf("a cluster being deleted: %+v, looked for Rancher %d times", resp, looked.Load())
	}
}

func TestBeforeClusterUpgradeActsOnTheStoredCluster(t *testing.T) {
	t.Parallel()
	c, class := refreshCluster(t, "web", nil)
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c, class).Build()
	now := day(20)
	h := &Hooks{Reader: kube, Client: kube, ClassNamespace: "kubehenge-system", Soak: DefaultNodeImageSoak, Now: func() time.Time { return now }}
	call := func(sent *clusterv1.Cluster, version string) *runtimehooksv1.BeforeClusterUpgradeResponse {
		resp := &runtimehooksv1.BeforeClusterUpgradeResponse{}
		h.BeforeClusterUpgrade(t.Context(), &runtimehooksv1.BeforeClusterUpgradeRequest{Cluster: *sent, ToKubernetesVersion: version}, resp)
		return resp
	}

	// The Cluster stays at its version, so no upgrade moves its node images.
	resp := call(c, "v1.37.1+rke2r1")
	got := &clusterv1.Cluster{}
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if resp.RetryAfterSeconds == 0 || !strings.Contains(resp.Message, "is at v1.36.5+rke2r1") || got.Annotations[NodeImageAnnotation] != c.Annotations[NodeImageAnnotation] {
		t.Errorf("an upgrade the Cluster does not name: %+v, node image time %q", resp, got.Annotations[NodeImageAnnotation])
	}
	sent := c.DeepCopy()
	sent.Annotations[TalosUpgradingAnnotation] = "web-cp-0"
	if resp := call(sent, c.Spec.Topology.Version); strings.Contains(resp.Message, "Talos upgrade") {
		t.Errorf("held for what only the request says: %+v", resp)
	}
}
