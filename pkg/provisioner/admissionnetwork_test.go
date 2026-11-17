package provisioner

import (
	"strings"
	"testing"
)

func TestNetworkIsFixedForTheClustersLife(t *testing.T) {
	t.Parallel()
	old := familyCluster(ipv4Network, ipv4Endpoint)
	h, _ := hooksFor(t, old)
	moved := strings.Replace(strings.Replace(ipv4Network, `"vmbr0"`, `"vmbr9"`, 1), `"bridge":"vmbr9",`, `"bridge":"vmbr9","vlan":30,`, 1)
	err := updateOf(t, h, old, familyCluster(moved, ipv4Endpoint))
	for _, want := range []string{"the bridge is chosen when the cluster is created", "the VLAN is chosen when the cluster is created"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("admission of a moved network gave %v, want %q", err, want)
		}
	}
	if err = updateOf(t, h, old, old.DeepCopy()); err != nil {
		t.Errorf("an unchanged network was refused: %v", err)
	}
}
