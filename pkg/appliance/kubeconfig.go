package appliance

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	authenticationv1 "k8s.io/api/authentication/v1"
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
	interfaceAccount   = "kubehenge-interface"
)

const (
	// interfaceTokenLifetime is how long each of the interface's tokens lasts. The token is the
	// appliance's only credential for the management cluster while it runs, and it lies in a file
	// that the container's backups carry off: a month lets an appliance that was switched off, or
	// whose management cluster was down, for weeks come back on its own, while a token copied out
	// with a backup stops working within a month. Renewal keeps the running appliance's near the
	// whole lifetime.
	interfaceTokenLifetime = 30 * 24 * time.Hour
	// interfaceTokenRenewAfter is how old a token gets before the interface asks for the next.
	interfaceTokenRenewAfter = 24 * time.Hour
	// interfaceTokenCheckEvery is how often the interface looks at its token's age. A renewal that
	// fails is tried again at the next look.
	interfaceTokenCheckEvery = time.Hour
)

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

// How often, and how long, to ask for the interface's first token while the chart that makes its
// ServiceAccount may still be settling.
const (
	tokenPoll    = 2 * time.Second
	tokenTimeout = 5 * time.Minute
)

// TokenPath is the interface's token for the management cluster, which its kubeconfig names.
func (a *Appliance) TokenPath() string { return filepath.Join(a.Dir, "interface.token") }

// InterfaceKubeconfig asks, with the management cluster's admin kubeconfig, for a token of the
// interface's ServiceAccount, writes it to tokenFile and returns a kubeconfig that reads it from
// there. The admin kubeconfig itself is never written down. It stays in the encrypted state.
func InterfaceKubeconfig(ctx context.Context, admin []byte, endpoint, tokenFile string) ([]byte, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(admin)
	if err != nil {
		return nil, fmt.Errorf("the admin kubeconfig: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("a client for the management cluster: %w", err)
	}
	return kubeconfigWithToken(ctx, kube, cfg.CAData, endpoint, tokenFile)
}

func kubeconfigWithToken(ctx context.Context, kube kubernetes.Interface, ca []byte, endpoint, tokenFile string) ([]byte, error) {
	if len(ca) == 0 {
		return nil, errors.New("the admin kubeconfig names no CA of the management cluster")
	}
	var token string
	var last error
	err := wait.PollUntilContextTimeout(ctx, tokenPoll, tokenTimeout, true, func(ctx context.Context) (bool, error) {
		token, last = requestInterfaceToken(ctx, kube)
		return last == nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", err, last)
	}
	if err = writeFileAtomic(tokenFile, []byte(token)); err != nil {
		return nil, err
	}
	out := clientcmdapi.NewConfig()
	out.Clusters["management"] = &clientcmdapi.Cluster{Server: endpoint, CertificateAuthorityData: ca}
	// A file rather than the token itself: client-go reads it again as the interface renews it.
	out.AuthInfos[interfaceAccount] = &clientcmdapi.AuthInfo{TokenFile: tokenFile}
	out.Contexts["management"] = &clientcmdapi.Context{Cluster: "management", AuthInfo: interfaceAccount}
	out.CurrentContext = "management"
	raw, err := clientcmd.Write(*out)
	if err != nil {
		return nil, fmt.Errorf("encoding the interface's kubeconfig: %w", err)
	}
	return raw, nil
}

// requestInterfaceToken asks for a new token of the interface's ServiceAccount, lasting
// interfaceTokenLifetime unless the API server holds tokens shorter.
func requestInterfaceToken(ctx context.Context, kube kubernetes.Interface) (string, error) {
	seconds := int64(interfaceTokenLifetime / time.Second)
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}
	got, err := kube.CoreV1().ServiceAccounts(interfaceNamespace).CreateToken(ctx, interfaceAccount, req, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("asking for a token of %s/%s: %w", interfaceNamespace, interfaceAccount, err)
	}
	if got.Status.Token == "" {
		return "", fmt.Errorf("the management cluster gave %s/%s an empty token", interfaceNamespace, interfaceAccount)
	}
	return got.Status.Token, nil
}

// RenewInterfaceToken keeps the interface's token fresh until ctx ends: once a token is a day old,
// or halfway through a life the API server held shorter, the interface asks for the next with the
// token itself, which its role allows for its own ServiceAccount alone. The token file is replaced
// whole, so client-go never reads half of one. Nothing happens while there is no management cluster.
func (a *Appliance) RenewInterfaceToken(ctx context.Context) {
	t := time.NewTicker(interfaceTokenCheckEvery)
	defer t.Stop()
	for {
		if err := a.renewTokenWhenDue(ctx, time.Now(), nil); err != nil {
			a.Log.Error("renewing the interface's token for the management cluster", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// renewTokenWhenDue renews the interface's token when it is due, through kube, or when nil, a
// client made from the interface's kubeconfig.
func (a *Appliance) renewTokenWhenDue(ctx context.Context, now time.Time, kube kubernetes.Interface) error {
	if !a.Ready() {
		return nil
	}
	if current, err := os.ReadFile(a.TokenPath()); err == nil && !tokenDue(string(current), now) {
		return nil
	}
	if kube == nil {
		kubeconfig, err := os.ReadFile(a.KubeconfigPath())
		if err != nil {
			return fmt.Errorf("the interface's kubeconfig: %w", err)
		}
		cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
		if err != nil {
			return fmt.Errorf("the interface's kubeconfig: %w", err)
		}
		if kube, err = kubernetes.NewForConfig(cfg); err != nil {
			return fmt.Errorf("a client for the management cluster: %w", err)
		}
	}
	token, err := requestInterfaceToken(ctx, kube)
	if apierrors.IsUnauthorized(err) {
		return fmt.Errorf("%w; the token ran out while the appliance was off: apply the management cluster again on the Administration page", err)
	}
	if err != nil {
		return err
	}
	return writeFileAtomic(a.TokenPath(), []byte(token))
}

// tokenDue tells whether a token is old enough to be renewed: a day old, or halfway through its
// life where that is shorter. One whose times cannot be read is renewed.
func tokenDue(token string, now time.Time) bool {
	issued, expires, ok := tokenTimes(token)
	if !ok {
		return true
	}
	return now.Sub(issued) >= min(interfaceTokenRenewAfter, expires.Sub(issued)/halves)
}

// halves splits a token's life in two: one the API server holds shorter than a day is renewed
// halfway through it.
const halves = 2

// jwtParts are a JSON Web Token's header, claims and signature.
const jwtParts = 3

// tokenTimes reads when a ServiceAccount token was issued and when it runs out from its claims.
// The signature is the API server's to check. These only schedule the renewal.
func tokenTimes(token string) (time.Time, time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != jwtParts {
		return time.Time{}, time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	var claims struct {
		IssuedAt  int64 `json:"iat"`
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.IssuedAt == 0 || claims.ExpiresAt <= claims.IssuedAt {
		return time.Time{}, time.Time{}, false
	}
	return time.Unix(claims.IssuedAt, 0), time.Unix(claims.ExpiresAt, 0), true
}
