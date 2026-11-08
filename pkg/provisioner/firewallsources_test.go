package provisioner

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// firewallCluster is a Kubehenge cluster named name, with the security groups given, and its
// ProxmoxCluster.
func firewallCluster(name, groups string) (*clusterv1.Cluster, *unstructured.Unstructured) {
	c := &clusterv1.Cluster{Name: name, Namespace: "team"}
	c.Spec.Topology.ClassRef = clusterv1.ClusterClassRef{Name: "kubehenge-kubeadm", Namespace: "kubehenge-system"}
	if groups != "" {
		c.Spec.Topology.Variables = []clusterv1.ClusterVariable{{Name: SecurityGroupsVariable, Value: apiextensionsv1.JSON{Raw: []byte(groups)}}}
	}
	pc := proxmoxClusterObject()
	pc.SetName(name + "-x1")
	pc.SetNamespace("team")
	pc.SetLabels(map[string]string{clusterv1.ClusterNameLabel: name})
	return c, pc
}

// The management cluster's nodes and the appliance's addresses reach the ProxmoxCluster of every
// cluster with security groups, and follow the appliance's ConfigMap when an address is added,
// changed or removed. A cluster without groups is left alone, and one whose groups go loses the
// annotation. A run that finds nothing changed writes nothing.
func TestFirewallSourcesFollowTheAppliance(t *testing.T) {
	t.Parallel()
	web, webPC := firewallCluster("web", `["k8s"]`)
	plain, plainPC := firewallCluster("plain", "")
	appliance := &corev1.ConfigMap{Name: ApplianceAddressesName, Namespace: "kubehenge-system"}
	patches := 0
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(web, plain, webPC, plainPC, appliance).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			return c.Patch(ctx, obj, patch, opts...)
		}}).Build()
	f := &FirewallSources{Client: kube, Reader: kube, Namespace: "kubehenge-system", Management: []string{"192.0.2.11", "2001:db8::11"}}
	held := func(pc *unstructured.Unstructured) (string, bool) {
		t.Helper()
		got := proxmoxClusterObject()
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pc), got); err != nil {
			t.Fatal(err)
		}
		v, ok := got.GetAnnotations()[FirewallSourcesAnnotation]
		return v, ok
	}
	setAppliance := func(addresses string) {
		t.Helper()
		appliance.Data = map[string]string{ApplianceAddressesKey: addresses}
		if err := kube.Update(t.Context(), appliance); err != nil {
			t.Fatal(err)
		}
	}

	for _, step := range []struct {
		name, appliance, want string
		patches               int
	}{
		{name: "the appliance has written nothing yet", want: "192.0.2.11,2001:db8::11", patches: 1},
		{name: "nothing changed", want: "192.0.2.11,2001:db8::11", patches: 1},
		{name: "the appliance's addresses", appliance: "192.0.2.5,2001:db8::5", want: "192.0.2.11,2001:db8::11,192.0.2.5,2001:db8::5", patches: 2},
		{name: "an IPv4 address renewed", appliance: "192.0.2.9,2001:db8::5", want: "192.0.2.11,2001:db8::11,192.0.2.9,2001:db8::5", patches: 3},
		{name: "an IPv6 address removed", appliance: "192.0.2.9", want: "192.0.2.11,2001:db8::11,192.0.2.9", patches: 4},
		{name: "an address a management node has too", appliance: "192.0.2.9,192.0.2.11", want: "192.0.2.11,2001:db8::11,192.0.2.9", patches: 4},
	} {
		if step.appliance != "" {
			setAppliance(step.appliance)
		}
		if err := f.runOnce(t.Context()); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got, ok := held(webPC); !ok || got != step.want {
			t.Errorf("%s: web's ProxmoxCluster holds %q (%v), want %q", step.name, got, ok, step.want)
		}
		if got, ok := held(plainPC); ok {
			t.Errorf("%s: the cluster without groups holds %q", step.name, got)
		}
		if patches != step.patches {
			t.Errorf("%s: %d writes, want %d", step.name, patches, step.patches)
		}
	}

	web.Spec.Topology.Variables = nil
	if err := kube.Update(t.Context(), web); err != nil {
		t.Fatal(err)
	}
	if err := f.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, ok := held(webPC); ok {
		t.Errorf("groups removed, the ProxmoxCluster still holds %q", got)
	}
}

// A cluster whose ProxmoxCluster is not made yet and one of another class are passed over without
// an error. The next run reaches a ProxmoxCluster made since.
func TestFirewallSourcesPassOverClustersNotReady(t *testing.T) {
	t.Parallel()
	early, earlyPC := firewallCluster("early", `["k8s"]`)
	foreign, foreignPC := firewallCluster("foreign", `["k8s"]`)
	foreign.Spec.Topology.ClassRef.Name = "someone-elses"
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(early, foreign, foreignPC).Build()
	f := &FirewallSources{Client: kube, Reader: kube, Namespace: "kubehenge-system", Management: []string{"192.0.2.11"}}
	if err := f.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(t.Context(), earlyPC); err != nil {
		t.Fatal(err)
	}
	if err := f.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	for pc, want := range map[*unstructured.Unstructured]bool{earlyPC: true, foreignPC: false} {
		got := proxmoxClusterObject()
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pc), got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got.GetAnnotations()[FirewallSourcesAnnotation]; ok != want {
			t.Errorf("%s annotated: %v, want %v", pc.GetName(), ok, want)
		}
	}
}
