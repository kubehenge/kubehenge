package ui

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A management cluster without a control-plane provider serves none of its kinds. The health read
// carries on without that object instead of failing the control-plane section.
func TestReadControlPlaneSkipsAKindNoProviderServes(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kube := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(meta.NewDefaultRESTMapper(nil)).Build()
	c := &clusterv1.Cluster{}
	c.Namespace, c.Name = "default", "one"
	c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: "controlplane.cluster.x-k8s.io", Kind: "RKE2ControlPlane", Name: "one-control-plane",
	}
	m := readManagement(t.Context(), kube, c)
	if m.controlPlaneErr != nil || m.controlPlane != nil {
		t.Errorf("control plane read as %v, %v, want neither", m.controlPlane, m.controlPlaneErr)
	}
	if got := m.failures(); len(got) != 0 {
		t.Errorf("failed reads %v, want none", got)
	}
}
