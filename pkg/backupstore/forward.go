package backupstore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/streaming/pkg/httpstream"

	"github.com/kubehenge/kubehenge/pkg/podforward"
)

// ForwardBase is the store's address for a Client whose HTTP goes through a Forward, which ignores
// the host.
const ForwardBase = "http://" + Service

// containerPort is the port the store's container listens on.
const containerPort = 8080

// forwardIdle ends a forward on which nothing moves this long: a store's pod lost with its node
// leaves the stream open with nothing on it.
const forwardIdle = 2 * time.Minute

// Forward reaches the store from outside the management cluster through a port-forward to its
// ready pod, a new one for each request. The store's NetworkPolicy then admits the controller's
// pods alone, since a port-forward arrives inside the pod's own network namespace.
type Forward struct {
	Kube kubernetes.Interface
	// Dial opens a port-forward connection to pod in Namespace.
	Dial func(ctx context.Context, pod string) (httpstream.Connection, error)
	// Idle is forwardIdle where 0.
	Idle time.Duration
}

// NewForward is a Forward through the API server cfg names.
func NewForward(cfg *rest.Config, kube kubernetes.Interface) *Forward {
	return &Forward{Kube: kube, Dial: func(_ context.Context, pod string) (httpstream.Connection, error) {
		return podforward.Dial(cfg, Namespace, pod)
	}}
}

// HTTPClient is a client whose every request reaches the store through a forward of its own.
func (f *Forward) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return f.dialStore(ctx) },
		DisableKeepAlives: true,
	}}
}

// readyPod is a pod behind the store's Service that is ready.
func (f *Forward) readyPod(ctx context.Context) (string, error) {
	slices, err := f.Kube.DiscoveryV1().EndpointSlices(Namespace).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + Service})
	if err != nil {
		return "", fmt.Errorf("finding the backup store's pod: %w", err)
	}
	for i := range slices.Items {
		for _, e := range slices.Items[i].Endpoints {
			if e.TargetRef != nil && e.TargetRef.Kind == "Pod" && e.Conditions.Ready != nil && *e.Conditions.Ready {
				return e.TargetRef.Name, nil
			}
		}
	}
	return "", errors.New("the backup store has no ready pod")
}

func (f *Forward) dialStore(ctx context.Context) (net.Conn, error) {
	pod, err := f.readyPod(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := f.Dial(ctx, pod)
	if err != nil {
		return nil, err
	}
	idle := f.Idle
	if idle == 0 {
		idle = forwardIdle
	}
	return podforward.Port(conn, pod, containerPort, idle) //nolint:wrapcheck // Port names the pod
}
