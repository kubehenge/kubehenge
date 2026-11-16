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
	// Three control planes and two workers are five nodes, and the control plane and the pool roll one
	// more each at once, so seven IDs.
	for _, c := range [][2]string{
		{`{"start":10000,"end":10999}`, ""},
		{`{"start":10000,"end":10006}`, ""},
		{`{"start":999999990,"end":999999999}`, ""},
		{`{"start":10000,"end":10005}`, "holds 6 IDs. The nodes need 7"},
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
	// Three nodes and a spare for each of the two parts fit in five IDs, five nodes and the spares do not.
	if _, err := v.ValidateUpdate(t.Context(), old, withReplicas(cluster(narrow), 1, 2)); err != nil && strings.Contains(err.Error(), VMIDRangeVariable) {
		t.Errorf("a scale that fits was refused: %v", err)
	}
	if _, err := v.ValidateUpdate(t.Context(), old, withReplicas(cluster(narrow), 1, 4)); err == nil || !strings.Contains(err.Error(), "holds 5 IDs. The nodes need 7") {
		t.Errorf("a scale past the range: %v", err)
	}
}

// Every pool rolls one machine beyond its size at the same time as the control plane.
func TestVMIDRangeKeepsASpareForTheControlPlaneAndEachPool(t *testing.T) {
	t.Parallel()
	c := withReplicas(cluster(), 3, 2)
	pools := c.Spec.Topology.Workers.MachineDeployments
	c.Spec.Topology.Workers.MachineDeployments = append(pools, pools[0], pools[0])
	// Three control planes and three pools of two are nine nodes, with a spare for each of four parts.
	if got := vmIDDemand(c); got != 13 {
		t.Errorf("demand %d, want 13", got)
	}
}

// siteRangeCluster is a cluster of three nodes (one control plane, two workers in one pool) in the
// range, which with its spares needs five IDs.
func siteRangeCluster(name, rng string) *clusterv1.Cluster {
	c := withReplicas(cluster(variableOf(VMIDRangeVariable, rng)), 1, 2)
	c.Name = name
	return c
}

func TestValidatorCountsEveryClusterAndTemplateThatSharesARange(t *testing.T) {
	t.Parallel()
	const shared = `{"start":10000,"end":10009}`
	first := siteRangeCluster("first", shared)
	h, _ := hooksFor(t, first)
	v := &ClusterValidator{Hooks: h}
	// The first cluster needs five of ten IDs. A second needs five more and fits.
	if _, err := v.ValidateCreate(t.Context(), siteRangeCluster("second", shared)); err != nil && strings.Contains(err.Error(), VMIDRangeVariable) {
		t.Errorf("a second cluster that fits was refused: %v", err)
	}
	// A third does not, though it fits the range alone.
	third := siteRangeCluster("third", shared)
	second := siteRangeCluster("second", shared)
	h, _ = hooksFor(t, first, second)
	v = &ClusterValidator{Hooks: h}
	_, err := v.ValidateCreate(t.Context(), third)
	if err == nil || !strings.Contains(err.Error(), "needs 5 and the 2 other clusters on it 10") {
		t.Errorf("a third cluster on a full range: %v", err)
	}
	// A cluster on another range is not counted, and one's own update does not count itself.
	other := siteRangeCluster("third", `{"start":20000,"end":20004}`)
	if _, err = v.ValidateCreate(t.Context(), other); err != nil && strings.Contains(err.Error(), VMIDRangeVariable) {
		t.Errorf("a cluster on a range of its own was refused: %v", err)
	}
	grown := siteRangeCluster("first", shared)
	grown.Spec.Topology.ControlPlane.Replicas = new(int32(1))
	grown.Spec.Topology.Workers.MachineDeployments[0].Replicas = new(int32(3))
	if _, err = v.ValidateUpdate(t.Context(), first, grown); err == nil || !strings.Contains(err.Error(), "needs 6 and the 1 other clusters on it 5") {
		t.Errorf("a scale of a cluster that shares the range: %v", err)
	}
}

func TestValidatorKeepsTheIDsOfTemplatesOutOfARange(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	var sites []TemplateSite
	for _, id := range []int{10000, 10001, 10002, 5} {
		sites = append(sites, TemplateSite{Name: "t", VMID: id})
	}
	h.Sites = fixedSites{&Sites{Templates: sites}}
	v := &ClusterValidator{Hooks: h}
	// Seven IDs hold five, after three templates in them.
	_, err := v.ValidateCreate(t.Context(), siteRangeCluster("web", `{"start":10000,"end":10006}`))
	if err == nil || !strings.Contains(err.Error(), "4 IDs free after 3 templates") {
		t.Errorf("a range the templates fill: %v", err)
	}
}

type fixedSites struct{ sites *Sites }

func (f fixedSites) Sites(context.Context) (*Sites, error) { return f.sites, nil }

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
	p := &Placer{Client: kube, ClassNamespace: "kubehenge-system", Nodes: &fakeNodes{online: []string{"pve1"}}}
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
	p.placeAll(t.Context())
	if got := condition(); got != "No free VM ID in 10000-10005, and the machine that asked stays failed. Widen the range, then delete the failed machine so Cluster API makes it again" {
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
	p.placeAll(t.Context())
	if got := condition(); got != "" {
		t.Errorf("the condition stayed as %q once no machine lacked an ID", got)
	}
}

func TestBeforeClusterCreateRefusesAVMIDRangeTooNarrowForItsNodes(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	c := withAddresses(networkCluster("192.168.1.1", 24), `["192.168.1.10-192.168.1.20"]`)
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, variableOf(VMIDRangeVariable, `{"start":10000,"end":10000}`))
	if resp := callBeforeClusterCreate(t, hooks, c); !strings.Contains(resp.Message, "VM ID range 10000-10000 holds 1 IDs") {
		t.Errorf("a range of one ID: %+v", resp)
	}
}
