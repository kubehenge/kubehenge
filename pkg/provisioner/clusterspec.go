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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// SealedSecretAnnotation holds a Secret's values in a spec, sealed.
const SealedSecretAnnotation = "kubehenge.dev/sealed-data" //nolint:gosec // an annotation's name

// SecretSealer seals a Secret's data, as JSON, for a spec, so that only the holder of a recovery key
// reads it again. A spec made without one holds no Secret value at all.
type SecretSealer func(plain []byte) (string, error)

// specSecretsPrefix opens a spec's first line, which says whether its Secrets hold their values.
const specSecretsPrefix = "# Secrets: "

// Secret states a spec's first line gives.
const (
	SpecSecretsNone    = "none"
	SpecSecretsSealed  = "sealed"
	SpecSecretsOmitted = "omitted"
)

// ClusterSpecYAML is a cluster's spec as a YAML stream: the Cluster as it is created, without its
// status and the fields the API server fills in, then the Kubehenge objects that belong to it.
// A Secret keeps its keys. Its values are sealed by seal into an annotation, or left out where seal
// is nil. The first line says which. The same cluster gives the same bytes where seal does.
func ClusterSpecYAML(c *clusterv1.Cluster, referenced []client.Object, seal SecretSealer) ([]byte, error) {
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
	secrets := 0
	for _, o := range referenced {
		if s, ok := o.(*corev1.Secret); ok {
			secrets++
			if o, err = sealedCopy(s, seal); err != nil {
				return nil, err
			}
		}
		doc, err := cleanYAML(o)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	state := SpecSecretsNone
	switch {
	case secrets > 0 && seal != nil:
		state = SpecSecretsSealed
	case secrets > 0:
		state = SpecSecretsOmitted
	}
	head := fmt.Sprintf("%s%s, %d\n", specSecretsPrefix, state, secrets)
	return append([]byte(head), bytes.Join(docs, []byte("---\n"))...), nil
}

// SpecSecretsState reads the state of a spec's Secrets from its first line.
func SpecSecretsState(raw []byte) (string, int) {
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	rest, ok := strings.CutPrefix(string(line), specSecretsPrefix)
	if !ok {
		return SpecSecretsOmitted, 0
	}
	state, n, _ := strings.Cut(rest, ", ")
	count, _ := strconv.Atoi(n)
	return state, count
}

// sealedCopy is the Secret as a spec keeps it: the keys with no value, and the values sealed.
func sealedCopy(s *corev1.Secret, seal SecretSealer) (*corev1.Secret, error) {
	out := s.DeepCopy()
	out.APIVersion, out.Kind = "v1", "Secret"
	plain := map[string][]byte{}
	for k, v := range s.Data {
		plain[k] = v
		out.Data[k] = []byte{}
	}
	out.StringData = nil
	if seal == nil {
		return out, nil
	}
	raw, err := json.Marshal(plain)
	if err != nil {
		return nil, fmt.Errorf("encoding Secret %s: %w", s.Name, err)
	}
	sealed, err := seal(raw)
	if err != nil {
		return nil, fmt.Errorf("sealing Secret %s: %w", s.Name, err)
	}
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	out.Annotations[SealedSecretAnnotation] = sealed
	return out, nil
}

// SpecSecret is a Secret as a spec holds it.
type SpecSecret struct {
	Name, Namespace string
	Labels          map[string]string
	Annotations     map[string]string
	Type            corev1.SecretType
	// Sealed is the values, sealed. It is empty where the spec was made without them.
	Sealed string
}

// ParseSpecSecrets reads the Secrets out of an exported spec.
func ParseSpecSecrets(raw []byte) ([]SpecSecret, error) {
	var out []SpecSecret
	for doc := range bytes.SplitSeq(raw, []byte("\n---")) {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &head); err != nil || head.Kind != secretKind {
			continue
		}
		var s corev1.Secret
		if err := yaml.Unmarshal(doc, &s); err != nil {
			return nil, fmt.Errorf("a Secret in the exported spec: %w", err)
		}
		sealed := s.Annotations[SealedSecretAnnotation]
		delete(s.Annotations, SealedSecretAnnotation)
		out = append(out, SpecSecret{Name: s.Name, Namespace: s.Namespace, Labels: s.Labels, Annotations: s.Annotations, Type: s.Type, Sealed: sealed})
	}
	return out, nil
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

// providerSecretSuffixes are the suffixes of the Secrets the control-plane and bootstrap providers
// create beside a Cluster and look up again when they exist, read from the pinned sources of each.
// A suffix a cluster's distribution does not use is skipped, since ClusterSpecObjects keeps only
// the Secrets that exist.
//   - kubeadm (Cluster API util/secret): ca, etcd, proxy (front proxy CA), sa (service account key)
//   - RKE2 (pkg/secret): ca (server CA), cca (client CA), peer-etcd (etcd peer CA), etcd (etcd server
//     CA) and token (the join token, which also encrypts RKE2's bootstrap data in etcd)
//   - k3s (pkg/secret, pkg/token): ca, cca, etcd (embedded etcd only) and token
//   - Talos (the bootstrap provider's secrets.go): talos (the secrets bundle, which holds the
//     Kubernetes, etcd, aggregator and Talos CAs and the service account key) and ca (the
//     Kubernetes CA derived from it)
var providerSecretSuffixes = []string{"ca", "cca", etcdCASuffix, "peer-etcd", "proxy", "sa", joinTokenSuffix, "talos"}

// The providers' suffixes that other names in this package spell the same.
const (
	etcdCASuffix    = etcdKey
	joinTokenSuffix = dns01TokenKey
)

// ClusterSpecSecretNames are the Secrets in a cluster's namespace that a rebuilt cluster cannot do
// without, by the names the controller and the interface give them. Each is here because it cannot
// be made again, or only at a cost:
//   - the credentials typed into the interface for the cluster (a backup target's keys, a DNS
//     provider's token, a Git repository's login, an identity provider's client secret) exist
//     nowhere else
//   - the GitOps Secret holds the deploy key registered at the repository host
//   - the backup keys decrypt the cluster's backups in the target
//   - the encryption key decrypts the Secrets in a restored etcd
//   - the ingress CA is the root clients already trust
//   - the certificate authorities, service account key and join token the control-plane providers
//     keep (see providerSecretSuffixes): a restored etcd keeps the old certificates and tokens, and
//     a rebuilt control plane that made new ones would break every kubeconfig, service account
//     token, etcd member and webhook that trusts the old
//
// Left out because the controller makes them again: the kubeconfig and the talosconfig (both are
// signed from the authorities above), the Proxmox VE identities' tokens (new ones are minted), and
// the autoscaler's and cluster mesh's copies, which the controller derives from other Secrets.
// Credentials the appliance keeps for every cluster (notifications, a shared backup or DNS login)
// belong to the appliance and come back with the management cluster's etcd. A Secret of the
// cluster's owner inside the workload cluster, named by existingSecret, is not in the management
// cluster.
func ClusterSpecSecretNames(c *clusterv1.Cluster) []string {
	names := []string{GitOpsSecretName(c.Name), BackupKeysSecretName(c.Name), encryptionSecretName(c), IngressCAName(c.Name)}
	for _, suffix := range providerSecretSuffixes {
		names = append(names, c.Name+"-"+suffix)
	}
	for _, p := range credentialPurposes {
		if !p.ApplianceOnly {
			names = append(names, CredentialName(c.Name, p.Name))
		}
	}
	return names
}

// SpecSecretBelongs tells whether a Secret of a spec may be restored for the cluster in it: one the
// cluster is known by, in the cluster's own namespace, or one labelled for it.
func SpecSecretBelongs(c *clusterv1.Cluster, s *SpecSecret) bool {
	if s.Namespace != c.Namespace {
		return false
	}
	return slices.Contains(ClusterSpecSecretNames(c), s.Name) || s.Labels[ClusterSpecClusterLabel] == c.Name
}

// ClusterSpecObjects collects what a cluster is rebuilt from besides itself, in a fixed order: the
// Secrets named by ClusterSpecSecretNames, and every ConfigMap and Secret labelled
// kubehenge.dev/cluster-name for the cluster, for what a user wants kept besides. The Secrets keep
// their values, which ClusterSpecYAML seals or leaves out.
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
	have := map[string]bool{}
	var out []client.Object
	for i := range maps.Items {
		cm := &maps.Items[i]
		cm.APIVersion, cm.Kind = "v1", "ConfigMap"
		cm.Annotations = withoutKubectl(cm.Annotations)
		out = append(out, cm)
	}
	for i := range secrets.Items {
		have[secrets.Items[i].Name] = true
		out = append(out, &secrets.Items[i])
	}
	for _, name := range ClusterSpecSecretNames(c) {
		if have[name] {
			continue
		}
		s := &corev1.Secret{}
		switch err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, s); {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("reading Secret %s: %w", name, err)
		}
		out = append(out, s)
	}
	for _, o := range out {
		if s, ok := o.(*corev1.Secret); ok {
			s.APIVersion, s.Kind = "v1", secretKind
			s.Annotations = withoutKubectl(s.Annotations)
		}
	}
	slices.SortFunc(out, func(a, b client.Object) int {
		return strings.Compare(a.GetObjectKind().GroupVersionKind().Kind+a.GetName(), b.GetObjectKind().GroupVersionKind().Kind+b.GetName())
	})
	return out, nil
}

// ClusterSpecSum identifies a cluster's spec with its Secrets' values, so that a write happens on a
// change only. Sealing is random, so the sum is over the values themselves, never stored.
func ClusterSpecSum(c *clusterv1.Cluster, referenced []client.Object) (string, error) {
	raw, err := ClusterSpecYAML(c, referenced, func(plain []byte) (string, error) {
		sum := sha256.Sum256(plain)
		return hex.EncodeToString(sum[:]), nil
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
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
