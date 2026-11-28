package provisioner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
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

// Workload reads a workload cluster, and found is false until get finds it.
func (rs *Remotes) Workload(ctx context.Context, cluster *clusterv1.Cluster) (client.Reader, bool, error) { //nolint:ireturn // a CertificateWatch's Workload, which tests stand in for with fake clients
	r, found, err := rs.get(ctx, cluster)
	if err != nil || !found {
		return nil, found, err
	}
	return r.client, true, nil
}

// Client reaches a workload cluster for writing, and found is false until get finds it.
func (rs *Remotes) Client(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, bool, error) {
	r, found, err := rs.get(ctx, cluster)
	if err != nil || !found {
		return nil, found, err
	}
	return r.client, true, nil
}

// Attach streams stdin to a container of a pod in a workload cluster, as `kubectl attach -i` does,
// over WebSocket where the API server speaks it and SPDY where not. It returns once the stream is
// sent and the container closed its side.
func (rs *Remotes) Attach(ctx context.Context, cluster *clusterv1.Cluster, namespace, pod, container string, stdin io.Reader) error {
	r, found, err := rs.get(ctx, cluster)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("the cluster has no kubeconfig yet")
	}
	cs, err := kubernetes.NewForConfig(r.config)
	if err != nil {
		return fmt.Errorf("a client for the cluster: %w", err)
	}
	u := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{Container: container, Stdin: true}, scheme.ParameterCodec).URL()
	ws, err := remotecommand.NewWebSocketExecutor(r.config, http.MethodGet, u.String())
	if err != nil {
		return fmt.Errorf("attaching to pod %s: %w", pod, err)
	}
	spdyExec, err := remotecommand.NewSPDYExecutor(r.config, http.MethodPost, u)
	if err != nil {
		return fmt.Errorf("attaching to pod %s: %w", pod, err)
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdyExec, httpstream.IsUpgradeFailure)
	if err != nil {
		return fmt.Errorf("attaching to pod %s: %w", pod, err)
	}
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin}); err != nil {
		return fmt.Errorf("streaming to pod %s: %w", pod, err)
	}
	return nil
}

// get returns the cluster's client and its configuration, and false until the cluster's control
// plane is initialised and Cluster API has made its kubeconfig. Cluster API writes the kubeconfig
// before the first API server answers, so a read before then fails on a cluster still coming up.
func (rs *Remotes) get(ctx context.Context, cluster *clusterv1.Cluster) (remote, bool, error) {
	if rs == nil {
		return remote{}, false, nil
	}
	now := time.Now()
	rs.mu.Lock()
	r, ok := rs.byCluster[remoteKey(cluster)]
	rs.mu.Unlock()
	if ok && now.Before(r.until) {
		// A kept client was made once the control plane was initialised, which Cluster API never
		// takes back.
		return r, true, nil
	}
	if !controlPlaneInitialised(cluster) {
		return remote{}, false, nil
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

// controlPlaneInitialised tells whether Cluster API has seen the cluster's first API server answer.
func controlPlaneInitialised(c *clusterv1.Cluster) bool {
	if initialised := c.Status.Initialization.ControlPlaneInitialized; initialised != nil && *initialised {
		return true
	}
	return meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition)
}

// apiDown words a failure to reach the cluster's own API on its address as its user would, and
// reports false for any other error.
func apiDown(c *clusterv1.Cluster, err error) (string, bool) {
	op, ok := errors.AsType[*net.OpError](err)
	endpoint := c.Spec.ControlPlaneEndpoint
	if !ok || op.Op != "dial" || op.Addr == nil || endpoint.Host == "" {
		return "", false
	}
	address := net.JoinHostPort(endpoint.Host, strconv.Itoa(int(endpoint.Port)))
	if op.Addr.String() != address {
		return "", false
	}
	return fmt.Sprintf("the cluster's API at %s does not answer", address), true
}
