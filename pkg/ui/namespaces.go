package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// interfaceConfigMap is where the chart lists the namespaces it gives the interface rights in,
// beside the ClusterClasses, under namespacesKey as a JSON list.
const (
	interfaceConfigMap = "kubehenge-interface"
	namespacesKey      = "namespaces"
)

// managedNamespaces are the namespaces the interface creates and changes clusters in, the one new
// clusters go to unless they name another first.
type managedNamespaces []string

func (m managedNamespaces) has(namespace string) bool { return slices.Contains(m, namespace) }

// namespacesPhrase names the namespaces in a sentence: "namespace clusters", or "namespaces
// clusters, team-a".
func namespacesPhrase(m managedNamespaces) string {
	if len(m) == 1 {
		return "namespace " + m[0]
	}
	return "namespaces " + strings.Join(m, ", ")
}

// namespaces reads the namespaces the chart gives the interface rights in. A chart that lists
// none, or that does not let the interface read the list, gives it rights in Namespace alone.
func (s *Server) namespaces(ctx context.Context, kube client.Client) (managedNamespaces, error) {
	own := managedNamespaces{s.Namespace}
	if kube == nil {
		return own, nil
	}
	cm := &corev1.ConfigMap{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: interfaceConfigMap}, cm)
	switch {
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		return own, nil
	case err != nil:
		return nil, fmt.Errorf("reading the namespaces the interface manages: %w", err)
	}
	var listed managedNamespaces
	if err := json.Unmarshal([]byte(cm.Data[namespacesKey]), &listed); err != nil || len(listed) == 0 || slices.Contains(listed, "") {
		return nil, fmt.Errorf("ConfigMap %s/%s lists no namespaces under %s", s.ClassNamespace, interfaceConfigMap, namespacesKey)
	}
	return listed, nil
}
