package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// A run that could not start says why on its ConfigMap and in a Warning event for kubectl.
func TestRefusedRefreshRecordsAnEvent(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.ConfigMap{Namespace: "kubehenge-system", Name: provisioner.TemplateRefreshName}).Build()
	s := &refreshStatus{kube: kube, namespace: "kubehenge-system", out: &bytes.Buffer{}}
	if err := s.refused(context.Background(), "schedule", errors.New("no API token")); err == nil {
		t.Fatal("no error")
	}
	cm := &corev1.ConfigMap{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: provisioner.TemplateRefreshName}, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data[provisioner.RefreshState] != "failed" || cm.Data[provisioner.RefreshError] != "no API token" {
		t.Errorf("status %v", cm.Data)
	}
	var events corev1.EventList
	if err := kube.List(context.Background(), &events, client.InNamespace("kubehenge-system")); err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("events %+v", events.Items)
	}
	e := events.Items[0]
	if e.Type != corev1.EventTypeWarning || e.Reason != "TemplateBuildFailed" || e.InvolvedObject.Name != provisioner.TemplateRefreshName ||
		!strings.Contains(e.Message, "template build (schedule) failed: no API token") {
		t.Errorf("event %+v", e)
	}
}
