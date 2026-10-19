package provisioner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeCloud stands in for Proxmox VE: it records a volume owner before "creating" it, and can be
// told to fail right after, as a real creation can.
type fakeCloud struct {
	failAfterRecord bool
	created         int
	removed         []Names
	removedHeld     []Credentials
}

func (f *fakeCloud) Ensure(_ context.Context, n Names, held Credentials, record func(Credentials) error) (Credentials, error) {
	c := held
	if c.VolumeOwner == 0 {
		c.VolumeOwner, c.VolumeOwnerNode = 4242, "pve"
		if err := record(c); err != nil {
			return c, err
		}
		f.created++
		if f.failAfterRecord {
			return c, errors.New("describing volume owner VM 4242: not authorized")
		}
	}
	if c.TokenSecret == "" {
		c.TokenSecret = "s3cret"
	}
	c.TokenID, c.Region = n.TokenID(), "lab"
	return c, nil
}

func (f *fakeCloud) Remove(_ context.Context, n Names, held Credentials) error {
	f.removed = append(f.removed, n)
	f.removedHeld = append(f.removedHeld, held)
	return nil
}

func newReconciler(t *testing.T, cloud Cloud, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme, addonsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	scheme.AddKnownTypeWithName(helmChartProxyGVK, &unstructured.Unstructured{})
	listGVK := helmChartProxyGVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	classes := []client.Object{
		&clusterv1.ClusterClass{Name: "kubehenge-rke2", Namespace: "kubehenge-system"},
		&clusterv1.ClusterClass{
			Name: "kubehenge-kubeadm", Namespace: "kubehenge-system",
			Annotations: map[string]string{CNIAnnotation: "cilium,calico", DefaultPodsAnnotation: "10.244.0.0/16"},
		},
		&clusterv1.ClusterClass{
			Name: "kubehenge-k3s", Namespace: "kubehenge-system",
			Annotations: map[string]string{CNIAnnotation: "cilium,calico", CNIForwardingAnnotation: "true", DefaultPodsAnnotation: "10.42.0.0/16"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(slices.Concat(classes, objs)...).Build()
	return &Reconciler{
		Client:         c,
		Cloud:          cloud,
		WorkloadPool:   "kubehenge",
		ClassNamespace: "kubehenge-system",
		Endpoint:       Endpoint{URL: "https://pve.example.org:8006", CA: "PEM"},
		Charts:         testCharts,
	}, c
}

func TestReconcileInstallsTheChosenCNIWhereTheDistributionHasNone(t *testing.T) {
	t.Parallel()
	c := cluster(
		clusterv1.ClusterVariable{Name: "storage", Value: apiextensionsv1.JSON{Raw: []byte(`"local-lvm"`)}},
		clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"cilium"`)}},
	)
	c.Spec.Topology.ClassRef.Name = "kubehenge-kubeadm"
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-cni"}, hcp); err != nil {
		t.Fatal(err)
	}
	if spec := lookup[map[string]any](t, hcp.Object, "spec"); spec["chartName"] != "cilium" {
		t.Errorf("CNI proxy %v", spec)
	}

	// RKE2 brings its own: the cni variable is RKE2's to act on.
	rke2 := storageCluster()
	rke2.Name = "other"
	rke2.Spec.Topology.Variables = append(rke2.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"cilium"`)}})
	if err := kube.Create(context.Background(), rke2); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, rke2); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "other-cni"}, hcp); err == nil {
		t.Error("a CNI proxy for a distribution that brings the chosen CNI itself")
	}

	// k3s brings flannel, and leaves the others to Kubehenge.
	for name, cni := range map[string]string{"flannel": "flannel", "calico": "calico"} {
		k3s := storageCluster()
		k3s.Name = name
		k3s.Spec.Topology.ClassRef.Name = "kubehenge-k3s"
		k3s.Spec.Topology.Variables = append(k3s.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"` + cni + `"`)}})
		if err := kube.Create(context.Background(), k3s); err != nil {
			t.Fatal(err)
		}
		if err := reconcile(t, r, k3s); err != nil {
			t.Fatal(err)
		}
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "flannel-cni"}, hcp); err == nil {
		t.Error("a CNI proxy for k3s's own flannel")
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "calico-cni"}, hcp); err != nil {
		t.Fatal(err)
	}
	resources := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "calico-calico"}, resources); err != nil {
		t.Fatal(err)
	}
	if m := string(resources.Data["calico.yaml"]); !strings.Contains(m, "10.42.0.0/16") || !strings.Contains(m, "containerIPForwarding: Enabled") {
		t.Errorf("Calico on k3s: %s", m)
	}
}

func TestReconcileUpdatesWhatItAppliedBefore(t *testing.T) {
	t.Parallel()
	c := cluster(
		clusterv1.ClusterVariable{Name: "storage", Value: apiextensionsv1.JSON{Raw: []byte(`"local-lvm"`)}},
		clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"calico"`)}},
	)
	c.Spec.Topology.ClassRef.Name = "kubehenge-kubeadm"
	stale := &corev1.Secret{
		Namespace: "team-a", Name: "web-proxmox",
		Type: addonsv1.ClusterResourceSetSecretType,
		Data: map[string][]byte{workloadSecret + ".yaml": []byte("an old token")},
	}
	staleSet := &addonsv1.ClusterResourceSet{
		Namespace: "team-a", Name: "web-proxmox",
		Spec: addonsv1.ClusterResourceSetSpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{clusterv1.ClusterNameLabel: "web"}},
			Resources:       []addonsv1.ResourceRef{{Kind: "Secret", Name: "web-proxmox"}},
		},
	}
	r, kube := newReconciler(t, &fakeCloud{}, c, stale, staleSet)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}

	got := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(stale), got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Data[workloadSecret+".yaml"]), "s3cret") {
		t.Errorf("the workload Secret kept %q", got.Data)
	}
	set := &addonsv1.ClusterResourceSet{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(staleSet), set); err != nil {
		t.Fatal(err)
	}
	if len(set.Spec.Resources) != 2 || set.Spec.Resources[1].Name != "web-calico" || set.Spec.Strategy != string(addonsv1.ClusterResourceSetStrategyReconcile) {
		t.Errorf("ClusterResourceSet %+v", set.Spec)
	}
	calico := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-calico"}, calico); err != nil {
		t.Fatal(err)
	}
	if m := string(calico.Data["calico.yaml"]); calico.Type != addonsv1.ClusterResourceSetSecretType || !strings.Contains(m, "kind: Installation") ||
		!strings.Contains(m, "10.244.0.0/16") || strings.Contains(m, "containerIPForwarding") {
		t.Errorf("Calico's resources %s: %s", calico.Type, m)
	}
}

