package ui

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// watchAttention adds what Kubehenge's controller found watching the cluster, as conditions on the
// Cluster, so that the page says what kubectl does.
func watchAttention(c *clusterv1.Cluster, a *attention) {
	if cond := trueCondition(c, provisioner.RollStalledCondition); cond != nil {
		severity := severityWarning
		if cond.Reason == provisioner.RollStalledUnreachable {
			severity = severityError
		}
		a.add(severity, sectionControlPlane, "Control-plane roll stalled", provisioner.RollStallHead(cond.Message), cond.LastTransitionTime.Time)
	}
}

// trueCondition is the cluster's condition of that type while it is true, else nil.
func trueCondition(c *clusterv1.Cluster, kind string) *metav1.Condition {
	cond := meta.FindStatusCondition(c.Status.Conditions, kind)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return nil
	}
	return cond
}
