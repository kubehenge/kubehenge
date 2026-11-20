package acme

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
)

func leafOf(t *testing.T, in *Issued) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(in.CertPEM)
	if block == nil {
		t.Fatal("the certificate is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func checkIssued(t *testing.T, r *rig, in *Issued) {
	t.Helper()
	leaf := leafOf(t, in)
	if err := leaf.VerifyHostname(testHost); err != nil {
		t.Error(err)
	}
	if len(in.KeyPEM) == 0 || in.OrderID == "" {
		t.Errorf("the key has %d bytes, the order id is %q", len(in.KeyPEM), in.OrderID)
	}
	if v := r.m.View(); v == nil || v.Error != "" {
		t.Errorf("the view after the order is %+v", v)
	}
}

// A new setting starts the first order with no further request, and tls-alpn-01 is proved by the
// certificate GetCertificate gives on the handshake.
func TestTLSALPNProofIssuesTheFirstOrderOnSave(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	if _, err := r.m.Save(&Request{Directory: r.server.URL, Proof: ProofTLSALPN}); err != nil {
		t.Fatal(err)
	}
	checkIssued(t, r, r.next(t))
}

// http-01 is answered by the listener on port 80.
func TestHTTPProofIssuesThroughThePort80Listener(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	if _, err := r.m.Save(&Request{Directory: r.server.URL, Proof: ProofHTTP}); err != nil {
		t.Fatal(err)
	}
	checkIssued(t, r, r.next(t))
}

// dns-01 through RFC 2136 sets the record on a DNS server at 127.0.0.1 and removes it afterwards.
func TestDNSProofIssuesThroughRFC2136(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	if _, err := r.m.Save(r.rfc2136(t)); err != nil {
		t.Fatal(err)
	}
	checkIssued(t, r, r.next(t))
	if r.dns.updateCount() < 2 {
		t.Errorf("the DNS server took %d updates, want the record added and removed", r.dns.updateCount())
	}
	r.dns.mu.Lock()
	defer r.dns.mu.Unlock()
	for name, values := range r.dns.txt {
		if len(values) > 0 {
			t.Errorf("%s keeps %v", name, values)
		}
	}
}

func (r *rig) rfc2136(t *testing.T) *Request {
	t.Helper()
	return &Request{
		Directory: r.server.URL, Proof: ProofDNS, DNSProvider: "rfc2136", Resolvers: []string{r.dns.Addr},
		DNSCredentials: map[string]string{
			"RFC2136_NAMESERVER": r.dns.Addr, "RFC2136_TSIG_ALGORITHM": "hmac-sha256.", "RFC2136_TSIG_KEY": r.dns.tsigName,
			"RFC2136_TSIG_SECRET": r.dns.tsigSecret, "RFC2136_PROPAGATION_TIMEOUT": "30", "RFC2136_POLLING_INTERVAL": "1", "RFC2136_TTL": "1",
		},
	}
}

// A server that demands an external account binding registers the account with the key given, and
// refuses an account with none.
func TestExternalAccountBindingRegistersTheAccount(t *testing.T) {
	t.Parallel()
	key := base64.RawURLEncoding.EncodeToString([]byte("secret-external-account-key-123456"))
	r := newRig(t, rigOptions{bindings: map[string]string{"kid-1": key}})
	if _, err := r.m.Save(&Request{Directory: r.server.URL, Proof: ProofTLSALPN}); err != nil {
		t.Fatal(err)
	}
	r.failed(t)
	if _, err := r.m.Save(&Request{Directory: r.server.URL, Proof: ProofTLSALPN, ExternalAccountBinding: &Binding{KeyID: "kid-1", HMACKey: key}}); err != nil {
		t.Fatal(err)
	}
	checkIssued(t, r, r.next(t))
}
