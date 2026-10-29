package provisioner

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// autoscaledPoolOf is pool md-0, which the autoscaler scales from least to most workers.
func autoscaledPoolOf(least, most string) clusterv1.MachineDeploymentTopology {
	return clusterv1.MachineDeploymentTopology{
		Class: "worker", Name: "md-0",
		Metadata: clusterv1.ObjectMeta{Annotations: map[string]string{AutoscalerMinAnnotation: least, AutoscalerMaxAnnotation: most}},
	}
}

// withAddresses gives a networkCluster's nodes these addresses.
func withAddresses(c *clusterv1.Cluster, addresses string) *clusterv1.Cluster {
	for i := range c.Spec.Topology.Variables {
		if v := &c.Spec.Topology.Variables[i]; v.Name == "network" {
			v.Value = apiextensionsv1.JSON{Raw: []byte(`{"bridge":"vmbr0","addresses":` + addresses + `,"prefix":24,"gateway":"192.168.1.1","dnsServers":["192.168.1.1"]}`)}
		}
	}
	return c
}

func TestWorkerRange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		pool       clusterv1.MachineDeploymentTopology
		least, top int32
	}{
		{"replicas unset", clusterv1.MachineDeploymentTopology{}, 1, 1},
		{"three replicas", clusterv1.MachineDeploymentTopology{Replicas: new(int32(3))}, 3, 3},
		{"autoscaled", autoscaledPoolOf("0", "5"), 0, 5},
		// The validator refuses it; until then it counts as Cluster API runs it.
		{"bounds that do not read", autoscaledPoolOf("none", "5"), 1, 1},
	} {
		if got := workerRange(&tc.pool); got.least != tc.least || got.most != tc.top {
			t.Errorf("%s: %d to %d, want %d to %d", tc.name, got.least, got.most, tc.least, tc.top)
		}
	}
}

func TestValidateAutoscaling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pool clusterv1.MachineDeploymentTopology
		want string
	}{
		{"fixed", clusterv1.MachineDeploymentTopology{Name: "md-0", Replicas: new(int32(2))}, ""},
		{"from none", autoscaledPoolOf("0", "2"), ""},
		{"at the most", autoscaledPoolOf("100", "100"), ""},
		{"only a least", clusterv1.MachineDeploymentTopology{Name: "md-0", Metadata: clusterv1.ObjectMeta{Annotations: map[string]string{AutoscalerMinAnnotation: "1"}}}, "sets both"},
		{"not numbers", autoscaledPoolOf("one", "2"), "whole numbers"},
		{"least above most", autoscaledPoolOf("3", "2"), "never below its least"},
		{"most of none", autoscaledPoolOf("0", "0"), "from 1 to 100"},
		{"too many", autoscaledPoolOf("1", "101"), "from 1 to 100"},
		{"below none", autoscaledPoolOf("-1", "2"), "0 or more"},
		{"with replicas", func() clusterv1.MachineDeploymentTopology {
			p := autoscaledPoolOf("1", "2")
			p.Replicas = new(int32(1))
			return p
		}(), "leaves its number of workers to the cluster autoscaler"},
	} {
		c := cluster()
		c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{tc.pool}
		errs := validateAutoscaling(c)
		switch {
		case tc.want == "" && len(errs) > 0:
			t.Errorf("%s: %v", tc.name, errs)
		case tc.want != "" && (len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, errs, tc.want)
		}
	}
}

func TestAddressCount(t *testing.T) {
	t.Parallel()
	n, err := addressCount([]string{"10.0.0.32/28", "10.0.0.21-10.0.0.29", "10.0.0.50", "10.0.0.60/31"})
	if err != nil || n != 14+9+1+2 {
		t.Errorf("%d, %v, want 26", n, err)
	}
	if _, err := addressCount([]string{"10.0.0.9-10.0.0.1"}); err == nil {
		t.Error("a range backwards")
	}
}

func TestAutoscaledPoolsNeedAddressesAtTheirMost(t *testing.T) {
	t.Parallel()
	autoscaledOn := func(addresses, most string) *clusterv1.Cluster {
		c := withAddresses(networkCluster("192.168.1.1", 24), addresses)
		c.Spec.Topology.ControlPlane.Replicas = new(int32(3))
		c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{
			autoscaledPoolOf("1", most), {Class: "worker", Name: "md-1", Replicas: new(int32(2))},
		}
		return c
	}
	// Three control planes, four workers at most, two fixed and one for a rollout.
	if err := checkNodeAddresses(nil, autoscaledOn(`["192.168.1.10-192.168.1.19"]`, "4")); err != nil {
		t.Errorf("ten addresses for ten machines: %v", err)
	}
	err := checkNodeAddresses(nil, autoscaledOn(`["192.168.1.10-192.168.1.19"]`, "5"))
	if err == nil || !strings.Contains(err.Error(), "give 10, and the cluster may run 11 machines at once") {
		t.Errorf("ten addresses for eleven machines: %v", err)
	}
	// Pools of fixed size need the same room: a rollout waits for ever for an address otherwise.
	fixed := func(addresses string) *clusterv1.Cluster {
		c := withAddresses(networkCluster("192.168.1.1", 24), addresses)
		c.Spec.Topology.ControlPlane.Replicas = new(int32(3))
		c.Spec.Topology.Workers.MachineDeployments[0].Replicas = new(int32(2))
		return c
	}
	if err = checkNodeAddresses(nil, fixed(`["192.168.1.10-192.168.1.15"]`)); err != nil {
		t.Errorf("six addresses for three control planes, two workers and a rollout: %v", err)
	}
	err = checkNodeAddresses(nil, fixed(`["192.168.1.10-192.168.1.14"]`))
	if err == nil || !strings.Contains(err.Error(), "give 5, and the cluster may run 6 machines at once") {
		t.Errorf("five addresses for six machines in fixed pools: %v", err)
	}
	short := autoscaledOn(`["192.168.1.10-192.168.1.15"]`, "5")
	edited := short.DeepCopy()
	edited.Labels = map[string]string{"team": "a"}
	if err := checkNodeAddresses(short, edited); err != nil {
		t.Errorf("an unrelated edit of a cluster already short: %v", err)
	}
	if err := checkNodeAddresses(short, autoscaledOn(`["192.168.1.10-192.168.1.15"]`, "6")); err == nil {
		t.Error("a cluster already short of addresses grown further")
	}
}

