package provisioner

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/streaming/pkg/httpstream"
	"k8s.io/streaming/pkg/httpstream/spdy"
)

const forwardPath = "/api/v1/namespaces/team-a/pods/etcd-web-cp-0/portforward"

// fakeKubelet is an API server forwarding to pod etcd-web-cp-0 as the kubelet does: over SPDY, an
// error stream and a data stream per port. The pod's port echoes what it is sent, unless refuse
// is set, which the kubelet then reports on the error stream.
type fakeKubelet struct {
	refuse string
	stop   chan struct{}

	mu      sync.Mutex
	streams []http.Header
}

func (k *fakeKubelet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != forwardPath {
		http.Error(w, "pods/portforward is forbidden", http.StatusForbidden)
		return
	}
	if _, err := httpstream.Handshake(r, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
		return
	}
	opened := make(chan httpstream.Stream, 2)
	conn := spdy.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, _ <-chan struct{}) error {
		opened <- s
		return nil
	})
	if conn == nil {
		return
	}
	defer func() { _ = conn.Close() }()
	var errStream, data httpstream.Stream
	for errStream == nil || data == nil {
		select {
		case s := <-opened:
			k.mu.Lock()
			k.streams = append(k.streams, s.Headers())
			k.mu.Unlock()
			if s.Headers().Get(corev1.StreamType) == corev1.StreamTypeError {
				errStream = s
			} else {
				data = s
			}
		case <-conn.CloseChan():
			return
		case <-k.stop:
			return
		}
	}
	if k.refuse != "" {
		_, _ = io.WriteString(errStream, k.refuse)
		_ = errStream.Close()
	} else {
		go func() { _, _ = io.Copy(data, data) }()
	}
	select {
	case <-conn.CloseChan():
	case <-k.stop:
	}
}

func (k *fakeKubelet) headers() []http.Header {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.streams)
}

func startKubelet(t *testing.T, refuse string) (*fakeKubelet, *rest.Config) {
	t.Helper()
	k := &fakeKubelet{refuse: refuse, stop: make(chan struct{})}
	config := serveKubeAPI(t, k)
	// Registered after the server's own clean-up, so it runs first and lets the handlers return.
	t.Cleanup(func() { close(k.stop) })
	config.Timeout = 30 * time.Second
	return k, config
}

// The connection reaches the pod's port through the API server, and carries bytes both ways.
func TestPortForwardReachesThePodsPort(t *testing.T) {
	t.Parallel()
	k, config := startKubelet(t, "")
	conn, err := portForward(config, "team-a", "etcd-web-cp-0", etcdPort)
	if err != nil {
		t.Fatal(err)
	}
	sent := []byte("etcd's gRPC, as far as the port can tell")
	if _, err := conn.Write(sent); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(sent))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sent) {
		t.Errorf("came back %q", got)
	}
	headers := k.headers()
	types := make([]string, 0, len(headers))
	for _, h := range headers {
		types = append(types, h.Get(corev1.StreamType))
		if h.Get(corev1.PortHeader) != "2379" || h.Get(corev1.PortForwardRequestIDHeader) != "0" {
			t.Errorf("stream %v", h)
		}
	}
	slices.Sort(types)
	if !slices.Equal(types, []string{corev1.StreamTypeData, corev1.StreamTypeError}) {
		t.Errorf("streams %v", types)
	}
	if config.Timeout != 30*time.Second {
		t.Errorf("the caller's configuration was changed: timeout %s", config.Timeout)
	}
	if conn.RemoteAddr().Network() != "port-forward" || conn.LocalAddr().String() != "port-forward" || conn.SetDeadline(time.Now()) != nil {
		t.Error("the forwarded connection does not stand in for a network connection")
	}
	if err := conn.Close(); err != nil {
		t.Errorf("closing: %v", err)
	}
}

// A port the kubelet cannot reach ends the connection, so the etcd client fails rather than waits.
func TestPortForwardEndsWhenTheKubeletReportsAnError(t *testing.T) {
	t.Parallel()
	const metricsPort = 2381
	_, config := startKubelet(t, "an error occurred forwarding 2381 -> 2381: connection refused")
	conn, err := portForward(config, "team-a", "etcd-web-cp-0", metricsPort)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Read(make([]byte, 16)); err == nil {
		t.Errorf("read %d bytes from a refused port", n)
	}
}

func TestPortForwardFailsWhereTheAPIServerRefuses(t *testing.T) {
	t.Parallel()
	_, config := startKubelet(t, "")
	if _, err := portForward(config, "team-a", "kube-apiserver-web-cp-0", etcdPort); err == nil ||
		!strings.Contains(err.Error(), "port-forward to team-a/kube-apiserver-web-cp-0") {
		t.Errorf("a pod the API server does not forward to: %v", err)
	}
	broken := rest.CopyConfig(config)
	broken.CAData = []byte("not a certificate")
	if _, err := portForward(broken, "team-a", "etcd-web-cp-0", etcdPort); err == nil || !strings.Contains(err.Error(), "port-forward") {
		t.Errorf("an unusable configuration: %v", err)
	}
}
