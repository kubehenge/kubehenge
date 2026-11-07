package provisioner

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubeMetrics opens the /metrics of the API server a client reaches.
func KubeMetrics(kube kubernetes.Interface) MetricsSource {
	return func(ctx context.Context) (io.ReadCloser, error) {
		body, err := kube.CoreV1().RESTClient().Get().AbsPath("/metrics").Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("the API server's metrics: %w", err)
		}
		return body, nil
	}
}

// UpgradeChecker reads what an upgrade of a workload cluster would run into inside it.
type UpgradeChecker interface {
	Check(ctx context.Context, cluster *clusterv1.Cluster, target string) UpgradeFindings
}

// upgradeCheckTimeout bounds one cluster's check: the metrics are a few MiB.
const upgradeCheckTimeout = time.Minute

// IdentityChecker reads a workload cluster as its health identity, with the token Kubehenge keeps
// in the identity's Secret beside the Cluster.
type IdentityChecker struct {
	Reader client.Reader
	Now    func() time.Time
}

// Check implements UpgradeChecker.
func (k *IdentityChecker) Check(ctx context.Context, cluster *clusterv1.Cluster, target string) UpgradeFindings {
	unread := func(why string) UpgradeFindings {
		return UpgradeFindings{RemovedAPIs: []RemovedAPI{}, Webhooks: []RiskyWebhook{}, Unchecked: []string{why}}
	}
	ctx, cancel := context.WithTimeout(ctx, upgradeCheckTimeout)
	defer cancel()
	secret := &corev1.Secret{}
	err := k.Reader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: IdentitySecretName(cluster.Name, IdentityHealth)}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return unread("Kubehenge's read access to the cluster is not set up yet")
	case err != nil:
		return unread(fmt.Sprintf("reading the health identity (%v)", err))
	}
	expires, err := time.Parse(time.RFC3339, secret.Annotations[IdentityExpiresAnnotation])
	token := string(secret.Data[IdentityTokenKey])
	server := string(secret.Data[IdentityServerKey])
	switch {
	case err != nil || token == "" || server == "":
		return unread("Kubehenge's read access to the cluster is not set up yet")
	case !k.now().Before(expires):
		return unread("Kubehenge's read access to the cluster has expired")
	}
	kube, err := kubernetes.NewForConfig(&rest.Config{
		Host: server, BearerToken: token, CAData: secret.Data[IdentityCAKey],
		Timeout: upgradeCheckTimeout, UserAgent: "kubehenge-upgrader",
	})
	if err != nil {
		return unread(fmt.Sprintf("reaching the cluster (%v)", err))
	}
	found := CheckUpgrade(ctx, kube, KubeMetrics(kube), target)
	found.ApplyHistory(HistoryOf(cluster), k.now())
	return found
}

func (k *IdentityChecker) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}
