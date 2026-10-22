package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// The chart's default autoscaler.namespace, which the integration tests install it with.
const autoscalerNamespace = "kubehenge-autoscaler"

var testAutoscalerImages = []string{
	"registry.k8s.io/autoscaling/cluster-autoscaler:v1.34.5@sha256:5c3a56440ffc76eac79e7d7e840153c1fbe7b6e02d415e76a45ba2e0ef99a266",
	"registry.k8s.io/autoscaling/cluster-autoscaler:v1.36.1@sha256:461388d03eb5b55db58941ee7a81d9a619dd15302b01cbab9f5a9946715994b4",
	"registry.k8s.io/autoscaling/cluster-autoscaler:v1.35.2@sha256:aac369dc283927a623deb1af54696efcc722ae79255aa07788422e495bab887d",
}

func TestAutoscalerImageFollowsTheClusterMinor(t *testing.T) {
	t.Parallel()
	images, err := ParseAutoscalerImages(testAutoscalerImages)
	if err != nil {
		t.Fatal(err)
	}
	for version, want := range map[string]string{
		"v1.35.4+k3s1":   "v1.35.2",
		"v1.36.5":        "v1.36.1",
		"v1.37.1+rke2r1": "v1.36.1",
		"v1.33.9":        "v1.34.5",
	} {
		if got := images.For(version); !strings.Contains(got, ":"+want+"@") {
			t.Errorf("Kubernetes %s runs %s, want %s", version, got, want)
		}
	}
	for _, bad := range [][]string{
		{"registry.k8s.io/autoscaling/cluster-autoscaler:v1.36.1"},
		{testAutoscalerImages[1], strings.Replace(testAutoscalerImages[1], "v1.36.1", "v1.36.0", 1)},
		nil,
	} {
		if _, err := ParseAutoscalerImages(bad); err == nil {
			t.Errorf("%v read", bad)
		}
	}
}

func jsonValue(raw string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(raw)} }

// autoscalerFixture is an AutoscalerReconciler, and the WorkloadIdentities that keeps the
// autoscaler identity's token, with the management and workload clusters they act on, and what
// issues their tokens.
type autoscalerFixture struct {
	a        *AutoscalerReconciler
	w        *WorkloadIdentities
	kube     client.Client
	workload client.Client
	issuer   *tokenIssuer
	events   *events.FakeRecorder
}

// reconcile has both reconcilers look at the cluster.
func (f *autoscalerFixture) reconcile(t *testing.T, key client.ObjectKey) ctrl.Result {
	t.Helper()
	result := reconcileAutoscaler(t, f.a, key)
	if _, err := f.w.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	return result
}

func newAutoscaler(t *testing.T, c *clusterv1.Cluster, clock *time.Time, objs ...client.Object) autoscalerFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme, authenticationv1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	scheme.AddKnownTypeWithName(proxmoxMachineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(proxmoxMachineGVK.GroupVersion().WithKind("ProxmoxMachineList"), &unstructured.UnstructuredList{})
	class := &clusterv1.ClusterClass{
		Name: "kubehenge-rke2", Namespace: "kubehenge-system",
		Spec: clusterv1.ClusterClassSpec{Variables: []clusterv1.ClusterClassVariable{machineVariable("workerMachine")}},
	}
	issuer := &tokenIssuer{now: func() time.Time { return *clock }}
	kube := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, class, c)...).Build(),
		interceptor.Funcs{SubResourceCreate: issuer.create})
	inside := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(&corev1.Namespace{Name: metav1.NamespaceSystem}).Build()
	workload := interceptor.NewClient(inside, interceptor.Funcs{SubResourceCreate: issuer.create})
	images, err := ParseAutoscalerImages(testAutoscalerImages)
	if err != nil {
		t.Fatal(err)
	}
	recorder := events.NewFakeRecorder(10)
	config := &rest.Config{Host: "https://10.0.0.20:6443", CAData: []byte("the cluster CA")}
	remotes := &Remotes{byCluster: map[string]remote{remoteKey(c): {config: config, client: workload, until: clock.Add(24 * time.Hour)}}}
	a := &AutoscalerReconciler{
		Client: kube, ClassNamespace: "kubehenge-system", Namespace: autoscalerNamespace, Images: images,
		Remotes: remotes,
		Events:  recorder,
		now:     func() time.Time { return *clock },
	}
	w := &WorkloadIdentities{
		Client: kube, ClassNamespace: "kubehenge-system", MountNamespace: autoscalerNamespace, Remotes: remotes,
		now: func() time.Time { return *clock },
	}
	return autoscalerFixture{a: a, w: w, kube: kube, workload: workload, issuer: issuer, events: recorder}
}

