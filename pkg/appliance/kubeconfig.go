package appliance

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// The chart's identity for the interface (charts/kubehenge/templates/interface.yaml).
const (
	interfaceNamespace = "kubehenge-system"
	interfaceSecret    = "kubehenge-interface-token" //nolint:gosec // a Secret's name, not a credential
)

// RootSecret is where Kubehenge's controller reads root@pam's password, under rootPasswordKey, for
// the CSI plugin of clusters that take volume snapshots (the module's cluster_volume_snapshots).
const (
	RootSecret      = "kubehenge-root-pam" //nolint:gosec // a Secret's name, not a credential
	rootPasswordKey = "password"
)

// ErrNoRootPassword is an apply with volume snapshots on and no root@pam password, given or stored.
var ErrNoRootPassword = errors.New("volume snapshots need root@pam's password, and none is stored")

// StoreRootPassword keeps root@pam's password in the management cluster, with its admin
// kubeconfig, and nowhere else: written when password is given, left as it is when it is empty,
// and deleted with volume snapshots off.
func StoreRootPassword(ctx context.Context, admin []byte, on bool, password string) error {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(admin)
	if err != nil {
		return fmt.Errorf("the admin kubeconfig: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("a client for the management cluster: %w", err)
	}
	secrets := kube.CoreV1().Secrets(interfaceNamespace)
	switch {
	case !on:
		err = secrets.Delete(ctx, RootSecret, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
	case password == "":
		var s *corev1.Secret
		s, err = secrets.Get(ctx, RootSecret, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || err == nil && len(s.Data[rootPasswordKey]) == 0 {
			return ErrNoRootPassword
		}
	default:
		s := &corev1.Secret{
			Name: RootSecret, Namespace: interfaceNamespace,
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{rootPasswordKey: []byte(password)},
		}
		_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, s, metav1.CreateOptions{})
		}
	}
	if err != nil {
		return fmt.Errorf("root@pam's password in %s/%s: %w", interfaceNamespace, RootSecret, err)
	}
	return nil
}

// CredentialsExpire is when the first client certificate in the management cluster's admin
// kubeconfig and talosconfig runs out, zero when neither holds one.
func CredentialsExpire(kubeconfig, talos []byte) time.Time {
	var certs [][]byte
	if cfg, err := clientcmd.Load(kubeconfig); err == nil {
		for _, auth := range cfg.AuthInfos {
			certs = append(certs, auth.ClientCertificateData)
		}
	}
	if cfg, err := talosconfig.FromBytes(talos); err == nil {
		for _, c := range cfg.Contexts {
			if crt, err := base64.StdEncoding.DecodeString(c.Crt); err == nil {
				certs = append(certs, crt)
			}
		}
	}
	var first time.Time
	for _, data := range certs {
		block, _ := pem.Decode(data)
		if block == nil {
			continue
		}
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil && (first.IsZero() || cert.NotAfter.Before(first)) {
			first = cert.NotAfter
		}
	}
	return first
}

// How often, and how long, to wait for Kubernetes to fill in the interface's token.
const (
	tokenPoll    = 2 * time.Second
	tokenTimeout = 5 * time.Minute
)

// InterfaceKubeconfig reads the interface's ServiceAccount token with the management cluster's
// admin kubeconfig and returns a kubeconfig holding only that token. The admin kubeconfig itself
// is never written down; it stays in the encrypted state.
func InterfaceKubeconfig(ctx context.Context, admin []byte, endpoint string) ([]byte, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(admin)
	if err != nil {
		return nil, fmt.Errorf("the admin kubeconfig: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("a client for the management cluster: %w", err)
	}
	var secret *corev1.Secret
	// Kubernetes fills the token in shortly after the Secret is created.
	err = wait.PollUntilContextTimeout(ctx, tokenPoll, tokenTimeout, true, func(ctx context.Context) (bool, error) {
		s, getErr := kube.CoreV1().Secrets(interfaceNamespace).Get(ctx, interfaceSecret, metav1.GetOptions{})
		if getErr != nil {
			return false, nil //nolint:nilerr // retried until the timeout; the API may still be settling
		}
		secret = s
		return len(s.Data[corev1.ServiceAccountTokenKey]) > 0 && len(s.Data[corev1.ServiceAccountRootCAKey]) > 0, nil
	})
	if err != nil {
		return nil, fmt.Errorf("waiting for %s/%s: %w", interfaceNamespace, interfaceSecret, err)
	}
	out := clientcmdapi.NewConfig()
	out.Clusters["management"] = &clientcmdapi.Cluster{
		Server:                   endpoint,
		CertificateAuthorityData: secret.Data[corev1.ServiceAccountRootCAKey],
	}
	out.AuthInfos["kubehenge-interface"] = &clientcmdapi.AuthInfo{Token: string(secret.Data[corev1.ServiceAccountTokenKey])}
	out.Contexts["management"] = &clientcmdapi.Context{Cluster: "management", AuthInfo: "kubehenge-interface"}
	out.CurrentContext = "management"
	raw, err := clientcmd.Write(*out)
	if err != nil {
		return nil, fmt.Errorf("encoding the interface's kubeconfig: %w", err)
	}
	return raw, nil
}
