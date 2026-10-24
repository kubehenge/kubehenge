//go:build integration

package provisioner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The Headlamp identities against a real API server's RBAC and TokenRequest API, standing in for a
// workload cluster: the issuer makes the admin account's tokens and nothing else, a token made acts
// as cluster-admin and carries the ID the audit log records, and removing the identities ends it.
func TestIntegrationHeadlampIdentities(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	cfg, _, _ := startAPIConfig(t)
	ctx := context.Background()
	workload, err := client.New(cfg, client.Options{Scheme: remoteScheme})
	if err != nil {
		t.Fatal(err)
	}
	headlampIssuer := identityOf(IdentityHeadlampIssuer)
	for _, id := range []*workloadIdentity{identityOf(IdentityHealth), headlampIssuer} {
		ready, ensureErr := id.ensure(ctx, workload)
		if ensureErr != nil || !ready {
			t.Fatalf("creating %s: %v", id.id, ensureErr)
		}
	}
	issuerToken, _, err := requestToken(ctx, workload, headlampIssuer.namespace, headlampIssuer.account())
	if err != nil {
		t.Fatal(err)
	}
	as := func(token string) *kubernetes.Clientset {
		t.Helper()
		c := rest.AnonymousClientConfig(cfg)
		c.BearerToken = token
		clientset, cerr := kubernetes.NewForConfig(c)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return clientset
	}
	issuer := as(issuerToken)
	// The TokenRequest API's shortest token, as the interface asks for at a session's end.
	seconds := int64(600)
	request := func() *authenticationv1.TokenRequest {
		return &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}
	}

	made, err := issuer.CoreV1().ServiceAccounts(metav1.NamespaceSystem).CreateToken(ctx, HeadlampAccount, request(), metav1.CreateOptions{})
	if err != nil || made.Status.Token == "" {
		t.Fatalf("the issuer made no token of %s: %v", HeadlampAccount, err)
	}
	for _, account := range []string{identityOf(IdentityHealth).account(), headlampIssuer.account()} {
		if _, err = issuer.CoreV1().ServiceAccounts(metav1.NamespaceSystem).CreateToken(ctx, account, request(), metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
			t.Errorf("the issuer made a token of %s: %v", account, err)
		}
	}
	if _, err = issuer.CoreV1().Secrets(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("the issuer listed Secrets: %v", err)
	}

	headlamp := as(made.Status.Token)
	review, err := headlamp.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: "*", Group: "*", Resource: "*"}},
	}, metav1.CreateOptions{})
	if err != nil || !review.Status.Allowed {
		t.Fatalf("the token made is not cluster-admin: %v %+v", err, review)
	}
	who, err := headlamp.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ids := who.Status.UserInfo.Extra["authentication.kubernetes.io/credential-id"]
	if who.Status.UserInfo.Username != "system:serviceaccount:kube-system:"+HeadlampAccount || len(ids) != 1 || !slices.Contains(ids, credentialIDOf(t, made.Status.Token)) {
		t.Errorf("the token made is %q with credential IDs %v", who.Status.UserInfo.Username, ids)
	}

	if err = headlampIssuer.remove(ctx, workload); err != nil {
		t.Fatal(err)
	}
	// Its binding gone, the token has no rights left at once; the API server keeps a token it took
	// for up to ten seconds (its token cache) before it finds the account gone.
	if _, err = headlamp.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) && !apierrors.IsUnauthorized(err) {
		t.Errorf("a token made before the identities were removed still serves: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err = headlamp.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if apierrors.IsUnauthorized(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a token made before the identities were removed still signs in: %v", err)
		}
		time.Sleep(time.Second)
	}
	if err = headlampIssuer.remove(ctx, workload); err != nil {
		t.Errorf("removing them again: %v", err)
	}
}

// credentialIDOf is the ID the API server records for a ServiceAccount token: its jti claim.
func credentialIDOf(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("a token of %d parts", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.JTI == "" {
		t.Fatalf("a token without an ID: %v", err)
	}
	return "JTI=" + claims.JTI
}
