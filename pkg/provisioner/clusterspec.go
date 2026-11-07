package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// ClusterSpecFile is the name of a cluster's spec in the backup target.
const ClusterSpecFile = "cluster.yaml"

// clusterSpecKind is the kind of the one object a spec must hold.
const clusterSpecKind = "Cluster"

// ClusterSpecClusterLabel marks the ConfigMaps and Secrets that belong to one Cluster and travel
// with its spec.
const ClusterSpecClusterLabel = "kubehenge.dev/cluster-name"

// ClusterSpecKey is where a cluster's spec is kept in a target, beside nothing the cluster's own
// backups write.
func ClusterSpecKey(prefix, namespace, name string) string {
	return path.Join(prefix, "clusters", namespace+"."+name, ClusterSpecFile)
}

// ClusterSpecYAML is a cluster's spec as a YAML stream: the Cluster as it is created, without its
// status and the fields the API server fills in, then the Kubehenge objects that belong to it.
// Secrets keep their keys and lose their values. The same cluster gives the same bytes.
func ClusterSpecYAML(c *clusterv1.Cluster, referenced []client.Object) ([]byte, error) {
	keep := &clusterv1.Cluster{
		Name: c.Name, Namespace: c.Namespace, Labels: c.Labels, Annotations: withoutKubectl(c.Annotations), Spec: c.Spec,
	}
	keep.APIVersion, keep.Kind = clusterv1.GroupVersion.String(), clusterSpecKind
	docs := [][]byte{}
	doc, err := cleanYAML(keep)
	if err != nil {
		return nil, err
	}
	docs = append(docs, doc)
	for _, o := range referenced {
		doc, err := cleanYAML(o)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return bytes.Join(docs, []byte("---\n")), nil
}

func withoutKubectl(in map[string]string) map[string]string {
	var out map[string]string
	for k, v := range in {
		if strings.HasPrefix(k, "kubectl.kubernetes.io/") {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// cleanYAML encodes an object without the fields the API server fills in.
func cleanYAML(o any) ([]byte, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("encoding a cluster's object: %w", err)
	}
	var m map[string]any
	if err = json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("encoding a cluster's object: %w", err)
	}
	delete(m, "status")
	if meta, ok := m["metadata"].(map[string]any); ok {
		for _, k := range []string{"creationTimestamp", "resourceVersion", "uid", "generation", "managedFields", "ownerReferences", "finalizers", "deletionTimestamp"} {
			delete(meta, k)
		}
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding a cluster's object: %w", err)
	}
	return out, nil
}

// ClusterSpecObjects lists the ConfigMaps and Secrets that belong to the cluster, Secrets without
// their values, in a fixed order.
func ClusterSpecObjects(ctx context.Context, kube client.Reader, c *clusterv1.Cluster) ([]client.Object, error) {
	in := client.InNamespace(c.Namespace)
	label := client.MatchingLabels{ClusterSpecClusterLabel: c.Name}
	var maps corev1.ConfigMapList
	if err := kube.List(ctx, &maps, in, label); err != nil {
		return nil, fmt.Errorf("listing the cluster's ConfigMaps: %w", err)
	}
	var secrets corev1.SecretList
	if err := kube.List(ctx, &secrets, in, label); err != nil {
		return nil, fmt.Errorf("listing the cluster's Secrets: %w", err)
	}
	var out []client.Object
	for i := range maps.Items {
		cm := &maps.Items[i]
		cm.APIVersion, cm.Kind = "v1", "ConfigMap"
		out = append(out, cm)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		s.APIVersion, s.Kind = "v1", "Secret"
		for k := range s.Data {
			s.Data[k] = []byte{}
		}
		s.StringData = nil
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b client.Object) int {
		return strings.Compare(a.GetObjectKind().GroupVersionKind().Kind+a.GetName(), b.GetObjectKind().GroupVersionKind().Kind+b.GetName())
	})
	return out, nil
}

// ClusterSpecSum identifies a cluster's exported spec, so that a write happens on a change only.
func ClusterSpecSum(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ParseClusterSpec reads the Cluster out of an exported spec.
func ParseClusterSpec(raw []byte) (*clusterv1.Cluster, error) {
	for doc := range bytes.SplitSeq(raw, []byte("\n---")) {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &head); err != nil || head.Kind != clusterSpecKind {
			continue
		}
		c := &clusterv1.Cluster{}
		if err := yaml.UnmarshalStrict(doc, c); err != nil {
			return nil, fmt.Errorf("the exported cluster: %w", err)
		}
		if c.Name == "" {
			return nil, errors.New("the exported cluster has no name")
		}
		return c, nil
	}
	return nil, errors.New("the file holds no Cluster")
}
