package provisioner

import (
	"context"
	"fmt"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// remoteScheme holds the types read and written in workload clusters: Secrets, ConfigMaps and the
// other core types, persistent volumes, certificate signing requests, network policies, the health
// identities' roles and token requests, and Rancher's own through unstructured objects.
var remoteScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, storagev1.AddToScheme, certificatesv1.AddToScheme, networkingv1.AddToScheme,
		rbacv1.AddToScheme, authenticationv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	return s
}()

// remoteTTL is how long a workload cluster's client is kept. Cluster API renews a kubeconfig's
// certificate long before it runs out, so a client this old still signs in.
const remoteTTL = 10 * time.Minute

// Remotes keeps one client per workload cluster for a while, shared by everything that reads
// workload clusters: making one reads the cluster's kubeconfig Secret, and a new client discovers
// the cluster's API again on its first request.
type Remotes struct {
	// Reader reads the kubeconfig Secrets, past any cache.
	Reader client.Reader

	mu        sync.Mutex
	byCluster map[string]remote
}

// remoteKey tells clusters apart, one created again under an earlier one's name too.
func remoteKey(c *clusterv1.Cluster) string {
	return string(c.UID) + "/" + c.Namespace + "/" + c.Name
}

type remote struct {
	config *rest.Config
	client client.Client
	until  time.Time
}

// get returns the cluster's client and its configuration, and false until Cluster API has made its
// kubeconfig.
func (rs *Remotes) get(ctx context.Context, cluster *clusterv1.Cluster) (remote, bool, error) {
	if rs == nil {
		return remote{}, false, nil
	}
	now := time.Now()
	rs.mu.Lock()
	r, ok := rs.byCluster[remoteKey(cluster)]
	rs.mu.Unlock()
	if ok && now.Before(r.until) {
		return r, true, nil
	}
	config, found, err := remoteConfig(ctx, rs.Reader, client.ObjectKeyFromObject(cluster))
	if err != nil || !found {
		return remote{}, false, err
	}
	c, err := client.New(config, client.Options{Scheme: remoteScheme})
	if err != nil {
		return remote{}, false, fmt.Errorf("reaching cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.byCluster == nil {
		rs.byCluster = map[string]remote{}
	}
	for key, kept := range rs.byCluster {
		if now.After(kept.until) {
			delete(rs.byCluster, key)
		}
	}
	r = remote{config: config, client: c, until: now.Add(remoteTTL)}
	rs.byCluster[remoteKey(cluster)] = r
	return r, true, nil
}

// forget drops a cluster's client after it failed, so the next use reads the kubeconfig afresh.
func (rs *Remotes) forget(cluster *clusterv1.Cluster) {
	if rs == nil || cluster == nil {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.byCluster, remoteKey(cluster))
}
