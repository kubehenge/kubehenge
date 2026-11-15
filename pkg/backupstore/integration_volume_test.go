//go:build integration

package backupstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/test/integration"
)

// TestIntegrationControllerAsksForTheVolume runs Volume as the controller's account against the
// storage chart the module installs for a backup volume. The account asks for the store and takes
// the request away again, and changes nothing else of the store.
func TestIntegrationControllerAsksForTheVolume(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	if err := integration.OwnChildren(); err != nil {
		t.Fatal(err)
	}
	objects := integration.RenderChart(t, "kubehenge-storage", "../../management-cluster/charts/storage", "kube-system", "../appliance/testdata/storage-zones-values.yaml")
	checkStorePortOpen(t, objects)
	api := integration.Start(t, &integration.Options{Objects: objects, Namespaces: []string{Namespace, "kube-system"}})
	admin, err := kubernetes.NewForConfig(api.Config)
	if err != nil {
		t.Fatal(err)
	}
	as := rest.CopyConfig(api.Config)
	as.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + Namespace + ":kubehenge-controller",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + Namespace, "system:authenticated"},
	}
	controller, err := kubernetes.NewForConfig(as)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	patch := func(c kubernetes.Interface, name, body string) error {
		_, perr := c.AppsV1().Deployments(Namespace).Patch(ctx, name, types.MergePatchType, []byte(body), metav1.PatchOptions{})
		return perr
	}

	// The policy takes a moment to be enforced after it is made.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		err = patch(controller, Service, `{"metadata":{"annotations":{"probe":"x"}}}`)
		if apierrors.IsInvalid(err) {
			break
		}
		if err != nil {
			t.Fatalf("another annotation of the store: %v", err)
		}
		if err = patch(admin, Service, `{"metadata":{"annotations":{"probe":null}}}`); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the API server let the controller change any annotation of the store for 30 seconds")
		}
	}

	v := &Volume{Kube: controller}
	for _, holder := range []string{"snapshots of a/web", "restore of b/db"} {
		if _, err = v.Open(ctx, holder); !errors.Is(err, provisioner.ErrVolumeAsleep) {
			t.Fatalf("%s asking for the sleeping store: %v, want it asleep", holder, err)
		}
	}
	for i, holder := range []string{"snapshots of a/web", "restore of b/db"} {
		if err = v.Done(ctx, holder); err != nil {
			t.Fatalf("taking %s's request away: %v", holder, err)
		}
		d, gerr := admin.AppsV1().Deployments(Namespace).Get(ctx, Service, metav1.GetOptions{})
		if gerr != nil {
			t.Fatal(gerr)
		}
		if left := i == 0; WakeRequested(d, time.Now()) != left {
			t.Fatalf("after %s's Done the store is asked for %v, want %v: %v", holder, !left, left, d.Annotations)
		}
	}

	const outside = "the controller changes nothing of the backup store but its wake request"
	for what, body := range map[string]string{
		"adding an annotation":              `{"metadata":{"annotations":{"probe":"x"}}}`,
		"the interface's wake request":      `{"metadata":{"annotations":{"` + InterfaceWakeAnnotation + `":"2026-11-15T00:00:00Z"}}}`,
		"a label":                           `{"metadata":{"labels":{"probe":"x"}}}`,
		"scaling":                           `{"spec":{"replicas":1}}`,
		"the wake request and the spec too": `{"metadata":{"annotations":{"` + WakeRequestAnnotation + `":"2026-11-15T00:00:00Z"}},"spec":{"replicas":1}}`,
	} {
		if err := patch(controller, Service, body); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), outside) {
			t.Errorf("%s: %v, want refused", what, err)
		}
	}
	if _, err := controller.AppsV1().Deployments("kube-system").List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("listing other Deployments: %v, want forbidden", err)
	}
}

// storePort is the store's port in the storage chart.
const storePort = 8080

// checkStorePortOpen holds the store's NetworkPolicy to letting any source reach its port: the API
// server's service proxy, through which the appliance and the interface reach the store, comes from
// a node's address or from the pod network's own on another node, and no pod selector covers it.
func checkStorePortOpen(t *testing.T, objects []*unstructured.Unstructured) {
	t.Helper()
	for _, o := range objects {
		if o.GetKind() != "NetworkPolicy" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(o.Object, "spec", "ingress")
		for _, r := range rules {
			rule, _ := r.(map[string]any)
			ports, _, _ := unstructured.NestedSlice(rule, "ports")
			_, narrowed := rule["from"]
			if !narrowed && slices.ContainsFunc(ports, func(p any) bool {
				port, _ := p.(map[string]any)
				return fmt.Sprint(port["port"]) == strconv.Itoa(storePort)
			}) {
				return
			}
		}
		t.Errorf("NetworkPolicy %s lets only some sources reach the store's port %d: %v", o.GetName(), storePort, rules)
		return
	}
	t.Error("the storage chart holds no NetworkPolicy for the store")
}
