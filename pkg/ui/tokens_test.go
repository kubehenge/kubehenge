package ui

import (
	"encoding/json"
	"net/http"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAnAPITokenActsAsItsUser(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	script := &browser{h: h, token: "dev@pve!ci=secret"} //nolint:gosec // the fake accounts' own

	rec := script.do("GET", "/api/session", "")
	expect(t, rec, http.StatusOK)
	var v sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.User != "dev@pve!ci" || v.CSRF != "" {
		t.Fatalf("session = %+v", v)
	}
	// A change needs no CSRF token, and is the token's, by its name.
	expect(t, script.do("PATCH", "/api/clusters/default/one", `{"controlPlaneReplicas":3}`), http.StatusOK)
	got := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, got); err != nil {
		t.Fatal(err)
	}
	if n := got.Spec.Topology.ControlPlane.Replicas; n == nil || *n != 3 {
		t.Fatalf("control planes = %v", n)
	}
	// Proxmox VE is asked once for the token, not for every request.
	expect(t, script.do("GET", "/api/clusters", ""), http.StatusOK)
	h.accounts.mu.Lock()
	checks := h.accounts.tokenChecks
	h.accounts.mu.Unlock()
	if checks != 1 {
		t.Errorf("the token was checked %d times", checks)
	}

	// The token holds only its user's privileges.
	viewer := &browser{h: h, token: "viewer@pve!ci=secret"} //nolint:gosec // the fake accounts' own
	expect(t, viewer.do("GET", "/api/clusters/default/one", ""), http.StatusOK)
	expect(t, viewer.do("PATCH", "/api/clusters/default/one", `{"controlPlaneReplicas":1}`), http.StatusForbidden)

	// A token Proxmox VE does not take, or a user's name with a made-up secret, gets nothing.
	for _, token := range []string{"dev@pve!ci=guess", "dev@pve", "nobody@pve!ci=secret"} {
		expect(t, (&browser{h: h, token: token}).do("GET", "/api/clusters", ""), http.StatusUnauthorized)
	}
}
