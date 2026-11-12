package installer

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandshakeReturnsTheChainOfAnUntrustedServer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	want := srv.Certificate()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	chain, err := handshake(ctx, conn, "127.0.0.1")
	if err != nil {
		t.Fatalf("handshake with a server this node does not trust: %v", err)
	}
	if len(chain) == 0 || !bytes.Equal(chain[0].Raw, want.Raw) {
		t.Fatalf("handshake returned %d certificates, want the server's own first", len(chain))
	}
}

func TestHandshakeFailsWithoutTLS(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if chain, err := handshake(ctx, conn, "127.0.0.1"); err == nil {
		t.Fatalf("handshake with a plain HTTP server returned %d certificates and no error", len(chain))
	}
}
