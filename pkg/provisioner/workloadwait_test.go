package provisioner

import (
	"net"
	"net/netip"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// apiDownCluster is a cluster whose kubeconfig names an address nothing listens on, as a new
// cluster's does while its first control-plane VM boots. kube-vip once ran in it, so the reconcile
// looks for the Services kube-vip handled.
func apiDownCluster(t *testing.T, c *clusterv1.Cluster) (*Reconciler, *clusterv1.Cluster) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address, err := netip.ParseAddrPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	c.Annotations = map[string]string{kubeVipServicesAnnotation: trueText}
	c.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "127.0.0.1", Port: int32(address.Port())}
	r, kube := newReconciler(t, &fakeCloud{}, c, kubeconfigSecret(c, &rest.Config{Host: "https://" + address.String()}))
	r.Remotes = &Remotes{Reader: kube}
	return r, c
}

// Cluster API writes the kubeconfig before the first API server answers. Until the control plane is
// initialised the cluster is coming up, not failing, and its deletion waits on nothing in it.
func TestWorkloadReadsWaitForTheControlPlane(t *testing.T) {
	t.Parallel()
	r, c := apiDownCluster(t, storageCluster())
	if _, found, err := r.Remotes.get(t.Context(), c); found || err != nil {
		t.Fatalf("get = found %v, %v before the control plane is initialised, want neither", found, err)
	}
	if waiting, err := r.releaseKubeVipServices(t.Context(), c); waiting || err != nil {
		t.Errorf("releasing kube-vip's Services = waiting %v, %v, want done so deletion goes on", waiting, err)
	}
	if err := reconcile(t, r, c); err != nil {
		t.Errorf("a cluster coming up failed its reconcile: %v", err)
	}
}

// Once the control plane is initialised, an API that does not answer is a fault, said in the
// user's words.
func TestAnInitialisedClusterWhoseAPIDoesNotAnswer(t *testing.T) {
	t.Parallel()
	r, c := apiDownCluster(t, initialised(storageCluster()))
	if err := reconcile(t, r, c); err == nil {
		t.Fatal("no error while the cluster's API does not answer")
	}
	cond := kubehengeReady(t, r.Client, c)
	want := "the cluster's API at " + net.JoinHostPort("127.0.0.1", strconv.Itoa(int(c.Spec.ControlPlaneEndpoint.Port))) + " does not answer"
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Message != want {
		t.Errorf("%s = %+v, want False with %q", KubehengeReadyCondition, cond, want)
	}
}
