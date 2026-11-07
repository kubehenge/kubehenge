package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func managementNodeObject(name string, ready corev1.ConditionStatus, message string) *corev1.Node {
	return &corev1.Node{
		Name: name,
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.34.1"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready, Message: message}},
		},
	}
}

func managementDeployment(namespace, name string, wanted, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		Namespace: namespace,
		Name:      name,
		Spec:      appsv1.DeploymentSpec{Replicas: new(wanted)},
		Status:    appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func readHealth(t *testing.T, h *harness, user string, status int) managementHealthView {
	t.Helper()
	b := h.browser()
	b.signIn(user)
	rec := b.do("GET", "/api/setup/health", "")
	expect(t, rec, status)
	var v managementHealthView
	if status == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

func TestManagementHealthListsNodesAndControllers(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		managementNodeObject("mgmt-2", corev1.ConditionFalse, "kubelet stopped posting node status"),
		managementNodeObject("mgmt-1", corev1.ConditionTrue, ""),
		managementDeployment("capmox-system", "capmox-controller-manager", 1, 0),
		managementDeployment("capi-system", "capi-controller-manager", 1, 1),
	)
	got := readHealth(t, h, "admin@pve", http.StatusOK)
	want := managementHealthView{
		Answers: true,
		Nodes: []managementNode{
			{Name: "mgmt-1", Ready: true, Kubelet: "v1.34.1"},
			{Name: "mgmt-2", Message: "kubelet stopped posting node status", Kubelet: "v1.34.1"},
		},
		Controllers: []managementController{
			{Namespace: "capi-system", Name: "capi-controller-manager", Ready: 1, Wanted: 1},
			{Namespace: "capmox-system", Name: "capmox-controller-manager", Ready: 0, Wanted: 1},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("health %+v, want %+v", got, want)
	}
}

func TestManagementHealthShowsWhetherTheControllerReachesProxmoxVE(t *testing.T) {
	t.Parallel()
	failing := `{"ok":false,"error":"Get \"https://pve.internal:8006/api2/json/version\": dial tcp: lookup pve.internal: no such host","checked":"2026-01-02T12:00:00Z"}`
	h := newHarness(t, &corev1.ConfigMap{
		Namespace: "kubehenge-system", Name: "kubehenge-proxmox-reach",
		Data: map[string]string{"reach": failing},
	})
	got := readHealth(t, h, "admin@pve", http.StatusOK)
	if got.Proxmox == nil || got.Proxmox.OK || !strings.Contains(got.Proxmox.Error, "lookup pve.internal: no such host") {
		t.Fatalf("health %+v, want the failed reach with its error", got.Proxmox)
	}
	if none := readHealth(t, newHarness(t), "admin@pve", http.StatusOK); none.Proxmox != nil {
		t.Fatalf("no record yet, but health says %+v", none.Proxmox)
	}
}

func TestManagementHealthSaysWhenHostMaintenanceIsNotVisible(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &corev1.ConfigMap{
		Namespace: "kubehenge-system", Name: "kubehenge-proxmox-reach",
		Data: map[string]string{"reach": `{"ok":true,"blind":"Sys.Audit on /","checked":"2026-01-02T12:00:00Z"}`},
	})
	got := readHealth(t, h, "admin@pve", http.StatusOK)
	if got.Proxmox == nil || !got.Proxmox.OK || got.Proxmox.Blind != "Sys.Audit on /" {
		t.Fatalf("health %+v, want the missing privilege", got.Proxmox)
	}
}

const failingTokenReach = `{"ok":true,"checked":"2026-01-02T12:00:00Z","tokens":[` +
	`{"name":"Controller","ok":false,"problem":"Privileges","missing":[{"path":"/access/realm/pve","privilege":"Realm.AllocateUser"}]},` +
	`{"name":"Template builder","ok":true}]}`

func reachMap(raw string) *corev1.ConfigMap {
	return &corev1.ConfigMap{Namespace: "kubehenge-system", Name: "kubehenge-proxmox-reach", Data: map[string]string{"reach": raw}}
}

func TestManagementHealthNamesEachTokenThatFailsItsTest(t *testing.T) {
	t.Parallel()
	got := readHealth(t, newHarness(t, reachMap(failingTokenReach)), "admin@pve", http.StatusOK)
	if got.Proxmox == nil || len(got.Proxmox.Tokens) != 1 {
		t.Fatalf("health %+v, want only the failing token", got.Proxmox)
	}
	if tk := got.Proxmox.Tokens[0]; tk.Name != "Controller" || tk.Problem != "Privileges" || !strings.Contains(tk.Message, "Realm.AllocateUser on /access/realm/pve") {
		t.Fatalf("token %+v", tk)
	}
}

func TestManagementHealthIsForThoseWhoMaySetUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	readHealth(t, h, "dev@pve", http.StatusForbidden)
}

// A management cluster that does not answer is reported, not failed: the page that repairs it
// has to keep working.
func TestManagementHealthSaysWhenTheClusterDoesNotAnswer(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	kube, ok := h.kube.(client.WithWatch)
	if !ok {
		t.Fatal("the harness's client does not watch")
	}
	h.server.setKube(interceptor.NewClient(kube, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("dial tcp 192.0.2.20:6443: connect: connection refused")
		},
	}))
	got := readHealth(t, h, "admin@pve", http.StatusOK)
	if got.Answers || got.Error != "listing the nodes: dial tcp 192.0.2.20:6443: connect: connection refused" {
		t.Fatalf("health %+v, want the error and not answering", got)
	}
}
