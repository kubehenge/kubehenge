package ui

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// Certificates being renewed are under way; ones that are not are a warning that says how.
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
