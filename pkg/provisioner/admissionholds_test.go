package provisioner

import (
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// The interface refuses these changes, so admission has to refuse them too: a cluster applied with
// kubectl or by GitOps is held to the same rules.

func updateOf(t *testing.T, h *Hooks, old, c *clusterv1.Cluster) error {
	t.Helper()
	_, err := (&ClusterValidator{Hooks: h}).ValidateUpdate(t.Context(), old, c)
	return err
}

// changedVariable is a copy of the cluster with the variable set to raw.
func changedVariable(c *clusterv1.Cluster, name, raw string) *clusterv1.Cluster {
	out := c.DeepCopy()
	for i := range out.Spec.Topology.Variables {
		if out.Spec.Topology.Variables[i].Name == name {
			out.Spec.Topology.Variables[i].Value.Raw = []byte(raw)
			return out
		}
	}
	out.Spec.Topology.Variables = append(out.Spec.Topology.Variables, variableOf(name, raw))
	return out
}

func TestAdmissionRefusesAVariableFixedAtCreationToChange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, was, now string }{
		{storageVariable, `"ceph"`, `"tank"`},
		{"controlPlaneEndpoint", `"192.0.2.10"`, `"192.0.2.11"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			old := storedCluster("team-a", "web", "ceph", variableOf("controlPlaneEndpoint", `"192.0.2.10"`))
			h, _ := hooksFor(t, old)
			err := updateOf(t, h, old, changedVariable(old, tc.name, tc.now))
			if err == nil || !strings.Contains(err.Error(), tc.name+" cannot change after the cluster is created") {
				t.Errorf("changing %s from %s to %s: %v", tc.name, tc.was, tc.now, err)
			}
		})
	}
}

func TestAdmissionAdmitsAFixedVariableWrittenAgain(t *testing.T) {
	t.Parallel()
	old := storedCluster("team-a", "web", "ceph", variableOf("controlPlaneEndpoint", `"192.0.2.10"`))
	h, _ := hooksFor(t, old)
	same := changedVariable(changedVariable(old, storageVariable, ` "ceph" `), "controlPlaneEndpoint", `"192.0.2.10"`)
	same = changedVariable(same, "headlamp", `{"enabled":true}`)
	if err := updateOf(t, h, old, same); err != nil {
		t.Errorf("an update that keeps the storage and the endpoint was refused: %v", err)
	}
}
