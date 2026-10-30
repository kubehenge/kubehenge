package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestAMachineIsCordonedAndUncordoned(t *testing.T) {
	t.Parallel()
	h, workloads := healthHarness(t)
	const path = "/api/clusters/default/one/machines/one-md-0-a/cordon"
	cordoned := func() bool {
		t.Helper()
		n, err := workloads.kube.CoreV1().Nodes().Get(context.Background(), "one-md-0-a", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return n.Spec.Unschedulable
	}

	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("PUT", path, ""), http.StatusForbidden)

	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("PUT", path, ""), http.StatusNoContent)
	if !cordoned() {
		t.Error("the node is not cordoned")
	}
	if !slices.Contains(workloads.connected(), "nodes-token") {
		t.Errorf("connected with %v, want the nodes identity's token", workloads.connected())
	}
	expect(t, dev.do("DELETE", path, ""), http.StatusNoContent)
	if cordoned() {
		t.Error("the node is still cordoned")
	}
	expect(t, dev.do("PUT", "/api/clusters/default/one/machines/one-gone/cordon", ""), http.StatusNotFound)

	// Without the identity's token, the interface says so.
	if err := h.kube.Delete(context.Background(), &corev1.Secret{Namespace: "default", Name: provisioner.IdentitySecretName("one", provisioner.IdentityNodes)}); err != nil {
		t.Fatal(err)
	}
	rec := dev.do("PUT", path, "")
	expect(t, rec, http.StatusConflict)
	if body := rec.Body.String(); !strings.Contains(body, "Node access to this cluster is not set up yet.") {
		t.Errorf("answered %s", body)
	}
}

func TestAMachineIsRebootedByThoseWhoMay(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	const path = "/api/clusters/default/one/machines/one-md-0-a/reboot"
	rebootable := func(b *browser) bool {
		t.Helper()
		rec := b.do("GET", "/api/clusters/default/one", "")
		expect(t, rec, http.StatusOK)
		var v clusterView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(v.Machines, func(m machineView) bool { return m.Name == "one-md-0-a" })
		return i >= 0 && v.Machines[i].Reboot
	}

	dev := h.browser()
	dev.signIn("dev@pve")
	if rebootable(dev) {
		t.Error("offered to a user without VM.PowerMgmt")
	}
	expect(t, dev.do("POST", path, ""), http.StatusForbidden)

	admin := h.browser()
	admin.signIn("admin@pve")
	if !rebootable(admin) {
		t.Error("not offered to a user with VM.PowerMgmt")
	}
	expect(t, admin.do("POST", path, ""), http.StatusAccepted)
	h.accounts.mu.Lock()
	rebooted := slices.Clone(h.accounts.rebooted)
	h.accounts.mu.Unlock()
	if !slices.Equal(rebooted, []int{202}) {
		t.Errorf("rebooted %v, want the worker's VM 202", rebooted)
	}
}
