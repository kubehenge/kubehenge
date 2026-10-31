package provisioner

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// meshRig is cluster a of team-a with Cluster Mesh on, listing b, which lists it back, and c,
// which lists only b; a's workload cluster is reached through Remotes.
type meshRig struct {
	r        *Reconciler
	kube     client.WithWatch
	workload client.Client
	events   *events.FakeRecorder
	a        *clusterv1.Cluster
}

// watching is a client of the fake API server, which the interceptor wraps.
func watching(t *testing.T, c client.Client) client.WithWatch {
	t.Helper()
	w, ok := c.(client.WithWatch)
	if !ok {
		t.Fatalf("%T cannot watch", c)
	}
	return w
}

// newMeshRig makes the rig, with funcs, if any, standing between the reconciler and the
// management cluster.
func newMeshRig(t *testing.T, a *clusterv1.Cluster, funcs *interceptor.Funcs, extra ...client.Object) *meshRig {
	t.Helper()
	b := meshCluster("b", 2, "10.0.0.11", "10.2.0.0/16", `{"enabled":true,"peers":["a"]}`)
	c := meshCluster("c", 3, "10.0.0.12", "10.3.0.0/16", `{"enabled":true,"peers":["b"]}`)
	r, kube := newReconciler(t, &fakeCloud{}, append([]client.Object{a, b, c}, extra...)...)
	if funcs == nil {
		funcs = &interceptor.Funcs{}
	}
	m := &meshRig{
		r: r, kube: interceptor.NewClient(watching(t, kube), *funcs),
		workload: fake.NewClientBuilder().WithScheme(remoteScheme).Build(),
		events:   events.NewFakeRecorder(10), a: a,
	}
	r.Client, r.Events = m.kube, m.events
	r.Remotes = &Remotes{Reader: kube, byCluster: map[string]remote{remoteKey(a): {client: m.workload, until: time.Now().Add(time.Hour)}}}
	return m
}

func meshedA() *clusterv1.Cluster {
	return meshCluster("a", 1, "10.0.0.10", "10.1.0.0/16", `{"enabled":true,"peers":["b","c"]}`)
}

var meshOn = &Addons{ClusterMesh: ClusterMesh{Enabled: true, Peers: []string{"b", "c"}}}

