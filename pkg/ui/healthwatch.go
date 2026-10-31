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
	if cond := trueCondition(c, provisioner.DeletionStalledCondition); cond != nil && !provisioner.Abandoned(c) {
		a.add(severityWarning, sectionClusterAPI, "Deletion stalled", provisioner.DeletionStallHead(cond.Message), cond.LastTransitionTime.Time)
	}
	if cond := trueCondition(c, provisioner.CertificatesCondition); cond != nil {
		if cond.Reason == provisioner.CertificatesRenewing {
			a.add(severityProgress, sectionNodes, "Renewing certificates", provisioner.CertificatesHead(cond.Message), cond.LastTransitionTime.Time)
		} else {
			a.add(severityWarning, sectionNodes, "Certificates expire", provisioner.CertificatesSummary(cond.Message), cond.LastTransitionTime.Time)
		}
	}
	if cond := trueCondition(c, provisioner.VMsCondition); cond != nil {
		severity := severityWarning
		if cond.Reason == provisioner.VMsIOError || cond.Reason == provisioner.VMsPanicked {
			severity = severityError
		}
		a.add(severity, sectionNodes, vmTitles[cond.Reason], cond.Message, cond.LastTransitionTime.Time)
	}
	if cond := trueCondition(c, provisioner.StorageCondition); cond != nil {
		severity, what := severityWarning, "Storage nearly full"
		if cond.Reason == provisioner.StorageFull {
			severity, what = severityError, "Storage full"
		}
		a.add(severity, sectionVolumes, what, provisioner.StorageSummary(cond.Message), cond.LastTransitionTime.Time)
	}
}

// vmTitles name the worst state a cluster's VMs are in.
var vmTitles = map[string]string{
	provisioner.VMsIOError:  "VM disk error",
	provisioner.VMsPanicked: "VM crashed",
	provisioner.VMsStopped:  "VM stopped",
	provisioner.VMsPaused:   "VM paused",
}

// trueCondition is the cluster's condition of that type while it is true, else nil.
func trueCondition(c *clusterv1.Cluster, kind string) *metav1.Condition {
	cond := meta.FindStatusCondition(c.Status.Conditions, kind)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return nil
	}
	return cond
}
