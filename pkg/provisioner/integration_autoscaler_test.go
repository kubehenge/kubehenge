//go:build integration

package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func poolDeployment(cluster, name string) *clusterv1.MachineDeployment {
	labels := map[string]string{clusterv1.ClusterNameLabel: cluster}
	return &clusterv1.MachineDeployment{
		Namespace: interfaceNamespace, Name: name, Labels: labels,
		Spec: clusterv1.MachineDeploymentSpec{
			ClusterName: cluster,
			Replicas:    new(int32(1)),
			Selector:    metav1.LabelSelector{MatchLabels: labels},
			Template: clusterv1.MachineTemplateSpec{
				ObjectMeta: clusterv1.ObjectMeta{Labels: labels},
				Spec: clusterv1.MachineSpec{
					ClusterName: cluster,
					Bootstrap:   clusterv1.Bootstrap{DataSecretName: new(name + "-bootstrap")},
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "ProxmoxMachineTemplate", Name: name,
					},
				},
			},
		},
	}
}

func poolMachine(cluster, name string) *clusterv1.Machine {
	return &clusterv1.Machine{
		Namespace: interfaceNamespace, Name: name, Labels: map[string]string{clusterv1.ClusterNameLabel: cluster},
		Spec: clusterv1.MachineSpec{
			ClusterName: cluster,
			Bootstrap:   clusterv1.Bootstrap{DataSecretName: new(name + "-bootstrap")},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "ProxmoxMachine", Name: name,
			},
		},
	}
}

// TestIntegrationAutoscalerPolicy holds each cluster's autoscaler to its own cluster's pools and
// machines, though the role it is bound to covers the namespace every interface cluster shares: the
// autoscaler of prod reaches neither the pool of prod-md, whose MachineDeployment's name starts like
// its own, nor that cluster's machines.
func TestIntegrationAutoscalerPolicy(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	cfg, _ := startAPIConfig(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme, autoscalingv1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	a := &AutoscalerReconciler{Client: admin, Namespace: autoscalerNamespace}
	prod, other := poolDeployment("prod", "prod-md-0-aaaaa"), poolDeployment("prod-md", "prod-md-0-bbbbb")
	prodMachine, otherMachine := poolMachine("prod", "prod-md-0-aaaaa-11111"), poolMachine("prod-md", "prod-md-0-bbbbb-22222")
	for _, o := range []client.Object{prod, other, prodMachine, otherMachine} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	accounts := map[string]string{}
	for _, c := range []struct {
		name string
		md   *clusterv1.MachineDeployment
	}{{"prod", prod}, {"prod-md", other}, {"stray", nil}} {
		key := client.ObjectKey{Namespace: interfaceNamespace, Name: c.name}
		accounts[c.name] = autoscalerName(key)
		sa := &corev1.ServiceAccount{Namespace: autoscalerNamespace, Name: autoscalerName(key)}
		if err := admin.Create(ctx, sa); err != nil {
			t.Fatal(err)
		}
		binding := &rbacv1.RoleBinding{
			Namespace: interfaceNamespace, Name: autoscalerBinding(c.name),
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: AutoscalerRole},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa.Name, Namespace: autoscalerNamespace}},
		}
		if err := admin.Create(ctx, binding); err != nil {
			t.Fatal(err)
		}
		if c.md == nil {
			continue
		}
		cluster := &clusterv1.Cluster{Namespace: interfaceNamespace, Name: c.name}
		cluster.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("0", "2")}
		c.md.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel] = "md-0"
		if err := a.recordPools(ctx, cluster, []clusterv1.MachineDeployment{*c.md}); err != nil {
			t.Fatal(err)
		}
	}
	as := func(cluster string) client.Client {
		t.Helper()
		impersonated := rest.CopyConfig(cfg)
		impersonated.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:" + autoscalerNamespace + ":" + accounts[cluster],
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + autoscalerNamespace, "system:authenticated"},
		}
		c, err := client.New(impersonated, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	autoscaler := as("prod")
	scale := func(c client.Client, md *clusterv1.MachineDeployment, replicas string) error {
		return c.SubResource("scale").Patch(ctx, md.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":`+replicas+`}}`)),
			client.WithSubResourceBody(&autoscalingv1.Scale{}))
	}

	// The API server takes the policy up from its own watch, a moment after the chart is applied.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		err := scale(autoscaler, other, "2")
		if apierrors.IsInvalid(err) {
			break
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("scaling another cluster's pool: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the API server let one cluster's autoscaler scale another's pool for 30 seconds")
		}
	}

	refused := func(what string, err error, message string) {
		t.Helper()
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), message) {
			t.Errorf("%s: %v, want refused for %q", what, err, message)
		}
	}
	if err := scale(autoscaler, prod, "2"); err != nil {
		t.Errorf("scaling its own pool: %v", err)
	}
	mine := &clusterv1.Machine{}
	if err := autoscaler.Get(ctx, client.ObjectKeyFromObject(prodMachine), mine); err != nil {
		t.Fatal(err)
	}
	mine.Annotations = map[string]string{clusterv1.DeleteMachineAnnotation: "yes"}
	if err := autoscaler.Update(ctx, mine); err != nil {
		t.Errorf("marking its own machine for removal: %v", err)
	}
	theirs := &clusterv1.Machine{}
	if err := autoscaler.Get(ctx, client.ObjectKeyFromObject(otherMachine), theirs); err != nil {
		t.Fatal(err)
	}
	theirs.Annotations = map[string]string{clusterv1.DeleteMachineAnnotation: "yes"}
	refused("marking another cluster's machine", autoscaler.Update(ctx, theirs), "marks its own cluster's machines")
	if err := autoscaler.Get(ctx, client.ObjectKeyFromObject(prodMachine), mine); err != nil {
		t.Fatal(err)
	}
	mine.Labels[clusterv1.ClusterNameLabel] = "prod-md"
	refused("moving its machine to another cluster", autoscaler.Update(ctx, mine), "marks its own cluster's machines")
	if err := autoscaler.Get(ctx, client.ObjectKeyFromObject(prodMachine), mine); err != nil {
		t.Fatal(err)
	}
	mine.Spec.ProviderID = "proxmox://elsewhere"
	refused("changing its machine's spec", autoscaler.Update(ctx, mine), "changes nothing else of them")
	refused("an autoscaler the policy has no entry for", scale(as("stray"), prod, "1"), "serves no cluster")

	// An entry removed, the autoscaler can do nothing more.
	if err := a.setPolicyEntry(ctx, accounts["prod"], nil); err != nil {
		t.Fatal(err)
	}
	refused("an autoscaler switched off", scale(autoscaler, prod, "1"), "serves no cluster")

	// Anyone else, the controller or kubectl, is the policy's no concern.
	if err := scale(admin, other, "3"); err != nil {
		t.Errorf("the administrator scaling a pool: %v", err)
	}
}
