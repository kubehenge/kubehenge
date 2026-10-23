package provisioner

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
	"io"
	"math/big"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/streaming/pkg/httpstream"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// etcdPort is the client port of every etcd member, and of k3s's proxy in front of its own.
	etcdPort = 2379
	// etcdServerName is a name every distribution's etcd serving certificate carries.
	etcdServerName = "localhost"
	// etcdClientName is the common name of the client certificate the controller signs itself.
	etcdClientName = "kubehenge-etcd-snapshot"
	// etcdClientLife is how long that certificate is valid; it is signed anew for every snapshot and
	// never stored, so it need not outlive one.
	etcdClientLife = time.Hour
	// etcdClockSkew backdates it, for an etcd member whose clock runs behind.
	etcdClockSkew   = 5 * time.Minute
	etcdDialTimeout = 10 * time.Second
	serialBits      = 128
)

// EtcdStream is a member's database as etcd streams it, with the point in time it was taken at.
type EtcdStream struct {
	io.ReadCloser

	// Revision is the member's revision just before the snapshot began, which the snapshot holds.
	Revision int64
	// Version is the etcd release of the member that took it.
	Version string
}

// EtcdSource streams a snapshot of a workload cluster's etcd.
type EtcdSource interface {
	Snapshot(ctx context.Context, cluster *clusterv1.Cluster) (*EtcdStream, error)
}

// EtcdDialer reaches a workload cluster's etcd as Cluster API's control-plane providers do: through
// the cluster's API server, by port-forward to an etcd member's pod (kubeadm, RKE2) or to the k3s
// provider's proxy in front of each server's etcd, with a client certificate signed in memory from
// the cluster's etcd CA, which every provider keeps in the management cluster.
type EtcdDialer struct {
	// Reader reads the CA's Secret, past any cache.
	Reader  client.Reader
	Remotes *Remotes
	// Now is the clock the certificate is signed by; unset, the system's.
	Now func() time.Time
	// Talos reaches a Talos cluster's control-plane nodes, whose etcd runs as a Talos service, not a
	// pod: their Talos API streams the snapshot.
	Talos TalosDialer
}

var _ EtcdSource = &EtcdDialer{}

// Snapshot streams a snapshot from the first member that answers.
func (d *EtcdDialer) Snapshot(ctx context.Context, cluster *clusterv1.Cluster) (*EtcdStream, error) {
	if isTalos(cluster) {
		return d.talosSnapshot(ctx, cluster)
	}
	ca, err := etcdCA(ctx, d.Reader, cluster)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	tlsConfig, err := etcdClientTLS(ca, now())
	if err != nil {
		return nil, err
	}
	workload, found, err := d.Remotes.get(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("the cluster has no kubeconfig yet")
	}
	pods, err := etcdPods(ctx, workload.client, distributionOf(cluster))
	if err != nil {
		d.Remotes.forget(cluster)
		return nil, err
	}
	if len(pods) == 0 {
		return nil, errors.New("no etcd member of the cluster is running and ready")
	}
	var errs []error
	for i := range pods {
		namespace, name := pods[i].Namespace, pods[i].Name
		dial := func(context.Context) (net.Conn, error) {
			return portForward(workload.config, namespace, name, etcdPort)
		}
		s, err := snapshotThrough(ctx, dial, tlsConfig)
		if err == nil {
			return s, nil
		}
		errs = append(errs, fmt.Errorf("through pod %s: %w", name, err))
	}
	return nil, errors.Join(errs...)
}

// etcdCAPair is the cluster's etcd CA, with which client certificates are signed.
type etcdCAPair struct {
	cert *x509.Certificate
	key  crypto.Signer
	pem  []byte
}

// etcdCASecret names the Secret with the cluster's etcd CA, the same for each provider: kubeadm's
// etcd CA, and RKE2's and k3s's etcd server CA, which they trust their clients by.
func etcdCASecret(cluster string) string { return cluster + "-etcd" }

func etcdCA(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) (*etcdCAPair, error) {
	s := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: etcdCASecret(cluster.Name)}, s)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("the cluster's etcd CA (Secret %s) is not there yet", etcdCASecret(cluster.Name))
	}
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's etcd CA: %w", err)
	}
	return parseCA(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
}

// parseCA reads a CA certificate and its key, in the PKCS #1, SEC 1 or PKCS #8 forms the providers
// write.
func parseCA(certPEM, keyPEM []byte) (*etcdCAPair, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the etcd CA's certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the etcd CA's certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("the etcd CA's certificate is not a CA's")
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("the etcd CA's key is not PEM")
	}
	var key any
	switch kb.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(kb.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(kb.Bytes)
	default:
		key, err = x509.ParsePKCS8PrivateKey(kb.Bytes)
	}
	if err != nil {
		return nil, fmt.Errorf("the etcd CA's key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("the etcd CA's key cannot sign")
	}
	return &etcdCAPair{cert: cert, key: signer, pem: certPEM}, nil
}