// autoscaledCluster is a cluster whose pool md-0 the autoscaler scales from none to two, with the
// MachineDeployment Cluster API made for it.
func autoscaledCluster(variables ...clusterv1.ClusterVariable) (*clusterv1.Cluster, *clusterv1.MachineDeployment) {
	c := initialised(cluster(variables...))
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("0", "2")}
	md := &clusterv1.MachineDeployment{
		Namespace: c.Namespace, Name: "web-md-0-x7k2p",
		Labels: map[string]string{clusterv1.ClusterNameLabel: c.Name, clusterv1.ClusterTopologyMachineDeploymentNameLabel: "md-0"},
		Spec:   clusterv1.MachineDeploymentSpec{ClusterName: c.Name, Replicas: new(int32(0))},
	}
	return c, md
}

func reconcileAutoscaler(t *testing.T, a *AutoscalerReconciler, key client.ObjectKey) ctrl.Result {
	t.Helper()
	result, err := a.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// present tells whether each object exists, failing the test on any other error.
func present(t *testing.T, c client.Client, objects ...client.Object) []bool {
	t.Helper()
	found := make([]bool, len(objects))
	for i, o := range objects {
		err := c.Get(context.Background(), client.ObjectKeyFromObject(o), o)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		found[i] = err == nil
	}
	return found
}

func policyEntries(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: autoscalerNamespace, Name: AutoscalerPolicy}, cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return cm.Data
}

func TestAutoscalerRunsBesideItsCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c, md := autoscaledCluster(clusterv1.ClusterVariable{Name: AutoscalerVariable, Value: jsonValue(`{"scaleDownUnneededMinutes":1}`)})
	f := newAutoscaler(t, c, &clock, md)
	key := client.ObjectKeyFromObject(c)
	name := autoscalerName(key)

	if got := f.reconcile(t, key); got.RequeueAfter != 0 {
		t.Errorf("requeued after %v, want once something changes: the identity's token is renewed beside it", got.RequeueAfter)
	}
	if got := policyEntries(t, f.kube)[name]; got != "team-a/web/web-md-0-x7k2p" {
		t.Errorf("the policy holds the autoscaler to %q", got)
	}
	checkAutoscalerBeside(t, f, c, md)

	// Before its renewal is due, the token stays.
	clock = clock.Add(5 * time.Minute)
	f.reconcile(t, key)
	if got := f.issuer.tokensOf(autoscalerAccount); got != 1 {
		t.Errorf("%d tokens of the autoscaler identity after 5 minutes, want the first alone", got)
	}

	// Autoscaling off: nothing of the autoscaler stays, in either cluster.
	off := &clusterv1.Cluster{}
	if err := f.kube.Get(ctx, key, off); err != nil {
		t.Fatal(err)
	}
	off.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
	if err := f.kube.Update(ctx, off); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, key)
	checkAutoscalerGone(t, f, key, md)
}

