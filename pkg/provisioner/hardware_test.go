package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// hardwareSite is the newest build of the cluster's first template line, made with hw. swap
// replaces tags pairwise, old then new, to put the template on another line.
func hardwareSite(t *testing.T, c *clusterv1.Cluster, hw templates.Hardware, swap ...string) TemplateSite {
	t.Helper()
	lines := templateLines(c)
	if len(lines) == 0 {
		t.Fatal("the cluster has no template line")
	}
	tags := slices.Clone(lines[0])
	for i := 0; i+1 < len(swap); i += 2 {
		j := slices.Index(tags, swap[i])
		if j < 0 {
			t.Fatalf("the line %v has no tag %s", tags, swap[i])
		}
		tags[j] = swap[i+1]
	}
	at := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	tags = templates.NormaliseTags(append(tags, templates.BuildTag(at)))
	return TemplateSite{Name: "kubehenge-build", Node: "pve1", Tags: tags, CPU: hw.CPUType, Hardware: hw, Built: at}
}

func sitesWith(templates ...TemplateSite) fakeSites {
	return fakeSites{sites: &Sites{Templates: templates}}
}

func TestClusterNeedsFollowTheTemplatesHardware(t *testing.T) {
	t.Parallel()
	web, talos := cluster(), talosCluster("web", "v1.14.4")
	v4 := templates.Hardware{CPUType: "x86-64-v4"}
	for _, tc := range []struct {
		name string
		c    *clusterv1.Cluster
		site *TemplateSite
		want templateNeeds
	}{
		{"no templates listed", web, nil, templateNeeds{Level: 2}},
		{"templates without recorded hardware", web, new(hardwareSite(t, web, templates.Hardware{})), templateNeeds{Level: 2}},
		{"the defaults", web, new(hardwareSite(t, web, templates.DefaultHardware(templates.DefaultCPUType))), templateNeeds{Level: 2}},
		{"a CPU type above the node OS's", web, new(hardwareSite(t, web, v4)), templateNeeds{Level: 4, CPUType: "x86-64-v4"}},
		{"host asks for no level", web, new(hardwareSite(t, web, templates.Hardware{CPUType: "host"})), templateNeeds{Level: 2}},
		{"a model's level", web, new(hardwareSite(t, web, templates.Hardware{CPUType: "Haswell-noTSX"})), templateNeeds{Level: 3, CPUType: "Haswell-noTSX"}},
		{
			"a template of another node OS is no concern", web,
			new(hardwareSite(t, web, templates.Hardware{CPUType: "x86-64-v4", NUMA: true}, "os-almalinux-10", "os-rocky-10")),
			templateNeeds{Level: 2},
		},
		{"a template of another distribution is no concern", web, new(hardwareSite(t, web, v4, "rke2", "k3s")), templateNeeds{Level: 2}},
		{
			"NUMA", web,
			new(hardwareSite(t, web, templates.Hardware{CPUType: templates.DefaultCPUType, NUMA: true})),
			templateNeeds{Level: 2, NUMA: true},
		},
		{"Talos follows its own templates", talos, new(hardwareSite(t, talos, templates.Hardware{CPUType: "x86-64-v3"})), templateNeeds{Level: 3, CPUType: "x86-64-v3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s *Sites
			if tc.site != nil {
				s = &Sites{Templates: []TemplateSite{*tc.site}}
			}
			got, err := clusterNeeds(tc.c, s)
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
	nodes := &fakeNodes{online: []string{"pve1", "pve2"}, levels: map[string]int{"pve1": 2, "pve2": 3}}
	sites := sitesWith(hardwareSite(t, web, templates.Hardware{CPUType: "Haswell"}))
	(&Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: nodes, Sites: sites}).placeAll(t.Context())
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[OnlineNodesAnnotation]; v != "pve2" {
		t.Errorf("web records %q, want only the node that reaches Haswell's level", v)
	}
	// Without a look at the templates the node OS's own need stands.
	(&Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: nodes}).placeAll(t.Context())
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[OnlineNodesAnnotation]; v != "pve1,pve2" {
		t.Errorf("web records %q without the templates", v)
	}
}

func TestPlacementReadsTheTemplatesOnce(t *testing.T) {
	t.Parallel()
	web := cluster()
	h, _ := hooksFor(t)
	sites := &countingSites{sites: &Sites{Templates: []TemplateSite{hardwareSite(t, web, templates.Hardware{CPUType: templates.DefaultCPUType, NUMA: true})}}}
	h.Nodes, h.Sites = &fakeNodes{online: []string{"pve1"}}, sites
	// Whatever the hook decides, its node OS, NUMA and placement checks share one look.
	_ = h.refusal(t.Context(), web, addonClass())
	if sites.asked != 1 {
		t.Errorf("the hook read the templates %d times, want once", sites.asked)
	}
}

// countingSites counts the looks at the templates.
type countingSites struct {
	sites *Sites
	asked int
}

func (c *countingSites) Sites(context.Context) (*Sites, error) {
	c.asked++
	return c.sites, nil
}

func TestCheckNodeOSRefusesATemplateCPUTypeNoNodeRuns(t *testing.T) {
	t.Parallel()
	web, talos := cluster(), talosCluster("web", "v1.14.4")
	hw := templates.Hardware{CPUType: "x86-64-v4"}
	for _, tc := range []struct {
		name    string
		c       *clusterv1.Cluster
		site    TemplateSite
		levels  map[string]int
		refused string
	}{
		{"a node reaches it", web, hardwareSite(t, web, hw), map[string]int{"pve1": 4}, ""},
		{"no node does", web, hardwareSite(t, web, hw), map[string]int{"pve1": 3}, "use CPU type x86-64-v4, which no allowed node (pve1) offers"},
		{"other templates are no concern", web, hardwareSite(t, web, hw, "rke2", "k3s"), map[string]int{"pve1": 3}, ""},
		{"Talos too", talos, hardwareSite(t, talos, hw), map[string]int{"pve1": 3}, "use CPU type x86-64-v4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Hooks{Nodes: &fakeNodes{online: []string{"pve1"}, levels: tc.levels}, Sites: sitesWith(tc.site)}
			err := h.checkNodeOS(t.Context(), h.reads(), tc.c)
			if tc.refused == "" && err != nil || tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)) {
				t.Errorf("checkNodeOS = %v, want a refusal naming %q", err, tc.refused)
			}
		})
	}
}

