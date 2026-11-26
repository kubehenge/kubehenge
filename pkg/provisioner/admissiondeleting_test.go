package provisioner

import (
	"slices"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// beingDeleted is a copy of c being deleted, held by the finalizers Cluster API and Kubehenge set.
func beingDeleted(c *clusterv1.Cluster) *clusterv1.Cluster {
	out := c.DeepCopy()
	out.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	out.Finalizers = []string{"cluster.cluster.x-k8s.io", "kubehenge.dev/cluster"}
	return out
}

func TestAdmissionRefusesASpecChangeToAClusterBeingDeleted(t *testing.T) {
	t.Parallel()
	old := beingDeleted(storedCluster("team-a", "web", "ceph"))
	h, _ := hooksFor(t, old)
	for name, change := range map[string]func(*clusterv1.Cluster){
		"a variable": func(c *clusterv1.Cluster) {
			c.Spec.Topology.Variables = changedVariable(c, "headlamp", `{"enabled":true}`).Spec.Topology.Variables
		},
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

// A cluster that still sets a retired variable can lose it while it is being deleted, since Cluster
// API refuses every other write to it, the removal of its last finalizer included. Nothing else of
// its spec changes with it.
func TestAdmissionLetsAClusterBeingDeletedLoseARetiredVariable(t *testing.T) {
	t.Parallel()
	old := beingDeleted(storedCluster("team-a", "web", "ceph"))
	old.Spec.Topology.Variables = append(old.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "useExisting", Value: apiextensionsv1.JSON{Raw: []byte(`{"certManager":true}`)}})
	h, _ := hooksFor(t, old)
	stripped := old.DeepCopy()
	stripped.Spec.Topology.Variables = slices.DeleteFunc(stripped.Spec.Topology.Variables, retired)
	if err := updateOf(t, h, old, stripped); err != nil {
		t.Errorf("taking the retired variable off a cluster being deleted: %v", err)
	}
	for name, change := range map[string]func(*clusterv1.Cluster){
		"another variable with it": func(c *clusterv1.Cluster) {
			c.Spec.Topology.Variables = changedVariable(c, "headlamp", `{"enabled":true}`).Spec.Topology.Variables
		},
		"the version with it": func(c *clusterv1.Cluster) { c.Spec.Topology.Version = "v1.37.2+rke2r1" },
	} {
		c := stripped.DeepCopy()
		change(c)
		if err := updateOf(t, h, old, c); err == nil || !strings.Contains(err.Error(), ClusterBeingDeleted) {
			t.Errorf("%s, on a cluster being deleted: %v", name, err)
		}
	}
	changed := old.DeepCopy()
	for i := range changed.Spec.Topology.Variables {
		if changed.Spec.Topology.Variables[i].Name == "useExisting" {
			changed.Spec.Topology.Variables[i].Value.Raw = []byte(`{"certManager":false}`)
		}
	}
	if err := updateOf(t, h, old, changed); err == nil || !strings.Contains(err.Error(), ClusterBeingDeleted) {
		t.Errorf("a retired variable changed rather than taken off: %v", err)
	}
}

func TestAdmissionLeavesAClusterBeingDeletedToDeleteNotToTheCiliumCheck(t *testing.T) {
	t.Parallel()
	old := beingDeleted(storedCluster("team-a", "web", "ceph"))
	old.Annotations = map[string]string{CiliumClusterIDAnnotation: "7"}
	h, _ := hooksFor(t, old)
	c := old.DeepCopy()
	c.Annotations = map[string]string{AbandonAnnotation: c.Name}
	if err := updateOf(t, h, old, c); err != nil {
		t.Errorf("abandon-deletion with the Cilium id dropped, on a cluster being deleted: %v", err)
	}
}

func TestAdmissionAdmitsPausingAndTheEndpointOfAClusterBeingDeleted(t *testing.T) {
	t.Parallel()
	old := beingDeleted(storedCluster("team-a", "web", "ceph"))
	h, _ := hooksFor(t, old)
	c := old.DeepCopy()
	c.Spec.Paused = new(true)
	c.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "192.0.2.9", Port: 6443}
	if err := updateOf(t, h, old, c); err != nil {
		t.Errorf("pausing and the endpoint on a cluster being deleted: %v", err)
	}
}

func TestAdmissionAdmitsWhatTheControllersWriteToAClusterBeingDeleted(t *testing.T) {
	t.Parallel()
	old := beingDeleted(storedCluster("team-a", "web", "ceph"))
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