// checkAutoscalerBeside checks what runs a cluster's autoscaler beside it in the management
// cluster, its account in the cluster and its pool's capacity.
func checkAutoscalerBeside(t *testing.T, f autoscalerFixture, c *clusterv1.Cluster, md *clusterv1.MachineDeployment) {
	t.Helper()
	ctx := context.Background()
	name := autoscalerName(client.ObjectKeyFromObject(c))
	binding := &rbacv1.RoleBinding{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "kubehenge-autoscaler-web"}, binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Name != AutoscalerRole || len(binding.Subjects) != 1 || binding.Subjects[0].Name != name || binding.Subjects[0].Namespace != autoscalerNamespace {
		t.Errorf("the autoscaler is bound as %+v to %+v", binding.Subjects, binding.RoleRef)
	}
	if len(binding.OwnerReferences) != 1 || binding.OwnerReferences[0].Name != c.Name {
		t.Errorf("the binding is owned by %v, want the Cluster", binding.OwnerReferences)
	}
	secret := checkMountedToken(t, f.kube, c)
	dep := &appsv1.Deployment{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: autoscalerNamespace, Name: name}, dep); err != nil {
		t.Fatal(err)
	}
	container := dep.Spec.Template.Spec.Containers[0]
	for _, arg := range []string{
		"--node-group-auto-discovery=clusterapi:namespace=team-a,clusterName=web", "--kubeconfig=/etc/kubehenge/workload/kubeconfig",
		"--clusterapi-cloud-config-authoritative", "--scale-down-unneeded-time=1m", "--scale-down-delay-after-add=10m", "--expander=least-waste",
	} {
		if !slices.Contains(container.Args, arg) {
			t.Errorf("the autoscaler runs without %s: %v", arg, container.Args)
		}
	}
	if !strings.Contains(container.Image, ":v1.36.1@") || dep.Spec.Template.Spec.ServiceAccountName != name {
		t.Errorf("the autoscaler of a 1.37 cluster runs %s as %s", container.Image, dep.Spec.Template.Spec.ServiceAccountName)
	}
	if v := dep.Spec.Template.Spec.Volumes; len(v) != 1 || v[0].Secret == nil || v[0].Secret.SecretName != secret {
		t.Errorf("the autoscaler mounts %+v, want the autoscaler identity's Secret", v)
	}
	role := &rbacv1.ClusterRole{Name: autoscalerAccount}
	found := present(t, f.workload, role, &corev1.ServiceAccount{Namespace: "kube-system", Name: autoscalerAccount},
		&rbacv1.Role{Namespace: "kube-system", Name: autoscalerAccount})
	if slices.Contains(found, false) {
		t.Errorf("the autoscaler's account in the cluster: %v", found)
	}
	for _, r := range role.Rules {
		if slices.Contains(r.Resources, "secrets") || slices.ContainsFunc(r.Verbs, func(v string) bool { return v == "delete" || v == "*" }) {
			t.Errorf("the autoscaler may %v %v in its cluster", r.Verbs, r.Resources)
		}
	}
	got := &clusterv1.MachineDeployment{}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(md), got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[capacityPrefix+"cpu"] != "2" || got.Annotations[capacityPrefix+"memory"] != "3686Mi" || got.Annotations[capacityPrefix+"ephemeral-disk"] != "27648Mi" {
		t.Errorf("the pool's capacity: %v", got.Annotations)
	}
}

// checkMountedToken checks the autoscaler identity's Secret beside the autoscalers, with the
// kubeconfig that reads the files beside it, and returns its name.
func checkMountedToken(t *testing.T, kube client.Client, c *clusterv1.Cluster) string {
	t.Helper()
	name := mountedSecretName(client.ObjectKeyFromObject(c), IdentityAutoscaler)
	s := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: autoscalerNamespace, Name: name}, s); err != nil {
		t.Fatal(err)
	}
	kubeconfig := string(s.Data[IdentityKubeconfigKey])
	for _, want := range []string{"server: https://10.0.0.20:6443", "certificate-authority: ca.crt", "tokenFile: token"} {
		if !strings.Contains(kubeconfig, want) {
			t.Errorf("the autoscaler identity's kubeconfig lacks %q: %s", want, kubeconfig)
		}
	}
	if string(s.Data[IdentityCAKey]) != "the cluster CA" || len(s.Data[IdentityTokenKey]) == 0 || s.Labels[ClusterNameLabel] != c.Name || s.Labels[ClusterNamespaceLabel] != c.Namespace {
		t.Errorf("the autoscaler identity's Secret holds %q, labelled %v", s.Data, s.Labels)
	}
	return name
}