func numaSites(t *testing.T, c *clusterv1.Cluster, numa bool) fakeSites {
	t.Helper()
	return sitesWith(hardwareSite(t, c, templates.Hardware{CPUType: templates.DefaultCPUType, NUMA: numa}))
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
			h := &Hooks{Nodes: &fakeNodes{online: []string{"pve1"}}, Sites: numaSites(t, tc.c, tc.numa)}
			err := h.checkNUMA(t.Context(), h.reads(), tc.c, class)
			if tc.refused == "" && err != nil || tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)) {
				t.Errorf("checkNUMA = %v, want a refusal naming %q", err, tc.refused)
			}
			// The admission webhook refuses what the hook before cluster creation does.
			errs := (&ClusterValidator{Hooks: h}).validateNUMA(t.Context(), h.reads(), nil, tc.c, class)
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
	small := withAddons(sized("", new(int32(1))), map[string]string{"workerMachine": `{"cores":1,"memoryMiB":4096}`})
	v := &ClusterValidator{Hooks: &Hooks{Nodes: &fakeNodes{online: []string{"pve1"}}, Sites: numaSites(t, small, true)}}
	if errs := v.validateNUMA(t.Context(), v.Hooks.reads(), small.DeepCopy(), small, class); len(errs) != 0 {
		t.Errorf("an update that leaves the machines alone was refused: %v", errs)
	}
	grown := small.DeepCopy()
	grown.Spec.Topology.Variables = nil
	if errs := v.validateNUMA(t.Context(), v.Hooks.reads(), small, grown, class); len(errs) != 0 {
		t.Errorf("growing the machines was refused: %v", errs)
	}
	if errs := v.validateNUMA(t.Context(), v.Hooks.reads(), grown, small, class); len(errs) != 1 {
		t.Errorf("shrinking the machines to one core gave %v", errs)
	}
}
