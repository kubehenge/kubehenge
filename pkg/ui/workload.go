package ui

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// Workload is a workload cluster as one of Kubehenge's identities in it reaches it.
type Workload struct {
	Kube kubernetes.Interface
	// Dynamic reaches the resources without Go types here, Velero's.
	Dynamic dynamic.Interface
	// Readyz answers the API server's verbose readiness check, with any failed checks.
	Readyz func(ctx context.Context) (string, error)
	// Close lets go of its idle connections. It may be nil.
	Close func()
}

func (w *Workload) close() {
	if w.Close != nil {
		w.Close()
	}
}

// workloadTimeout bounds each request to a workload cluster's API server, as a last resort: every
// caller gives its requests a deadline of its own, none longer.
const workloadTimeout = 10 * time.Second

// connectWorkload reaches a workload cluster's API server with a bearer token. It trusts its CA alone.
func connectWorkload(server string, ca []byte, token string) (*Workload, error) {
	cfg := &rest.Config{
		Host: server, BearerToken: token, CAData: ca,
		Timeout: workloadTimeout, UserAgent: "kubehenge-interface",
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("the workload cluster's client: %w", err)
	}
	kube, err := kubernetes.NewForConfigAndClient(cfg, hc)
	if err != nil {
		return nil, fmt.Errorf("the workload cluster's client: %w", err)
	}
	dyn, err := dynamic.NewForConfigAndClient(cfg, hc)
	if err != nil {
		return nil, fmt.Errorf("the workload cluster's client: %w", err)
	}
	return &Workload{
		Kube:    kube,
		Dynamic: dyn,
		Readyz: func(ctx context.Context) (string, error) {
			// A failing check answers 500, with the checks in the body all the same.
			body, err := kube.CoreV1().RESTClient().Get().AbsPath("/readyz").Param("verbose", "true").DoRaw(ctx)
			if err != nil && len(body) == 0 {
				return "", fmt.Errorf("the API server's readiness: %w", err)
			}
			return string(body), nil
		},
		Close: hc.CloseIdleConnections,
	}, nil
}

func (s *Server) connect(server string, ca []byte, token string) (*Workload, error) {
	if s.ConnectWorkload != nil {
		return s.ConnectWorkload(server, ca, token)
	}
	return connectWorkload(server, ca, token)
}

// workloadIdle is how long a workload cluster's connection is kept unused.
const workloadIdle = 10 * time.Minute

type workloadConns struct {
	mu    sync.Mutex
	byKey map[string]*workloadConn
}

type workloadConn struct {
	token string
	w     *Workload
	used  time.Time
}

// reach connects to a cluster as one of its identities, with the token Kubehenge's controller keeps
// in the identity's Secret beside the Cluster, and keeps the connection while the token stays the
// same. Without a token to use it returns no Workload and no error: with the time the token ran
// out, or a zero time while the identity is not set up.
func (s *Server) reach(ctx context.Context, kube client.Client, c *clusterv1.Cluster, id provisioner.Identity) (*Workload, time.Time, error) {
	name := provisioner.IdentitySecretName(c.Name, id)
	secret := &corev1.Secret{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, secret)
	if apierrors.IsNotFound(err) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("reading Secret %s: %w", name, err)
	}
	server, ca := string(secret.Data[provisioner.IdentityServerKey]), secret.Data[provisioner.IdentityCAKey]
	token := string(secret.Data[provisioner.IdentityTokenKey])
	// The controller writes every token with when it runs out. One without is none of its.
	expires, err := time.Parse(time.RFC3339, secret.Annotations[provisioner.IdentityExpiresAnnotation])
	if server == "" || token == "" || err != nil {
		return nil, time.Time{}, nil
	}
	now := s.now()
	if !now.Before(expires) {
		return nil, expires, nil
	}
	key := c.Namespace + "/" + c.Name + "/" + string(c.UID) + "/" + string(id)
	wc := &s.workloads
	wc.mu.Lock()
	defer wc.mu.Unlock()
	for k, kept := range wc.byKey {
		if now.Sub(kept.used) > workloadIdle {
			kept.w.close()
			delete(wc.byKey, k)
		}
	}
	if kept, ok := wc.byKey[key]; ok && kept.token == token {
		kept.used = now
		return kept.w, time.Time{}, nil
	}
	w, err := s.connect(server, ca, token)
	if err != nil {
		return nil, time.Time{}, err
	}
	if kept, ok := wc.byKey[key]; ok {
		kept.w.close()
	}
	if wc.byKey == nil {
		wc.byKey = map[string]*workloadConn{}
	}
	wc.byKey[key] = &workloadConn{token: token, w: w, used: now}
	return w, time.Time{}, nil
}
