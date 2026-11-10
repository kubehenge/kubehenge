package provisioner

import (
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func asObjects(cs []*clusterv1.Cluster) []client.Object {
	out := make([]client.Object, len(cs))
	for i, c := range cs {
		out[i] = c
	}
	return out
}

// withWorker gives the cluster one worker, so that its control planes run no workloads.
func withWorker(c *clusterv1.Cluster) *clusterv1.Cluster {
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
	return c
}

// placementHooks know three nodes, two of them online.
func placementHooks(t *testing.T, objs ...*clusterv1.Cluster) *Hooks {
	t.Helper()
	h, _ := hooksFor(t, asObjects(objs)...)
	h.Nodes = &fakeNodes{online: []string{"pve1", "pve2"}}
	h.Sites = fakeSites{sites: &Sites{Nodes: []string{"pve1", "pve2", "pve3"}}}
	return h
}

func TestAdmissionHoldsTheAllowedNodesAsTheInterfaceDoes(t *testing.T) {
	t.Parallel()
	old := storedCluster("team-a", "web", "ceph", variableOf(allowedNodesVariable, `["pve1","pve2"]`))
	h := placementHooks(t, old)
	for _, tc := range []struct{ name, nodes, refusal string }{
		{"none", `[]`, "choose at least one Proxmox VE node"},
		{"one twice", `["pve1","pve1"]`, "pve1 is listed twice"},
		{"one Proxmox VE lacks", `["pve8"]`, "no Proxmox VE node pve8"},
		{"one that is offline", `["pve3"]`, ""},
		{"one kept and one dropped", `["pve2"]`, ""},
	} {
		err := updateOf(t, h, old, withVariable(old, allowedNodesVariable, tc.nodes))
		switch {
		case tc.refusal == "" && err != nil:
			t.Errorf("allowedNodes %s (%s) was refused: %v", tc.nodes, tc.name, err)
		case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("allowedNodes %s (%s): %v, want %q", tc.nodes, tc.name, err, tc.refusal)
		}
	}
}

func TestAdmissionRefusesNoNodesInANewCluster(t *testing.T) {
	t.Parallel()
	h := placementHooks(t)
	c := withWorker(storedCluster("team-a", "web", "ceph", variableOf(allowedNodesVariable, `[]`)))
	if _, err := (&ClusterValidator{Hooks: h}).ValidateCreate(t.Context(), c); err == nil || !strings.Contains(err.Error(), "choose at least one Proxmox VE node") {
		t.Errorf("a new cluster with an empty list of nodes: %v", err)
	}
}

func TestAdmissionKeepsANodeTheClusterHadWhenItIsUnknownNow(t *testing.T) {
	t.Parallel()
	old := storedCluster("team-a", "web", "ceph", variableOf(allowedNodesVariable, `["pve1","pve9"]`))
	h := placementHooks(t, old)
	if err := updateOf(t, h, old, withVariable(old, allowedNodesVariable, `["pve9","pve1","pve2"]`)); err != nil {
		t.Errorf("keeping a node the cluster already listed was refused: %v", err)
	}
}
