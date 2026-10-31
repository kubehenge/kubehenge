//go:build integration

package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestIntegrationNodesIdentityOnlyCordons holds the nodes identity, as it is kept in a workload
// cluster, to a node's cordon: it cordons and uncordons, and changes nothing else of a node.
func TestIntegrationNodesIdentityOnlyCordons(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	cfg, _ := startAPIConfig(t)
	ctx := context.Background()
	admin, err := client.New(cfg, client.Options{Scheme: remoteScheme})
	if err != nil {
		t.Fatal(err)
	}
	id := identityOf(IdentityNodes)
	done, err := id.ensure(ctx, admin)
	if err != nil || !done {
		t.Fatalf("keeping the identity: %t, %v", done, err)
	}
	node := &corev1.Node{Name: "one-md-0-abc", Labels: map[string]string{"kept": "yes"}, Spec: corev1.NodeSpec{ProviderID: "proxmox://one"}}
	if err = admin.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + id.namespace + ":" + id.account(),
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + id.namespace, "system:authenticated"},
	}
	nodes, err := client.New(as, client.Options{Scheme: remoteScheme})
	if err != nil {
		t.Fatal(err)
	}
	patch := func(body string) error {
		return nodes.Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(body)))
	}
	const outside = "cordons and uncordons nodes and changes nothing else of them"

	// The API server takes up a new policy within a few seconds.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		err := patch(`{"metadata":{"labels":{"probe":"x"}}}`)
		if apierrors.IsInvalid(err) {
			break
		}
		if err != nil {
			t.Fatalf("a label before the policy: %v", err)
		}
		if err := admin.Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"probe":null}}}`))); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the API server let the nodes identity change any node for 30 seconds")
		}
	}

	if err := patch(`{"spec":{"unschedulable":true}}`); err != nil {
		t.Fatalf("cordoning: %v", err)
	}
	got := &corev1.Node{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(node), got); err != nil || !got.Spec.Unschedulable {
		t.Fatalf("cordoned = %t, %v", got.Spec.Unschedulable, err)
	}
	if err := patch(`{"spec":{"unschedulable":false}}`); err != nil {
		t.Fatalf("uncordoning: %v", err)
	}

	for what, body := range map[string]string{
		"changing a label":          `{"metadata":{"labels":{"kept":"no"}}}`,
		"adding an annotation":      `{"metadata":{"annotations":{"a":"b"}}}`,
		"adding a finalizer":        `{"metadata":{"finalizers":["kubehenge.dev/held"]}}`,
		"adding a taint":            `{"spec":{"taints":[{"key":"k","effect":"NoSchedule"}]}}`,
		"cordoning and relabelling": `{"metadata":{"labels":{"kept":"no"}},"spec":{"unschedulable":true}}`,
	} {
		if err := patch(body); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), outside) {
			t.Errorf("%s: %v, want refused", what, err)
		}
	}
	if err := nodes.Delete(ctx, node.DeepCopy()); !apierrors.IsForbidden(err) {
		t.Errorf("deleting a node: %v, want forbidden", err)
	}
	if err := nodes.Status().Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"status":{"phase":"Running"}}`))); !apierrors.IsForbidden(err) {
		t.Errorf("patching a node's status: %v, want forbidden", err)
	}

	// Anyone else is the policy's no concern.
	if err := admin.Patch(ctx, node.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"kept":"no"}}}`))); err != nil {
		t.Errorf("the administrator changing a label: %v", err)
	}
}
