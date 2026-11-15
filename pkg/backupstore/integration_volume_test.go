//go:build integration

package backupstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// TestIntegrationInterfaceForwardsToTheStoreAlone runs the interface's account against the storage
// chart. It finds the store's pod in the Service's EndpointSlices and may open a port-forward to it,
// and to no other pod. The harness runs no kubelet, so a forward the policy lets through ends where
// the API server looks for the pod's node.
func TestIntegrationInterfaceForwardsToTheStoreAlone(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	if err := integration.OwnChildren(); err != nil {
		t.Fatal(err)
	}
	objects := integration.RenderChart(t, "kubehenge-storage", "../../management-cluster/charts/storage", "kube-system", "../appliance/testdata/storage-zones-values.yaml")
	api := integration.Start(t, &integration.Options{Objects: objects, Namespaces: []string{Namespace, "kube-system"}})
	admin, err := kubernetes.NewForConfig(api.Config)
	if err != nil {
		t.Fatal(err)
	}
	// With the interface on a port-forward, the store's port admits the controller's pods alone.
	nps, err := admin.NetworkingV1().NetworkPolicies(Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range nps.Items {
		for _, rule := range nps.Items[i].Spec.Ingress {
			if len(rule.From) != 1 || rule.From[0].PodSelector == nil || rule.From[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "kubehenge-controller" || rule.From[0].IPBlock != nil {
				t.Errorf("NetworkPolicy %s admits %+v", nps.Items[i].Name, rule.From)
			}
		}
	}
	as := rest.CopyConfig(api.Config)
	as.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + Namespace + ":kubehenge-interface",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + Namespace, "system:authenticated"},
	}
	iface, err := kubernetes.NewForConfig(as)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const store, other = "kubehenge-backup-store-5d8f7c9b4-abcde", "kubehenge-backup-store-zones-29384756-abcde"
	for _, name := range []string{store, other} {
		pod := &corev1.Pod{Name: name, Namespace: Namespace, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}}}
		if _, err = admin.CoreV1().Pods(Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	forward := func(name string) error {
		return iface.CoreV1().RESTClient().Post().Namespace(Namespace).Resource("pods").Name(name).SubResource("portforward").Do(ctx).Error()
	}
	const refused = "the interface forwards to the backup store's pods only"
	// The policy takes a moment to be enforced after it is made.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		err = forward(other)
		if err != nil && strings.Contains(err.Error(), refused) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a forward to %s: %v, want refused", other, err)
		}
	}
	if err = forward(store); err == nil || strings.Contains(err.Error(), refused) || apierrors.IsForbidden(err) {
		t.Errorf("a forward to the store's pod: %v, want it past RBAC and the policy", err)
	}
	if _, err = iface.DiscoveryV1().EndpointSlices(Namespace).List(ctx, metav1.ListOptions{}); err != nil {
		t.Errorf("listing the store's EndpointSlices: %v", err)
	}
	proxy := iface.CoreV1().RESTClient().Get().Namespace(Namespace).Resource("services").Name(Service + ":http").SubResource("proxy").Do(ctx).Error()
	if !apierrors.IsForbidden(proxy) {
		t.Errorf("the store's service proxy: %v, want forbidden", proxy)
	}
	if err = iface.CoreV1().RESTClient().Post().Namespace("kube-system").Resource("pods").Name(store).SubResource("portforward").Do(ctx).Error(); !apierrors.IsForbidden(err) {
		t.Errorf("a forward in kube-system: %v, want forbidden", err)
	}
}
