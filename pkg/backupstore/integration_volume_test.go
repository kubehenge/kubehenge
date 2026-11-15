//go:build integration

package backupstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	if _, err := v.Open(ctx); !errors.Is(err, provisioner.ErrVolumeAsleep) {
		t.Fatalf("asking for the sleeping store: %v, want it asleep", err)
	}
	d, err := admin.AppsV1().Deployments(Namespace).Get(ctx, Service, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !WakeRequested(d, time.Now()) {
		t.Fatalf("the store carries no wake request: %v", d.Annotations)
	}
	if err := v.Done(ctx); err != nil {
		t.Fatalf("taking the request away: %v", err)
	}
	if d, err = admin.AppsV1().Deployments(Namespace).Get(ctx, Service, metav1.GetOptions{}); err != nil || WakeRequested(d, time.Now()) {
		t.Fatalf("the request stayed: %v, %v", d.Annotations, err)
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
