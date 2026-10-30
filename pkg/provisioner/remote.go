package provisioner

import (
	"context"
	"fmt"
	"sync"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// remoteScheme holds the types read and written in workload clusters: Secrets, ConfigMaps and the
// other core types, persistent volumes, certificate signing requests, network policies, the
// interface's identities' roles and token requests, CRDs and admission policies, the autoscaler
// that runs inside a cluster, and Rancher's and Velero's own and the distributions' HelmCharts
// through unstructured objects or their metadata.
var remoteScheme = newRemoteScheme()

func newRemoteScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, storagev1.AddToScheme, certificatesv1.AddToScheme, networkingv1.AddToScheme,
		rbacv1.AddToScheme, authenticationv1.AddToScheme, appsv1.AddToScheme,
		apiextensionsv1.AddToScheme, admissionregistrationv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	return s
}

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

// ClusterAPIs tells whether workload clusters' API servers answer.
type ClusterAPIs interface {
	// Ready fails until the cluster's API server answers on the cluster's address that it is ready.
	Ready(ctx context.Context, cluster *clusterv1.Cluster) error
}

// Ready implements ClusterAPIs.
func (rs *Remotes) Ready(ctx context.Context, cluster *clusterv1.Cluster) error {
	r, found, err := rs.get(ctx, cluster)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("cluster %s/%s has no kubeconfig yet", cluster.Namespace, cluster.Name)
	}
	cs, err := kubernetes.NewForConfig(r.config)
	if err != nil {
		return fmt.Errorf("reaching cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	if _, err := cs.CoreV1().RESTClient().Get().AbsPath("/readyz").DoRaw(ctx); err != nil {
		return fmt.Errorf("the API server of cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return nil
}

// Workload reads a workload cluster; found is false until Cluster API has made its kubeconfig.
func (rs *Remotes) Workload(ctx context.Context, cluster *clusterv1.Cluster) (client.Reader, bool, error) { //nolint:ireturn // a CertificateWatch's Workload, which tests stand in for with fake clients
	r, found, err := rs.get(ctx, cluster)
	if err != nil || !found {
		return nil, found, err
	}
	return r.client, true, nil
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
