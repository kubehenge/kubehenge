package provisioner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var (
	certNow = time.Date(2027, 9, 1, 12, 0, 0, 0, time.UTC)
	k3sGVK  = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta2", Kind: "KThreesControlPlane"}
)

type certFixture struct {
	t        *testing.T
	kube     client.Client
	workload client.Client
	events   *events.FakeRecorder
	watch    *CertificateWatch
}

// certCluster is web of a distribution with replicas control planes and one worker deployment.
func certCluster(d string, replicas int32) *clusterv1.Cluster {
	c := cluster()
	c.Spec.Topology.ClassRef.Name = ClassPrefix + d
	c.Spec.Topology.ControlPlane.Replicas = new(replicas)
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Name: "md-0", Class: "workers"}}
	c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: k3sGVK.Group, Kind: k3sGVK.Kind, Name: "web-cp"}
	return c
}

// certMachine is a ready machine of web made a year before certNow, with the label that gives its role.
func certMachine(name, roleLabel, roleValue string) *clusterv1.Machine {
	return &clusterv1.Machine{
		Name: name, Namespace: "team-a",
		CreationTimestamp: metav1.NewTime(certNow.AddDate(-1, 0, 0)),
		Labels:            map[string]string{clusterv1.ClusterNameLabel: "web", roleLabel: roleValue},
		Spec:              clusterv1.MachineSpec{ClusterName: "web"},
		Status: clusterv1.MachineStatus{
			NodeRef:    clusterv1.MachineNodeReference{Name: name},
			Conditions: []metav1.Condition{{Type: clusterv1.MachineReadyCondition, Status: metav1.ConditionTrue, Reason: "Ready"}},
		},
	}
}

func certControlPlane(name string) *clusterv1.Machine {
	return certMachine(name, clusterv1.MachineControlPlaneLabel, "")
}

func certWorker(name string) *clusterv1.Machine {
	return certMachine(name, clusterv1.ClusterTopologyMachineDeploymentNameLabel, "md-0")
}

// report is what the certificate monitor of RKE2 and k3s records on a node whose certificates run
// out in days, seen ago.
func report(node string, days int, ago time.Duration) *corev1.Event {
	at := certNow.AddDate(0, 0, days).Format(time.RFC3339)
	return &corev1.Event{
		Name: node + ".cert", Namespace: metav1.NamespaceDefault,
		InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: node},
		Reason:         nodeCertificatesEvent,
		LastTimestamp:  metav1.NewTime(certNow.Add(-ago)),
		Message: "Node certificates require attention - restart rke2 on this node to trigger automatic rotation: " +
			"agent/client-kubelet.crt: certificate system:node:" + node + " will expire within 120 days at " + at +
			"\nagent/serving-kubelet.crt: certificate " + node + " will expire within 120 days at " + certNow.AddDate(0, 0, days+1).Format(time.RFC3339),
	}
}

func newCertFixture(t *testing.T, c *clusterv1.Cluster, objs []client.Object, reports ...client.Object) *certFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	scheme.AddKnownTypeWithName(k3sGVK, &unstructured.Unstructured{})
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{k3sGVK.GroupVersion()})
	mapper.Add(k3sGVK, meta.RESTScopeNamespace)
	cp := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	cp.SetGroupVersionKind(k3sGVK)
	cp.SetNamespace("team-a")
	cp.SetName("web-cp")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(append(objs, c, cp)...).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	workload := fake.NewClientBuilder().WithScheme(scheme).WithObjects(reports...).Build()
	f := &certFixture{t: t, kube: kube, workload: workload, events: events.NewFakeRecorder(20)}
	f.watch = &CertificateWatch{
		Client: kube, ClassNamespace: "kubehenge-system", Events: f.events, Now: func() time.Time { return certNow },
		Workload: func(context.Context, *clusterv1.Cluster) (client.Reader, bool, error) { return workload, true, nil },
	}
	return f
}

func (f *certFixture) look() *metav1.Condition {
	f.t.Helper()
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), webKey, c); err != nil {
		f.t.Fatal(err)
	}
	if err := f.watch.watch(context.Background(), c); err != nil {
		f.t.Fatal(err)
	}
	if err := f.kube.Get(context.Background(), webKey, c); err != nil {
		f.t.Fatal(err)
	}
	return meta.FindStatusCondition(c.Status.Conditions, CertificatesCondition)
}

