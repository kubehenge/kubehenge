package ui

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// stall marks cluster one's control-plane roll as stalled, as the controller's watch does.
func (h *etcdHarness) stall(reason string) {
	h.t.Helper()
	c := h.cluster()
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
		Type: provisioner.RollStalledCondition, Status: metav1.ConditionTrue, Reason: reason,
		Message: "one-cp-b, which replaces one-cp-a, is not ready", LastTransitionTime: metav1.Now(),
	})
	if err := h.kube.Update(h.t.Context(), c); err != nil {
		h.t.Fatal(err)
	}
}

func TestRecoverControlPlane(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	h.setClass(map[string]string{provisioner.AutomaticRestoreAnnotation: "true"})
	b := h.browser()
	b.signIn("dev@pve")
	body := `{"machine":"one-cp-a","confirm":"one"}`

	// Nothing to recover from while the roll goes on.
	expect(t, b.do("POST", "/api/clusters/default/one/control-plane-recovery", body), http.StatusConflict)
	h.stall(provisioner.RollStalledUnreachable)

	expect(t, b.do("POST", "/api/clusters/default/one/control-plane-recovery", `{"machine":"one-cp-a","confirm":"One"}`), http.StatusUnprocessableEntity)
	expect(t, b.do("POST", "/api/clusters/default/one/control-plane-recovery", `{"machine":"one-md-0-a","confirm":"one"}`), http.StatusUnprocessableEntity)
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("POST", "/api/clusters/default/one/control-plane-recovery", body), http.StatusForbidden)
	expect(t, viewer.do("GET", "/api/clusters/default/one/control-plane-recovery?machine=one-cp-a", ""), http.StatusForbidden)

	rec := b.do("GET", "/api/clusters/default/one/control-plane-recovery?machine=one-cp-a", "")
	expect(t, rec, http.StatusOK)
	var view recoveryView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Automatic || view.Plan != nil {
		t.Errorf("view %+v", view)
	}

	expect(t, b.do("POST", "/api/clusters/default/one/control-plane-recovery", body), http.StatusAccepted)
	req, ok, err := provisioner.EtcdRestoreRequestOf(h.cluster())
	if !ok || err != nil || req.Machine != "one-cp-a" || req.Snapshot != "" || req.By != "dev@pve" || time.Since(req.At) > time.Minute {
		t.Errorf("request %+v, %v", req, err)
	}
	expect(t, b.do("POST", "/api/clusters/default/one/control-plane-recovery", body), http.StatusConflict)
}

// Where the controller cannot reach the guests, the interface gives the steps to recover by hand.
func TestControlPlaneRecoveryByHand(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	h.stall(provisioner.RollStalledAnswering)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/control-plane-recovery?machine=one-cp-b", "")
	expect(t, rec, http.StatusOK)
	var view recoveryView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Automatic || view.Plan == nil || !slices.Equal(view.Plan.Replaced, []string{"one-cp-a"}) ||
		!slices.ContainsFunc(view.Plan.Steps, func(s provisioner.RestoreStep) bool { return strings.Contains(s.Command, "--cluster-reset") }) {
		t.Errorf("view %+v", view)
	}
	expect(t, b.do("POST", "/api/clusters/default/one/control-plane-recovery", `{"machine":"one-cp-b","confirm":"one"}`), http.StatusConflict)
}

func TestHealthSaysTheRollStalled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ reason, severity string }{
		{provisioner.RollStalledUnreachable, severityError},
		{provisioner.RollStalledAnswering, severityWarning},
	} {
		c := &clusterv1.Cluster{Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
			Type: provisioner.RollStalledCondition, Status: metav1.ConditionTrue, Reason: tc.reason, Message: "web-cp-1 is not ready. The cluster's API still answers.",
		}}}}
		var a attention
		watchAttention(c, &a)
		if len(a) != 1 || a[0].Severity != tc.severity || a[0].Section != sectionControlPlane || a[0].Message != "web-cp-1 is not ready." {
			t.Errorf("%s: %+v", tc.reason, a)
		}
	}
	var a attention
	watchAttention(&clusterv1.Cluster{Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
		Type: provisioner.RollStalledCondition, Status: metav1.ConditionFalse,
	}}}}, &a)
	if len(a) != 0 {
		t.Errorf("a cleared condition needs attention: %+v", a)
	}
}
