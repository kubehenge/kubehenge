package provisioner

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// addonCPUFigures are the chart's figures for the add-ons' CPU.
const addonCPUFigures = `{"headlamp":10,"monitoring":300,"logs":75,"backups":70,"kyverno":500,"istio":400,"clusterMesh":30,"loadBalancer":60,"certManager":30,"rancher":120,"rancherAgent":30,"argocd":90,"flux":50}`

// addonClass is the RKE2 class of hooksFor with figures for its add-ons' memory and CPU and its
// nodes' own, the CPU as the chart gives it.
func addonClass() *clusterv1.ClusterClass {
	return &clusterv1.ClusterClass{
		Name: "kubehenge-rke2",
		Annotations: map[string]string{
			ControlPlaneSizesAnnotation: rke2Sizes,
			AddonMemoryAnnotation:       `{"headlamp":50,"monitoring":3000,"kyverno":300,"istio":200,"clusterMesh":100,"certManager":100,"rancher":1500,"rancherAgent":250}`,
			NodeMemoryAnnotation:        `{"controlPlane":3840,"worker":640}`,
			AddonCPUAnnotation:          addonCPUFigures,
			NodeCPUAnnotation:           `{"controlPlane":1605,"worker":775}`,
		},
		Spec: clusterv1.ClusterClassSpec{Variables: []clusterv1.ClusterClassVariable{
			machineVariable("controlPlaneMachine"), machineVariable("workerMachine"),
		}},
	}
}

// withAddons gives the cluster these variables, raw JSON by name.
func withAddons(c *clusterv1.Cluster, vars map[string]string) *clusterv1.Cluster {
	for name, raw := range vars {
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(raw)}})
	}
	return c
}

func workerMemory(c *clusterv1.Cluster, raw string) *clusterv1.Cluster {
	return withAddons(c, map[string]string{"workerMachine": raw})
}

// everyAddon switches on every add-on but Rancher, whose agent counts while the cluster registers,
// and logs and backups, which the default machines leave no room for.
var everyAddon = map[string]string{
	"headlamp": `{"enabled":true}`, "monitoring": `{"enabled":true}`, "podSecurity": `{"kyverno":"restricted"}`, "istio": `{"enabled":true}`,
	"clusterMesh": `{"enabled":true}`,
}

