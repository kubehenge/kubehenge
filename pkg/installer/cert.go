package installer

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"time"
)

const (
	clusterCAKey = "/etc/pve/priv/pve-root-ca.key"
	// certDays is 825 days, the longest validity browsers accept from a private CA, less the hour
	// the certificate starts back. That hour keeps a browser whose clock is a few minutes behind
	// this node's from refusing it as not yet valid.
	certDays = 824
)

// Certificate is a certificate and its key as PEM.
type Certificate struct {
	CertPEM, KeyPEM []byte
	Fingerprint     string
	NotAfter        time.Time
}

func handshake(ctx context.Context, conn net.Conn, host string) ([]*x509.Certificate, error) {
	// The handshake verifies as usual. A chain this node does not trust yet comes back from the
	// verification error, and verifyAPI judges it against the CA the user vouches for.
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))
	c := tls.Client(conn, cfg)
	if err := c.HandshakeContext(ctx); err != nil {
		var unverified *tls.CertificateVerificationError
		if errors.As(err, &unverified) && len(unverified.UnverifiedCertificates) > 0 {
			return unverified.UnverifiedCertificates, nil
		}
		return nil, fmt.Errorf("handshake context: %w", err)
	}
	return c.ConnectionState().PeerCertificates, nil
}

func parseKey(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM key")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return key, nil
}

// clusterSigner reads the cluster CA and its key, when this node holds both.
func (n *Node) clusterSigner() (*x509.Certificate, crypto.Signer, bool) {
	caPEM, err := n.readFile(clusterCA)
	if err != nil {
		return nil, nil, false
	}
	keyPEM, err := n.readFile(clusterCAKey)
	if err != nil {
		return nil, nil, false
	}
	ca, err := parseCertFile(caPEM)
	if err != nil {
		return nil, nil, false
	}
	caKey, err := parseKey(keyPEM)
	if err != nil {
		return nil, nil, false
	}
	return ca, caKey, true
}

// MakeCertificate makes a certificate and key for the appliance, signed by this cluster's CA so
// that browsers trusting Proxmox VE's certificates trust Kubehenge's too, else by itself. The CA's
// key stays on this node.
func (n *Node) MakeCertificate(hostname string, addresses []string, name string, now time.Time) (*Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, fmt.Errorf("int: %w", err)
	}
	cn := hostname
	dns := []string{hostname}
	if name != "" {
		cn = name
		dns = append(dns, name)
	}
	var ips []net.IP
	for _, a := range addresses {
		if p, perr := netip.ParseAddr(a); perr == nil {
			ips = append(ips, net.IP(p.AsSlice()))
		}
	}
	start := now.Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: start, NotAfter: start.AddDate(0, 0, certDays),
		DNSNames: dns, IPAddresses: ips,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	parent, signer := tmpl, crypto.Signer(key)
	if ca, caKey, ok := n.clusterSigner(); ok {
		parent, signer = ca, caKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal pkcs8 private key: %w", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &Certificate{
		CertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Fingerprint: fingerprint(cert), NotAfter: cert.NotAfter,
	}, nil
}

// OwnCertificate checks the administrator's --cert and --key for the public host.
func OwnCertificate(certPEM, keyPEM []byte, certFile, keyFile, host string, now time.Time) (*Certificate, error) {
	cert, err := parseCertFile(certPEM)
	if err != nil || cert.NotAfter.Before(now.Add(certMinValidity)) {
		return nil, userErr(certFile+" is not a PEM certificate valid for at least another day", "")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, userErr(keyFile+" is not the key of "+certFile, "")
	}
	_ = pair
	if err := cert.VerifyHostname(host); err != nil {
		return nil, userErr(certFile+" is not valid for "+host, "")
	}
	return &Certificate{CertPEM: certPEM, KeyPEM: keyPEM, Fingerprint: fingerprint(cert), NotAfter: cert.NotAfter}, nil
}
