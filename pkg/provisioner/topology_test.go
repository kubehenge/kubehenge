package provisioner

import (
	"context"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func TestClusterValidatorHoldsTopologyToWhatTheInterfaceOffers(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	base := func() *clusterv1.Cluster {
		c := cluster()
		c.Spec.Topology.ControlPlane.Replicas = new(int32(1))
		c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
		return c
	}
	if _, err := v.ValidateCreate(context.Background(), base()); err != nil {
		t.Fatalf("a cluster the interface would make: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(c *clusterv1.Cluster)
		want   string
	}{
		{"a name over 40 characters", func(c *clusterv1.Cluster) { c.Name = strings.Repeat("a", MaxClusterName+1) }, "up to 40"},
		{"a name with a capital", func(c *clusterv1.Cluster) { c.Name = "Web" }, "lowercase letters"},
		{"a version without RKE2's build", func(c *clusterv1.Cluster) { c.Spec.Topology.Version = "v1.37.1" }, "use the form v1.36.5+rke2r1"},
		{"two control planes", func(c *clusterv1.Cluster) { c.Spec.Topology.ControlPlane.Replicas = new(int32(2)) }, "1, 3, 5 or 7"},
		{"nine control planes", func(c *clusterv1.Cluster) { c.Spec.Topology.ControlPlane.Replicas = new(int32(9)) }, "1, 3, 5 or 7"},
		{"a pool of 101", func(c *clusterv1.Cluster) { c.Spec.Topology.Workers.MachineDeployments[0].Replicas = new(int32(101)) }, "workers: 0 to 100 in each pool"},
	} {
		c := base()
		tc.change(c)
		if _, err := v.ValidateCreate(context.Background(), c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestClusterValidatorHoldsUpgradesToOneMinorUp(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	old := cluster()
	for version, want := range map[string]string{
		"v1.37.2+rke2r1": "",
		"v1.38.0+rke2r1": "",
		"v1.39.0+rke2r1": "one minor version at most",
		"v1.36.9+rke2r1": "never down",
		"v1.38.0+k3s1":   "use the form v1.36.5+rke2r1",
	} {
		c := old.DeepCopy()
		c.Spec.Topology.Version = version
		_, err := v.ValidateUpdate(context.Background(), old, c)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", version, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", version, err, want)
		}
	}
	long := old.DeepCopy()
	long.Name = strings.Repeat("a", MaxClusterName+1)
	labelled := long.DeepCopy()
	labelled.Labels = map[string]string{"team": "a"}
	if _, err := v.ValidateUpdate(context.Background(), long, labelled); err != nil {
		t.Errorf("an edit of a cluster made before the name rule: %v", err)
	}
}

func TestVersionRulesMatchEachDistribution(t *testing.T) {
	t.Parallel()
	for d, good := range map[templates.Distribution]string{
		templates.Kubeadm: "v1.37.1", templates.RKE2: "v1.37.1+rke2r2", templates.K3s: "v1.37.1+k3s1", templates.Talos: "v1.37.1",
	} {
		if msg := VersionRule(d, good); msg != "" {
			t.Errorf("%s %s: %s", d, good, msg)
		}
		if msg := VersionRule(d, "1.37.1"); msg == "" {
			t.Errorf("%s accepts a version without its v", d)
		}
		if msg := VersionRule(d, VersionExample(d)); msg != "" {
			t.Errorf("%s's own example: %s", d, msg)
		}
	}
}
