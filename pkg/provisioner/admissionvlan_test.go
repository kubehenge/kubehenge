package provisioner

import (
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// onNetwork is a sound cluster on the bridge, with the VLAN tag when it is not 0.
func onNetwork(t *testing.T, bridge string, vlan int, extra ...clusterv1.ClusterVariable) *clusterv1.Cluster {
	t.Helper()
	network := map[string]any{"bridge": bridge, "addresses": []string{"192.0.2.0/24"}, "prefix": 24, "gateway": "192.0.2.1", "dnsServers": []string{"192.0.2.1"}}
	if vlan != 0 {
		network["vlan"] = vlan
	}
	return withWorker(storedCluster("team-a", "web", "ceph", append([]clusterv1.ClusterVariable{clusterVariable(t, "network", network)}, extra...)...))
}

func TestAdmissionRefusesAVLANTagOnAVNetThatTagsAlready(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	h.Sites = fakeSites{sites: &Sites{
		VNets:        map[string]string{"tagged": "z1", "aware": "z2"},
		TaggingVNets: map[string]bool{"tagged": true},
	}}
	const refusal = "VNet tagged is in a VLAN zone and tags already. Leave the VLAN tag empty."
	for _, tc := range []struct {
		name, bridge string
		vlan         int
		refused      bool
	}{
		{"a tag on a tagging VNet", "tagged", 100, true},
		{"no tag on a tagging VNet", "tagged", 0, false},
		{"a tag on a VLAN-aware VNet", "aware", 100, false},
		{"a tag on a node's bridge", "vmbr0", 100, false},
	} {
		_, err := (&ClusterValidator{Hooks: h}).ValidateCreate(t.Context(), onNetwork(t, tc.bridge, tc.vlan))
		if !tc.refused && err != nil {
			t.Errorf("%s was refused: %v", tc.name, err)
		}
		if tc.refused && (err == nil || !strings.Contains(err.Error(), refusal)) {
			t.Errorf("%s: %v, want %q", tc.name, err, refusal)
		}
	}
}

func TestAdmissionRefusesAVLANTagAddedToAClusterOnATaggingVNet(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	h.Sites = fakeSites{sites: &Sites{VNets: map[string]string{"tagged": "z1"}, TaggingVNets: map[string]bool{"tagged": true}}}
	old := onNetwork(t, "tagged", 0)
	if err := updateOf(t, h, old, onNetwork(t, "tagged", 100)); err == nil || !strings.Contains(err.Error(), "tags already") {
		t.Errorf("adding a tag to a cluster on a tagging VNet: %v", err)
	}
	other := changedVariable(onNetwork(t, "tagged", 100), "headlamp", `{"enabled":true}`)
	if err := updateOf(t, h, onNetwork(t, "tagged", 100), other); err != nil && strings.Contains(err.Error(), "tags already") {
		t.Errorf("an unrelated change to a cluster that already had the tag was held to it: %v", err)
	}
}