func TestWorkersHaveRoomForTheAddons(t *testing.T) {
	t.Parallel()
	class := addonClass()
	for _, tc := range []struct {
		name string
		c    *clusterv1.Cluster
		want string
	}{
		{"the agent on the least worker", sized("", new(int32(1))), ""},
		{"not registering, nothing on", withAddons(sized("", new(int32(1))), map[string]string{"rancher": `{"register":false}`}), ""},
		{
			"every add-on on the least worker", withAddons(sized("", new(int32(1))), everyAddon),
			"add-ons (Headlamp, monitoring, Kyverno, Istio, Cluster Mesh, cert-manager, Rancher's agent) need 4000 MiB of memory on workers, which have 3456 MiB for them",
		},
		{"Kyverno off", withAddons(sized("", new(int32(1))), map[string]string{
			"headlamp": `{"enabled":true}`, "monitoring": `{"enabled":true}`, "podSecurity": `{"admission":"restricted","kyverno":"off"}`,
		}), ""},
		{"room on one worker", workerMemory(withAddons(sized("", new(int32(1))), everyAddon), `{"cores":3,"memoryMiB":4644}`), ""},
		{"room shared over workers", withAddons(sized("", new(int32(2))), everyAddon), ""},
		{"a pool scaled to none", withAddons(sized("", new(int32(1)), new(int32(0))), everyAddon), "which have 3456 MiB"},
		{
			"Rancher in place of its agent",
			withAddons(sized(`{"cores":4,"memoryMiB":8192}`, new(int32(1))), map[string]string{"monitoring": `{"enabled":true}`, "rancher": `{"install":true,"hostname":"r.example.org"}`}),
			"(monitoring, cert-manager, Rancher) need 4600 MiB",
		},
		{"without workers, the agent", sized(`{"cores":4,"memoryMiB":8192}`), ""},
		{
			"without workers, every add-on", withAddons(sized(`{"cores":4,"memoryMiB":7680}`), everyAddon),
			"need 4000 MiB of memory on control planes without workers, which have 3840 MiB",
		},
		{"without workers, room on the control planes", withAddons(sized(`{"cores":4,"memoryMiB":7840}`), everyAddon), ""},
	} {
		err := checkAddonRoom(nil, tc.c, class)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
}

func TestControlPlanesHaveRoomForTheAddons(t *testing.T) {
	t.Parallel()
	class := addonClass()
	class.Annotations[AddonControlPlaneMemoryAnnotation] = `{"monitoring":768,"kyverno":256,"rancherAgent":512}`
	every := func(controlPlane string) *clusterv1.Cluster {
		return workerMemory(withAddons(sized(controlPlane, new(int32(1))), everyAddon), `{"cores":3,"memoryMiB":4644}`)
	}
	for _, tc := range []struct {
		name string
		c    *clusterv1.Cluster
		want string
	}{
		{"nothing on", withAddons(sized("", new(int32(1))), map[string]string{"rancher": `{"register":false}`}), ""},
		{
			"the agent on the least control plane", sized("", new(int32(1))),
			"each control plane needs 4352 MiB of memory with add-ons (Rancher's agent), and has 4096 MiB",
		},
		{"the agent on a control plane with room", sized(`{"memoryMiB":4352}`, new(int32(1))), ""},
		{"every add-on, the room the agent leaves", every(`{"memoryMiB":4352}`), "each control plane needs 5376 MiB of memory with add-ons (Headlamp, monitoring, Kyverno, Istio, Cluster Mesh, cert-manager, Rancher's agent), and has 4352 MiB"},
		{"every add-on on a control plane with room", every(`{"memoryMiB":5376}`), ""},
		{
			"without workers, every add-on", withAddons(sized(`{"cores":4,"memoryMiB":9375}`), everyAddon),
			"on control planes without workers, which have 3999 MiB",
		},
		{"without workers, room on the control planes", withAddons(sized(`{"cores":4,"memoryMiB":9376}`), everyAddon), ""},
	} {
		err := checkAddonRoom(nil, tc.c, class)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
	short := sized("", new(int32(1)))
	edited := short.DeepCopy()
	edited.Labels = map[string]string{"team": "a"}
	if err := checkAddonRoom(short, edited, class); err != nil {
		t.Errorf("an unrelated edit to a cluster whose control planes are already short: %v", err)
	}
	bare := withAddons(sized("", new(int32(1))), map[string]string{"rancher": `{"register":false}`})
	if err := checkAddonRoom(bare, withAddons(bare.DeepCopy(), map[string]string{"monitoring": `{"enabled":true}`}), class); err == nil ||
		!strings.Contains(err.Error(), "each control plane needs 4608 MiB") {
		t.Errorf("monitoring switched on in a cluster whose control planes have no room for it: %v", err)
	}
}

// everyAddonButRancher adds to everyAddon logs, backups, the load balancer and Flux, as the
// end-to-end tests switch them on.
func everyAddonButRancher(c *clusterv1.Cluster) *clusterv1.Cluster {
	return withAddons(withAddons(c, everyAddon), map[string]string{
		"logs": `{"enabled":true}`, "backups": `{"enabled":true,"target":"store"}`, "gitops": `{"enabled":true,"engine":"flux"}`,
		"loadBalancer": `{"enabled":true,"implementation":"kube-vip","addresses":["192.0.2.100-192.0.2.110"]}`,
	})
}

func TestWorkersHaveCPUForTheAddons(t *testing.T) {
	t.Parallel()
	class := addonClass()
	for _, tc := range []struct {
		name string
		c    *clusterv1.Cluster
		want string
	}{
		{
			"every add-on but Rancher on the least worker", workerMemory(everyAddonButRancher(sized("", new(int32(1)))), `{"memoryMiB":8192}`),
			"add-ons (Headlamp, monitoring, logs, backups, Kyverno, Istio, Cluster Mesh, the load balancer, cert-manager, Rancher's agent, Flux) need 1555 millicores of CPU on workers, which have 1225 millicores for them",
		},
		{"every add-on but Rancher with a core more", workerMemory(everyAddonButRancher(sized("", new(int32(1)))), `{"cores":3,"memoryMiB":8192}`), ""},
		{"every add-on but Rancher shared over workers", workerMemory(everyAddonButRancher(sized("", new(int32(2)))), `{"memoryMiB":8192}`), ""},
		{
			"without workers, on two cores", withAddons(sized(`{"cores":2,"memoryMiB":16384}`), everyAddon),
			"need 1300 millicores of CPU on control planes without workers, which have 395 millicores",
		},
		{"without workers, on the least they need", withAddons(sized(`{"cores":4,"memoryMiB":16384}`), everyAddon), ""},
	} {
		err := checkAddonRoom(nil, tc.c, class)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
	// Add-ons whose pods ask for no CPU need no room for it: a class without a figure for Cluster
	// Mesh, cert-manager or Rancher's agent counts none for them.
	bare := addonClass()
	bare.Annotations[AddonCPUAnnotation] = `{"headlamp":10}`
	bare.Annotations[NodeCPUAnnotation] = `{"controlPlane":1605,"worker":2000}`
	if err := checkRoomFor(addonCPU, nil, withAddons(sized("", new(int32(1))), map[string]string{"clusterMesh": `{"enabled":true}`}), bare); err != nil {
		t.Errorf("only add-ons that request no CPU, on a worker with none to spare: %v", err)
	}
	// A class that says nothing of its nodes' own CPU counts their least for it.
	delete(bare.Annotations, NodeCPUAnnotation)
	if err := checkRoomFor(addonCPU, nil, withAddons(sized("", new(int32(1))), map[string]string{"headlamp": `{"enabled":true}`}), bare); err == nil ||
		!strings.Contains(err.Error(), "need 10 millicores of CPU on workers, which have 0 millicores") {
		t.Errorf("Headlamp on the least worker, without node figures: %v", err)
	}
	delete(bare.Annotations, AddonCPUAnnotation)
	if err := checkRoomFor(addonCPU, nil, everyAddonButRancher(sized("", new(int32(1)))), bare); err != nil {
		t.Errorf("a class without CPU figures: %v", err)
	}
}

func TestAddonCPUHoldsChangesThatLeaveLess(t *testing.T) {
	t.Parallel()
	class := addonClass()
	class.Annotations[AddonCPUAnnotation] = strings.Replace(addonCPUFigures, `"flux":50`, `"flux":500`, 1)
	short := workerMemory(everyAddonButRancher(sized("", new(int32(1)))), `{"memoryMiB":8192}`)
	edited := short.DeepCopy()
	edited.Labels = map[string]string{"team": "a"}
	if err := checkAddonRoom(short, edited, class); err != nil {
		t.Errorf("an unrelated edit to a cluster already short: %v", err)
	}
	if err := checkAddonRoom(short, withAddons(short.DeepCopy(), map[string]string{"rancher": `{"install":true,"hostname":"r.example.org"}`}), class); err == nil ||
		!strings.Contains(err.Error(), "millicores of CPU") {
		t.Errorf("Rancher switched on in a cluster already short of CPU: %v", err)
	}
}

func TestAddonRoomWithoutNodeFigures(t *testing.T) {
	t.Parallel()
	class := addonClass()
	delete(class.Annotations, NodeMemoryAnnotation)
	// A worker then uses its least itself, and a control plane without workers the least it needs.
	if err := checkAddonRoom(nil, sized("", new(int32(1))), class); err == nil || !strings.Contains(err.Error(), "on workers, which have 0 MiB") {
		t.Errorf("the agent on the least worker: %v", err)
	}
	if err := checkAddonRoom(nil, sized(`{"cores":4,"memoryMiB":8448}`), class); err != nil {
		t.Errorf("the agent without workers, with room beyond the control planes' least: %v", err)
	}
	if err := checkAddonRoom(nil, sized(""), &clusterv1.ClusterClass{}); err != nil {
		t.Errorf("a class without figures: %v", err)
	}
}

func TestAddonRoomHoldsChangesThatLeaveLess(t *testing.T) {
	t.Parallel()
	class := addonClass()
	short := withAddons(sized("", new(int32(1))), everyAddon)
	edited := short.DeepCopy()
	edited.Labels = map[string]string{"team": "a"}
	if err := checkAddonRoom(short, edited, class); err != nil {
		t.Errorf("an unrelated edit to a cluster already short: %v", err)
	}
	if err := checkAddonRoom(short, withAddons(short.DeepCopy(), map[string]string{"rancher": `{"install":true,"hostname":"r.example.org"}`}), class); err == nil {
		t.Error("Rancher switched on in a cluster already short")
	}
	if err := checkAddonRoom(short, workerMemory(short.DeepCopy(), `{"memoryMiB":4224}`), class); err != nil {
		t.Errorf("workers made larger in a cluster still short: %v", err)
	}
}
