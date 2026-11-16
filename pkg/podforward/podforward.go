// Package podforward reaches a pod's port through the Kubernetes API server, as kubectl
// port-forward does.
package podforward

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/streaming/pkg/httpstream"
)

// Dial opens a port-forward connection to pod in namespace through the API server cfg names, over
// WebSockets where the API server takes them and SPDY otherwise.
func Dial(cfg *rest.Config, namespace, pod string) (httpstream.Connection, error) { //nolint:ireturn // the dialers give the connection as an interface
	cfg = rest.CopyConfig(cfg)
	// A forward carries a whole snapshot. The API's request timeout would cut it off.
	cfg.Timeout = 0
	rt, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("port-forward to %s/%s: %w", namespace, pod, err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("port-forward to %s/%s: %w", namespace, pod, err)
	}
	u := kube.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").Name(pod).SubResource("portforward").URL()
	ws, err := portforward.NewSPDYOverWebsocketDialerForStreaming(u, cfg)
	if err != nil {
		return nil, fmt.Errorf("port-forward to %s/%s: %w", namespace, pod, err)
	}
	legacy := spdy.NewDialerForStreaming(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)
	dialer := portforward.NewFallbackDialerForStreaming(ws, legacy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	conn, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("port-forward to %s/%s: %w", namespace, pod, err)
	}
	return conn, nil
}

// errorStreamLag is how long a forward's end waits for the error stream to say why.
const errorStreamLag = time.Second

// Port is conn's forward to port of pod as a net.Conn, which owns conn. Idle above 0 ends a forward
// on which nothing moves that long, as when the pod's node is lost and the stream stays open with
// nothing on it. What ends it early, a message on the error stream or the idle time, becomes the
// error its reads and writes return.
func Port(conn httpstream.Connection, pod string, port int, idle time.Duration) (net.Conn, error) {
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(port))
	headers.Set(corev1.PortForwardRequestIDHeader, "0")
	errStream, err := conn.CreateStream(headers)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("a port-forward to pod %s: %w", pod, err)
	}
	// The error stream only reads.
	_ = errStream.Close()
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := conn.CreateStream(headers)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("a port-forward to pod %s: %w", pod, err)
	}
	c := &forwardConn{conn: conn, data: data, pod: pod, idle: idle, failed: make(chan struct{}), errorsRead: make(chan struct{})}
	if idle > 0 {
		c.watchdog = time.AfterFunc(idle, c.stalled)
	}
	go c.readErrors(errStream)
	return c, nil
}

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

func (c *forwardConn) Read(b []byte) (int, error) {
	n, err := c.data.Read(b)
	c.moved(n)
	if err != nil {
		err = c.errorAfter(err)
	}
	return n, err
}

func (c *forwardConn) Write(b []byte) (int, error) {
	n, err := c.data.Write(b)
	c.moved(n)
	if err != nil {
		err = c.errorAfter(err)
	}
	return n, err
}

func (c *forwardConn) Close() error {
	if c.watchdog != nil {
		c.watchdog.Stop()
	}
	c.conn.RemoveStreams(c.data)
	return errors.Join(c.data.Close(), c.conn.Close())
}

func (*forwardConn) LocalAddr() net.Addr  { return forwardAddr{} }
func (*forwardConn) RemoteAddr() net.Addr { return forwardAddr{} }

// A stream has no deadlines. The watchdog and the caller's context bound each call instead.
func (*forwardConn) SetDeadline(time.Time) error      { return nil }
func (*forwardConn) SetReadDeadline(time.Time) error  { return nil }
func (*forwardConn) SetWriteDeadline(time.Time) error { return nil }

func (c *forwardConn) moved(n int) {
	if n > 0 && c.watchdog != nil {
		c.watchdog.Reset(c.idle)
	}
}

// readErrors waits for the error stream, which the kubelet writes when the forward fails, such as
// when the pod goes or its port refuses.
func (c *forwardConn) readErrors(s httpstream.Stream) {
	defer close(c.errorsRead)
	msg, err := io.ReadAll(s)
	switch {
	case len(msg) > 0:
		c.fail(fmt.Errorf("pod %s ended the transfer: %s", c.pod, msg))
	case err != nil:
		c.fail(fmt.Errorf("the port-forward to pod %s broke: %w", c.pod, err))
	}
}

func (c *forwardConn) stalled() {
	c.fail(fmt.Errorf("pod %s sent nothing for %s, so the transfer was ended", c.pod, c.idle))
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

// forwardAddr names a forward's two ends, which have no network address.
type forwardAddr struct{}

func (forwardAddr) Network() string { return "port-forward" }
func (forwardAddr) String() string  { return "port-forward" }
