package ui

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// Certificates being renewed are under way. Ones that are not are a warning that says how.
func TestHealthSaysCertificatesExpire(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason, severity, what, message string
	}{
		{provisioner.CertificatesRenewing, severityProgress, "Renewing certificates", "Certificates run out: machine web-cp-a on 2027-10-16."},
		{provisioner.CertificatesRestartNeeded, severityWarning, "Certificates expire", "Certificates run out: machine web-cp-a on 2027-10-16. To renew them, restart rke2-server on web-cp-a."},
	} {
		c := &clusterv1.Cluster{Name: "web", Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
			Type: provisioner.CertificatesCondition, Status: metav1.ConditionTrue, Reason: tc.reason,
			Message: "Certificates run out: machine web-cp-a on 2027-10-16. To renew them, restart rke2-server on web-cp-a." +
				" A control plane of fewer than three machines cannot be replaced one at a time without stopping the cluster.",
		}}}}
		var a attention
		watchAttention(c, &a)
		if len(a) != 1 || a[0].Severity != tc.severity || a[0].Section != sectionNodes || a[0].What != tc.what || a[0].Message != tc.message {
			t.Errorf("%s: %+v", tc.reason, a)
		}
	}
}

// A VM paused by a disk error and a storage that is full are errors. A VM paused by hand, a
// storage nearly full and an upgrade Rancher holds are warnings. One a Talos upgrade holds is under way.
func TestHealthSaysVMsStorageAndHeldUpgrades(t *testing.T) {
	t.Parallel()
	const (
		storage = "Storage local-lvm is 96% full on pve1 (96 of 100 GiB). Free space or add capacity."
		rancher = "for Rancher, choose Kubernetes 1.34 to 1.36, not v1.37.1+k3s1"
	)
	for _, tc := range []struct {
		condType, reason, message string
		want                      attentionView
	}{
		{
			provisioner.VMsCondition, provisioner.VMsIOError, "web-cp-0 (VM 101 on pve1) paused: disk error. Free space or fix the disk, then resume it in Proxmox VE.",
			attentionView{Severity: severityError, Section: sectionNodes, What: "VM disk error", Message: "web-cp-0 (VM 101 on pve1) paused: disk error. Free space or fix the disk, then resume it in Proxmox VE."},
		},
		{
			provisioner.VMsCondition, provisioner.VMsPaused, "web-cp-0 (VM 101 on pve1) paused. Resume it in Proxmox VE.",
			attentionView{Severity: severityWarning, Section: sectionNodes, What: "VM paused", Message: "web-cp-0 (VM 101 on pve1) paused. Resume it in Proxmox VE."},
		},
		{
			provisioner.StorageCondition, provisioner.StorageFull, storage + " A VM whose disk write finds no space is paused.",
			attentionView{Severity: severityError, Section: sectionVolumes, What: "Storage full", Message: storage},
		},
		{
			provisioner.StorageCondition, provisioner.StorageNearlyFull, storage,
			attentionView{Severity: severityWarning, Section: sectionVolumes, What: "Storage nearly full", Message: storage},
		},
		{
			provisioner.StorageUnavailableCondition, provisioner.StorageUnavailableReason,
			"Storage nfs is unavailable on pve2. Its machines are not replaced for 30 minutes. Bring the storage back in Proxmox VE.",
			attentionView{Severity: severityError, Section: sectionVolumes, What: "Storage unavailable", Message: "Storage nfs is unavailable on pve2"},
		},
		{
			provisioner.RemediationHeldCondition, provisioner.HeldNodeMaintenance,
			"web-md-0-a: Proxmox VE node pve2 is in maintenance, replacement waits until 14:40 UTC",
			attentionView{
				Severity: severityWarning, Section: sectionNodes, What: "Replacement waits",
				Message: "web-md-0-a: Proxmox VE node pve2 is in maintenance, replacement waits until 14:40 UTC",
			},
		},
		{
			provisioner.UpgradeHeldCondition, provisioner.UpgradeHeldRancherKubernetes, rancher,
			attentionView{Severity: severityWarning, Section: sectionClusterAPI, What: "Upgrade held for Rancher", Message: rancher},
		},
		{
			provisioner.UpgradeHeldCondition, provisioner.UpgradeHeldRancherElsewhere, "cluster team-a/rancher already runs Rancher. Only one cluster may run it",
			attentionView{Severity: severityWarning, Section: sectionClusterAPI, What: "Upgrade held for Rancher", Message: "cluster team-a/rancher already runs Rancher. Only one cluster may run it"},
		},
		{
			provisioner.UpgradeHeldCondition, provisioner.UpgradeHeldTalosUpgrade, "waiting for the Talos upgrade of web-cp-0",
			attentionView{Severity: severityProgress, Section: sectionClusterAPI, What: "Upgrade waits for Talos", Message: "waiting for the Talos upgrade of web-cp-0"},
		},
	} {
		c := &clusterv1.Cluster{Name: "web", Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
			Type: tc.condType, Status: metav1.ConditionTrue, Reason: tc.reason, Message: tc.message,
		}}}}
		var a attention
		watchAttention(c, &a)
		if len(a) != 1 || a[0].Severity != tc.want.Severity || a[0].Section != tc.want.Section || a[0].What != tc.want.What || a[0].Message != tc.want.Message {
			t.Errorf("%s: %+v", tc.reason, a)
		}
	}
}
