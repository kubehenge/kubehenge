package acme

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letsencrypt/pebble/v2/ca"
	"github.com/letsencrypt/pebble/v2/db"
	"github.com/letsencrypt/pebble/v2/va"
	"github.com/letsencrypt/pebble/v2/wfe"
	"github.com/miekg/dns"
)

const (
	testHost = "kubehenge.example.org"
	testZone = "example.org."
	waitFor  = 90 * time.Second
)

func TestMain(m *testing.M) {
	// Pebble answers at once and keeps no authorisation from an earlier order, so each order is
	// proved again.
	for k, v := range map[string]string{"PEBBLE_VA_NOSLEEP": "1", "PEBBLE_WFE_NONCEREJECT": "0", "PEBBLE_AUTHZREUSE": "0"} {
		_ = os.Setenv(k, v)
	}
	m.Run()
}

// fakeDNS is a DNS server on 127.0.0.1 that takes RFC 2136 updates, answers the queries lego and
// pebble make and keeps the TXT records it was given.
type fakeDNS struct {
	Addr       string
	tsigName   string
	tsigSecret string

	mu      sync.Mutex
	txt     map[string][]string
	updates int
}

func newFakeDNS(t *testing.T) *fakeDNS {
	t.Helper()
	f := &fakeDNS{txt: map[string][]string{}, tsigName: "kubehenge-key.", tsigSecret: base64.StdEncoding.EncodeToString([]byte("secret-tsig-key-for-tests"))}
	var lc net.ListenConfig
	var lastErr error
	for range 20 {
		pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		l, err := lc.Listen(t.Context(), "tcp", pc.LocalAddr().String())
		if err != nil {
			_ = pc.Close()
			lastErr = err
			continue
		}
		f.Addr = pc.LocalAddr().String()
		secrets := map[string]string{f.tsigName: f.tsigSecret}
		// The library refuses dynamic updates unless the server accepts every message.
		accept := func(dns.Header) dns.MsgAcceptAction { return dns.MsgAccept }
		udp := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(f.serve), TsigSecret: secrets, MsgAcceptFunc: accept}
		tcp := &dns.Server{Listener: l, Handler: dns.HandlerFunc(f.serve), TsigSecret: secrets, MsgAcceptFunc: accept}
		go func() { _ = udp.ActivateAndServe() }()
		go func() { _ = tcp.ActivateAndServe() }()
		t.Cleanup(func() { _ = udp.Shutdown(); _ = tcp.Shutdown() })
		return f
	}
	t.Fatalf("no port for the fake DNS server: %v", lastErr)
	return nil
}

func (f *fakeDNS) serve(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Opcode {
	case dns.OpcodeUpdate:
		if r.IsTsig() == nil || w.TsigStatus() != nil {
			m.Rcode = dns.RcodeNotAuth
			break
		}
		f.updates++
		for _, rr := range r.Ns {
			txt, ok := rr.(*dns.TXT)
			if !ok {
				continue
			}
			name := strings.ToLower(txt.Hdr.Name)
			switch txt.Hdr.Class {
			case dns.ClassINET:
				f.txt[name] = append(f.txt[name], strings.Join(txt.Txt, ""))
			case dns.ClassNONE:
				kept := f.txt[name][:0:0]
				for _, v := range f.txt[name] {
					if v != strings.Join(txt.Txt, "") {
						kept = append(kept, v)
					}
				}
				f.txt[name] = kept
			case dns.ClassANY:
				delete(f.txt, name)
			}
		}
	default:
		f.answer(m, &r.Question[0])
	}
	if t := r.IsTsig(); t != nil && w.TsigStatus() == nil {
		m.SetTsig(t.Hdr.Name, t.Algorithm, 300, time.Now().Unix())
	}
	_ = w.WriteMsg(m)
}

func (f *fakeDNS) answer(m *dns.Msg, q *dns.Question) {
	name := strings.ToLower(q.Name)
	switch q.Qtype {
	case dns.TypeSOA:
		if name != testZone {
			m.Rcode = dns.RcodeNameError
			return
		}
		m.Answer = append(m.Answer, &dns.SOA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60}, Ns: "ns." + testZone, Mbox: "admin." + testZone, Serial: 1, Refresh: 60, Retry: 60, Expire: 60, Minttl: 60})
	case dns.TypeTXT:
		for _, v := range f.txt[name] {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 1}, Txt: []string{v}})
		}
	case dns.TypeA:
		if strings.HasSuffix(name, "."+testZone) {
			m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.IPv4(127, 0, 0, 1)})
		}
	case dns.TypeAAAA, dns.TypeCAA:
	default:
		m.Rcode = dns.RcodeNotImplemented
	}
}

func (f *fakeDNS) updateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updates
}

// acmeServer is a Pebble ACME server on 127.0.0.1 that proves names against the fake DNS server and
// connects to the ports of the listeners the test gives.
type acmeServer struct {
	URL  string
	Cert *x509.Certificate
	// Orders counts the accounts' new orders.
	srv *httptest.Server
}

type acmeOptions struct {
	// bindings are the external account bindings the server demands, key ID to base64url key.
	bindings          map[string]string
	httpPort, tlsPort int
	resolver          string
}