func TestAutoscaledPoolsCountTheirLeastForAddonsAndTheirMostForControlPlanes(t *testing.T) {
	t.Parallel()
	class := addonClass()
	c := withAddons(sized(""), everyAddon)
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("0", "3")}
	err := checkAddonRoom(nil, c, class)
	if err == nil || !strings.Contains(err.Error(), "may go down to no workers") {
		t.Errorf("add-ons on a pool that may have no workers: %v", err)
	}
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("2", "3")}
	if err = checkAddonRoom(nil, c, class); err != nil {
		t.Errorf("add-ons on the least of two workers: %v", err)
	}
	// Without add-ons only Rancher's agent counts, and it holds no worker up: a pool from none keeps
	// the control planes tainted, so nothing runs there, and the agent's pod brings a worker up.
	bare := sized("")
	bare.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("0", "3")}
	room, err := addonRoomOf(addonMemory, bare, class)
	if err != nil || room.WithoutWorkers || len(room.Addons) != 0 || room.Need != 0 {
		t.Errorf("a pool from none: %+v, %v, want the workers to hold nothing", room, err)
	}
	if err = checkAddonRoom(nil, bare, class); err != nil {
		t.Errorf("a pool from none with only Rancher's agent: %v", err)
	}
	bare.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("1", "3")}
	if room, err = addonRoomOf(addonMemory, bare, class); err != nil || !slices.Contains(room.Addons, rancherAgentFigure) {
		t.Errorf("a pool that keeps a worker: %+v, %v, want Rancher's agent counted", room, err)
	}

	many := sized("")
	many.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("1", "300")}
	need, have, err := controlPlaneFit(many, class)
	if err != nil || have.Workers != 300 || need.Cores != 4 {
		t.Errorf("an autoscaled pool of up to 300: need %+v, have %+v, %v, want the control planes sized for 300", need, have, err)
	}
}

func TestCapacityAnnotations(t *testing.T) {
	t.Parallel()
	got := capacityAnnotations(machineSize{Cores: new(4), MemoryMiB: new(8192), DiskGiB: new(40)}, map[string]string{
		"node.cluster.x-k8s.io/pool":         "batch",
		"gpu.node.cluster.x-k8s.io/model":    "none",
		"node-role.kubernetes.io/batch":      "",
		clusterv1.ClusterNameLabel:           "web",
		"topology.kubernetes.io/zone":        "a",
		"example.org/node.cluster.x-k8s.io":  "no",
		"notnode.cluster.x-k8s.io.example/x": "no",
	})
	want := map[string]string{
		capacityPrefix + "cpu":            "4",
		capacityPrefix + "memory":         "7372Mi",
		capacityPrefix + "ephemeral-disk": "36864Mi",
		capacityPrefix + "labels":         "gpu.node.cluster.x-k8s.io/model=none,node-role.kubernetes.io/batch=,node.cluster.x-k8s.io/pool=batch",
	}
	if !maps.Equal(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
	bare := capacityAnnotations(machineSize{Cores: new(2), MemoryMiB: new(4096)}, nil)
	if _, ok := bare[capacityPrefix+"ephemeral-disk"]; ok {
		t.Error("a disk of no known size")
	}
	if _, ok := bare[capacityPrefix+"labels"]; ok {
		t.Error("labels without any")
	}
}

func TestBeforeClusterCreateRefusesAutoscaledPoolsItCannotScale(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	c := withAddresses(networkCluster("192.168.1.1", 24), `["192.168.1.10-192.168.1.20"]`)
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("3", "2")}
	if resp := callBeforeClusterCreate(t, hooks, c); !strings.Contains(resp.Message, "never below its least") {
		t.Errorf("least above most: %+v", resp)
	}
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("1", "20")}
	if resp := callBeforeClusterCreate(t, hooks, c); !strings.Contains(resp.Message, "the cluster may run 22 machines") {
		t.Errorf("more workers than addresses: %+v", resp)
	}
}

func TestClusterValidatorHoldsAutoscaledPools(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	c := withAddresses(networkCluster("192.168.1.1", 24), `["192.168.1.10-192.168.1.20"]`)
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("1", "3")}
	if _, err := v.ValidateCreate(context.Background(), c); err != nil {
		t.Errorf("a pool of one to three: %v", err)
	}
	both := c.DeepCopy()
	both.Spec.Topology.Workers.MachineDeployments[0].Replicas = new(int32(2))
	if _, err := v.ValidateUpdate(context.Background(), c, both); err == nil || !strings.Contains(err.Error(), "replicas") {
		t.Errorf("replicas beside the autoscaler's bounds: %v", err)
	}
}
