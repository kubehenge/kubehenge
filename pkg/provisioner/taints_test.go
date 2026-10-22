package provisioner

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func controlPlaneNode(name string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{Name: name, Labels: map[string]string{controlPlaneRole: "true"}, Spec: corev1.NodeSpec{Taints: taints}}
}

func workerNode(name string) *corev1.Node {
	return &corev1.Node{Name: name}
}

func taintsOf(t *testing.T, c client.Client, name string) []corev1.Taint {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, n); err != nil {
		t.Fatal(err)
	}
	return n.Spec.Taints
}

func hasControlPlaneTaint(taints []corev1.Taint) bool {
	return slices.ContainsFunc(taints, func(t corev1.Taint) bool { return controlPlaneTaint.MatchTaint(&t) })
}

func TestControlPlanesTaintedOnceAWorkerJoins(t *testing.T) {
	t.Parallel()
	other := corev1.Taint{Key: "example.com/dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}
	for _, tc := range []struct {
		name         string
		wantsWorkers bool
		autoscaled   bool
		worker       bool
		tainted      bool
	}{
		{"workers wanted and one joined", true, false, true, true},
		// Until a worker joins, the control planes are all there is to run the add-ons' workloads.
		{"workers wanted, none joined yet", true, false, false, false},
		// Scaled to none: the workers being drained are still there, and their pods need somewhere to go.
		{"no workers wanted", false, false, true, false},
		{"no workers at all", false, false, false, false},
		// A pending workload is what has the autoscaler add the first worker.
		{"autoscaled, no worker yet", true, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objects := []client.Object{
				controlPlaneNode("cp-1", other),
				controlPlaneNode("cp-2", controlPlaneTaint),
			}
			if tc.worker {
				objects = append(objects, workerNode("worker-1"))
			}
			workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(objects...).Build()
			if err := taintControlPlanes(context.Background(), workload, tc.wantsWorkers, tc.autoscaled); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"cp-1", "cp-2"} {
				taints := taintsOf(t, workload, name)
				if hasControlPlaneTaint(taints) != tc.tainted {
					t.Errorf("%s has taints %v, want the control-plane taint %v", name, taints, tc.tainted)
				}
				if c := len(slices.DeleteFunc(slices.Clone(taints), func(t corev1.Taint) bool { return controlPlaneTaint.MatchTaint(&t) })); name == "cp-1" && c != 1 {
					t.Errorf("cp-1 lost its other taint: %v", taints)
				}
			}
			if tc.worker && len(taintsOf(t, workload, "worker-1")) != 0 {
				t.Errorf("the worker got taints: %v", taintsOf(t, workload, "worker-1"))
			}
		})
	}
}

func TestWantsWorkers(t *testing.T) {
	t.Parallel()
	withDeployments := func(replicas ...*int32) *clusterv1.Cluster {
		c := &clusterv1.Cluster{}
		for _, r := range replicas {
			c.Spec.Topology.Workers.MachineDeployments = append(c.Spec.Topology.Workers.MachineDeployments, clusterv1.MachineDeploymentTopology{Replicas: r})
		}
		return c
	}
	for _, tc := range []struct {
		name    string
		cluster *clusterv1.Cluster
		want    bool
	}{
		{"no machine deployments", withDeployments(), false},
		{"scaled to none", withDeployments(new(int32(0))), false},
		{"one of two has workers", withDeployments(new(int32(0)), new(int32(2))), true},
		// Cluster API gives a machine deployment without replicas one.
		{"replicas unset", withDeployments(nil), true},
		{"autoscaled from none", func() *clusterv1.Cluster {
			c := withDeployments(nil)
			c.Spec.Topology.Workers.MachineDeployments[0].Metadata.Annotations = map[string]string{AutoscalerMinAnnotation: "0", AutoscalerMaxAnnotation: "2"}
			return c
		}(), true},
	} {
		if got := wantsWorkers(tc.cluster); got != tc.want {
			t.Errorf("%s: wantsWorkers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
