package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func mustProxies(t *testing.T, specs ...string) []string {
	t.Helper()
	prefixes, err := ParseTrustedProxies(specs)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(prefixes))
	for i, p := range prefixes {
		out[i] = p.String()
	}
	return out
}

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()
	got := mustProxies(t, "10.0.0.0/8", " 192.0.2.7 ", "2001:db8::/32", "2001:db8::1", "::ffff:192.0.2.9", "")
	want := []string{"10.0.0.0/8", "192.0.2.7/32", "2001:db8::/32", "2001:db8::1/128", "192.0.2.9/32"}
	if !slices.Equal(got, want) {
		t.Errorf("ParseTrustedProxies = %v, want %v", got, want)
	}
	for _, bad := range []string{"proxy.example", "10.0.0.0/33", "10.0.0.0/8/9", "300.0.0.1"} {
		if _, err := ParseTrustedProxies([]string{bad}); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("ParseTrustedProxies(%q) = %v", bad, err)
		}
	}
}

// requestFrom is a request from a peer with the X-Forwarded-For lines given, answered with the
// address the interface counts it under.
func requestFrom(t *testing.T, h *harness, peer string, forwarded ...string) string {
	t.Helper()
	var seen string
	handler := h.server.withClientAddress(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = clientAddress(r) }))
	req := httptest.NewRequestWithContext(t.Context(), "GET", "https://kubehenge.example/api/realms", nil)
	req.RemoteAddr = peer
	for _, line := range forwarded {
		req.Header.Add("X-Forwarded-For", line)
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return seen
}

func TestClientAddressBehindTrustedProxies(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var err error
	if h.server.TrustedProxies, err = ParseTrustedProxies([]string{"10.0.0.0/8", "2001:db8:1::/48"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, peer string
		forwarded  []string
		want       string
	}{
		{"a trusted peer's single entry", "10.0.0.1:443", []string{"198.51.100.7"}, "198.51.100.7"},
		{"the right-most entry that is not trusted", "10.0.0.1:443", []string{"203.0.113.9, 198.51.100.7, 10.0.0.5"}, "198.51.100.7"},
		{"a client's own forged entries stay left of the real one", "10.0.0.1:443", []string{"1.1.1.1, 2.2.2.2", "198.51.100.7"}, "198.51.100.7"},
		{"entries over several header lines", "10.0.0.1:443", []string{"203.0.113.9", "198.51.100.7,10.0.0.5"}, "198.51.100.7"},
		{"an IPv6 client", "10.0.0.1:443", []string{"2001:db8:2::5"}, "2001:db8:2::5"},
		{"an IPv6 client with a port", "10.0.0.1:443", []string{"[2001:db8:2::5]:5000"}, "2001:db8:2::5"},
		{"an IPv4 client with a port", "10.0.0.1:443", []string{"198.51.100.7:5000"}, "198.51.100.7"},
		{"an IPv6 trusted peer", "[2001:db8:1::9]:443", []string{"198.51.100.7"}, "198.51.100.7"},
		{"an IPv6 trusted hop", "10.0.0.1:443", []string{"198.51.100.7, 2001:db8:1::2"}, "198.51.100.7"},
		{"an IPv4-mapped address of a trusted range", "10.0.0.1:443", []string{"198.51.100.7, ::ffff:10.0.0.9"}, "198.51.100.7"},
		{"only trusted entries leave the left-most", "10.0.0.1:443", []string{"10.0.0.8, 10.0.0.5"}, "10.0.0.8"},
		{"no header leaves the peer", "10.0.0.1:443", nil, "10.0.0.1"},
		{"a garbage entry stops at the hop that sent it", "10.0.0.1:443", []string{"198.51.100.7, not-an-address, 10.0.0.5"}, "10.0.0.5"},
		{"a header from an untrusted peer is ignored", "192.0.2.10:50000", []string{"198.51.100.7"}, "192.0.2.10"},
		{"a spoofed trusted entry from an untrusted peer is ignored", "192.0.2.10:50000", []string{"10.0.0.1"}, "192.0.2.10"},
		{"an IPv6 untrusted peer is ignored", "[2001:db8:9::1]:50000", []string{"198.51.100.7"}, "2001:db8:9::1"},
	} {
		if got := requestFrom(t, h, c.peer, c.forwarded...); got != c.want {
			t.Errorf("%s: client %q, want %q", c.name, got, c.want)
		}
	}
}

// Without trusted proxies a forwarding header changes nothing.
func TestClientAddressIgnoresForwardingByDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if got := requestFrom(t, h, "10.0.0.1:443", "198.51.100.7"); got != "10.0.0.1" {
		t.Errorf("client %q, want the peer", got)
	}
}

// Behind a proxy, one person's typos lock out themselves and not everyone the proxy serves.
func TestFailedSignInsBehindAProxyHoldBackTheirClient(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var err error
	if h.server.TrustedProxies, err = ParseTrustedProxies([]string{"192.0.2.0/24"}); err != nil {
		t.Fatal(err)
	}
	h.handler = h.server.Handler()
	signIn := func(client, password string) int {
		req := httptest.NewRequestWithContext(t.Context(), "POST", "https://kubehenge.example/api/session",
			strings.NewReader(fmt.Sprintf(`{"username":"admin@pve","password":%q}`, password)))
		req.RemoteAddr = "192.0.2.10:50000"
		req.Header.Set("X-Forwarded-For", client)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for range failuresAllowed {
		if code := signIn("198.51.100.7", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("a wrong password gave %d", code)
		}
	}
	if code := signIn("198.51.100.7", "secret"); code != http.StatusTooManyRequests {
		t.Errorf("the client that failed is not held back: %d", code)
	}
	if code := signIn("198.51.100.8", "secret"); code != http.StatusOK {
		t.Errorf("another client behind the proxy is held back: %d", code)
	}
}
