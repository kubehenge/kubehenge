package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
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
