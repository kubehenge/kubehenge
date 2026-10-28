package provisioner

import (
	"context"
	"strconv"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

// rke2Sizes is RKE2's server sizing guide as the chart gives it.
const rke2Sizes = `[{"workers":0,"cores":2,"memoryMiB":4096},{"workers":226,"cores":4,"memoryMiB":8192},{"workers":451,"cores":8,"memoryMiB":16384},{"workers":1301,"cores":16,"memoryMiB":32768}]`

// pools are as many pools of at most MaxWorkers each as hold workers.
func pools(workers int32) []*int32 {
	var out []*int32
	for ; workers > 0; workers -= min(workers, MaxWorkers) {
		out = append(out, new(min(workers, MaxWorkers)))
	}
	return out
}

// sized is a cluster with machine deployments of these replicas, nil for one that leaves them
// unset, and control planes of controlPlane, or the class's default when it is empty.
func sized(controlPlane string, replicas ...*int32) *clusterv1.Cluster {
	c := networkCluster("192.168.1.1", 16)
	if controlPlane != "" {
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "controlPlaneMachine", Value: apiextensionsv1.JSON{Raw: []byte(controlPlane)}})
	}
	c.Spec.Topology.Workers.MachineDeployments = nil
	for i, r := range replicas {
		c.Spec.Topology.Workers.MachineDeployments = append(c.Spec.Topology.Workers.MachineDeployments,
			clusterv1.MachineDeploymentTopology{Class: "worker", Name: "md-" + strconv.Itoa(i), Replicas: r})
	}
	return c
}

func TestControlPlanesFollowTheSizingGuide(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	for _, tc := range []struct {
		name string
		c    *clusterv1.Cluster
		want string
	}{
		{"a few workers on the class's default", sized("", new(int32(3))), ""},
		{"no workers on the default", sized(""), "without workers run its workloads too and need at least 4 vCPUs and 8192 MiB of memory, and have 2 vCPUs and 4096 MiB"},
		{"pools scaled to none", sized("", new(int32(0)), new(int32(0))), "without workers"},
		{"no workers on a control plane and a worker's least", sized(`{"cores":4,"memoryMiB":8192}`), ""},
		{"no workers, memory short", sized(`{"cores":4,"memoryMiB":6144}`), "have 4 vCPUs and 6144 MiB"},
		{"226 workers on the default", sized("", pools(226)...), "with 226 workers need at least 4 vCPUs and 8192 MiB of memory, and have 2 vCPUs and 4096 MiB"},
		{"226 workers across uneven pools", sized("", new(int32(100)), new(int32(26)), new(int32(100))), "with 226 workers"},
		{"a pool without replicas starts with one", sized("", append(pools(225), nil)...), "with 226 workers"},
		{"225 workers on the default", sized("", pools(225)...), ""},
		{"control planes sized for their workers", sized(`{"cores":4,"memoryMiB":8192}`, pools(450)...), ""},
		{"memory left to the default", sized(`{"cores":4}`, pools(300)...), "have 4 vCPUs and 4096 MiB"},
		{"vCPUs short", sized(`{"cores":2,"memoryMiB":16384}`, pools(300)...), "have 2 vCPUs and 16384 MiB"},
		{"past the guide's last step", sized(`{"cores":8,"memoryMiB":16384}`, pools(2000)...), "need at least 16 vCPUs and 32768 MiB"},
	} {
		created := callBeforeClusterCreate(h, tc.c)
		switch {
		case tc.want == "" && created.Status == runtimehooksv1.ResponseStatusFailure:
			t.Errorf("%s: refused: %s", tc.name, created.Message)
		case tc.want != "" && (created.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(created.Message, tc.want)):
			t.Errorf("%s: %s %q, want a refusal naming %q", tc.name, created.Status, created.Message, tc.want)
		}
	}
}

func TestClusterValidatorHoldsControlPlanesToTheirWorkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	few, many := sized("", new(int32(3))), sized("", pools(300)...)
	larger := sized(`{"cores":4,"memoryMiB":6144}`, pools(300)...)
	enough := sized(`{"cores":4,"memoryMiB":8192}`, pools(300)...)
	if _, err := v.ValidateCreate(ctx, many); err == nil || !strings.Contains(err.Error(), "need at least 4 vCPUs") {
		t.Errorf("a new cluster of 300 workers on the default control planes: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, few, many); err == nil {
		t.Error("workers scaled past what the control planes carry")
	}
	if _, err := v.ValidateUpdate(ctx, enough, many); err == nil {
		t.Error("control planes cut below what the workers need")
	}
	// Already short, as once a newer class asks more: an unrelated edit goes through, and so does a
	// fix, but more workers or smaller control planes do not.
	edited := many.DeepCopy()
	edited.Labels = map[string]string{"team": "a"}
	if _, err := v.ValidateUpdate(ctx, many, edited); err != nil {
		t.Errorf("an unrelated edit to a cluster already short: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, many, larger); err != nil {
		t.Errorf("a short cluster's control planes made larger: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, many, sized("", pools(500)...)); err == nil {
		t.Error("more workers on control planes already short of them")
	}
	if _, err := v.ValidateUpdate(ctx, larger, many); err == nil {
		t.Error("control planes already short made smaller")
	}
	if _, err := v.ValidateUpdate(ctx, many, enough); err != nil {
		t.Errorf("a short cluster's control planes sized for its workers: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, few, sized("", new(int32(0)))); err == nil || !strings.Contains(err.Error(), "without workers") {
		t.Errorf("the last workers taken from control planes that cannot carry the workloads: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, sized(`{"cores":4,"memoryMiB":8192}`, new(int32(1))), sized(`{"cores":4,"memoryMiB":8192}`, new(int32(0)))); err != nil {
		t.Errorf("the last workers taken from control planes that carry the workloads: %v", err)
	}
}
