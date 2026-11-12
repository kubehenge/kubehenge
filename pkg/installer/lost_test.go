package installer

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The hosts a container that cannot resolve the best one is tried with: the one chosen first, then
// the rest by rank, none twice.
func TestAPIHostsOrdered(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	s := prepared(t, h, Config{})
	s.Facts.Candidates = []APICandidate{
		{Rank: 1, Host: "pve.example.org"}, {Rank: 2, Host: "api.example.org"},
		{Rank: 3, Host: "pve1.example.org"}, {Rank: 4, Host: "192.0.2.11"},
	}
	for chosen, want := range map[string]string{
		"api.example.org": "api.example.org,pve.example.org,pve1.example.org,192.0.2.11",
		"pve.example.org": "pve.example.org,api.example.org,pve1.example.org,192.0.2.11",
	} {
		s.Cfg.APIHost, s.Cfg.ProxmoxURL = chosen, APIURL(chosen)
		if got := strings.Join(h.runner(s, nil).apiHostsOrdered(), ","); got != want {
			t.Errorf("chosen %s: got %s, want %s", chosen, got, want)
		}
	}
}

// The check of the appliance's realms goes to the container's address whatever the name in the URL
// resolves to, and never through a proxy.
//
//nolint:paralleltest // t.Setenv sets the proxy variables the check must ignore.
func TestRealmsCheckBypassesProxyAndReachesTheAddress(t *testing.T) {
	h := newHarness(t, opts{})
	cert, err := h.node.MakeCertificate("kubehenge", []string{"127.0.0.1"}, "kubehenge.example.org", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert.CertPEM, cert.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	if got := realmsStatusAt(context.Background(), "kubehenge.example.org", srv.Listener.Addr().String(), cert.CertPEM); got != http.StatusOK || path != "/api/realms" {
		t.Errorf("status %d for %q", got, path)
	}
	if got := realmsStatusAt(context.Background(), "other.example.org", srv.Listener.Addr().String(), cert.CertPEM); got != 0 {
		t.Errorf("a certificate for another name answered %d", got)
	}
}

// The name an installed appliance is reached by: a certificate this cluster's CA signed keeps its
// own, and a name the container claims counts only where this node's DNS resolves it to the
// appliance's address, in the address's family.
func TestPublicName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, opts{hosts: map[string][]string{
		"kubehenge.example.org":  {"192.0.2.40"},
		"kubehenge6.example.org": {"2001:db8::40"},
	}})
	outside := *h.node
	outside.Root = t.TempDir()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	make1 := func(n *Node, name string) *Certificate {
		c, err := n.MakeCertificate("kubehenge", []string{"192.0.2.40", "2001:db8::40"}, name, now)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	signed, signedUnnamed, self := make1(h.node, "kubehenge.example.org"), make1(h.node, ""), make1(&outside, "bank.example.com")
	name := func(node *Node, c *Certificate, claim, address string) string {
		h.exec.on("pct exec 100 -- cat /etc/kubehenge/appliance.env", "KUBEHENGE_PUBLIC_URL=https://"+claim+"\n")
		parsed, err := parseCertFile(c.CertPEM)
		if err != nil {
			t.Fatal(err)
		}
		s := h.session(Config{CTID: "100"})
		s.Node = node
		return h.runner(s, nil).publicName(ctx, parsed, address, io.Discard)
	}
	for _, tc := range []struct {
		what    string
		node    *Node
		cert    *Certificate
		claim   string
		address string
		want    string
	}{
		{"signed here keeps its name, whatever the container claims", h.node, signed, "bank.example.com", "192.0.2.40", "kubehenge.example.org"},
		{"signed here without a name gets none", h.node, signedUnnamed, "kubehenge.example.org", "192.0.2.40", ""},
		{"a name the container alone claims stays out", &outside, self, "bank.example.com", "192.0.2.40", ""},
		{"a name this node's DNS resolves to the appliance goes in", &outside, self, "kubehenge.example.org", "192.0.2.40", "kubehenge.example.org"},
		{"an address is no name", &outside, self, "192.0.2.40", "192.0.2.40", ""},
		{"a list of names is none", &outside, self, "kubehenge.example.org, DNS:bank.example.com", "192.0.2.40", ""},
		{"an IPv6 address is no name", &outside, self, "[2001:db8::40]", "2001:db8::40", ""},
		{"a name resolving to the appliance's IPv6 address goes in", &outside, self, "kubehenge6.example.org", "2001:db8::40", "kubehenge6.example.org"},
		{"a name that leads to IPv6 only is none of an IPv4 appliance", &outside, self, "kubehenge6.example.org", "192.0.2.40", ""},
	} {
		if got := name(tc.node, tc.cert, tc.claim, tc.address); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.what, got, tc.want)
		}
	}
}

// Only a 404 for the pinned template sends the install to the index. Any other answer, and an
// unreachable mirror, leave it to the download and its own error.
func TestTemplateGone(t *testing.T) {
	t.Parallel()
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		switch r.URL.Path {
		case "/gone":
			w.WriteHeader(http.StatusNotFound)
		case "/error":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	f := NewFetcher("")
	for path, want := range map[string]bool{"/gone": true, "/there": false, "/error": false} {
		if got := f.TemplateGone(context.Background(), srv.URL+path); got != want {
			t.Errorf("%s: gone is %v, want %v", path, got, want)
		}
	}
	for _, m := range methods {
		if m != http.MethodHead {
			t.Errorf("the check sent a %s", m)
		}
	}
	srv.Close()
	if f.TemplateGone(context.Background(), srv.URL+"/gone") {
		t.Error("an unreachable mirror was called a gone template")
	}
}
