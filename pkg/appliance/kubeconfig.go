package appliance

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
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
