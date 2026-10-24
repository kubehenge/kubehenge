package provisioner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/test/bufconn"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// testCA is a CA as the control-plane providers keep a cluster's etcd CA.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pair *etcdCAPair
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "etcd-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := parseCA(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: pair.cert, key: key, pair: pair}
}

// serving signs a member's serving certificate, for localhost as every distribution's carries.
func (ca *testCA) serving(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "web-cp-0"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeMember is an etcd member's maintenance API, which streams its database in chunks as etcd does.
type fakeMember struct {
	etcdserverpb.UnimplementedMaintenanceServer

	data   []byte
	mu     sync.Mutex
	client string
}

func (m *fakeMember) Snapshot(_ *etcdserverpb.SnapshotRequest, srv etcdserverpb.Maintenance_SnapshotServer) error {
	if p, ok := peer.FromContext(srv.Context()); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
			m.mu.Lock()
			m.client = info.State.PeerCertificates[0].Subject.CommonName
			m.mu.Unlock()
		}
	}
	const chunk = 32 << 10
	for off := 0; off < len(m.data); off += chunk {
		end := min(off+chunk, len(m.data))
		err := srv.Send(&etcdserverpb.SnapshotResponse{
			Header:         &etcdserverpb.ResponseHeader{Revision: 4711},
			RemainingBytes: uint64(len(m.data) - end), //nolint:gosec // end is at most the length
			Blob:           m.data[off:end],
			Version:        "3.6.5",
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// startMember serves a member over a connection in memory, trusting clients by ca's certificate.
func startMember(t *testing.T, ca *testCA, serving *tls.Certificate, data []byte) (*fakeMember, func(context.Context) (net.Conn, error)) {
	t.Helper()
	clients := x509.NewCertPool()
	clients.AddCert(ca.cert)
	creds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*serving},
		ClientCAs: clients, ClientAuth: tls.RequireAndVerifyClientCert,
	})
	srv := grpc.NewServer(grpc.Creds(creds))
	member := &fakeMember{data: data}
	etcdserverpb.RegisterMaintenanceServer(srv, member)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return member, lis.DialContext
}

// The controller signs itself a client certificate from the cluster's etcd CA and streams the
// snapshot through whatever connection reaches the member.
func TestSnapshotThroughSignsItsOwnClientCertificate(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	data := etcdData(200 << 10)
	member, dial := startMember(t, ca, ca.serving(t), data)
	tlsConfig, err := etcdClientTLS(ca.pair, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(leaf.NotAfter) > etcdClientLife || leaf.IsCA || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("client certificate valid until %s, CA %v, usages %v", leaf.NotAfter, leaf.IsCA, leaf.ExtKeyUsage)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stream, err := snapshotThrough(ctx, dial, tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("closing: %v", err)
	}
	if !bytes.Equal(got, data) || stream.Revision != 4711 || stream.Version != "3.6.5" {
		t.Errorf("streamed %d of %d bytes, revision %d, version %s", len(got), len(data), stream.Revision, stream.Version)
	}
	member.mu.Lock()
	defer member.mu.Unlock()
	if member.client != etcdClientName {
		t.Errorf("the member saw client %q", member.client)
	}
}

// A member whose certificate another CA signed is not the cluster's.
func TestSnapshotThroughRefusesAnotherCAsMember(t *testing.T) {
	t.Parallel()
	ca, other := newTestCA(t), newTestCA(t)
	_, dial := startMember(t, ca, other.serving(t), etcdData(10))
	tlsConfig, err := etcdClientTLS(ca.pair, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if s, err := snapshotThrough(ctx, dial, tlsConfig); err == nil {
		_ = s.Close()
		t.Fatal("streamed from a member of another CA")
	}
}

func TestParseCA(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
	pkcs8, err := x509.MarshalPKCS8PrivateKey(ca.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = parseCA(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})); err != nil {
		t.Errorf("PKCS #8: %v", err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "etcd-ca"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign,
	}
	rsaDER, err := x509.CreateCertificate(rand.Reader, rsaTemplate, rsaTemplate, &rsaKey.PublicKey, rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	rsaPair, err := parseCA(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rsaDER}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}))
	if err != nil {
		t.Fatalf("PKCS #1: %v", err)
	}
	if _, err := etcdClientTLS(rsaPair, time.Now()); err != nil {
		t.Errorf("signing with an RSA CA: %v", err)
	}

	leaf := ca.serving(t)
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]})
	for name, tc := range map[string]struct{ cert, key []byte }{
		"no PEM":   {[]byte("nothing"), nil},
		"not a CA": {leafPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})},
		"no key":   {certPEM, []byte("nothing")},
		"bad key":  {certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("x")})},
	} {
		if _, err := parseCA(tc.cert, tc.key); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEtcdCAIsReadFromTheProvidersSecret(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	keyDER, err := x509.MarshalECPrivateKey(ca.key)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		Namespace: "team-a", Name: "web-etcd",
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		},
	}
	kube := fake.NewClientBuilder().WithObjects(secret).Build()
	if _, err := etcdCA(context.Background(), kube, cluster()); err != nil {
		t.Error(err)
	}
	other := cluster()
	other.Name = "db"
	if _, err := etcdCA(context.Background(), kube, other); err == nil || !strings.Contains(err.Error(), "not there yet") {
		t.Errorf("a cluster without its CA: %v", err)
	}
}

func TestEtcdPods(t *testing.T) {
	t.Parallel()
	pod := func(name string, labels map[string]string, ready bool) *corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return &corev1.Pod{
			Namespace: metav1.NamespaceSystem, Name: name, Labels: labels,
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
		}
	}
	etcd := map[string]string{"component": "etcd"}
	proxy := map[string]string{"app": "etcd-proxy"}
	kube := fake.NewClientBuilder().WithObjects(
		pod("etcd-web-cp-1", etcd, true), pod("etcd-web-cp-0", etcd, true), pod("etcd-web-cp-2", etcd, false),
		pod("etcd-proxy-a", proxy, true), pod("kube-apiserver-web-cp-0", map[string]string{"component": "kube-apiserver"}, true),
	).Build()
	names := func(d templates.Distribution) string {
		pods, err := etcdPods(context.Background(), kube, d)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(pods))
		for i := range pods {
			out = append(out, pods[i].Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(templates.RKE2); got != "etcd-web-cp-0,etcd-web-cp-1" {
		t.Errorf("RKE2: %s", got)
	}
	if got := names(templates.K3s); got != "etcd-proxy-a" {
		t.Errorf("k3s: %s", got)
	}
}