func TestReconcileSwitchesHeadlampOnAndOff(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "headlamp", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)}})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	key := client.ObjectKey{Namespace: "team-a", Name: "web-headlamp"}
	if err := kube.Get(context.Background(), key, hcp); err != nil {
		t.Fatalf("Headlamp switched on: %v", err)
	}

	current := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Topology.Variables[len(current.Spec.Topology.Variables)-1].Value.Raw = []byte(`{"enabled":false}`)
	if err := kube.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, current); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), key, hcp); err == nil {
		t.Error("Headlamp's proxy is still there after switching it off")
	}
}

// Rancher's removal waits until Rancher lets go of its registered clusters, which lasts as long as
// Rancher's cluster cannot be reached; the cluster's other objects must not wait with it.
func TestReconcileAppliesTheRestWhileRancherIsBeingRemoved(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables,
		clusterv1.ClusterVariable{Name: "rancher", Value: apiextensionsv1.JSON{Raw: []byte(`{"install":false}`)}},
		clusterv1.ClusterVariable{Name: "headlamp", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)}},
	)
	c.Status.Conditions = []metav1.Condition{{
		Type: clusterv1.AvailableCondition, Status: metav1.ConditionTrue, Reason: "Available", LastTransitionTime: metav1.Now(),
	}}
	installed := &unstructured.Unstructured{}
	installed.SetGroupVersionKind(helmChartProxyGVK)
	installed.SetNamespace(c.Namespace)
	installed.SetName(rancherProxy(c))
	unreachable := &corev1.Secret{
		Namespace: c.Namespace, Name: c.Name + "-kubeconfig",
		Data: map[string][]byte{"value": []byte("not a kubeconfig")},
	}
	r, kube := newReconciler(t, &fakeCloud{}, c, installed, unreachable)
	r.Registrar = &Registrar{Client: kube, ClassNamespace: "kubehenge-system", Remotes: &Remotes{Reader: kube}}

	if err := reconcile(t, r, c); err == nil {
		t.Fatal("Rancher was removed without letting go of its registered clusters")
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(installed), installed.DeepCopy()); err != nil {
		t.Errorf("Rancher's proxy is gone while its clusters are still registered: %v", err)
	}
	creds := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-proxmox-token"}, creds); err != nil || !isReady(creds) {
		t.Errorf("the cluster was not marked ready while Rancher's removal waited: %v", err)
	}
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-headlamp"}, hcp); err != nil {
		t.Errorf("Headlamp was not switched on while Rancher's removal waited: %v", err)
	}
}

