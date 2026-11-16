//go:build integration

package appliance

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kubehenge/kubehenge/test/integration"
)

// TestIntegrationWakersFollowZones runs followZones as the interface's and the controller's
// accounts against the charts the module installs for a backup volume on one node's own storage.
// Each corrects the zone labels of the management nodes and changes nothing else of a node.
func TestIntegrationWakersFollowZones(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	if err := integration.OwnChildren(); err != nil {
		t.Fatal(err)
	}
	objects, _ := integration.Charts(t, "../..")
	objects = append(objects, integration.RenderChart(t, "kubehenge-storage", "../../management-cluster/charts/storage", "kube-system", "testdata/storage-zones-values.yaml")...)
	api := integration.Start(t, &integration.Options{Objects: objects, Namespaces: []string{integration.ClassNamespace, "kube-system"}})
	admin, err := kubernetes.NewForConfig(api.Config)
	if err != nil {
		t.Fatal(err)
	}
	as := rest.CopyConfig(api.Config)
	as.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + integration.ClassNamespace + ":kubehenge-interface",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + integration.ClassNamespace, "system:authenticated"},
	}
	ui, err := kubernetes.NewForConfig(as)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// mgmt-cp-2 joined since the last apply, with no zone yet.
	nodes := []*corev1.Node{zoned("mgmt-cp-0", "pve1", withBeta...), zoned("mgmt-cp-1", "pve1", zoneOnly...), zoned("mgmt-cp-2", ""), zoned("worker", "pve1", zoneOnly...)}
	for _, n := range nodes {
		n.Labels["kept"] = "yes"
		if _, err = admin.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	patch := func(c kubernetes.Interface, node, body string) error {
		_, perr := c.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, []byte(body), metav1.PatchOptions{})
		return perr
	}

	// The policy takes a moment to be enforced after it is made.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		err = patch(ui, "mgmt-cp-0", `{"metadata":{"labels":{"kept":"no"}}}`)
		if apierrors.IsInvalid(err) {
			break
		}
		if err != nil {
			t.Fatalf("another label of a management node: %v", err)
		}
		if err = patch(admin, "mgmt-cp-0", `{"metadata":{"labels":{"kept":"yes"}}}`); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the API server let the interface change any label of a node for 30 seconds")
		}
	}

	a := &Appliance{vmHosts: hostsAt(t, map[string]string{"mgmt-cp-0": "pve2", "mgmt-cp-1": "pve1", "mgmt-cp-2": "pve3", "worker": "pve1"})}
	if _, err = a.followZones(ctx, ui, "pve1"); err != nil {
		t.Fatalf("the interface could not correct the zones: %v", err)
	}
	moved, err := admin.CoreV1().Nodes().Get(ctx, "mgmt-cp-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Labels[corev1.LabelTopologyZone] != "pve2" || moved.Labels[corev1.LabelFailureDomainBetaZone] != "pve2" || moved.Labels["kept"] != "yes" {
		t.Errorf("the moved node's labels are %v, want both zones pve2 and the rest kept", moved.Labels)
	}
	added, err := admin.CoreV1().Nodes().Get(ctx, "mgmt-cp-2", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if added.Labels[corev1.LabelTopologyZone] != "pve3" || added.Labels["kept"] != "yes" {
		t.Errorf("the joined node's labels are %v, want zone pve3 and the rest kept", added.Labels)
	}
	if _, beta := added.Labels[corev1.LabelFailureDomainBetaZone]; beta {
		t.Error("the joined node gained a beta zone label it never had")
	}

	const outside = "changes nothing of a node but the values of its zone labels"
	for what, body := range map[string]string{
		"changing another label":        `{"metadata":{"labels":{"kept":"no"}}}`,
		"removing another label":        `{"metadata":{"labels":{"kept":null}}}`,
		"removing the zone label":       `{"metadata":{"labels":{"topology.kubernetes.io/zone":null}}}`,
		"adding an annotation":          `{"metadata":{"annotations":{"probe":"x"}}}`,
		"cordoning":                     `{"spec":{"unschedulable":true}}`,
		"the zone and the spec at once": `{"metadata":{"labels":{"topology.kubernetes.io/zone":"pve3"}},"spec":{"unschedulable":true}}`,
	} {
		if err = patch(ui, "mgmt-cp-1", body); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), outside) {
			t.Errorf("%s: %v, want refused", what, err)
		}
	}
	if err = patch(ui, "worker",`{"metadata":{"labels":{"topology.kubernetes.io/zone":"pve2"}}}`); !apierrors.IsForbidden(err) {
		t.Errorf("a node outside the management nodes: %v, want forbidden", err)
	}

	controllerFollowsZones(ctx, t, admin, as, a, patch)
}

// controllerFollowsZones moves mgmt-cp-0's zone back to pve1 and follows the zones as the
// controller, which wakes the store for the clusters' etcd snapshots with no appliance.
func controllerFollowsZones(ctx context.Context, t *testing.T, admin kubernetes.Interface, as *rest.Config, a *Appliance, patch func(kubernetes.Interface, string, string) error) {
	t.Helper()
	as.Impersonate.UserName = "system:serviceaccount:" + integration.ClassNamespace + ":kubehenge-controller"
	controller, err := kubernetes.NewForConfig(as)
	if err != nil {
		t.Fatal(err)
	}
	if err = patch(admin, "mgmt-cp-0", `{"metadata":{"labels":{"topology.kubernetes.io/zone":"pve1","failure-domain.beta.kubernetes.io/zone":"pve1"}}}`); err != nil {
		t.Fatal(err)
	}
	if _, err = a.followZones(ctx, controller, "pve1"); err != nil {
		t.Fatalf("the controller could not correct the zones: %v", err)
	}
	moved, err := admin.CoreV1().Nodes().Get(ctx, "mgmt-cp-0", metav1.GetOptions{})
	if err != nil || moved.Labels[corev1.LabelTopologyZone] != "pve2" {
		t.Fatalf("after the controller's wake the moved node is %v: %v", moved, err)
	}
	err = patch(controller, "mgmt-cp-1", `{"metadata":{"labels":{"kept":"no"}}}`)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "changes nothing of a node but the values of its zone labels") {
		t.Errorf("the controller changing another label: %v, want refused", err)
	}
}
