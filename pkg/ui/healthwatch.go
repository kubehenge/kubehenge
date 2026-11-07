package ui

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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
	if cond := trueCondition(c, provisioner.UpgradeHeldCondition); cond != nil {
		severity, what := severityWarning, "Upgrade held"
		switch cond.Reason {
		case provisioner.UpgradeHeldRancherKubernetes, provisioner.UpgradeHeldRancherElsewhere:
			what = "Upgrade held for Rancher"
		case provisioner.UpgradeHeldTalosUpgrade:
			severity, what = severityProgress, "Upgrade waits for Talos"
		}
		a.add(severity, sectionClusterAPI, what, cond.Message, cond.LastTransitionTime.Time)
	}
	versionAttention(c, a)
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
	hostAttention(c, a)
	if cond := trueCondition(c, provisioner.StorageCondition); cond != nil {
		severity, what := severityWarning, "Storage nearly full"
		if cond.Reason == provisioner.StorageFull {
			severity, what = severityError, "Storage full"
		}
		a.add(severity, sectionVolumes, what, provisioner.StorageSummary(cond.Message), cond.LastTransitionTime.Time)
	}
}

// hostAttention adds what the Proxmox VE hosts and storage under the cluster do to it: machines
// whose replacement waits for a host, and storage that is gone.
func hostAttention(c *clusterv1.Cluster, a *attention) {
	if cond := trueCondition(c, provisioner.RemediationHeldCondition); cond != nil {
		a.add(severityWarning, sectionNodes, "Replacement waits", cond.Message, cond.LastTransitionTime.Time)
	}
	if cond := trueCondition(c, provisioner.StorageUnavailableCondition); cond != nil {
		a.add(severityError, sectionVolumes, "Storage unavailable", provisioner.MessageHead(cond.Message), cond.LastTransitionTime.Time)
	}
}

// versionAttention adds what the controller found about the cluster's Kubernetes version: that it
// is out of support, and that an automatic upgrade waits.
func versionAttention(c *clusterv1.Cluster, a *attention) {
	if cond := trueCondition(c, provisioner.KubernetesOutOfSupportCondition); cond != nil {
		a.add(severityWarning, sectionControlPlane, outOfSupportWhat, cond.Message, cond.LastTransitionTime.Time)
	}
	if cond := trueCondition(c, provisioner.AutoUpgradeSkippedCondition); cond != nil {
		a.add(severityWarning, sectionClusterAPI, "Automatic upgrade skipped", cond.Message, cond.LastTransitionTime.Time)
		(*a)[len(*a)-1].Accept = provisioner.SkippedTarget(cond.Message)
	}
}

// pausedNoteAfter is how long a cluster stays paused before its Health page says so: a pause for a
// maintenance is no problem, a forgotten one holds back upgrades, renewals and replacements.
const pausedNoteAfter = 7 * 24 * time.Hour

// outOfSupportWhat is the Health item of a cluster on a Kubernetes minor older than any offered.
const outOfSupportWhat = "Kubernetes out of support"

// pausedAttention notes a cluster paused for pausedNoteAfter. Cluster API records when its Paused
// condition last changed.
func pausedAttention(c *clusterv1.Cluster, now time.Time, a *attention) {
	cond := trueCondition(c, clusterv1.PausedCondition)
	if cond == nil || !ptr.Deref(c.Spec.Paused, false) || now.Sub(cond.LastTransitionTime.Time) < pausedNoteAfter {
		return
	}
	since := cond.LastTransitionTime.UTC().Format(time.DateOnly)
	a.add(severityWarning, sectionClusterAPI, "Paused",
		"Paused since "+since+". Nothing upgrades, renews certificates or replaces machines while the cluster is paused.", cond.LastTransitionTime.Time)
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
