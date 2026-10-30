package ui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func machineOf(namespace, cluster, name string) *clusterv1.Machine {
	return &clusterv1.Machine{Namespace: namespace, Name: name, Labels: map[string]string{clusterv1.ClusterNameLabel: cluster}}
}

func TestSupportBundleIsHandedOutLikeTheKubeconfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"))
	h.server.NewClientset = func() (kubernetes.Interface, error) { return kubefake.NewClientset(), nil }
	dev := h.browser()
	dev.signIn("dev@pve")
	rec := dev.do("GET", "/api/clusters/default/one/support-bundle", "")
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Header().Get("Content-Disposition"), `filename="default-one-`) || rec.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("headers %v", rec.Header())
	}
	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	if !strings.Contains(strings.Join(names, " "), "default-one/objects/cluster.x-k8s.io/Cluster/one.yaml") {
		t.Errorf("the bundle holds %v", names)
	}
	expect(t, dev.do("GET", "/api/clusters/default/two/support-bundle", ""), http.StatusForbidden)

	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("GET", "/api/clusters/default/one/support-bundle", ""), http.StatusForbidden)
}

func TestBootLogIsAskedForAndRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), machineOf("default", "one", "one-cp-a"), machineOf("default", "two", "two-cp-a"))
	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("POST", "/api/clusters/default/one/machines/two-cp-a/boot-log", ""), http.StatusNotFound)
	rec := dev.do("POST", "/api/clusters/default/one/machines/one-cp-a/boot-log", "")
	expect(t, rec, http.StatusAccepted)
	var v bootLogView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || !v.Asked || v.Log != nil {
		t.Fatalf("asked: %s", rec.Body)
	}
	// Asked again, the machine is named once.
	expect(t, dev.do("POST", "/api/clusters/default/one/machines/one-cp-a/boot-log", ""), http.StatusAccepted)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	if got := c.Annotations[provisioner.BootLogRequestAnnotation]; got != "one-cp-a" {
		t.Errorf("request = %q", got)
	}

	// The controller answers.
	delete(c.Annotations, provisioner.BootLogRequestAnnotation)
	if err := h.kube.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		Namespace: "default", Name: provisioner.BootLogsConfigMap("one"),
		Data: map[string]string{"one-cp-a": `{"read":"2026-10-13T09:00:00Z","sections":[{"name":"cloud-init-output.log","lines":["done"]}]}`},
	}
	if err := h.kube.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	rec = dev.do("GET", "/api/clusters/default/one/machines/one-cp-a/boot-log", "")
	expect(t, rec, http.StatusOK)
	v = bootLogView{}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Asked || v.Log == nil || v.Log.Sections[0].Lines[0] != "done" {
		t.Fatalf("read: %s", rec.Body)
	}

	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("GET", "/api/clusters/default/one/machines/one-cp-a/boot-log", ""), http.StatusForbidden)
	expect(t, viewer.do("POST", "/api/clusters/default/one/machines/one-cp-a/boot-log", ""), http.StatusForbidden)
}
