package provisioner

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
)

const testManagementAPI = "https://192.0.2.10:6443"

// autoscalerPlaced is an autoscaled cluster whose autoscaler runs where placement says.
func autoscalerPlaced(placement string) *clusterv1.Cluster {
	c := withAddresses(networkCluster("192.168.1.1", 24), `["192.168.1.10-192.168.1.20"]`)
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, variableOf(AutoscalerVariable, `{"placement":"`+placement+`"}`))
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("1", "3")}
	return c
}

func TestAutoscalerInClusterNeedsTheManagementAddress(t *testing.T) {
	t.Parallel()
	const refused = "installed without the management cluster's address"
	inCluster := autoscalerPlaced(PlacementWorkload)
	grown := inCluster.DeepCopy()
	grown.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("1", "4")}
	notScaled := autoscalerPlaced(PlacementWorkload)
	notScaled.Spec.Topology.Workers.MachineDeployments = nil
	for name, tc := range map[string]struct {
		old, c *clusterv1.Cluster
		api    string
		want   string
	}{
		"new, in the cluster without the address":    {nil, inCluster, "", refused},
		"new, in the cluster with it":                {nil, inCluster, testManagementAPI, ""},
		"new, in the management cluster":             {nil, autoscalerPlaced(PlacementManagement), "", ""},
		"new, no pool is autoscaled":                 {nil, notScaled, "", ""},
		"moved into the cluster without the address": {autoscalerPlaced(PlacementManagement), inCluster, "", refused},
		"moved into the cluster with it":             {autoscalerPlaced(PlacementManagement), inCluster, testManagementAPI, ""},
		"already in the cluster, another change":     {inCluster, grown, "", ""},
		"moved out of the cluster":                   {inCluster, autoscalerPlaced(PlacementManagement), "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			errs := validateAutoscaling(tc.old, tc.c, tc.api).ToAggregate()
			switch {
			case tc.want == "" && errs != nil:
				t.Errorf("refused: %v", errs)
			case tc.want != "" && (errs == nil || !strings.Contains(errs.Error(), tc.want)):
				t.Errorf("got %v, want %q", errs, tc.want)
			}
		})
	}
}

// The webhook and the create hook both judge the placement by the address they were given.
func TestAdmissionJudgesThePlacementByTheManagementAddress(t *testing.T) {
	t.Parallel()
	const refused = "installed without the management cluster's address"
	for _, api := range []string{"", testManagementAPI} {
		hooks, _ := hooksFor(t)
		hooks.ManagementAPI = api
		_, err := (&ClusterValidator{Hooks: hooks}).ValidateCreate(context.Background(), autoscalerPlaced(PlacementWorkload))
		resp := callBeforeClusterCreate(t, hooks, autoscalerPlaced(PlacementWorkload))
		if api == "" {
			if err == nil || !strings.Contains(err.Error(), refused) {
				t.Errorf("webhook without the address: %v", err)
			}
			if resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, refused) {
				t.Errorf("create hook without the address: %+v", resp)
			}
			continue
		}
		if err != nil {
			t.Errorf("webhook with the address: %v", err)
		}
		if resp.Status != runtimehooksv1.ResponseStatusSuccess {
			t.Errorf("create hook with the address: %+v", resp)
		}
	}
}

// etcdAt is a cluster with an etcd limit of quota GiB and, when status is set, that read recorded.
func etcdAt(t *testing.T, quota int, status *EtcdStatus) *clusterv1.Cluster {
	t.Helper()
	c := cluster(variableOf(etcdVariable, `{"quotaGiB":`+strconv.Itoa(quota)+`}`))
	c.Spec.Topology.ControlPlane.Replicas = new(int32(3))
	if status != nil {
		raw, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		c.Annotations = map[string]string{EtcdStatusAnnotation: string(raw)}
	}
	return c
}

func TestEtcdQuotaLowersOnlyBelowTheDatabases(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	read := func(sizes ...int64) *EtcdStatus {
		s := &EtcdStatus{At: now.Add(-etcdReadInterval), QuotaBytes: 8 * gib}
		for i, size := range sizes {
			s.Members = append(s.Members, EtcdMember{Name: "cp-" + strconv.Itoa(i), DBSize: size})
		}
		return s
	}
	failed := read(gib, gib, gib)
	failed.Error = "etcd member cp-2: deadline exceeded"
	stale := read(gib, gib, gib)
	stale.At = now.Add(-etcdStatusFresh - time.Minute)
	for name, tc := range map[string]struct {
		old, now *clusterv1.Cluster
		want     string
	}{
		"created at any size":          {nil, etcdAt(t, 2, nil), ""},
		"raised":                       {etcdAt(t, 4, read(3*gib, 3*gib, 3*gib)), etcdAt(t, 6, nil), ""},
		"raised with sizes unknown":    {etcdAt(t, 4, nil), etcdAt(t, 6, nil), ""},
		"raised from a stale read":     {etcdAt(t, 4, stale), etcdAt(t, 6, nil), ""},
		"lowered with room":            {etcdAt(t, 8, read(gib, gib, gib)), etcdAt(t, 4, nil), ""},
		"lowered to the size":          {etcdAt(t, 8, read(gib, 3*gib, gib)), etcdAt(t, 4, nil), "too large for a limit of 4 GiB"},
		"lowered with sizes unknown":   {etcdAt(t, 8, nil), etcdAt(t, 4, nil), "none is recorded"},
		"lowered after a failed read":  {etcdAt(t, 8, failed), etcdAt(t, 4, nil), "the last read failed: etcd member cp-2"},
		"lowered with a member unread": {etcdAt(t, 8, read(gib, gib)), etcdAt(t, 4, nil), "2 of 3 members were read"},
		"lowered from a stale read":    {etcdAt(t, 8, stale), etcdAt(t, 4, nil), "the last read is from"},
		"unchanged":                    {etcdAt(t, 8, nil), etcdAt(t, 8, nil), ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateEtcdQuota(tc.old, tc.now, now).ToAggregate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// The trusted age follows the maintainer's own cycle: one interval and the read it starts.
func TestEtcdStatusFreshCoversOneReadCycle(t *testing.T) {
	t.Parallel()
	if cycle := etcdReadInterval + etcdReadTimeout; etcdStatusFresh < cycle || etcdStatusFresh > 2*cycle {
		t.Errorf("etcdStatusFresh %v against a cycle of %v", etcdStatusFresh, cycle)
	}
}

func TestClusterValidatorRefusesALowerEtcdLimitWithoutSizes(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	old := withAddresses(networkCluster("192.168.1.1", 24), `["192.168.1.10-192.168.1.20"]`)
	old.Spec.Topology.Variables = append(old.Spec.Topology.Variables, variableOf(etcdVariable, `{"quotaGiB":8}`))
	lower := old.DeepCopy()
	for i := range lower.Spec.Topology.Variables {
		if lower.Spec.Topology.Variables[i].Name == etcdVariable {
			lower.Spec.Topology.Variables[i] = variableOf(etcdVariable, `{"quotaGiB":4}`)
		}
	}
	_, err := (&ClusterValidator{Hooks: hooks}).ValidateUpdate(context.Background(), old, lower)
	if err == nil || !strings.Contains(err.Error(), "the etcd limit cannot be lowered") {
		t.Errorf("lowered with sizes unknown: %v", err)
	}
}
