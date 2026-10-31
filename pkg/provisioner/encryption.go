package provisioner

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// encryptionConfigKey is the key of the Secret's data that kubeadm's ClusterClass writes to every
// control-plane node as the API server's encryption configuration.
const encryptionConfigKey = "encryption-config.yaml"

// secretboxKeyBytes is the size of a secretbox key, which XSalsa20 takes.
const secretboxKeyBytes = 32

func encryptionSecretName(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-encryption"
}

// ensureEncryption makes the key a kubeadm cluster's API servers encrypt Secrets in etcd with.
// RKE2 and k3s make their own. It is made once and never replaced: Secrets written under one key
// cannot be read with another.
func (r *Reconciler) ensureEncryption(ctx context.Context, cluster *clusterv1.Cluster) error {
	if distributionOf(cluster) != templates.Kubeadm {
		return nil
	}
	s := &corev1.Secret{Namespace: cluster.Namespace, Name: encryptionSecretName(cluster)}
	var failed error
	err := applyOwned(ctx, r.Client, cluster, s, func(got *corev1.Secret) {
		if len(got.Data[encryptionConfigKey]) > 0 {
			return
		}
		config, err := encryptionConfig()
		if err != nil {
			failed = err
			return
		}
		got.Type = corev1.SecretTypeOpaque
		got.Data = map[string][]byte{encryptionConfigKey: config}
	})
	if failed != nil {
		return failed
	}
	return err
}

// encryptionConfig is an EncryptionConfiguration with a new secretbox key, which the CIS benchmark
// accepts, as it does aescbc, and secretbox authenticates what it encrypts.
func encryptionConfig() ([]byte, error) {
	key := make([]byte, secretboxKeyBytes)
	_, _ = rand.Read(key) // crypto/rand.Read never fails
	config, err := yaml.Marshal(map[string]any{
		apiVersionKey: "apiserver.config.k8s.io/v1",
		"kind":        "EncryptionConfiguration",
		"resources": []any{map[string]any{
			"resources": []string{"secrets"},
			"providers": []any{map[string]any{
				"secretbox": map[string]any{"keys": []any{map[string]any{
					"name": "key1", "secret": base64.StdEncoding.EncodeToString(key),
				}}},
			}},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("writing the encryption configuration: %w", err)
	}
	return config, nil
}
