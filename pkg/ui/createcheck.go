package ui

import (
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// createCheck is what a dry run of creating a cluster answers: the Cluster as the API server would
// store it, every webhook and admission policy having passed, as kubectl would apply it.
type createCheck struct {
	Manifest string `json:"manifest"`
}

// checkCreate has the API server run every check of creating a cluster without creating it.
func (s *Server) checkCreate(w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster) {
	if err := u.kube.Create(r.Context(), c, client.DryRunAll); err != nil {
		s.kubeError(w, r, err)
		return
	}
	manifest, err := clusterManifest(c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, createCheck{Manifest: manifest})
}

// clusterManifest is a Cluster as YAML to apply: what the API server sets of its own, its identity,
// history and status, left out.
func clusterManifest(c *clusterv1.Cluster) (string, error) {
	shown := c.DeepCopy()
	shown.SetGroupVersionKind(clusterv1.GroupVersion.WithKind("Cluster"))
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(shown)
	if err != nil {
		return "", fmt.Errorf("converting the cluster: %w", err)
	}
	delete(fields, "status")
	for _, f := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields"} {
		unstructured.RemoveNestedField(fields, "metadata", f)
	}
	out, err := yaml.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("writing the cluster: %w", err)
	}
	return string(out), nil
}