func storageCluster() *clusterv1.Cluster {
	return cluster(clusterv1.ClusterVariable{Name: "storage", Value: apiextensionsv1.JSON{Raw: []byte(`"local-lvm"`)}})
}

func reconcile(t *testing.T, r *Reconciler, c *clusterv1.Cluster) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
	return err
}

func TestReconcileProvisionsAndMarksReadyLast(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	cloud := &fakeCloud{}
	r, kube := newReconciler(t, cloud, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}

	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != Finalizer {
		t.Errorf("finalizers %v", got.Finalizers)
	}
	creds := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-proxmox-token"}, creds); err != nil {
		t.Fatal(err)
	}
	if !isReady(creds) || string(creds.Data["tokenSecret"]) != "s3cret" || string(creds.Data["volumeOwner"]) != "4242" {
		t.Errorf("credentials %v", creds.Data)
	}
	if len(creds.OwnerReferences) != 1 || creds.OwnerReferences[0].Name != "web" {
		t.Errorf("credentials not owned by the cluster: %v", creds.OwnerReferences)
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-proxmox"}, &addonsv1.ClusterResourceSet{}); err != nil {
		t.Errorf("ClusterResourceSet: %v", err)
	}
	for _, name := range []string{"web-proxmox-ccm", "web-proxmox-csi"} {
		hcp := &unstructured.Unstructured{}
		hcp.SetGroupVersionKind(helmChartProxyGVK)
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, hcp); err != nil {
			t.Errorf("HelmChartProxy %s: %v", name, err)
		}
	}

	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if cloud.created != 1 {
		t.Errorf("volume owner created %d times", cloud.created)
	}
}

func TestReconcileNeverCreatesASecondVolumeOwner(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	cloud := &fakeCloud{failAfterRecord: true}
	r, kube := newReconciler(t, cloud, c)
	if err := reconcile(t, r, c); err == nil {
		t.Fatal("no error from a failed creation")
	}
	creds := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-proxmox-token"}, creds); err != nil {
		t.Fatal(err)
	}
	if isReady(creds) || string(creds.Data["volumeOwner"]) != "4242" {
		t.Errorf("the chosen VMID must be kept, not ready: %v", creds.Data)
	}
	cloud.failAfterRecord = false
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if cloud.created != 1 {
		t.Errorf("volume owner created %d times", cloud.created)
	}
}

func TestReconcileRemovesOnlyAfterTheMachines(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Finalizers = []string{Finalizer}
	now := metav1.Now()
	c.DeletionTimestamp = &now
	machine := &clusterv1.Machine{
		Name: "web-cp", Namespace: "team-a", Labels: map[string]string{clusterv1.ClusterNameLabel: "web"},
	}
	cloud := &fakeCloud{}
	recorded := credentialsSecret(c, Credentials{VolumeOwner: 4242, VolumeOwnerNode: "pve"}, true)
	r, kube := newReconciler(t, cloud, c, machine, recorded)

	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if len(cloud.removed) != 0 {
		t.Fatal("removed Proxmox VE objects while a machine remains")
	}
	if err := kube.Delete(context.Background(), machine); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if len(cloud.removed) != 1 || cloud.removed[0].Pool != "kubehenge/team-a.web" {
		t.Errorf("removed %+v", cloud.removed)
	}
	// Proxmox VE may not list a VM made seconds ago in its pool; the recorded VMID finds it.
	if len(cloud.removedHeld) != 1 || cloud.removedHeld[0].VolumeOwner != 4242 {
		t.Errorf("removal was not given the recorded volume owner: %+v", cloud.removedHeld)
	}
	// The fake client deletes an object being deleted once its last finalizer goes.
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), &clusterv1.Cluster{}); err == nil {
		t.Error("the finalizer is still on the cluster")
	}
}
