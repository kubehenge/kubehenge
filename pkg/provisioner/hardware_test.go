package provisioner

import (
	"errors"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// templateTagsOf are the tags of the template of a line with this hardware.
func templateTagsOf(hw templates.Hardware, line ...string) []string {
	return templates.NormaliseTags(append(append([]string{"kubehenge"}, line...), hw.Tags()...))
}

func TestClusterNeedsFollowTheTemplatesHardware(t *testing.T) {
	t.Parallel()
	almaRKE2 := []string{"rke2", "os-almalinux-10"}
	for _, tc := range []struct {
		name string
		c    *clusterv1.Cluster
		tags [][]string
		want templateNeeds
	}{
		{"no templates listed", cluster(), nil, templateNeeds{Level: 2}},
		{"templates without recorded hardware", cluster(), [][]string{{"kubehenge", "rke2", "os-almalinux-10"}}, templateNeeds{Level: 2}},
		{
			"the defaults", cluster(),
			[][]string{templateTagsOf(templates.DefaultHardware(templates.DefaultCPUType), almaRKE2...)},
			templateNeeds{Level: 2},
		},
		{
			"a CPU type above the node OS's", cluster(),
			[][]string{templateTagsOf(templates.Hardware{CPUType: "x86-64-v4"}, almaRKE2...)},
			templateNeeds{Level: 4, CPUType: "x86-64-v4"},
		},
		{
			"host asks for no level", cluster(),
			[][]string{templateTagsOf(templates.Hardware{CPUType: "host"}, almaRKE2...)},
			templateNeeds{Level: 2},
		},
		{
			"a model's level", cluster(),
			[][]string{templateTagsOf(templates.Hardware{CPUType: "Haswell-noTSX"}, almaRKE2...)},
			templateNeeds{Level: 3, CPUType: "Haswell-noTSX"},
		},
		{
			"a template of another node OS is no concern", cluster(),
			[][]string{templateTagsOf(templates.Hardware{CPUType: "x86-64-v4", NUMA: true}, "rke2", "os-rocky-10")},
			templateNeeds{Level: 2},
		},
		{
			"a template of another distribution is no concern", cluster(),
			[][]string{templateTagsOf(templates.Hardware{CPUType: "x86-64-v4"}, "k3s", "os-almalinux-10")},
			templateNeeds{Level: 2},
		},
		{
			"NUMA", cluster(),
			[][]string{templateTagsOf(templates.Hardware{CPUType: templates.DefaultCPUType, NUMA: true}, almaRKE2...)},
			templateNeeds{Level: 2, NUMA: true},
		},
		{
			"Talos follows its own templates", talosCluster("web", "v1.14.4"),
			[][]string{templateTagsOf(templates.Hardware{CPUType: "x86-64-v3"}, "talos", "talos-v1.14.4")},
			templateNeeds{Level: 3, CPUType: "x86-64-v3"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := clusterNeeds(tc.c, tc.tags)
			if err != nil || got != tc.want {
				t.Errorf("needs %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestPlacerKeepsAClusterOffNodesThatCannotRunItsTemplates(t *testing.T) {
	t.Parallel()
	web := cluster()
	_, c := newReconciler(t, nil, web)
	nodes := &fakeNodes{
		online: []string{"pve1", "pve2"}, levels: map[string]int{"pve1": 2, "pve2": 3},
		templateTags: [][]string{templateTagsOf(templates.Hardware{CPUType: "Haswell"}, "rke2", "os-almalinux-10")},
	}
	(&Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: nodes}).placeAll(t.Context())
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[OnlineNodesAnnotation]; v != "pve2" {
		t.Errorf("web records %q, want only the node that reaches Haswell's level", v)
	}
	// While the templates cannot be listed the node OS's own need stands.
	nodes.templateTags, nodes.tagsErr = nil, errors.New("403 permission check failed")
	(&Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: nodes}).placeAll(t.Context())
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[OnlineNodesAnnotation]; v != "pve1,pve2" {
		t.Errorf("web records %q after the listing failed", v)
	}
}

func TestCheckNodeOSRefusesATemplateCPUTypeNoNodeRuns(t *testing.T) {
	t.Parallel()
	hw := templates.Hardware{CPUType: "x86-64-v4"}
	for _, tc := range []struct {
		name    string
		c       *clusterv1.Cluster
		tags    [][]string
		levels  map[string]int
		refused string
	}{
		{"a node reaches it", cluster(), [][]string{templateTagsOf(hw, "rke2", "os-almalinux-10")}, map[string]int{"pve1": 4}, ""},
		{"no node does", cluster(), [][]string{templateTagsOf(hw, "rke2", "os-almalinux-10")}, map[string]int{"pve1": 3}, "use CPU type x86-64-v4, which no allowed node (pve1) offers"},
		{"other templates are no concern", cluster(), [][]string{templateTagsOf(hw, "k3s", "os-almalinux-10")}, map[string]int{"pve1": 3}, ""},
		{
			"Talos too", talosCluster("web", "v1.14.4"), [][]string{templateTagsOf(hw, "talos", "talos-v1.14.4")}, map[string]int{"pve1": 3},
			"use CPU type x86-64-v4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Hooks{Nodes: &fakeNodes{online: []string{"pve1"}, levels: tc.levels, templateTags: tc.tags}}
			err := h.checkNodeOS(t.Context(), tc.c)
			if tc.refused == "" && err != nil || tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)) {
				t.Errorf("checkNodeOS = %v, want a refusal naming %q", err, tc.refused)
			}
		})
	}
}

func numaTemplates(numa bool) [][]string {
	return [][]string{templateTagsOf(templates.Hardware{CPUType: templates.DefaultCPUType, NUMA: numa}, "rke2", "os-almalinux-10")}
}

func workerOverride(c *clusterv1.Cluster, pool int, raw string) *clusterv1.Cluster {
	c.Spec.Topology.Workers.MachineDeployments[pool].Variables.Overrides = []clusterv1.ClusterVariable{
		{Name: "workerMachine", Value: apiextensionsv1.JSON{Raw: []byte(raw)}},
	}
	return c
}

func TestNUMATemplatesNeedTwoCoresPerMachine(t *testing.T) {
	t.Parallel()
	class := addonClass()
	for _, tc := range []struct {
		name    string
		c       *clusterv1.Cluster
		numa    bool
		refused string
	}{
		{"the default machines", sized("", new(int32(1))), true, ""},
		{"a worker pool of one core", workerOverride(sized("", new(int32(1)), new(int32(1))), 1, `{"cores":1,"memoryMiB":4096}`), true, "worker pool md-1 has 1"},
		{"a worker pool of one core without NUMA", workerOverride(sized("", new(int32(1))), 0, `{"cores":1,"memoryMiB":4096}`), false, ""},
		{"the whole cluster's workers of one core", withAddons(sized("", new(int32(1))), map[string]string{"workerMachine": `{"cores":1,"memoryMiB":4096}`}), true, "worker pool md-0 has 1"},
		{"a control plane of one core", sized(`{"cores":1,"memoryMiB":4096}`, new(int32(1))), true, "the control plane has 1"},
		{"two cores are enough", workerOverride(sized("", new(int32(1))), 0, `{"cores":2,"memoryMiB":4096}`), true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Hooks{Nodes: &fakeNodes{online: []string{"pve1"}, templateTags: numaTemplates(tc.numa)}}
			err := h.checkNUMA(t.Context(), tc.c, class)
			if tc.refused == "" && err != nil || tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)) {
				t.Errorf("checkNUMA = %v, want a refusal naming %q", err, tc.refused)
			}
			// The admission webhook refuses what the hook before cluster creation does.
			errs := (&ClusterValidator{Hooks: h}).validateNUMA(t.Context(), nil, tc.c, class)
			if (len(errs) > 0) != (tc.refused != "") {
				t.Errorf("the webhook gave %v, want a refusal %q", errs, tc.refused)
			}
			if tc.refused == "" {
				return
			}
			if refusal := h.refusal(t.Context(), tc.c, class); refusal == nil || !strings.Contains(refusal.Error(), "NUMA") {
				t.Errorf("BeforeClusterCreate would not refuse: %v", refusal)
			}
		})
	}
}

func TestNUMAIsCheckedOnlyWhenMachinesChange(t *testing.T) {
	t.Parallel()
	class := addonClass()
	nodes := &fakeNodes{online: []string{"pve1"}, templateTags: numaTemplates(true)}
	v := &ClusterValidator{Hooks: &Hooks{Nodes: nodes}}
	small := withAddons(sized("", new(int32(1))), map[string]string{"workerMachine": `{"cores":1,"memoryMiB":4096}`})
	if errs := v.validateNUMA(t.Context(), small.DeepCopy(), small, class); len(errs) != 0 {
		t.Errorf("an update that leaves the machines alone was refused: %v", errs)
	}
	grown := small.DeepCopy()
	grown.Spec.Topology.Variables = nil
	if errs := v.validateNUMA(t.Context(), small, grown, class); len(errs) != 0 {
		t.Errorf("growing the machines was refused: %v", errs)
	}
	if errs := v.validateNUMA(t.Context(), grown, small, class); len(errs) != 1 {
		t.Errorf("shrinking the machines to one core gave %v", errs)
	}
}
