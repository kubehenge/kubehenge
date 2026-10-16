package provisioner

import (
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

var (
	rke2ControlPlaneTemplate = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta2", Kind: "RKE2ControlPlaneTemplate"}
	rke2ConfigTemplate       = schema.GroupVersionKind{Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta2", Kind: "RKE2ConfigTemplate"}
)

func rke2Templates(controlPlane string) string {
	return `apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: RKE2ControlPlaneTemplate
metadata:
  name: ` + controlPlane + `
  namespace: kubehenge-system
spec:
  template:
    spec: {}
---
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: RKE2ConfigTemplate
metadata:
  name: kubehenge-rke2-worker-1
  namespace: kubehenge-system
spec:
  template:
    spec: {}
`
}

func providerDeployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		Namespace: ns, Name: name, Labels: map[string]string{providerLabel: "x"},
		Spec:   appsv1.DeploymentSpec{Replicas: new(int32(1))},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
}

func newDistributions(t *testing.T, objs ...client.Object) (*Distributions, client.WithWatch) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	for _, gvk := range []schema.GroupVersionKind{rke2ControlPlaneTemplate, rke2ConfigTemplate} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		list := gvk
		list.Kind += "List"
		scheme.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
		metav1.AddToGroupVersion(scheme, gvk.GroupVersion())
	}
	base := []client.Object{
		&clusterv1.ClusterClass{Namespace: "kubehenge-system", Name: "kubehenge-rke2"},
		&corev1.ConfigMap{
			Namespace: "kubehenge-system", Name: "kubehenge-rke2", Labels: map[string]string{ClassLabel: "kubehenge-rke2"},
			Data: map[string]string{templatesKey: rke2Templates("kubehenge-rke2-control-plane-1")},
		},
		providerDeployment("rke2-bootstrap-system", "rke2-bootstrap-controller-manager"),
		providerDeployment("rke2-control-plane-system", "rke2-control-plane-controller-manager"),
		// Another distribution's, left to its own class.
		providerDeployment("k3s-bootstrap-system", "capi-k3s-bootstrap-controller-manager"),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(slices.Concat(base, objs)...).Build()
	return &Distributions{
		Client:         c,
		ClassNamespace: "kubehenge-system",
		Providers: map[string][]string{
			"kubehenge-rke2": {"rke2-bootstrap-system", "rke2-control-plane-system"},
			"kubehenge-k3s":  {"k3s-bootstrap-system", "k3s-control-plane-system"},
		},
	}, c
}

func reconcileRKE2(t *testing.T, d *Distributions) {
	t.Helper()
	req := ctrl.Request{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}
	if _, err := d.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func replicas(t *testing.T, c client.Client, ns, name string) int32 {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, dep); err != nil {
		t.Fatal(err)
	}
	return *dep.Spec.Replicas
}

func templateNames(t *testing.T, c client.Client, gvk schema.GroupVersionKind) []string {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.List(context.Background(), list, client.InNamespace("kubehenge-system")); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list.Items))
	for _, u := range list.Items {
		if u.GetLabels()[ClassLabel] != "kubehenge-rke2" || u.GetLabels()[TemplatesAnnotation] == "" {
			t.Errorf("%s %s is not labelled for its class and version: %v", gvk.Kind, u.GetName(), u.GetLabels())
		}
		names = append(names, u.GetName())
	}
	slices.Sort(names)
	return names
}

func classMark(t *testing.T, c client.Client) string {
	t.Helper()
	cc := &clusterv1.ClusterClass{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, cc); err != nil {
		t.Fatal(err)
	}
	return cc.Annotations[TemplatesAnnotation]
}