// checkAutoscalerGone checks that nothing of a cluster's autoscaler is left in either cluster, nor
// its pool's capacity.
func checkAutoscalerGone(t *testing.T, f autoscalerFixture, key client.ObjectKey, md *clusterv1.MachineDeployment) {
	t.Helper()
	name := autoscalerName(key)
	found := present(t, f.kube,
		&appsv1.Deployment{Namespace: autoscalerNamespace, Name: name},
		&corev1.Secret{Namespace: autoscalerNamespace, Name: mountedSecretName(key, IdentityAutoscaler)},
		&corev1.ServiceAccount{Namespace: autoscalerNamespace, Name: name}, &rbacv1.RoleBinding{Namespace: "team-a", Name: "kubehenge-autoscaler-web"},
	)
	if slices.Contains(found, true) {
		t.Errorf("left in the management cluster: %v", found)
	}
	if _, ok := policyEntries(t, f.kube)[name]; ok {
		t.Error("the policy still names the autoscaler")
	}
	found = present(t, f.workload, &rbacv1.ClusterRole{Name: autoscalerAccount}, &rbacv1.ClusterRoleBinding{Name: autoscalerAccount},
		&corev1.ServiceAccount{Namespace: "kube-system", Name: autoscalerAccount}, &rbacv1.Role{Namespace: "kube-system", Name: autoscalerAccount},
	)
	if slices.Contains(found, true) {
		t.Errorf("left in the cluster: %v", found)
	}
	got := &clusterv1.MachineDeployment{}
	if err := f.kube.Get(context.Background(), client.ObjectKeyFromObject(md), got); err != nil {
		t.Fatal(err)
	}
	for k := range got.Annotations {
		if strings.HasPrefix(k, capacityPrefix) {
			t.Errorf("a fixed pool keeps %s", k)
		}
	}
}

func TestAutoscalerInsideItsCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c, md := autoscaledCluster(clusterv1.ClusterVariable{Name: AutoscalerVariable, Value: jsonValue(`{"placement":"workload"}`)})
	f := newAutoscaler(t, c, &clock, md)
	f.a.Management = ManagementAPI{Server: "https://192.0.2.10:6443", CA: []byte("the management CA")}
	key := client.ObjectKeyFromObject(c)
	name := autoscalerName(key)

	if got := f.reconcile(t, key); got.RequeueAfter != tokenRenewal {
		t.Errorf("requeued after %v, want the management token's renewal", got.RequeueAfter)
	}
	s := &corev1.Secret{}
	if err := f.workload.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: autoscalerManagementSecret}, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(s.Data[IdentityKubeconfigKey]), "server: https://192.0.2.10:6443") || string(s.Data[IdentityCAKey]) != "the management CA" || len(s.Data[IdentityTokenKey]) == 0 {
		t.Errorf("the management cluster's Secret in the cluster holds %q", s.Data)
	}
	if f.issuer.tokensOf(name) != 1 || f.issuer.tokensOf(autoscalerAccount) != 0 {
		t.Errorf("tokens of %v, want one of the autoscaler's management account and none of its identity", f.issuer.issued)
	}
	if found := present(t, f.kube, &corev1.Secret{Namespace: autoscalerNamespace, Name: mountedSecretName(key, IdentityAutoscaler)}); found[0] {
		t.Error("the autoscaler identity's token beside the cluster while the autoscaler runs inside it")
	}
	clusterRights(t, f.workload, autoscalerAccount)
	if got := namespacedRights(t, f.workload, "kube-system", autoscalerAccount); !apiequality.Semantic.DeepEqual(got, autoscalerStatusRules) {
		t.Errorf("the autoscaler inside may do %+v in kube-system", got)
	}
	clock = clock.Add(5 * time.Minute)
	if got := f.reconcile(t, key); got.RequeueAfter != 15*time.Minute || f.issuer.tokensOf(name) != 1 {
		t.Errorf("after 5 minutes: requeued after %v with %d management tokens, want 15m and the first alone", got.RequeueAfter, f.issuer.tokensOf(name))
	}
	dep := &appsv1.Deployment{}
	if err := f.workload.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: autoscalerAccount}, dep); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dep.Spec.Template.Spec.Containers[0].Args, "--cloud-config=/etc/kubehenge/management/kubeconfig") || dep.Spec.Template.Spec.Affinity == nil {
		t.Errorf("the autoscaler inside the cluster: %+v", dep.Spec.Template.Spec)
	}
	if found := present(t, f.kube, &appsv1.Deployment{Namespace: autoscalerNamespace, Name: name}); found[0] {
		t.Error("an autoscaler in the management cluster too")
	}

	// Moved back, it leaves the cluster.
	back := &clusterv1.Cluster{}
	if err := f.kube.Get(ctx, key, back); err != nil {
		t.Fatal(err)
	}
	back.Spec.Topology.Variables = nil
	if err := f.kube.Update(ctx, back); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, key)
	if found := present(t, f.workload, &appsv1.Deployment{Namespace: "kube-system", Name: autoscalerAccount}, &corev1.Secret{Namespace: "kube-system", Name: autoscalerManagementSecret}); slices.Contains(found, true) {
		t.Errorf("left inside the cluster: %v", found)
	}
	if found := present(t, f.kube, &appsv1.Deployment{Namespace: autoscalerNamespace, Name: name}, &corev1.Secret{Namespace: autoscalerNamespace, Name: mountedSecretName(key, IdentityAutoscaler)}); slices.Contains(found, false) {
		t.Errorf("the autoscaler in the management cluster and its identity's token: %v", found)
	}
}

func TestAutoscalerInsideNeedsTheManagementEndpoint(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c, md := autoscaledCluster(clusterv1.ClusterVariable{Name: AutoscalerVariable, Value: jsonValue(`{"placement":"workload"}`)})
	f := newAutoscaler(t, c, &clock, md)
	reconcileAutoscaler(t, f.a, client.ObjectKeyFromObject(c))
	select {
	case e := <-f.events.Events:
		if !strings.Contains(e, "AutoscalerNotPlaced") {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("no event says why the cluster has no autoscaler")
	}
	if found := present(t, f.workload, &appsv1.Deployment{Namespace: "kube-system", Name: autoscalerAccount}); found[0] {
		t.Error("an autoscaler without a way into the management cluster")
	}
}

func TestAutoscalerGoesWithItsCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c, md := autoscaledCluster()
	f := newAutoscaler(t, c, &clock, md)
	key := client.ObjectKeyFromObject(c)
	name := autoscalerName(key)
	secret := &corev1.Secret{Namespace: autoscalerNamespace, Name: mountedSecretName(key, IdentityAutoscaler)}
	f.reconcile(t, key)
	if found := present(t, f.kube, secret); !found[0] {
		t.Fatal("no token of the autoscaler identity")
	}
	if err := f.kube.Delete(ctx, &clusterv1.Cluster{Namespace: c.Namespace, Name: c.Name}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, key)
	found := present(t, f.kube,
		&appsv1.Deployment{Namespace: autoscalerNamespace, Name: name}, secret,
		&corev1.ServiceAccount{Namespace: autoscalerNamespace, Name: name},
	)
	if slices.Contains(found, true) {
		t.Errorf("left after the cluster went: %v", found)
	}
	if _, ok := policyEntries(t, f.kube)[name]; ok {
		t.Error("the policy still names the autoscaler")
	}
}

