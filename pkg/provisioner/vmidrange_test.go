package provisioner

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func withReplicas(c *clusterv1.Cluster, controlPlanes, workers int32) *clusterv1.Cluster {
	c.Spec.Topology.ControlPlane.Replicas = &controlPlanes
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Name: "pool", Class: "worker", Replicas: &workers}}
	return c
}

func TestValidatorRefusesAnInvertedOutOfLimitOrTooNarrowVMIDRange(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	// Three control planes and two workers are five nodes, and an upgrade has a sixth beside them.
	for _, c := range [][2]string{
		{`{"start":10000,"end":10999}`, ""},
		{`{"start":10000,"end":10005}`, ""},
		{`{"start":999999990,"end":999999999}`, ""},
		{`{"start":10000,"end":10004}`, "holds 5 IDs. The nodes need 6"},
		{`{"start":2000,"end":1000}`, "starts above its end"},
		{`{"start":99,"end":1000}`, "outside 100 to 999999999"},
		{`{"start":100,"end":1000000000}`, "outside 100 to 999999999"},
		{`{"start":"a","end":1}`, "start and end, whole numbers"},
	} {
		raw, want := c[0], c[1]
		_, err := v.ValidateCreate(t.Context(), withReplicas(cluster(variableOf(VMIDRangeVariable, raw)), 3, 2))
		switch {
		case want == "" && err != nil && strings.Contains(err.Error(), "variables["+VMIDRangeVariable+"]"):
			t.Errorf("%s: refused: %v", raw, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: err = %v, want it to say %q", raw, err, want)
		}
	}
	if _, err := v.ValidateCreate(t.Context(), withReplicas(cluster(), 3, 2)); err != nil && strings.Contains(err.Error(), VMIDRangeVariable) {
		t.Errorf("a cluster without a range was refused for it: %v", err)
	}
}

func TestValidatorHoldsTheRangeToTheNodesWhenTheyGrow(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	narrow := variableOf(VMIDRangeVariable, `{"start":10000,"end":10004}`)
	old := withReplicas(cluster(narrow), 1, 1)
	// Three nodes and the extra one fit in five IDs, five nodes and the extra one do not.
	if _, err := v.ValidateUpdate(t.Context(), old, withReplicas(cluster(narrow), 1, 2)); err != nil && strings.Contains(err.Error(), VMIDRangeVariable) {
		t.Errorf("a scale that fits was refused: %v", err)
	}
	if _, err := v.ValidateUpdate(t.Context(), old, withReplicas(cluster(narrow), 1, 4)); err == nil || !strings.Contains(err.Error(), "holds 5 IDs. The nodes need 6") {
		t.Errorf("a scale past the range: %v", err)
	}
}

func TestDefaulterGivesANewClusterTheSitesVMIDRangeAndKeepsItsOwn(t *testing.T) {
	t.Parallel()
	h, reader := hooksFor(t)
	class := &clusterv1.ClusterClass{}
	key := client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}
	if err := reader.Get(t.Context(), key, class); err != nil {
		t.Fatal(err)
	}
	d := &ClusterDefaulter{Hooks: h}
	rangeOf := func(c *clusterv1.Cluster) string {
		t.Helper()
		if err := d.Default(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		raw, _, _ := optionalVariable[VMIDRange](c, VMIDRangeVariable)
		return raw.String()
	}
	site := func(annotation string, declared bool) {
		t.Helper()
		class.Annotations = map[string]string{DefaultPodsAnnotation: "10.42.0.0/16", DefaultServicesAnnotation: "10.43.0.0/16"}
		if annotation != "" {
			class.Annotations[SiteVMIDRangeAnnotation] = annotation
		}
		class.Spec.Variables = nil
		if declared {
			class.Spec.Variables = []clusterv1.ClusterClassVariable{{Name: VMIDRangeVariable}}
		}
		if err := reader.Update(t.Context(), class); err != nil {
			t.Fatal(err)
		}
	}
	site("", true)
	if got := rangeOf(cluster()); got != "0-0" {
		t.Errorf("without a site range the cluster got %s", got)
	}
	site("10000-10999", true)
	if got := rangeOf(cluster()); got != "10000-10999" {
		t.Errorf("the cluster got %s, want the site's", got)
	}
	if got := rangeOf(cluster(variableOf(VMIDRangeVariable, `{"start":20000,"end":20099}`))); got != "20000-20099" {
		t.Errorf("a cluster's own range became %s", got)
	}
	// A class without the variable would refuse a cluster that carries it.
	site("10000-10999", false)
	if got := rangeOf(cluster()); got != "0-0" {
		t.Errorf("a class without the variable gave the cluster %s", got)
	}
	site("10000-5", true)
	if err := d.Default(context.Background(), cluster()); err == nil || !strings.Contains(err.Error(), "starts above its end") {
		t.Errorf("an unusable site range: %v", err)
	}
}

func machineSaying(name, message string) *unstructured.Unstructured {
	pm := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"conditions": []any{map[string]any{
			"type": "VirtualMachineProvisioned", "status": "False", "reason": "VMProvisionFailed", "message": message,
		}}},
	}}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	pm.SetNamespace("team-a")
	pm.SetName(name)
	pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "web"})
	return pm
}

func TestFullVMIDRangeIsAConditionNamingTheRangeAndFixAndClearsOnceFree(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(proxmoxMachineGVK, &unstructured.Unstructured{})
	c := cluster(variableOf(VMIDRangeVariable, `{"start":10000,"end":10005}`))
	pm := machineSaying("web-md-0-x", "No free vmid found in vmIDRange")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&clusterv1.Cluster{}).WithObjects(c, pm).Build()
	p := &Placer{Client: kube}
	condition := func() string {
		t.Helper()
		got := &clusterv1.Cluster{}
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, VMIDRangeFullCondition)
		if cond == nil {
			return ""
		}
		return cond.Message
	}
	reload := func() *clusterv1.Cluster {
		t.Helper()
		got := &clusterv1.Cluster{}
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	p.markVMIDRange(t.Context(), reload())
	if got := condition(); got != "No free VM ID in 10000-10005. Widen the cluster's VM ID range, or free IDs in it" {
		t.Fatalf("the condition's message is %q", got)
	}
	// Another reason a machine fails says nothing of the range.
	other := machineSaying("web-md-0-x", "cloning failed")
	other.SetResourceVersion("")
	if err := kube.Delete(t.Context(), pm); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	p.markVMIDRange(t.Context(), reload())
	if got := condition(); got != "" {
		t.Errorf("the condition stayed as %q once no machine lacked an ID", got)
	}
}
