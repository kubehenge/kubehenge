package backupstore

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

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/streaming/pkg/httpstream"
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

// NewForward is a Forward through the API server cfg names, over WebSockets where the API server
// takes them and SPDY otherwise.
func NewForward(cfg *rest.Config, kube kubernetes.Interface) (*Forward, error) {
	rt, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("a port-forward to the backup store: %w", err)
	}
	return &Forward{Kube: kube, Dial: func(_ context.Context, pod string) (httpstream.Connection, error) {
		u := kube.CoreV1().RESTClient().Post().Namespace(Namespace).Resource("pods").Name(pod).SubResource("portforward").URL()
		ws, err := portforward.NewSPDYOverWebsocketDialerForStreaming(u, cfg)
		if err != nil {
			return nil, fmt.Errorf("a port-forward to the backup store: %w", err)
		}
		legacy := spdy.NewDialerForStreaming(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)
		dialer := portforward.NewFallbackDialerForStreaming(ws, legacy, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
		conn, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
		if err != nil {
			return nil, fmt.Errorf("a port-forward to the backup store's pod %s: %w", pod, err)
		}
		return conn, nil
	}}, nil
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
	for _, s := range slices.Items {
		for _, e := range s.Endpoints {
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
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(containerPort))
	headers.Set(corev1.PortForwardRequestIDHeader, "0")
	errStream, err := conn.CreateStream(headers)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("a port-forward to the backup store's pod %s: %w", pod, err)
	}
	// The error stream only reads.
	_ = errStream.Close()
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := conn.CreateStream(headers)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("a port-forward to the backup store's pod %s: %w", pod, err)
	}
	idle := f.Idle
	if idle == 0 {
		idle = forwardIdle
	}
	c := &forwardConn{conn: conn, data: data, pod: pod, idle: idle, failed: make(chan struct{}), errorsRead: make(chan struct{})}
	c.watchdog = time.AfterFunc(idle, c.stalled)
	go c.readErrors(errStream)
	return c, nil
}

// forwardConn is one forward to the store as a net.Conn. What ends it early, a message on the
// error stream or nothing moving for idle, becomes the error its reads and writes return.
type forwardConn struct {
	conn     httpstream.Connection
	data     httpstream.Stream
	pod      string
	idle     time.Duration
	watchdog *time.Timer

	// errorsRead closes once the error stream has ended.
	errorsRead chan struct{}

	mu     sync.Mutex
	err    error
	failed chan struct{}
}

// readErrors waits for the error stream, which the kubelet writes when the forward fails, such as
// when the pod goes.
func (c *forwardConn) readErrors(s httpstream.Stream) {
	defer close(c.errorsRead)
	msg, err := io.ReadAll(s)
	switch {
	case len(msg) > 0:
		c.fail(fmt.Errorf("the backup store's pod %s ended the transfer: %s", c.pod, msg))
	case err != nil:
		c.fail(fmt.Errorf("the port-forward to the backup store's pod %s broke: %w", c.pod, err))
	}
}

func (c *forwardConn) stalled() {
	c.fail(fmt.Errorf("the backup store's pod %s sent nothing for %s, so the transfer was ended", c.pod, c.idle))
}

// fail records the first cause and ends the forward, which unblocks a read or write waiting on it.
func (c *forwardConn) fail(err error) {
	c.mu.Lock()
	first := c.err == nil
	if first {
		c.err = err
		close(c.failed)
	}
	c.mu.Unlock()
	if first {
		_ = c.data.Reset()
		_ = c.conn.Close()
	}
}

// errorAfter is the forward's cause in place of err, the data stream's end, where it has one. The
// kubelet ends the error stream after the data stream, so this waits a moment for it.
func (c *forwardConn) errorAfter(err error) error {
	select {
	case <-c.errorsRead:
	case <-c.failed:
	case <-time.After(errorStreamLag):
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return err
}

// errorStreamLag is how long a forward's end waits for the error stream to say why.
const errorStreamLag = time.Second

func (c *forwardConn) Read(b []byte) (int, error) {
	n, err := c.data.Read(b)
	if n > 0 {
		c.watchdog.Reset(c.idle)
	}
	if err != nil {
		err = c.errorAfter(err)
	}
	return n, err
}

func (c *forwardConn) Write(b []byte) (int, error) {
	n, err := c.data.Write(b)
	if n > 0 {
		c.watchdog.Reset(c.idle)
	}
	if err != nil {
		err = c.errorAfter(err)
	}
	return n, err
}

func (c *forwardConn) Close() error {
	c.watchdog.Stop()
	c.conn.RemoveStreams(c.data)
	return errors.Join(c.data.Close(), c.conn.Close())
}

func (*forwardConn) LocalAddr() net.Addr  { return forwardAddr{} }
func (*forwardConn) RemoteAddr() net.Addr { return forwardAddr{} }

// The deadlines are the watchdog's: a stream has none of its own.
func (*forwardConn) SetDeadline(time.Time) error      { return nil }
func (*forwardConn) SetReadDeadline(time.Time) error  { return nil }
func (*forwardConn) SetWriteDeadline(time.Time) error { return nil }

// forwardAddr names a forward's two ends, which have no network address.
type forwardAddr struct{}

func (forwardAddr) Network() string { return "portforward" }
func (forwardAddr) String() string  { return Namespace + "/" + Service }
