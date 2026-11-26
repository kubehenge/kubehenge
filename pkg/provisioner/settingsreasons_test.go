package provisioner

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

func TestAutoscalerInClusterNeedsTheManagementAddress(t *testing.T) {
	t.Parallel()
	const api = "https://192.0.2.10:6443"
	for name, tc := range map[string]struct {
		placement, api string
		autoscaled     bool
		want           string
	}{
		"in the cluster without the address": {PlacementWorkload, "", true, "installed without the management cluster's address"},
		"in the cluster with it":             {PlacementWorkload, api, true, ""},
		"in the management cluster":          {PlacementManagement, "", true, ""},
		"no pool is autoscaled":              {PlacementWorkload, "", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := cluster(variableOf(AutoscalerVariable, `{"placement":"`+tc.placement+`"}`))
			if tc.autoscaled {
				c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{autoscaledPoolOf("1", "3")}
			}
			errs := validateAutoscaling(c, tc.api).ToAggregate()
			switch {
			case tc.want == "" && errs != nil:
				t.Errorf("refused: %v", errs)
			case tc.want != "" && (errs == nil || !strings.Contains(errs.Error(), tc.want)):
				t.Errorf("got %v, want %q", errs, tc.want)
			}
		})
	}
}

func TestEtcdQuotaLowersOnlyBelowTheDatabases(t *testing.T) {
	t.Parallel()
	withStatus := func(quota int, sizes ...int64) *clusterv1.Cluster {
		c := cluster(variableOf(etcdVariable, `{"quotaGiB":`+strconv.Itoa(quota)+`}`))
		if len(sizes) > 0 {
			status := EtcdStatus{QuotaBytes: int64(quota) * gib}
			for _, size := range sizes {
				status.Members = append(status.Members, EtcdMember{Name: "cp-0", DBSize: size})
			}
			raw, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			c.Annotations = map[string]string{EtcdStatusAnnotation: string(raw)}
		}
		return c
	}
	for name, tc := range map[string]struct {
		old, now *clusterv1.Cluster
		want     string
	}{
		"created at any size":        {nil, withStatus(2), ""},
		"raised":                     {withStatus(4, 3*gib), withStatus(6), ""},
		"lowered with room":          {withStatus(8, gib), withStatus(4), ""},
		"lowered to the size":        {withStatus(8, 3*gib), withStatus(4), "too large for a limit of 4 GiB"},
		"lowered with sizes unknown": {withStatus(8), withStatus(4), "until the controller has read the members' database sizes"},
		"unchanged":                  {withStatus(8), withStatus(8), ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateEtcdQuota(tc.old, tc.now).ToAggregate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}