func TestDistributionRunsOnlyWhileAClusterUsesIt(t *testing.T) {
	t.Parallel()
	d, c := newDistributions(t)
	ctx := context.Background()
	const bootstrap, controlPlane = "rke2-bootstrap-system", "rke2-control-plane-system"

	reconcileRKE2(t, d)
	if replicas(t, c, bootstrap, "rke2-bootstrap-controller-manager") != 0 || replicas(t, c, controlPlane, "rke2-control-plane-controller-manager") != 0 {
		t.Fatal("the providers of an unused distribution still run")
	}
	if replicas(t, c, "k3s-bootstrap-system", "capi-k3s-bootstrap-controller-manager") != 1 {
		t.Fatal("another class's providers were scaled")
	}
	if names := templateNames(t, c, rke2ControlPlaneTemplate); len(names) != 0 || classMark(t, c) != "" {
		t.Fatalf("templates of an unused distribution: %v, class marked %q", names, classMark(t, c))
	}

	// The first cluster: the providers come up, and until they serve no template is applied.
	cluster := &clusterv1.Cluster{
		Namespace: "clusters", Name: "a",
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-rke2", Namespace: "kubehenge-system"}}},
	}
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	starting := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: controlPlane, Name: "rke2-control-plane-controller-manager"}, starting); err != nil {
		t.Fatal(err)
	}
	starting.Status.AvailableReplicas = 0
	if err := c.Status().Update(ctx, starting); err != nil {
		t.Fatal(err)
	}
	reconcileRKE2(t, d)
	if replicas(t, c, bootstrap, "rke2-bootstrap-controller-manager") != 1 || replicas(t, c, controlPlane, "rke2-control-plane-controller-manager") != 1 {
		t.Fatal("the providers were not started for the first cluster")
	}
	if names := templateNames(t, c, rke2ControlPlaneTemplate); len(names) != 0 {
		t.Fatalf("templates applied before their webhooks serve: %v", names)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(starting), starting); err != nil {
		t.Fatal(err)
	}
	starting.Status.AvailableReplicas = 1
	if err := c.Status().Update(ctx, starting); err != nil {
		t.Fatal(err)
	}
	reconcileRKE2(t, d)
	if got := templateNames(t, c, rke2ControlPlaneTemplate); !slices.Equal(got, []string{"kubehenge-rke2-control-plane-1"}) {
		t.Fatalf("control-plane templates = %v", got)
	}
	if got := templateNames(t, c, rke2ConfigTemplate); !slices.Equal(got, []string{"kubehenge-rke2-worker-1"}) {
		t.Fatalf("bootstrap templates = %v", got)
	}
	first := classMark(t, c)
	if first == "" {
		t.Fatal("the class was not touched, so Cluster API would not look at it again")
	}

	// A new chart version renames a changed template: the old one goes, the class is touched again.
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data[templatesKey] = rke2Templates("kubehenge-rke2-control-plane-2")
	if err := c.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	reconcileRKE2(t, d)
	if got := templateNames(t, c, rke2ControlPlaneTemplate); !slices.Equal(got, []string{"kubehenge-rke2-control-plane-2"}) {
		t.Fatalf("control-plane templates after the upgrade = %v", got)
	}
	if classMark(t, c) == first {
		t.Fatal("the class was not touched for the new templates")
	}

	// The last cluster gone: templates first, then the providers.
	if err := c.Delete(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	reconcileRKE2(t, d)
	if names := append(templateNames(t, c, rke2ControlPlaneTemplate), templateNames(t, c, rke2ConfigTemplate)...); len(names) != 0 {
		t.Fatalf("templates left after the last cluster: %v", names)
	}
	if classMark(t, c) != "" {
		t.Fatal("the class still claims templates")
	}
	if replicas(t, c, bootstrap, "rke2-bootstrap-controller-manager") != 0 || replicas(t, c, controlPlane, "rke2-control-plane-controller-manager") != 0 {
		t.Fatal("the providers still run after the last cluster")
	}
}

func TestDistributionInUseByAControlPlaneWithoutTheClass(t *testing.T) {
	t.Parallel()
	d, c := newDistributions(t, &clusterv1.Cluster{
		Namespace: "elsewhere", Name: "b",
		Spec: clusterv1.ClusterSpec{ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
			APIGroup: "controlplane.cluster.x-k8s.io", Kind: "RKE2ControlPlane", Name: "b",
		}},
	})
	reconcileRKE2(t, d)
	if replicas(t, c, "rke2-control-plane-system", "rke2-control-plane-controller-manager") != 1 {
		t.Fatal("the providers of a cluster built without the class were stopped")
	}
}

func TestDistributionScalesPastAStaleCache(t *testing.T) {
	t.Parallel()
	d, c := newDistributions(t)
	ctx := context.Background()
	stale := &appsv1.DeploymentList{}
	if err := c.List(ctx, stale, client.InNamespace("rke2-control-plane-system")); err != nil {
		t.Fatal(err)
	}
	reconcileRKE2(t, d)

	writes := 0
	d.Client = interceptor.NewClient(c, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	if err := d.scale(ctx, stale.Items, 0); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("%d scale writes for a provider already at none", writes)
	}
	fresh := &appsv1.DeploymentList{}
	if err := c.List(ctx, fresh, client.InNamespace("rke2-control-plane-system")); err != nil {
		t.Fatal(err)
	}
	if err := d.scale(ctx, fresh.Items, 1); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || replicas(t, c, "rke2-control-plane-system", "rke2-control-plane-controller-manager") != 1 {
		t.Fatalf("%d scale writes, the provider not started", writes)
	}
}

func TestDistributionWakesOnlyForUse(t *testing.T) {
	t.Parallel()
	was := &clusterv1.Cluster{Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-rke2"}}}}
	status := was.DeepCopy()
	status.Status.Conditions = []metav1.Condition{{Type: clusterv1.AvailableCondition, Status: metav1.ConditionTrue}}
	if usageChanged.Update(event.UpdateEvent{ObjectOld: was, ObjectNew: status}) {
		t.Error("a status change woke the distributions")
	}
	moved := was.DeepCopy()
	moved.Spec.Topology.ClassRef.Name = "kubehenge-k3s"
	if !usageChanged.Update(event.UpdateEvent{ObjectOld: was, ObjectNew: moved}) {
		t.Error("a cluster moved to another class passed unseen")
	}
}

func TestParseClassProviders(t *testing.T) {
	t.Parallel()
	got, err := ParseClassProviders([]string{"kubehenge-rke2=rke2-bootstrap-system,rke2-control-plane-system", "kubehenge-k3s=k3s"})
	if err != nil || !slices.Equal(got["kubehenge-rke2"], []string{"rke2-bootstrap-system", "rke2-control-plane-system"}) || !slices.Equal(got["kubehenge-k3s"], []string{"k3s"}) {
		t.Fatalf("%v, %v", got, err)
	}
	for _, bad := range [][]string{{"kubehenge-rke2"}, {"=ns"}, {"kubehenge-rke2=a,,b"}, {"a=x", "a=y"}} {
		if _, err := ParseClassProviders(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
