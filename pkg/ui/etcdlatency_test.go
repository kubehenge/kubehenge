package ui

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestHealthShowsASlowEtcdDiskOnlyAboveTheLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		p99  time.Duration
		row  bool
	}{
		{"below", 24 * time.Millisecond, false},
		{"at", provisioner.EtcdFsyncLimit, false},
		{"above", 26 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &clusterv1.Cluster{}
			cond := provisioner.EtcdLatencyConditionOf([]provisioner.MemberFsync{{Member: "etcd-a", P99: tc.p99, Syncs: 5000}})
			if cond != nil {
				cond.Type = provisioner.EtcdLatencyCondition
				cond.LastTransitionTime = metav1.Now()
				c.Status.Conditions = append(c.Status.Conditions, *cond)
			}
			var a attention
			watchAttention(c, &a)
			if got := len(a) == 1; got != tc.row {
				t.Fatalf("rows %v for p99 %s", a, tc.p99)
			}
			if !tc.row {
				return
			}
			row := a[0]
			if row.Severity != severityWarning || row.What != "etcd disk slow" || !strings.Contains(row.Message, "etcd-a 26 ms") || strings.Contains(row.Message, "Ceph") {
				t.Errorf("row %+v", row)
			}
		})
	}
}