// A cluster deleted while the controller is down is held by the autoscaler's finalizer until the
// controller, back, removes what its autoscaler had beside it, though the identities' reconciler
// never sees the deletion.
func TestAutoscalerFinalizerOutlivesAControllerDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c, md := autoscaledCluster()
	f := newAutoscaler(t, c, &clock, md)
	key := client.ObjectKeyFromObject(c)
	name := autoscalerName(key)
	secret := &corev1.Secret{Namespace: autoscalerNamespace, Name: mountedSecretName(key, IdentityAutoscaler)}
	f.reconcile(t, key)
	held := &clusterv1.Cluster{}
	if err := f.kube.Get(ctx, key, held); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(held, AutoscalerFinalizer) {
		t.Fatalf("the cluster's finalizers are %v, want %s", held.Finalizers, AutoscalerFinalizer)
	}

	if err := f.kube.Delete(ctx, held); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Get(ctx, key, held); err != nil {
		t.Fatalf("the cluster went with its autoscaler's objects still there: %v", err)
	}
	reconcileAutoscaler(t, f.a, key)
	found := present(t, f.kube,
		&appsv1.Deployment{Namespace: autoscalerNamespace, Name: name}, secret,
		&corev1.ServiceAccount{Namespace: autoscalerNamespace, Name: name},
	)
	if slices.Contains(found, true) {
		t.Errorf("left after the cluster went: %v", found)
	}
	if _, ok := policyEntries(t, f.kube)[name]; ok {
		t.Error("the policy still names the autoscaler")
	}
	if err := f.kube.Get(ctx, key, held); !apierrors.IsNotFound(err) {
		t.Errorf("the cluster is still held, finalizers %v: %v", held.Finalizers, err)
	}
}

// Switched off, the cluster no longer carries the finalizer, so nothing holds its deletion.
func TestAutoscalerSwitchedOffReleasesTheCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c, md := autoscaledCluster()
	f := newAutoscaler(t, c, &clock, md)
	key := client.ObjectKeyFromObject(c)
	f.reconcile(t, key)
	off := &clusterv1.Cluster{}
	if err := f.kube.Get(ctx, key, off); err != nil {
		t.Fatal(err)
	}
	off.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
	if err := f.kube.Update(ctx, off); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, key)
	if err := f.kube.Get(ctx, key, off); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(off, AutoscalerFinalizer) {
		t.Errorf("a cluster without an autoscaled pool keeps %s", AutoscalerFinalizer)
	}
	checkAutoscalerGone(t, f, key, md)
}

func TestAutoscalerReportsAWorkerProxmoxHasNoRoomFor(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c, md := autoscaledCluster()
	md.Spec.Replicas = new(int32(1))
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	pm.SetNamespace(c.Namespace)
	pm.SetName("web-md-0-x7k2p-abcde")
	pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: c.Name, clusterv1.ClusterTopologyMachineDeploymentNameLabel: "md-0"})
	conditions := []any{map[string]any{
		"type": "VirtualMachineProvisioned", "status": "False", "reason": "VMProvisionFailed",
		"message": "cannot reserve 4294967296B of memory on node pve1: 1073741824B available memory left",
	}}
	if err := unstructured.SetNestedSlice(pm.Object, conditions, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	f := newAutoscaler(t, c, &clock, md, pm)
	if got := reconcileAutoscaler(t, f.a, client.ObjectKeyFromObject(c)); got.RequeueAfter != scalingLook {
		t.Errorf("a growing pool looked at again after %v, want %v", got.RequeueAfter, scalingLook)
	}
	select {
	case e := <-f.events.Events:
		if !strings.Contains(e, "AutoscalerNoRoom") || !strings.Contains(e, "pool md-0") || !strings.Contains(e, "on node pve1") {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("no event says Proxmox VE has no room")
	}
}