// current reads cluster a as the management cluster has it now.
func (m *meshRig) current(t *testing.T) *clusterv1.Cluster {
	t.Helper()
	c := &clusterv1.Cluster{}
	if err := m.kube.Get(context.Background(), client.ObjectKeyFromObject(m.a), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (m *meshRig) mesh(t *testing.T, addons *Addons) bool {
	t.Helper()
	waiting, err := m.r.meshCluster(context.Background(), m.current(t), addons)
	if err != nil {
		t.Fatal(err)
	}
	return waiting
}

func (m *meshRig) caSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := m.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: meshCASecret}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A cluster switching Cluster Mesh on is marked before anything is written into it, gets the
// namespace's CA, made once and kept, and links only with the peers that list it back; each it
// lists that does not is reported on it.
func TestMeshClusterLinksWhatBothSidesListWithTheNamespacesCA(t *testing.T) {
	t.Parallel()
	m := newMeshRig(t, meshedA(), nil)
	if m.mesh(t, meshOn) {
		t.Fatal("waits for a kubeconfig it has")
	}
	if got := m.current(t).Annotations[meshWrittenAnnotation]; got != "true" {
		t.Errorf("the cluster is not marked as having the mesh written into it: %q", got)
	}
	ca := m.caSecret(t)
	if ca.Type != corev1.SecretTypeTLS || len(ca.Data[corev1.TLSPrivateKeyKey]) == 0 {
		t.Errorf("the CA is stored as %s with keys %d", ca.Type, len(ca.Data))
	}
	kvstoremesh := &corev1.Secret{}
	if err := m.workload.Get(context.Background(), client.ObjectKey{Namespace: "kube-system", Name: "cilium-kvstoremesh"}, kvstoremesh); err != nil {
		t.Fatal(err)
	}
	if _, ok := kvstoremesh.Data["b"]; !ok || len(kvstoremesh.Data) != 1 || !strings.Contains(string(kvstoremesh.Data["b"]), "https://10.0.0.11:32379") {
		t.Errorf("linked with %v", kvstoremesh.Data)
	}
	server := &corev1.Secret{}
	if err := m.workload.Get(context.Background(), client.ObjectKey{Namespace: "kube-system", Name: "clustermesh-apiserver-server-cert"}, server); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(server.Data["ca.crt"], ca.Data[corev1.TLSCertKey]) {
		t.Error("the workload cluster trusts another CA than the namespace's")
	}
	select {
	case e := <-m.events.Events:
		if !strings.Contains(e, "ClusterMeshNotLinked") || !strings.Contains(e, "not linked with c") {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("the peer that does not list it back was not reported")
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := m.workload.Get(context.Background(), client.ObjectKey{Namespace: "kube-system", Name: meshPolicy}, policy); !apierrors.IsNotFound(err) {
		t.Errorf("a cluster without RKE2's CIS profile got the mesh's NetworkPolicy: %v", err)
	}

	m.mesh(t, meshOn)
	if again := m.caSecret(t); !bytes.Equal(again.Data[corev1.TLSCertKey], ca.Data[corev1.TLSCertKey]) {
		t.Error("the namespace's CA was made again")
	}
}

// Switched off, the mesh's objects come out of the cluster and the mark goes; with nothing marked,
// the workload cluster is not even reached.
func TestMeshClusterSwitchedOffTakesOutWhatItWrote(t *testing.T) {
	t.Parallel()
	m := newMeshRig(t, meshedA(), nil)
	m.mesh(t, meshOn)
	if m.mesh(t, &Addons{}) {
		t.Error("waits while switching the mesh off")
	}
	if got, ok := m.current(t).Annotations[meshWrittenAnnotation]; ok {
		t.Errorf("the mark stayed: %q", got)
	}
	left := &corev1.SecretList{}
	if err := m.workload.List(context.Background(), left); err != nil || len(left.Items) != 0 {
		t.Errorf("left in the workload cluster: %d secrets, %v", len(left.Items), err)
	}

	m.r.Remotes = nil
	if m.mesh(t, &Addons{}) {
		t.Error("waits with nothing to do")
	}
}

// A cluster whose kubeconfig Cluster API has not written yet waits, unmarked and without a CA; one
// switched off before its kubeconfig came back loses the mark all the same.
func TestMeshClusterWaitsForTheKubeconfig(t *testing.T) {
	t.Parallel()
	m := newMeshRig(t, meshedA(), nil)
	m.r.Remotes = &Remotes{Reader: m.kube}
	if !m.mesh(t, meshOn) {
		t.Error("did not wait for the kubeconfig")
	}
	if _, ok := m.current(t).Annotations[meshWrittenAnnotation]; ok {
		t.Error("marked before anything was written")
	}
	if err := m.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: meshCASecret}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("a CA was made for a cluster that cannot be reached: %v", err)
	}

	marked := meshedA()
	marked.Annotations[meshWrittenAnnotation] = "true"
	m = newMeshRig(t, marked, nil)
	m.r.Remotes = &Remotes{Reader: m.kube}
	m.mesh(t, &Addons{})
	if _, ok := m.current(t).Annotations[meshWrittenAnnotation]; ok {
		t.Error("the mark stayed on a cluster that switched the mesh off")
	}
}

// RKE2's CIS profile lets only kube-system's own pods reach kube-system; the mesh's peers are let
// through to the clustermesh-apiserver, and only while the profile is on.
func TestMeshClusterOpensTheAPIServerUnderRKE2sCISProfile(t *testing.T) {
	t.Parallel()
	a := meshedA()
	a.Spec.Topology.ClassRef.Name = "kubehenge-rke2"
	m := newMeshRig(t, a, nil)
	m.mesh(t, &Addons{ClusterMesh: meshOn.ClusterMesh, CIS: true})
	key := client.ObjectKey{Namespace: "kube-system", Name: meshPolicy}
	if err := m.workload.Get(context.Background(), key, &networkingv1.NetworkPolicy{}); err != nil {
		t.Errorf("no NetworkPolicy under the CIS profile: %v", err)
	}
	m.mesh(t, meshOn)
	if err := m.workload.Get(context.Background(), key, &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Errorf("the NetworkPolicy stayed once the profile was off: %v", err)
	}
}

func TestMeshClusterReportsWhatItCannotDo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	broken := &corev1.Secret{
		Namespace: "team-a", Name: meshCASecret, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: []byte("not a certificate"), corev1.TLSPrivateKeyKey: []byte("nor a key")},
	}
	m := newMeshRig(t, meshedA(), nil, broken)
	if _, err := m.r.meshCluster(ctx, m.current(t), meshOn); err == nil || !strings.Contains(err.Error(), "reading the Cluster Mesh CA") {
		t.Errorf("a broken CA: %v", err)
	}
	if _, ok := m.current(t).Annotations[meshWrittenAnnotation]; ok {
		t.Error("marked although nothing could be written")
	}

	refused := errors.New("etcd is out of space")
	for name, funcs := range map[string]*interceptor.Funcs{
		"reading the Cluster Mesh CA of namespace team-a": {Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == meshCASecret {
				return refused
			}
			return c.Get(ctx, key, obj, opts...)
		}},
		"storing the Cluster Mesh CA of namespace team-a": {Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return refused
		}},
		"recording the cluster's Cluster Mesh objects": {Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return refused
		}},
		"listing the clusters of namespace team-a": {List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return refused
		}},
	} {
		m := newMeshRig(t, meshedA(), funcs)
		_, err := m.r.meshCluster(ctx, m.a.DeepCopy(), meshOn)
		if !errors.Is(err, refused) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
