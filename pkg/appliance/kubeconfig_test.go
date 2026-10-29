package appliance

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
)

// serviceAccountToken stands for a token the API server issued at issued, lasting life.
func serviceAccountToken(issued time.Time, life time.Duration) string {
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"sub":"system:serviceaccount:%s:%s"}`,
		issued.Unix(), issued.Add(life).Unix(), interfaceNamespace, interfaceAccount)
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
}

// tokenIssuer is the management cluster's TokenRequest API: it makes tokens of the interface's
// account as of now, or refuses with refuse when set.
type tokenIssuer struct {
	mu     sync.Mutex
	now    time.Time
	refuse error
	asked  []int64
}

func (i *tokenIssuer) client() *fake.Clientset {
	kube := fake.NewClientset()
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := a.(k8stesting.CreateActionImpl)
		req, isToken := create.GetObject().(*authenticationv1.TokenRequest)
		if !ok || !isToken || a.GetSubresource() != "token" || a.GetNamespace() != interfaceNamespace || create.Name != interfaceAccount {
			return true, nil, errors.New("not a token request for the interface's account")
		}
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.refuse != nil {
			return true, nil, i.refuse
		}
		i.asked = append(i.asked, *req.Spec.ExpirationSeconds)
		life := time.Duration(*req.Spec.ExpirationSeconds) * time.Second
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: serviceAccountToken(i.now, life)}}, nil
	})
	return kube
}

func (i *tokenIssuer) requests() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.asked)
}

func TestInterfaceKubeconfigReadsAnExpiringTokenFromAFile(t *testing.T) {
	t.Parallel()
	issuer := &tokenIssuer{now: time.Now()}
	tokenFile := filepath.Join(t.TempDir(), "interface.token")
	raw, err := kubeconfigWithToken(t.Context(), issuer.client(), []byte("CA"), "https://192.0.2.10:6443", tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "header.") {
		t.Fatalf("the kubeconfig holds the token itself:\n%s", raw)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(tokenFile) //nolint:gosec // the test's own temporary file
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BearerTokenFile != tokenFile || cfg.BearerToken != string(token) || string(cfg.CAData) != "CA" || cfg.Host != "https://192.0.2.10:6443" {
		t.Fatalf("config = %+v", cfg)
	}
	if issuer.asked[0] != int64(interfaceTokenLifetime/time.Second) {
		t.Fatalf("asked for a token of %ds", issuer.asked[0])
	}
	if _, err = kubeconfigWithToken(t.Context(), issuer.client(), nil, "https://192.0.2.10:6443", tokenFile); err == nil {
		t.Fatal("a kubeconfig without the management cluster's CA")
	}
}

func TestInterfaceTokenRenewal(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	a := &Appliance{Dir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	issuer := &tokenIssuer{now: start}
	kube := issuer.client()

	// No management cluster, nothing to renew.
	if err := a.renewTokenWhenDue(t.Context(), start, kube); err != nil || issuer.requests() != 0 {
		t.Fatalf("without a management cluster: %v, %d requests", err, issuer.requests())
	}
	if err := writeFileAtomic(a.KubeconfigPath(), []byte("kubeconfig")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(a.TokenPath(), []byte(serviceAccountToken(start, interfaceTokenLifetime))); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		after    time.Duration
		requests int
	}{
		{time.Hour, 0},
		{23 * time.Hour, 0},
		{25 * time.Hour, 1},
		{26 * time.Hour, 1},
	} {
		now := start.Add(step.after)
		issuer.mu.Lock()
		issuer.now = now
		issuer.mu.Unlock()
		if err := a.renewTokenWhenDue(t.Context(), now, kube); err != nil {
			t.Fatal(err)
		}
		if got := issuer.requests(); got != step.requests {
			t.Fatalf("after %v: %d requests, want %d", step.after, got, step.requests)
		}
	}
	raw, err := os.ReadFile(a.TokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if issued, _, ok := tokenTimes(string(raw)); !ok || !issued.Equal(start.Add(25*time.Hour)) {
		t.Fatalf("the token file holds one issued %v", issued)
	}

	issuer.mu.Lock()
	issuer.refuse = apierrors.NewUnauthorized("token expired")
	issuer.mu.Unlock()
	err = a.renewTokenWhenDue(t.Context(), start.Add(60*24*time.Hour), kube)
	if err == nil || !strings.Contains(err.Error(), "apply the management cluster again") {
		t.Fatalf("a token run out: %v", err)
	}
	if after, _ := os.ReadFile(a.TokenPath()); !bytes.Equal(after, raw) {
		t.Fatal("a refused renewal replaced the token")
	}
}

func TestTokenDue(t *testing.T) {
	t.Parallel()
	issued := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		token string
		at    time.Duration
		due   bool
	}{
		{"a fresh token", serviceAccountToken(issued, interfaceTokenLifetime), time.Hour, false},
		{"a day old", serviceAccountToken(issued, interfaceTokenLifetime), interfaceTokenRenewAfter, true},
		{"a short life held by the API server, before its half", serviceAccountToken(issued, 2*time.Hour), 59 * time.Minute, false},
		{"a short life held by the API server, at its half", serviceAccountToken(issued, 2*time.Hour), time.Hour, true},
		{"not a token", "garbage", 0, true},
		{"no times", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".b", 0, true},
	} {
		if got := tokenDue(c.token, issued.Add(c.at)); got != c.due {
			t.Errorf("%s: due %v, want %v", c.name, got, c.due)
		}
	}
}
