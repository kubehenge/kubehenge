package provisioner

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

func deleting(c *clusterv1.Cluster) *clusterv1.Cluster {
	out := c.DeepCopy()
	out.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	out.Finalizers = []string{"cluster.cluster.x-k8s.io", "kubehenge.dev/cluster"}
	return out
}

func TestAdmissionRefusesASpecChangeToAClusterBeingDeleted(t *testing.T) {
	t.Parallel()
	old := deleting(storedCluster("team-a", "web", "ceph"))
	h, _ := hooksFor(t, old)
	for name, change := range map[string]func(*clusterv1.Cluster){
		"a variable":  func(c *clusterv1.Cluster) { c.Spec.Topology.Variables = withVariable(c, "headlamp", `{"enabled":true}`).Spec.Topology.Variables },
		"the version": func(c *clusterv1.Cluster) { c.Spec.Topology.Version = "v1.37.2+rke2r1" },
		"a pool": func(c *clusterv1.Cluster) {
			c.Spec.Topology.Workers = clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{{Name: "pool"}}}
		},
	} {
		c := old.DeepCopy()
		change(c)
		if err := updateOf(t, h, old, c); err == nil || !strings.Contains(err.Error(), "the cluster is being deleted") {
			t.Errorf("a change of %s to a cluster being deleted: %v", name, err)
		}
	}
}

func TestAdmissionAdmitsWhatTheControllersWriteToAClusterBeingDeleted(t *testing.T) {
	t.Parallel()
	old := deleting(storedCluster("team-a", "web", "ceph"))
	h, _ := hooksFor(t, old)
	for name, change := range map[string]func(*clusterv1.Cluster){
		"a finalizer removed":    func(c *clusterv1.Cluster) { c.Finalizers = c.Finalizers[:1] },
		"every finalizer gone":   func(c *clusterv1.Cluster) { c.Finalizers = nil },
		"an annotation written":  func(c *clusterv1.Cluster) { c.Annotations = map[string]string{DeletionStatusAnnotation: "{}"} },
		"the abandon annotation": func(c *clusterv1.Cluster) { c.Annotations = map[string]string{AbandonAnnotation: c.Name} },
		"a label":                func(c *clusterv1.Cluster) { c.Labels = map[string]string{"a": "b"} },
	} {
		c := old.DeepCopy()
		change(c)
		if err := updateOf(t, h, old, c); err != nil {
			t.Errorf("%s on a cluster being deleted was refused: %v", name, err)
		}
	}
}
