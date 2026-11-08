package ui

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestHealthWarnsOfANodeThatIsNotReachableUntilItAnswers(t *testing.T) {
	t.Parallel()
	c := &clusterv1.Cluster{Name: "web", Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
		Type: provisioner.NodesSkippedCondition, Status: metav1.ConditionTrue, Reason: "NotReachable",
		Message: "Not reachable through the cluster: pve3 (HTTP 596: certificate verify failed)",
	}}}}
	var a attention
	watchAttention(c, &a)
	if len(a) != 1 || a[0].Severity != severityWarning || a[0].What != "Proxmox VE node not reachable" ||
		!strings.Contains(a[0].Message, "pve3 (HTTP 596: certificate verify failed)") ||
		!strings.Contains(a[0].Message, "pvecm updatecerts --force") || !strings.Contains(a[0].Docs, "#a-node-is-online-but-not-reachable") {
		t.Fatalf("rows %+v, want one warning naming the node, its 596 and the fix", a)
	}
	if got := verdict(c, false, a.sorted()); got != "degraded" {
		t.Errorf("verdict %q, want degraded", got)
	}
	c.Status.Conditions = nil
	a = nil
	watchAttention(c, &a)
	if len(a) != 0 {
		t.Errorf("rows %+v once the node answers", a)
	}
}

func TestHealthListsAProxmoxVEUpgradeUnderWay(t *testing.T) {
	t.Parallel()
	c := &clusterv1.Cluster{Name: "web", Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
		Type: provisioner.HARulesPendingCondition, Status: metav1.ConditionTrue, Reason: "MixedVersions", Message: "The HA rules wait.",
	}}}}
	c.Annotations = map[string]string{
		provisioner.ProxmoxVersionsAnnotation: "8=pve1,pve3;9=pve2",
		provisioner.NodeMTUsAnnotation:        "pve1=1500,pve2=9000",
	}
	var a attention
	watchAttention(c, &a)
	got := map[string]attentionView{}
	for _, item := range a {
		got[item.What] = item
	}
	versions, ok := got["Proxmox VE 8 and 9 nodes in one cluster"]
	if !ok || versions.Severity != severityWarning || !strings.Contains(versions.Message, "Proxmox VE 8: pve1, pve3. Proxmox VE 9: pve2.") {
		t.Errorf("versions row %+v in %+v", versions, a)
	}
	if row, ok := got["VMs differ in interface MTU"]; !ok || !strings.Contains(row.Message, "pve1: 1500, pve2: 9000") {
		t.Errorf("MTU row %+v", row)
	}
	if row, ok := got["HA rules wait for the Proxmox VE upgrade"]; !ok || row.Message != "The HA rules wait." {
		t.Errorf("HA row %+v", row)
	}

	c.Annotations, c.Status.Conditions = nil, nil
	a = nil
	watchAttention(c, &a)
	if len(a) != 0 {
		t.Errorf("rows %+v for a cluster in no upgrade", a)
	}
}
