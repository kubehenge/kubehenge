package ui

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// The add-ons the controller holds back are on the page as kubectl shows them: a conflict and a
// Gateway API that never came are warnings, as is a volume that cannot be made, and a wait for
// cert-manager to install is under way.
func TestHealthSaysWhatHoldsAddonsBack(t *testing.T) {
	t.Parallel()
	const conflict = "cert-manager is not installed: the cluster already runs its Deployment security/cert-manager, which no Kubehenge release owns. Remove it. Until then monitoring and loki wait"
	for _, tc := range []struct {
		condType, reason, message string
		want                      attentionView
	}{
		{provisioner.AddonConflictCondition, provisioner.AddonConflictReason, conflict, attentionView{Severity: severityWarning, Section: sectionAddons, What: "Add-on conflict", Message: conflict}},
		{provisioner.AddonsWaitingCondition, provisioner.AddonsWaitingForCertManager, "monitoring wait for cert-manager to be deployed", attentionView{Severity: severityProgress, Section: sectionAddons, What: "Add-ons wait for cert-manager", Message: "monitoring wait for cert-manager to be deployed"}},
		{provisioner.AddonsWaitingCondition, provisioner.AddonsWaitingGatewayAPIMissing, "monitoring wait for cert-manager, gone", attentionView{Severity: severityWarning, Section: sectionAddons, What: "Add-ons wait for cert-manager", Message: "monitoring wait for cert-manager, gone"}},
		{provisioner.AddonsWaitingCondition, provisioner.AddonsWaitingVolumeMissing, "logs has no volume: full", attentionView{Severity: severityWarning, Section: sectionAddons, What: "Add-ons wait for a volume", Message: "logs has no volume: full"}},
		{provisioner.AddonsWaitingCondition, provisioner.AddonsWaitingReason, "kyverno-policies waits for kyverno to be deployed. traefik waits for the cluster's Gateway API definitions", attentionView{Severity: severityProgress, Section: sectionAddons, What: "Add-ons wait", Message: "kyverno-policies waits for kyverno to be deployed. traefik waits for the cluster's Gateway API definitions"}},
		{provisioner.AddonsWaitingCondition, provisioner.AddonsWaitingCredentialMissing, "certificates waits for the DNS-01 credential in Secret team-a/web.dns01", attentionView{Severity: severityWarning, Section: sectionAddons, What: "Add-ons wait for a credential", Message: "certificates waits for the DNS-01 credential in Secret team-a/web.dns01"}},
	} {
		c := &clusterv1.Cluster{Name: "web", Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
			Type: tc.condType, Status: metav1.ConditionTrue, Reason: tc.reason, Message: tc.message,
		}}}}
		var a attention
		watchAttention(c, &a)
		if len(a) != 1 || a[0] != tc.want {
			t.Errorf("%s %s: %+v", tc.condType, tc.reason, a)
		}
	}
}

// Helm's refusal of a cert-manager kind never stands alone as why an add-on is not ready.
func TestHelmsMissingCertManagerKindSaysWhy(t *testing.T) {
	t.Parallel()
	const helm = "unable to build kubernetes objects from release manifest: resource mapping not found for name: kubehenge-loki-client namespace: logging from \"\": no matches for kind \"Certificate\" in version \"cert-manager.io/v1\""
	if got := withCertManagerCause(helm); !strings.HasPrefix(got, "The cluster lacks cert-manager's custom resource definitions.") || !strings.HasSuffix(got, helm) {
		t.Errorf("message %q", got)
	}
	if got := withCertManagerCause("timed out waiting for the condition"); got != "timed out waiting for the condition" {
		t.Errorf("another failure changed: %q", got)
	}
}
