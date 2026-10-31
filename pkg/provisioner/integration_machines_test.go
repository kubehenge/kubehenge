//go:build integration

package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestIntegrationInterfaceMarksMachines holds the interface's account to Cluster API's remediation
// and deletion marks on Machines: it sets and takes back those annotations, and changes nothing
// else of them.
func TestIntegrationInterfaceMarksMachines(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	cfg, admin := startAPIConfig(t)
	ctx := context.Background()
	ui, err := client.New(interfaceAccount(cfg), client.Options{Scheme: admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	m := poolMachine("one", "one-cp-1")
	m.Annotations = map[string]string{"kept": "yes"}
	if err := admin.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	patch := func(c client.Client, body string) error {
		return c.Patch(ctx, m.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(body)))
	}
	const outside = "changes nothing of a Machine but the " + clusterv1.RemediateMachineAnnotation + " and " + clusterv1.DeleteMachineAnnotation + " annotations"

	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		err := patch(ui, `{"metadata":{"annotations":{"probe":"x"}}}`)
		if apierrors.IsInvalid(err) {
			break
		}
		if err != nil {
			t.Fatalf("an annotation of its own: %v", err)
		}
		if err := patch(admin, `{"metadata":{"annotations":{"probe":null}}}`); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the API server let the interface change any Machine for 30 seconds")
		}
	}

	for _, mark := range []string{clusterv1.RemediateMachineAnnotation, clusterv1.DeleteMachineAnnotation} {
		if err := patch(ui, `{"metadata":{"annotations":{"`+mark+`":""}}}`); err != nil {
			t.Fatalf("marking a machine with %s: %v", mark, err)
		}
		if err := patch(ui, `{"metadata":{"annotations":{"`+mark+`":null}}}`); err != nil {
			t.Fatalf("taking %s back: %v", mark, err)
		}
	}

	for what, body := range map[string]string{
		"changing another annotation":   `{"metadata":{"annotations":{"kept":"no"}}}`,
		"removing another annotation":   `{"metadata":{"annotations":{"kept":null}}}`,
		"changing a label":              `{"metadata":{"labels":{"` + clusterv1.ClusterNameLabel + `":"two"}}}`,
		"adding a finalizer":            `{"metadata":{"finalizers":["kubehenge.dev/held"]}}`,
		"changing the spec":             `{"spec":{"version":"v1.36.5"}}`,
		"marking and changing the spec": `{"metadata":{"annotations":{"` + clusterv1.DeleteMachineAnnotation + `":""}},"spec":{"version":"v1.36.5"}}`,
	} {
		if err := patch(ui, body); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), outside) {
			t.Errorf("%s: %v, want refused", what, err)
		}
	}

	elsewhere := poolMachine("one", "one-cp-1")
	elsewhere.Namespace = "team-a"
	// team-a is not among the interface's namespaces here.
	if err := ui.Patch(ctx, elsewhere, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+clusterv1.RemediateMachineAnnotation+`":""}}}`))); !apierrors.IsForbidden(err) {
		t.Errorf("a machine outside the interface's namespaces: %v, want forbidden", err)
	}

	// Anyone else, a GitOps tool or kubectl, is the policy's no concern.
	if err := patch(admin, `{"metadata":{"annotations":{"kept":"no"}}}`); err != nil {
		t.Errorf("the administrator changing an annotation: %v", err)
	}
}
