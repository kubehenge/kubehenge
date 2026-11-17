package backupstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/streaming/pkg/httpstream"
	spdystream "k8s.io/streaming/pkg/httpstream/spdy"
)

// storePod is the name the fake EndpointSlice gives the store's ready pod.
const storePod = "kubehenge-backup-store-5d8f7c9b4-abcde"

// kubelet stands in for the kubelet's port-forward over SPDY, to the store at target. cut, where
// set, says how many bytes of the store's answer reach the client before the pod goes. stall, set,
// passes nothing of the answer on without ending anything, as a node lost does. drip, set, passes
// the answer on in pieces this far apart.
type kubelet struct {
	target string
	cut    int
	stall  bool
	drip   time.Duration
	pods   chan string
}

func (k *kubelet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := httpstream.Handshake(r, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
		return
	}
	streams := make(chan httpstream.Stream, 2)
	conn := spdystream.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, _ <-chan struct{}) error {
		streams <- s
		return nil
	})
	if conn == nil {
		return
	}
	defer func() { _ = conn.Close() }()
	k.pods <- r.URL.Path
	var errStream, data httpstream.Stream
	for errStream == nil || data == nil {
		s := <-streams
		if s.Headers().Get(corev1.StreamType) == corev1.StreamTypeError {
			errStream = s
		} else {
			data = s
		}
	}
	store, err := new(net.Dialer).DialContext(r.Context(), "tcp", k.target)
	if err != nil {
		_, _ = io.WriteString(errStream, err.Error())
		return
	}
	defer func() { _ = store.Close() }()
	go func() { _, _ = io.Copy(store, data) }()
	switch {
	case k.stall:
		<-r.Context().Done()
	case k.drip > 0:
		for {
			if _, err := io.CopyN(data, store, 4096); err != nil {
				break
			}
			time.Sleep(k.drip)
		}
		_ = errStream.Close()
		_ = data.Close()
	case k.cut > 0:
		_, _ = io.CopyN(data, store, int64(k.cut))
		_, _ = io.WriteString(errStream, "pod "+storePod+" was deleted")
		_ = errStream.Close()
		_ = data.Reset()
	default:
		_, _ = io.Copy(data, store)
		_ = errStream.Close()
		_ = data.Close()
	}
}

// forwardTo is a Forward to a store behind the fake kubelet k, with one ready pod.
func forwardTo(t *testing.T, k *kubelet) (*Forward, *Client) {
	t.Helper()
	srv := httptest.NewServer(k)
	t.Cleanup(srv.Close)
	ready := true
	kube := fake.NewClientset(&discoveryv1.EndpointSlice{
		Name: Service + "-x", Namespace: Namespace, Labels: map[string]string{discoveryv1.LabelServiceName: Service},
		Endpoints: []discoveryv1.Endpoint{
			{TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "kubehenge-backup-store-old-zzzzz"}, Conditions: discoveryv1.EndpointConditions{Ready: new(false)}},
			{TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: storePod}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
		},
	})
	rt, upgrader, err := spdy.RoundTripperFor(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	f := &Forward{Kube: kube, Dial: func(_ context.Context, pod string) (httpstream.Connection, error) {
		u, err := url.Parse(srv.URL + "/api/v1/namespaces/" + Namespace + "/pods/" + pod + "/portforward")
		if err != nil {
			return nil, err
		}
		conn, _, err := spdy.NewDialerForStreaming(upgrader, &http.Client{Transport: rt}, http.MethodPost, u).Dial(portforward.PortForwardProtocolV1Name)
		return conn, err
	}}
	return f, &Client{Base: ForwardBase, Token: "secret", HTTP: f.HTTPClient()}
}

// storeAt serves a store with an object of size bytes under "big".
func storeAt(t *testing.T, size int) string {
	t.Helper()
	l, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: (&Server{Dir: t.TempDir(), Token: "secret"}).Handler(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	c := &Client{Base: "http://" + l.Addr().String(), Token: "secret"}
	if _, err := c.Put(t.Context(), "big", bytes.NewReader(bytes.Repeat([]byte("x"), size)), nil); err != nil {
		t.Fatal(err)
	}
	return l.Addr().String()
}

// The store answers through a port-forward to its ready pod.
func TestForwardReachesTheStoresReadyPod(t *testing.T) {
	t.Parallel()
	k := &kubelet{target: storeAt(t, 4096), pods: make(chan string, 4)}
	_, c := forwardTo(t, k)
	r, err := c.Get(t.Context(), "big")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || len(got) != 4096 {
		t.Fatalf("read %d bytes: %v", len(got), err)
	}
	if path := <-k.pods; !strings.Contains(path, "/pods/"+storePod+"/") {
		t.Errorf("forwarded to %s", path)
	}
}

// A store's pod that goes mid-transfer ends the read at once with the kubelet's reason.
func TestForwardFailsWhenThePodGoesMidTransfer(t *testing.T) {
	t.Parallel()
	k := &kubelet{target: storeAt(t, 1<<20), cut: 64 << 10, pods: make(chan string, 4)}
	_, c := forwardTo(t, k)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r, err := c.Get(ctx, "big")
	if err == nil {
		_, err = io.ReadAll(r)
		_ = r.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "ended the transfer: pod "+storePod+" was deleted") {
		t.Fatalf("err = %v, want the pod's end named", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the read waited for the test's deadline")
	}
}

// A forward on which nothing moves, as when the pod's node is lost, ends after Idle instead of
// hanging.
func TestForwardEndsAStalledTransfer(t *testing.T) {
	t.Parallel()
	k := &kubelet{target: storeAt(t, 1<<20), stall: true, pods: make(chan string, 4)}
	f, c := forwardTo(t, k)
	f.Idle = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r, err := c.Get(ctx, "big")
	if err == nil {
		_, err = io.ReadAll(r)
		_ = r.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "sent nothing for 300ms") || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the stall named", err)
	}
}

// A transfer that keeps moving outlasts Idle, however long it takes in all.
func TestForwardKeepsATransferThatMoves(t *testing.T) {
	t.Parallel()
	k := &kubelet{target: storeAt(t, 12*4096), drip: 100 * time.Millisecond, pods: make(chan string, 4)}
	f, c := forwardTo(t, k)
	f.Idle = 400 * time.Millisecond
	r, err := c.Get(t.Context(), "big")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || len(got) != 12*4096 {
		t.Fatalf("read %d bytes: %v", len(got), err)
	}
}

// With no ready pod, the forward says so before dialling.
func TestForwardNeedsAReadyPod(t *testing.T) {
	t.Parallel()
	f := &Forward{Kube: fake.NewClientset(), Dial: func(context.Context, string) (httpstream.Connection, error) {
		return nil, errors.New("dialled with no ready pod")
	}}
	c := &Client{Base: ForwardBase, Token: "secret", HTTP: f.HTTPClient()}
	if _, err := c.Get(t.Context(), "big"); err == nil || !strings.Contains(err.Error(), "no ready pod") {
		t.Fatalf("err = %v", err)
	}
}
