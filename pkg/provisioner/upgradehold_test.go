package provisioner

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// askUpgrade stores c and asks BeforeClusterUpgrade about its version, as Cluster API does.
func askUpgrade(t *testing.T, h *Hooks, c *clusterv1.Cluster) *runtimehooksv1.BeforeClusterUpgradeResponse {
	t.Helper()
	storeCluster(t, h.Reader, c)
	resp := &runtimehooksv1.BeforeClusterUpgradeResponse{}
	h.BeforeClusterUpgrade(context.Background(), &runtimehooksv1.BeforeClusterUpgradeRequest{Cluster: *c, ToKubernetesVersion: c.Spec.Topology.Version}, resp)
	return resp
}

// upgradeHeld is the stored cluster's UpgradeHeld condition, or nil.
func upgradeHeld(t *testing.T, kube client.Client, c *clusterv1.Cluster) *metav1.Condition {
	t.Helper()
	stored := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), stored); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(stored.Status.Conditions, UpgradeHeldCondition)
}

func TestEveryUpgradeHoldSetsItsReason(t *testing.T) {
	t.Parallel()
	talos := cluster()
	talos.Name = "edge"
	talos.Annotations = map[string]string{TalosUpgradingAnnotation: "edge-cp-0 to v1.15.1"}
	second := clusterWithRancher("rancher-2", true)
	second.Spec.Topology.Version = "v1.36.5+rke2r1"
	for _, tc := range []struct {
		name    string
		others  []client.Object
		cluster *clusterv1.Cluster
		reason  string
		message string
		retry   int32
	}{
		{
			name: "Rancher past its Kubernetes", cluster: clusterWithRancher("rancher", true),
			reason: UpgradeHeldRancherKubernetes, message: "for Rancher, choose Kubernetes 1.34 to 1.36, not v1.37.1+rke2r1", retry: 300,
		},
		{
			name: "a second Rancher", others: []client.Object{withVersion(clusterWithRancher("rancher", true), "v1.36.5+rke2r1")}, cluster: second,
			reason: UpgradeHeldRancherElsewhere, message: "cluster team-a/rancher already runs Rancher. Only one cluster may run it", retry: 300,
		},
		{
			name: "a Talos upgrade under way", cluster: talos,
			reason: UpgradeHeldTalosUpgrade, message: "waiting for the Talos upgrade of edge-cp-0 to v1.15.1", retry: 30,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, kube := hooksFor(t, tc.others...)
			h.Client = kube
			h.RancherKubernetes = KubernetesMinors{Min: 34, Max: 36}
			resp := askUpgrade(t, h, tc.cluster)
			if resp.Status != runtimehooksv1.ResponseStatusSuccess || resp.RetryAfterSeconds != tc.retry || resp.Message != tc.message {
				t.Errorf("hook answered %+v", resp)
			}
			got := upgradeHeld(t, kube, tc.cluster)
			if got == nil || got.Status != metav1.ConditionTrue || got.Reason != tc.reason || got.Message != tc.message {
				t.Fatalf("UpgradeHeld: %+v", got)
			}
			before := got.LastTransitionTime
			askUpgrade(t, h, tc.cluster)
			if again := upgradeHeld(t, kube, tc.cluster); again == nil || !again.LastTransitionTime.Equal(&before) {
				t.Errorf("asked again, UpgradeHeld moved: %+v", again)
			}
		})
	}
}

func withVersion(c *clusterv1.Cluster, version string) *clusterv1.Cluster {
	c.Spec.Topology.Version = version
	return c
}

func TestUpgradeHeldGoesOnceTheHoldDoes(t *testing.T) {
	t.Parallel()
	h, kube := hooksFor(t)
	h.Client = kube
	h.RancherKubernetes = KubernetesMinors{Min: 34, Max: 36}
	r := clusterWithRancher("rancher", true)
	r.Annotations = map[string]string{TalosUpgradingAnnotation: "rancher-cp-0 to v1.15.1"}
	askUpgrade(t, h, r)
	if got := upgradeHeld(t, kube, r); got == nil || got.Reason != UpgradeHeldRancherKubernetes {
		t.Fatalf("held past Rancher's Kubernetes: %+v", got)
	}
	r.Spec.Topology.Version = "v1.36.5+rke2r1"
	askUpgrade(t, h, r)
	if got := upgradeHeld(t, kube, r); got == nil || got.Reason != UpgradeHeldTalosUpgrade {
		t.Fatalf("held by the Talos upgrade alone: %+v", got)
	}
	r.Annotations = nil
	askUpgrade(t, h, r)
	if got := upgradeHeld(t, kube, r); got != nil {
		t.Errorf("nothing holds the upgrade, yet: %+v", got)
	}
}

func TestUpgradeHeldWithoutRancherRange(t *testing.T) {
	t.Parallel()
	h, kube := hooksFor(t)
	h.Client = kube
	r := clusterWithRancher("rancher", true)
	askUpgrade(t, h, r)
	if got := upgradeHeld(t, kube, r); got != nil {
		t.Errorf("no Rancher range set, yet held: %+v", got)
	}
}

func TestControllerClearsAHoldThatNoLongerHolds(t *testing.T) {
	t.Parallel()
	held := func(name, version, reason string) *clusterv1.Cluster {
		c := withVersion(clusterWithRancher(name, name == "rancher"), version)
		c.Status.Conditions = []metav1.Condition{{
			Type: UpgradeHeldCondition, Status: metav1.ConditionTrue, Reason: reason, Message: "held", LastTransitionTime: metav1.Now(),
		}}
		return c
	}
	talosUnderWay := held("edge", "v1.37.1+rke2r1", UpgradeHeldTalosUpgrade)
	talosUnderWay.Annotations = map[string]string{TalosUpgradingAnnotation: "edge-cp-0 to v1.15.1"}
	for _, tc := range []struct {
		name    string
		cluster *clusterv1.Cluster
		stays   bool
	}{
		{"Rancher still past its Kubernetes", held("rancher", "v1.37.1+rke2r1", UpgradeHeldRancherKubernetes), true},
		{"Rancher runs on the version again", held("rancher", "v1.36.5+rke2r1", UpgradeHeldRancherKubernetes), false},
		{"the Talos upgrade still under way", talosUnderWay, true},
		{"the Talos upgrade done", held("edge", "v1.37.1+rke2r1", UpgradeHeldTalosUpgrade), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, kube := newReconciler(t, nil, tc.cluster)
			r.RancherKubernetes = KubernetesMinors{Min: 34, Max: 36}
			stored := &clusterv1.Cluster{}
			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(tc.cluster), stored); err != nil {
				t.Fatal(err)
			}
			if err := r.clearUpgradeHeld(t.Context(), stored); err != nil {
				t.Fatal(err)
			}
			if got := upgradeHeld(t, kube, tc.cluster); (got != nil) != tc.stays {
				t.Errorf("UpgradeHeld %+v, want it kept: %t", got, tc.stays)
			}
		})
	}
}

func TestEveryUpgradeHoldHasARetry(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{UpgradeHeldRancherKubernetes, UpgradeHeldRancherElsewhere, UpgradeHeldTalosUpgrade} {
		if upgradeHoldRetry[reason] <= 0 {
			t.Errorf("%s has no retry", reason)
		}
	}
}