func startACMEServer(t *testing.T, o acmeOptions) *acmeServer {
	t.Helper()
	logger := slog.NewLogLogger(slog.NewTextHandler(&testLog{t}, nil), slog.LevelInfo)
	store := db.NewMemoryStore()
	authority := ca.New(logger, store, "", "ecdsa", 0, 1, map[string]ca.Profile{"default": {Description: "default"}, "shortlived": {Description: "short", ValidityPeriod: 6 * 24 * 3600}})
	validator := va.New(logger, o.httpPort, o.tlsPort, false, o.resolver, store)
	for id, key := range o.bindings {
		if err := store.AddExternalAccountKeyByID(id, key); err != nil {
			t.Fatal(err)
		}
	}
	front := wfe.New(logger, store, validator, authority, []string{"pebble.letsencrypt.org"}, false, len(o.bindings) > 0, 0, 0)
	srv := httptest.NewTLSServer(front.Handler())
	t.Cleanup(srv.Close)
	return &acmeServer{URL: srv.URL + wfe.DirectoryPath, Cert: srv.Certificate(), srv: srv}
}

type testLog struct{ t *testing.T }

func (l *testLog) Write(p []byte) (int, error) {
	defer func() { _ = recover() }() // a server that logs after its test ended
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// pool is the roots that vouch for the ACME server.
func (s *acmeServer) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(s.Cert)
	return p
}

// challengeListeners serve the answers to the proofs on 127.0.0.1: tls-alpn-01 from the manager's
// challenge certificates, and http-01 from its handler.
type challengeListeners struct {
	httpPort, tlsPort int
}

func serveChallenges(t *testing.T, m *Manager) challengeListeners {
	t.Helper()
	var lc net.ListenConfig
	hl, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: m.HTTPHandler(), ReadHeaderTimeout: time.Minute}
	go func() { _ = hs.Serve(hl) }()
	t.Cleanup(func() { _ = hs.Close() })
	tl, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{ALPNProtocol},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if c := m.ChallengeCertificate(h); c != nil {
				return c, nil
			}
			return nil, errors.New("no challenge pending")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tl.Close() })
	go func() {
		for {
			c, acceptErr := tl.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				if tc, ok := c.(*tls.Conn); ok {
					_ = tc.HandshakeContext(t.Context())
				}
				_ = c.Close()
			}()
		}
	}()
	return challengeListeners{httpPort: portOf(t, hl.Addr()), tlsPort: portOf(t, tl.Addr())}
}

func portOf(t *testing.T, a net.Addr) int {
	t.Helper()
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		t.Fatalf("%v is not a TCP address", a)
	}
	return tcp.Port
}

// rig is a manager that runs against a Pebble server, with a fake DNS server beside it.
type rig struct {
	m         *Manager
	dns       *fakeDNS
	server    *acmeServer
	delivered chan *Issued
	mu        sync.Mutex
	serving   Served
}

type rigOptions struct {
	bindings map[string]string
}

func newRig(t *testing.T, o rigOptions) *rig {
	t.Helper()
	r := &rig{dns: newFakeDNS(t), delivered: make(chan *Issued, 8)}
	m := New(t.TempDir())
	m.Log = nil
	m.Name = func() string { return testHost }
	m.HTTPListening = true
	m.Deliver = func(_ context.Context, in *Issued) error {
		if !m.Current(in.Generation) {
			return errors.New("superseded")
		}
		block, _ := pem.Decode(in.CertPEM)
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.serving = Served{Leaf: leaf, Source: SourceACME, Order: in.OrderID}
		r.mu.Unlock()
		r.delivered <- in
		return nil
	}
	m.Served = func() Served {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.serving
	}
	// The listeners need the manager, the server needs the listeners' ports.
	ports := serveChallenges(t, m)
	r.server = startACMEServer(t, acmeOptions{bindings: o.bindings, httpPort: ports.httpPort, tlsPort: ports.tlsPort, resolver: r.dns.Addr})
	m.Roots = r.server.pool()
	r.m = m
	runUntilCleanup(t, m)
	return r
}

// runUntilCleanup runs the manager until the test ends, and waits for it to stop before the
// directory it writes to is removed.
func runUntilCleanup(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	go func() {
		defer close(stopped)
		m.Run(ctx)
	}()
}

// next waits for the next certificate delivered.
func (r *rig) next(t *testing.T) *Issued {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		select {
		case in := <-r.delivered:
			return in
		case <-time.After(50 * time.Millisecond):
			if v := r.m.View(); v != nil && v.Error != "" {
				t.Fatalf("the order failed: %s", v.Error)
			}
		}
	}
	t.Fatalf("no certificate was delivered. The order stands at %+v", r.m.View())
	return nil
}

// failed waits for the order to end in an error and gives it.
func (r *rig) failed(t *testing.T) *View {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if v := r.m.View(); v != nil && v.Error != "" {
			return v
		}
		select {
		case in := <-r.delivered:
			t.Fatalf("a certificate was delivered: %d bytes", len(in.CertPEM))
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("the order did not fail: %+v", r.m.View())
	return nil
}
