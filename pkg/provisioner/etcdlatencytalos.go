package provisioner

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// talosBundleSecret is the Secret the Talos bootstrap provider keeps a cluster's secrets bundle in,
// the etcd CA's key among them.
func talosBundleSecret(cluster string) string { return cluster + "-talos" }

// talosEtcdCA reads a Talos cluster's etcd CA from its secrets bundle. The nodes' etcd trusts it for
// clients, and serves a certificate it signed, as on every other distribution.
func talosEtcdCA(ctx context.Context, r client.Reader, cluster *clusterv1.Cluster) (*etcdCAPair, error) {
	s := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: talosBundleSecret(cluster.Name)}, s)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("the cluster's secrets bundle (Secret %s) is not there yet", talosBundleSecret(cluster.Name))
	}
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's secrets bundle: %w", err)
	}
	var certs *secrets.Certs
	if raw, ok := s.Data["bundle"]; ok {
		bundle := &secrets.Bundle{}
		if err := yaml.Unmarshal(raw, bundle); err != nil {
			return nil, fmt.Errorf("reading the cluster's secrets bundle: %w", err)
		}
		certs = bundle.Certs
	} else {
		// The format the provider wrote before it kept the whole bundle.
		certs = &secrets.Certs{}
		if err := yaml.Unmarshal(s.Data["certs"], certs); err != nil {
			return nil, fmt.Errorf("reading the cluster's certificates: %w", err)
		}
	}
	if certs == nil || certs.Etcd == nil {
		return nil, errors.New("the cluster's secrets bundle holds no etcd CA")
	}
	return parseCA(pemOrBase64(certs.Etcd.Crt), pemOrBase64(certs.Etcd.Key))
}

// pemOrBase64 is a PEM block as it was stored, plain or wrapped in base64 as Talos's own files do.
func pemOrBase64(data []byte) []byte {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN")) {
		return data
	}
	if decoded, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(data))); err == nil {
		return decoded
	}
	return data
}

// talosWALFsync reads each control-plane node's etcd metrics at the node's address. Talos runs etcd
// as a service listening on every address of the node, and serves its metrics beside the API on the
// client port, to clients with a certificate of the etcd CA.
func (d *EtcdDialer) talosWALFsync(ctx context.Context, cluster *clusterv1.Cluster, tlsConfig *tls.Config, port int) ([]MemberFsync, error) {
	var machines clusterv1.MachineList
	err := d.Reader.List(ctx, &machines, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		return nil, fmt.Errorf("listing the control-plane machines: %w", err)
	}
	var out []MemberFsync
	var errs []error
	for i := range machines.Items {
		address := machineAddress(&machines.Items[i])
		if address == "" || !machines.Items[i].DeletionTimestamp.IsZero() {
			continue
		}
		dial := func(ctx context.Context) (net.Conn, error) {
			var dialer net.Dialer
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
			if err != nil {
				return nil, fmt.Errorf("reaching etcd of %s: %w", address, err)
			}
			return conn, nil
		}
		fsync, err := memberFsync(ctx, dial, tlsConfig)
		if err != nil {
			errs = append(errs, fmt.Errorf("member %s: %w", machines.Items[i].Name, err))
			continue
		}
		fsync.Member = machines.Items[i].Name
		out = append(out, fsync)
	}
	if len(out) == 0 && len(errs) == 0 {
		return nil, errors.New("no control-plane machine of the cluster has an address yet")
	}
	return out, errors.Join(errs...)
}
