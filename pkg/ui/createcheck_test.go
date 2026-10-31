package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestCheckCreate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("POST", "/api/clusters?dryRun=All", createBody)
	expect(t, rec, http.StatusOK)

	var check createCheck
	if err := json.Unmarshal(rec.Body.Bytes(), &check); err != nil {
		t.Fatal(err)
	}
	var applied clusterv1.Cluster
	if err := yaml.UnmarshalStrict([]byte(check.Manifest), &applied); err != nil {
		t.Fatalf("manifest %q: %v", check.Manifest, err)
	}
	if applied.APIVersion != clusterv1.GroupVersion.String() || applied.Kind != "Cluster" ||
		applied.Namespace != "default" || applied.Name != "web" || applied.Annotations[createdBy] != "dev@pve" ||
		*applied.Spec.Topology.ControlPlane.Replicas != 3 {
		t.Fatalf("manifest = %s", check.Manifest)
	}
	for _, kept := range []string{"status:", "uid:", "resourceVersion:", "creationTimestamp:", "managedFields:"} {
		if strings.Contains(check.Manifest, kept) {
			t.Errorf("manifest holds %s:\n%s", kept, check.Manifest)
		}
	}

	// A dry run creates nothing, so the cluster can still be created.
	err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "web"}, &clusterv1.Cluster{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("after the dry run, get = %v", err)
	}
	expect(t, b.do("POST", "/api/clusters", createBody), http.StatusCreated)
}

func TestCheckCreateNeedsTheRightsCreatingDoes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("viewer@pve")
	expect(t, b.do("POST", "/api/clusters?dryRun=All", createBody), http.StatusForbidden)
}
