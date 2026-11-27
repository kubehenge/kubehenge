package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// specSecretsKey holds the names in a cluster's SpecSecretsConfigMap, one to a line.
const specSecretsKey = "names"

// SpecSecretsConfigMap names the ConfigMap in a cluster's namespace that lists the Secrets labelled
// ClusterSpecClusterLabel for it, which the Cluster owns and takes with it. The interface may get a
// Secret by name but never list them, so it reads the names here, where the controller wrote them.
func SpecSecretsConfigMap(cluster string) string { return cluster + "-spec-secrets" }

// SpecSecretNames are the names of the Secrets labelled for the cluster, as the controller last
// recorded them. A cluster without a record has none.
func SpecSecretNames(ctx context.Context, kube client.Reader, c *clusterv1.Cluster) ([]string, error) {
	cm := &corev1.ConfigMap{}
	switch err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: SpecSecretsConfigMap(c.Name)}, cm); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", SpecSecretsConfigMap(c.Name), err)
	}
	return strings.Fields(cm.Data[specSecretsKey]), nil
}

// SpecSecrets keeps each cluster's SpecSecretsConfigMap current.
type SpecSecrets struct {
	Client   client.Client
	Interval time.Duration
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*SpecSecrets) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (s *SpecSecrets) Start(ctx context.Context) error {
	tick := time.NewTicker(s.Interval)
	defer tick.Stop()
	for {
		run, cancel := context.WithTimeout(ctx, s.Interval)
		err := s.RunOnce(run)
		cancel()
		if err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).WithName("spec-secrets").Error(err, "recording the Secrets labelled for clusters")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// RunOnce writes the record of every cluster whose labelled Secrets differ from it. A cluster that
// has none and no record gets none.
func (s *SpecSecrets) RunOnce(ctx context.Context) error {
	secrets := &corev1.SecretList{}
	if err := s.Client.List(ctx, secrets, client.HasLabels{ClusterSpecClusterLabel}); err != nil {
		return fmt.Errorf("listing the labelled Secrets: %w", err)
	}
	names := map[client.ObjectKey][]string{}
	for i := range secrets.Items {
		key := client.ObjectKey{Namespace: secrets.Items[i].Namespace, Name: secrets.Items[i].Labels[ClusterSpecClusterLabel]}
		names[key] = append(names[key], secrets.Items[i].Name)
	}
	clusters := &clusterv1.ClusterList{}
	if err := s.Client.List(ctx, clusters); err != nil {
		return fmt.Errorf("listing the clusters: %w", err)
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		want := names[client.ObjectKeyFromObject(c)]
		slices.Sort(want)
		if err := s.record(ctx, c, strings.Join(want, "\n")); err != nil {
			return err
		}
	}
	return nil
}

func (s *SpecSecrets) record(ctx context.Context, c *clusterv1.Cluster, want string) error {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: c.Namespace, Name: SpecSecretsConfigMap(c.Name)}
	switch err := s.Client.Get(ctx, key, cm); {
	case apierrors.IsNotFound(err):
		if want == "" {
			return nil
		}
		cm = &corev1.ConfigMap{
			Namespace: key.Namespace, Name: key.Name, OwnerReferences: []metav1.OwnerReference{CredentialOwner(c)},
			Data: map[string]string{specSecretsKey: want},
		}
		if err = s.Client.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("recording the Secrets labelled for cluster %s: %w", c.Name, err)
		}
	case err != nil:
		return fmt.Errorf("reading %s: %w", key.Name, err)
	case cm.Data[specSecretsKey] != want:
		cm.Data = map[string]string{specSecretsKey: want}
		if err = s.Client.Update(ctx, cm); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			return fmt.Errorf("recording the Secrets labelled for cluster %s: %w", c.Name, err)
		}
	}
	return nil
}