// etcdClientTLS signs a client certificate for etcd, valid for an hour from now, and trusts the
// members' serving certificates by the same CA, for the name they all carry.
func etcdClientTLS(ca *etcdCAPair, now time.Time) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("making a key for etcd: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, fmt.Errorf("making a serial number: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: etcdClientName},
		NotBefore:    now.Add(-etcdClockSkew),
		NotAfter:     now.Add(etcdClientLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("signing a client certificate for etcd: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
		ServerName:   etcdServerName,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}, nil
}

// etcdPods are the pods etcd is reached through, running and ready, by name: each member's static
// pod on kubeadm and RKE2, the k3s provider's proxy on every k3s server.
func etcdPods(ctx context.Context, workload client.Client, d templates.Distribution) ([]corev1.Pod, error) {
	labels := client.MatchingLabels{"component": "etcd"}
	if d == templates.K3s {
		labels = client.MatchingLabels{"app": "etcd-proxy"}
	}
	pods := &corev1.PodList{}
	if err := workload.List(ctx, pods, client.InNamespace(metav1.NamespaceSystem), labels); err != nil {
		return nil, fmt.Errorf("listing the etcd pods: %w", err)
	}
	ready := slices.DeleteFunc(pods.Items, func(p corev1.Pod) bool { return !podReady(&p) })
	slices.SortFunc(ready, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	return ready, nil
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || !p.DeletionTimestamp.IsZero() {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// snapshotThrough opens an etcd client over dial and starts a snapshot. The client goes with the
// stream when it is closed.
func snapshotThrough(ctx context.Context, dial func(context.Context) (net.Conn, error), tlsConfig *tls.Config) (*EtcdStream, error) {
	cli, err := clientv3.New(clientv3.Config{
		// The dialer goes to the one member it was made for, whatever the address.
		Endpoints:   []string{net.JoinHostPort(etcdServerName, strconv.Itoa(etcdPort))},
		TLS:         tlsConfig,
		DialTimeout: etcdDialTimeout,
		DialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return dial(ctx) })},
		Logger:      zap.NewNop(),
	})
	if err != nil {
		return nil, fmt.Errorf("reaching etcd: %w", err)
	}
	// etcd sends no response header with a snapshot, so the revision is read first, from the same
	// member: the snapshot commits and reads that member's store after it, so it holds at least
	// this revision. Serializable, the member answers from its own store, also without a quorum.
	read, err := cli.Get(ctx, etcdClientName, clientv3.WithSerializable(), clientv3.WithCountOnly())
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("reading the member's revision: %w", err)
	}
	resp, err := cli.SnapshotWithVersion(ctx)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("starting a snapshot: %w", err)
	}
	return &EtcdStream{
		ReadCloser: &closing{ReadCloser: resp.Snapshot, then: cli.Close},
		Version:    resp.Version,
		Revision:   read.Header.GetRevision(),
	}, nil
}

// closing closes something else after its reader.
type closing struct {
	io.ReadCloser

	then func() error
}

func (c *closing) Close() error {
	return errors.Join(c.ReadCloser.Close(), c.then())
}

// portForward opens a connection to a pod's port through the cluster's API server, as kubectl
// port-forward does, one SPDY connection per dial.
func portForward(config *rest.Config, namespace, pod string, port int) (net.Conn, error) {
	config = rest.CopyConfig(config)
	// The connection carries a whole snapshot; the API's request timeout would cut it off.
	config.Timeout = 0
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return nil, fmt.Errorf("port-forward: %w", err)
	}
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("port-forward: %w", err)
	}
	u := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	conn, _, err := spdy.NewDialerForStreaming(upgrader, &http.Client{Transport: transport}, http.MethodPost, u).Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("port-forward to %s/%s: %w", namespace, pod, err)
	}
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(port))
	headers.Set(corev1.PortForwardRequestIDHeader, "0")
	errStream, err := conn.CreateStream(headers)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("port-forward to %s/%s: %w", namespace, pod, err)
	}
	// Nothing is sent on the error stream; the kubelet only answers on it.
	_ = errStream.Close()
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := conn.CreateStream(headers)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("port-forward to %s/%s: %w", namespace, pod, err)
	}
	pc := &forwardedConn{Stream: data, conn: conn}
	go func() {
		// An error the kubelet reports, such as the port refusing it, ends the connection.
		if msg, rerr := io.ReadAll(errStream); rerr == nil && len(msg) > 0 {
			_ = pc.Close()
		}
	}()
	return pc, nil
}

// forwardedConn is the data stream of a port-forward as a net.Conn.
type forwardedConn struct {
	httpstream.Stream

	conn httpstream.Connection
}

var _ net.Conn = &forwardedConn{}

func (c *forwardedConn) Close() error {
	return errors.Join(c.Stream.Close(), c.conn.Close())
}

func (*forwardedConn) LocalAddr() net.Addr  { return forwardedAddr{} }
func (*forwardedConn) RemoteAddr() net.Addr { return forwardedAddr{} }

// The stream has no deadlines; the client's context bounds each call instead.
func (*forwardedConn) SetDeadline(time.Time) error      { return nil }
func (*forwardedConn) SetReadDeadline(time.Time) error  { return nil }
func (*forwardedConn) SetWriteDeadline(time.Time) error { return nil }

type forwardedAddr struct{}

func (forwardedAddr) Network() string { return "port-forward" }
func (forwardedAddr) String() string  { return "port-forward" }
