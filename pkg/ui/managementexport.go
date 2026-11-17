package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// exportObjects reads Kubehenge's clusters as their specs, Secrets sealed with the recovery key,
// and the ClusterClasses they use, as one YAML stream. Each management backup carries it.
func (s *Server) exportObjects(ctx context.Context) ([]byte, error) {
	kube := s.currentKube()
	if kube == nil {
		return nil, errors.New("the management cluster cannot be reached yet")
	}
	var list clusterv1.ClusterList
	if err := kube.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing the clusters: %w", err)
	}
	var docs [][]byte
	var classes []client.ObjectKey
	for i := range list.Items {
		c := &list.Items[i]
		if !provisioner.Owns(c, s.ClassNamespace) {
			continue
		}
		objects, err := provisioner.ClusterSpecObjects(ctx, kube, c)
		if err != nil {
			return nil, fmt.Errorf("cluster %s/%s: %w", c.Namespace, c.Name, err)
		}
		raw, err := provisioner.ClusterSpecYAML(c, objects, s.specSealer())
		if err != nil {
			return nil, fmt.Errorf("cluster %s/%s: %w", c.Namespace, c.Name, err)
		}
		docs = append(docs, raw)
		key := client.ObjectKey{Namespace: c.Spec.Topology.ClassRef.Namespace, Name: c.Spec.Topology.ClassRef.Name}
		if key.Namespace == "" {
			key.Namespace = c.Namespace
		}
		if !slices.Contains(classes, key) {
			classes = append(classes, key)
		}
	}
	slices.SortFunc(classes, func(a, b client.ObjectKey) int { return compareKeys(a.String(), b.String()) })
	for _, key := range classes {
		var class clusterv1.ClusterClass
		if err := kube.Get(ctx, key, &class); err != nil {
			return nil, fmt.Errorf("ClusterClass %s: %w", key, err)
		}
		keep := clusterv1.ClusterClass{Spec: class.Spec}
		keep.APIVersion, keep.Kind = clusterv1.GroupVersion.String(), "ClusterClass"
		keep.Name, keep.Namespace, keep.Labels, keep.Annotations = class.Name, class.Namespace, class.Labels, class.Annotations
		raw, err := yaml.Marshal(&keep)
		if err != nil {
			return nil, fmt.Errorf("ClusterClass %s: %w", key, err)
		}
		docs = append(docs, raw)
	}
	return bytes.Join(docs, []byte("---\n")), nil
}

func compareKeys(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