func (f *certFixture) drained() []string {
	var got []string
	for {
		select {
		case e := <-f.events.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func (f *certFixture) machine(name string) *clusterv1.Machine {
	f.t.Helper()
	m := &clusterv1.Machine{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

// An RKE2 cluster's nodes report certificates running out within the 60 days: the workers' deployment
// rolls, one control-plane machine at a time is replaced by its health check, and the cluster says so.
func TestCertificatesRenewedByReplacing(t *testing.T) {
	t.Parallel()
	f := newCertFixture(t, certCluster("rke2", 3),
		[]client.Object{certControlPlane("web-cp-a"), certControlPlane("web-cp-b"), certControlPlane("web-cp-c"), certWorker("web-md-0-a")},
		report("web-cp-a", 45, time.Minute), report("web-cp-b", 46, time.Minute), report("web-md-0-a", 50, time.Minute), report("web-cp-c", 90, time.Minute))
	cond := f.look()
	if cond == nil || cond.Reason != CertificatesRenewing {
		t.Fatalf("condition %+v", cond)
	}
	if head := CertificatesHead(cond.Message); head != "Certificates run out: machine web-cp-a on 2027-10-16, machine web-cp-b on 2027-10-17, machine web-md-0-a on 2027-10-21." {
		t.Errorf("head %q", head)
	}
	if _, ok := f.machine("web-cp-a").Annotations[clusterv1.RemediateMachineAnnotation]; !ok {
		t.Error("web-cp-a is not replaced")
	}
	if _, ok := f.machine("web-cp-b").Annotations[clusterv1.RemediateMachineAnnotation]; ok {
		t.Error("two control-plane machines are replaced at once")
	}
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), webKey, c); err != nil {
		t.Fatal(err)
	}
	if after := c.Spec.Topology.Workers.MachineDeployments[0].Rollout.After; !after.Equal(&metav1.Time{Time: certNow}) {
		t.Errorf("workers roll after %v", after)
	}
	if got := f.drained(); len(got) != 1 || !strings.HasPrefix(got[0], "Normal RenewingCertificates replacing web-cp-a, web-md-0-a") {
		t.Errorf("events %q", got)
	}

	f.look()
	if got := f.drained(); len(got) != 0 {
		t.Errorf("events %q on a second look", got)
	}
}

// staleCache has the watch's client read the cluster as it was before the look, as a cache that
// lags behind the watch's own writes does, while its reader reads past it.
func (f *certFixture) staleCache() {
	f.t.Helper()
	kube, ok := f.kube.(client.WithWatch)
	if !ok {
		f.t.Fatal("the fixture's client does not watch")
	}
	stale := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), webKey, stale); err != nil {
		f.t.Fatal(err)
	}
	f.watch.Reader = kube
	f.watch.Client = interceptor.NewClient(kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if cached, isCluster := obj.(*clusterv1.Cluster); isCluster {
				stale.DeepCopyInto(cached)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

// Workers of two pools due in one look both roll, and the cluster says so, though the cache holds
// the cluster from before the first roll.
func TestCertificatesRollTwoPoolsPastAStaleCache(t *testing.T) {
	t.Parallel()
	c := certCluster("rke2", 1)
	c.Spec.Topology.Workers.MachineDeployments = append(c.Spec.Topology.Workers.MachineDeployments,
		clusterv1.MachineDeploymentTopology{Name: "md-1", Class: "workers"})
	f := newCertFixture(t, c,
		[]client.Object{certControlPlane("web-cp-a"), certWorker("web-md-0-a"), certMachine("web-md-1-a", clusterv1.ClusterTopologyMachineDeploymentNameLabel, "md-1")},
		report("web-md-0-a", 45, time.Minute), report("web-md-1-a", 46, time.Minute))
	f.staleCache()
	if cond := f.look(); cond == nil || cond.Reason != CertificatesRenewing {
		t.Fatalf("condition %+v", cond)
	}
	now := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), webKey, now); err != nil {
		t.Fatal(err)
	}
	for _, md := range now.Spec.Topology.Workers.MachineDeployments {
		if !md.Rollout.After.Equal(&metav1.Time{Time: certNow}) {
			t.Errorf("%s rolls after %v", md.Name, md.Rollout.After)
		}
	}
}

// Two RKE2 control-plane machines cannot be replaced one by one: the cluster says to restart
// rke2-server, as a warning.
func TestCertificatesNeedARestart(t *testing.T) {
	t.Parallel()
	f := newCertFixture(t, certCluster("rke2", 1), []client.Object{certControlPlane("web-cp-a")}, report("web-cp-a", 40, time.Minute))
	cond := f.look()
	if cond == nil || cond.Reason != CertificatesRestartNeeded || !strings.Contains(cond.Message, "restart rke2-server on web-cp-a") {
		t.Fatalf("condition %+v", cond)
	}
	if _, ok := f.machine("web-cp-a").Annotations[clusterv1.RemediateMachineAnnotation]; ok {
		t.Error("the only control-plane machine is replaced")
	}
	if got := f.drained(); len(got) != 1 || !strings.HasPrefix(got[0], "Warning "+CertificatesCondition) {
		t.Errorf("events %q", got)
	}
}

// A k3s control plane rolls through its own rolloutAfter, once.
func TestCertificatesRollK3s(t *testing.T) {
	t.Parallel()
	f := newCertFixture(t, certCluster("k3s", 1), []client.Object{certControlPlane("web-cp-a")}, report("web-cp-a", 40, time.Minute))
	if cond := f.look(); cond == nil || cond.Reason != CertificatesRenewing {
		t.Fatalf("condition %+v", cond)
	}
	cp := &unstructured.Unstructured{}
	cp.SetGroupVersionKind(k3sGVK)
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-cp"}, cp); err != nil {
		t.Fatal(err)
	}
	if after, _, _ := unstructured.NestedString(cp.Object, "spec", "rolloutAfter"); after != certNow.Format(time.RFC3339) {
		t.Errorf("rolloutAfter %q", after)
	}
	f.drained()
	f.look()
	if got := f.drained(); len(got) != 0 {
		t.Errorf("events %q on a second look", got)
	}
}

// kubeadm's control plane renews its own. If still due within 30 days, it failed, and the cluster
// says what to do. Once renewed, the condition goes.
func TestCertificatesNotRenewed(t *testing.T) {
	t.Parallel()
	m := certControlPlane("web-cp-a")
	m.Status.CertificatesExpiryDate = metav1.NewTime(certNow.AddDate(0, 0, 20))
	f := newCertFixture(t, certCluster("kubeadm", 1), []client.Object{m})
	cond := f.look()
	if cond == nil || cond.Reason != CertificatesNotRenewed || !strings.Contains(cond.Message, "kubectl --namespace team-a delete machine web-cp-a") {
		t.Fatalf("condition %+v", cond)
	}
	if got := f.drained(); len(got) != 1 || !strings.HasPrefix(got[0], "Warning "+CertificatesCondition) {
		t.Errorf("events %q", got)
	}

	m = f.machine("web-cp-a")
	m.Status.CertificatesExpiryDate = metav1.NewTime(certNow.AddDate(1, 0, 0))
	if err := f.kube.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if cond := f.look(); cond != nil {
		t.Fatalf("condition %+v after the renewal", cond)
	}
	if got := f.drained(); len(got) != 1 || !strings.HasPrefix(got[0], "Normal CertificatesRenewed") {
		t.Errorf("events %q", got)
	}
}

// The kubeconfig Cluster API keeps, renewed by the control-plane provider, is a warning when it is not.
func TestCertificatesOfTheKubeconfig(t *testing.T) {
	t.Parallel()
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: web, cluster: {server: "https://192.0.2.10:6443"}}]
users: [{name: admin, user: {client-certificate-data: %s, client-key-data: a2V5}}]
contexts: [{name: web, context: {cluster: web, user: admin}}]
current-context: web
`, base64PEM(t, certNow.AddDate(0, 0, 10)))
	secret := &corev1.Secret{Name: "web-kubeconfig", Namespace: "team-a", Data: map[string][]byte{"value": []byte(kubeconfig)}}
	f := newCertFixture(t, certCluster("talos", 1), []client.Object{secret})
	cond := f.look()
	if cond == nil || cond.Reason != CertificatesNotRenewed || !strings.Contains(cond.Message, "the kubeconfig web-kubeconfig on 2027-09-11") {
		t.Fatalf("condition %+v", cond)
	}
}

// Reports older than the monitor's repeats are of certificates renewed since.
func TestCertificatesOldReports(t *testing.T) {
	t.Parallel()
	f := newCertFixture(t, certCluster("rke2", 1), []client.Object{certControlPlane("web-cp-a")}, report("web-cp-a", 40, 3*time.Hour))
	if cond := f.look(); cond != nil {
		t.Fatalf("condition %+v", cond)
	}
}

func TestFirstDate(t *testing.T) {
	t.Parallel()
	at, ok := firstDate("server/serving-kube-apiserver.crt: certificate kube-apiserver expired at 2027-08-01T10:00:00Z\n" +
		"server/client-ca.crt: certificate k3s-client-ca@1 will expire within 365 days at 2027-07-01T00:00:00+02:00")
	if !ok || !at.Equal(time.Date(2027, 6, 30, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("first %v %v", at, ok)
	}
	if _, ok := firstDate("Node and Certificate Authority certificates managed by k3s are OK"); ok {
		t.Error("a date in a message without one")
	}
}

// base64PEM is a certificate running out at notAfter, PEM in base64.
func base64PEM(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "admin"}, NotBefore: notAfter.AddDate(-1, 0, 0), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
