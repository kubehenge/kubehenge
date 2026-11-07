package provisioner

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
)

// bundleSecret is the Secret the Talos bootstrap provider writes: a real secrets bundle, marshalled
// as the provider marshals it.
func bundleSecret(t *testing.T, namespace string) *corev1.Secret {
	t.Helper()
	bundle, err := secrets.NewBundle(secrets.NewFixedClock(time.Now()), config.TalosVersionCurrent)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		Namespace: namespace, Name: talosBundleSecret("web"),
		Data: map[string][]byte{"bundle": raw},
	}
}

// startTalosEtcd serves what a Talos node's etcd serves on its client port: metrics, to clients with
// a certificate of the etcd CA, behind a certificate of that CA for localhost.
func startTalosEtcd(t *testing.T, ca *etcdCAPair, metrics string) int {
	t.Helper()
	signer, ok := ca.key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("the etcd CA's key is %T", ca.key)
	}
	authority := &testCA{cert: ca.cert, key: signer, pair: ca}
	clients := x509.NewCertPool()
	clients.AddCert(ca.cert)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(metrics))
	}))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*authority.serving(t)},
		ClientCAs: clients, ClientAuth: tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTalosEtcdMetricsReadAtTheNodes(t *testing.T) {
	t.Parallel()
	c := talosCluster("web", "v1.14.4")
	secret := bundleSecret(t, c.Namespace)
	slow := talosMachine("web-cp-0", "127.0.0.1", "v1.14.4")
	slow.Namespace = c.Namespace
	noAddress := talosMachine("web-cp-1", "", "v1.14.4")
	noAddress.Namespace = c.Namespace
	worker := talosMachine("web-md-0", "127.0.0.1", "v1.14.4")
	worker.Namespace = c.Namespace
	f := newEtcdFixture(t, c, secret, slow, noAddress, worker)

	ca, err := talosEtcdCA(t.Context(), f.kube, c)
	if err != nil {
		t.Fatal(err)
	}
	port := startTalosEtcd(t, ca, fsyncMetrics(0, 0, 0, 0, 0, 400, 500, 100))
	tlsConfig, err := etcdClientTLS(ca, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d := &EtcdDialer{Reader: f.kube}
	members, err := d.talosWALFsync(t.Context(), c, tlsConfig, port)
	if err != nil || len(members) != 1 {
		t.Fatalf("members %+v, %v", members, err)
	}
	if members[0].Member != "web-cp-0" || members[0].Syncs != 1000 || members[0].P99 <= EtcdFsyncLimit {
		t.Errorf("member %+v", members[0])
	}
	if cond := EtcdLatencyConditionOf(members); cond == nil || !strings.Contains(cond.Message, "web-cp-0") {
		t.Errorf("condition %+v", cond)
	}

	// A client of another CA is refused by the node, which is how the metrics stay the cluster's.
	other, err := etcdClientTLS(newTestCA(t).pair, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.talosWALFsync(t.Context(), c, other, port); err == nil {
		t.Error("a certificate of another CA was served")
	}
}

func TestTalosEtcdCAFromTheBundle(t *testing.T) {
	t.Parallel()
	c := talosCluster("web", "v1.14.4")
	if _, err := talosEtcdCA(t.Context(), newEtcdFixture(t, c).kube, c); err == nil || !strings.Contains(err.Error(), "web-talos") {
		t.Errorf("a cluster with no bundle: %v", err)
	}
	empty := bundleSecret(t, c.Namespace)
	empty.Data["bundle"] = []byte("certs: {}\n")
	if _, err := talosEtcdCA(t.Context(), newEtcdFixture(t, c, empty).kube, c); err == nil || !strings.Contains(err.Error(), "no etcd CA") {
		t.Errorf("a bundle with no etcd CA: %v", err)
	}
	plain, err := talosEtcdCA(t.Context(), newEtcdFixture(t, c, bundleSecret(t, c.Namespace)).kube, c)
	if err != nil || !plain.cert.IsCA {
		t.Fatalf("the bundle's CA: %+v, %v", plain, err)
	}
}

// Talos's own files wrap every PEM in base64, the provider's keep it as it is.
func TestPEMOrBase64(t *testing.T) {
	t.Parallel()
	const block = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
	if got := pemOrBase64([]byte(block)); string(got) != block {
		t.Errorf("plain: %q", got)
	}
	if got := pemOrBase64([]byte(base64.StdEncoding.EncodeToString([]byte(block)))); string(got) != block {
		t.Errorf("wrapped: %q", got)
	}
}

func TestTalosWALFsyncNeedsTheBundle(t *testing.T) {
	t.Parallel()
	c := talosCluster("web", "v1.14.4")
	d := &EtcdDialer{Reader: newEtcdFixture(t, c).kube}
	if _, err := d.WALFsync(t.Context(), c); err == nil || !strings.Contains(err.Error(), "secrets bundle") {
		t.Errorf("a Talos cluster with no bundle yet: %v", err)
	}
}
