package ui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// tokenMinter stands in for a workload cluster's TokenRequest API, as the issuer reaches it.
type tokenMinter struct {
	mu sync.Mutex
	// asked are the lifetimes asked for, in seconds, one per token made.
	asked []int64
	// accounts are the accounts tokens were made for.
	accounts []string
	now      func() time.Time
}

func (m *tokenMinter) workloads() *fakeWorkloads {
	kube := fake.NewClientset()
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := a.(k8stesting.CreateActionImpl)
		req, isToken := create.GetObject().(*authenticationv1.TokenRequest)
		if !ok || !isToken || a.GetSubresource() != "token" || a.GetNamespace() != metav1.NamespaceSystem {
			return true, nil, errors.New("not a token request in kube-system")
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.asked = append(m.asked, *req.Spec.ExpirationSeconds)
		m.accounts = append(m.accounts, create.Name)
		claims := base64.RawURLEncoding.EncodeToString([]byte(`{"jti":"id-` + strconv.Itoa(len(m.asked)) + `"}`))
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
			Token:               "header." + claims + ".signature",
			ExpirationTimestamp: metav1.NewTime(m.now().Add(time.Duration(*req.Spec.ExpirationSeconds) * time.Second)),
		}}, nil
	})
	return &fakeWorkloads{kube: kube}
}

func (m *tokenMinter) made() ([]int64, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.asked...), append([]string(nil), m.accounts...)
}

// issuerSecret is the issuer's token as Kubehenge's controller keeps it beside cluster one.
func issuerSecret(expires time.Time) *corev1.Secret {
	return &corev1.Secret{
		Namespace: "default", Name: provisioner.IdentitySecretName("one", provisioner.IdentityHeadlampIssuer),
		Annotations: map[string]string{provisioner.IdentityExpiresAnnotation: expires.UTC().Format(time.RFC3339)},
		Data: map[string][]byte{
			provisioner.IdentityServerKey: []byte("https://10.0.0.20:6443"), provisioner.IdentityCAKey: []byte("the cluster CA"),
			provisioner.IdentityTokenKey: []byte("issuer-token"),
		},
	}
}

func TestHeadlampSignsInThoseWhoMayDownloadTheKubeconfig(t *testing.T) {
	t.Parallel()
	one := cluster("default", "one")
	one.UID = "uid-one"
	h := newHarness(t, one, adminKubeconfig("default", "one", "https://10.0.0.20:6443"),
		issuerSecret(time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)))
	seen := withHeadlamp(t, h)
	minter := &tokenMinter{now: func() time.Time { return h.clock }}
	workloads := minter.workloads()
	h.server.ConnectWorkload = workloads.connect
	var logged bytes.Buffer
	h.server.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	h.server.sessions.maxAge = 30 * time.Minute
	if err := h.server.writeHeadlampKubeconfig(t.Context(), h.kube); err != nil {
		t.Fatal(err)
	}
	upstream := func(b *browser, cookies ...string) string {
		t.Helper()
		*seen = nil
		rec := b.headlamp("GET", "/clusters/default.one/api/v1/namespaces", cookies...)
		if rec.Code != http.StatusOK || len(*seen) != 1 {
			t.Fatalf("through the gate: %d %s", rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String()+strings.Join(rec.Result().Header.Values("Set-Cookie"), ";"), "signature") {
			t.Fatal("the token reached the browser")
		}
		return (*seen)[0].authorization
	}

	dev := h.browser()
	dev.signIn("dev@pve")
	got := upstream(dev)
	if !strings.HasPrefix(got, "Bearer header.") {
		t.Fatalf("Headlamp got %q for a user who may download the kubeconfig, want the session's token", got)
	}
	if again := upstream(dev); again != got {
		t.Errorf("the next request got %q, want the same token", again)
	}
	asked, accounts := minter.made()
	// The session has 30 minutes, so the token gets no more.
	if len(asked) != 1 || asked[0] != 1800 || accounts[0] != provisioner.HeadlampAccount {
		t.Fatalf("tokens made: %v for %v, want one of 1800 s for %s", asked, accounts, provisioner.HeadlampAccount)
	}
	if tokens := workloads.connected(); len(tokens) != 1 || tokens[0] != "issuer-token" {
		t.Errorf("reached the cluster with %v, want the issuer's token", tokens)
	}
	if !strings.Contains(logged.String(), `"user":"dev@pve"`) || !strings.Contains(logged.String(), `"credentialID":"JTI=id-1"`) {
		t.Errorf("the sign-in is not logged with its user and the token's ID: %s", logged.String())
	}

	// Nearly run out, the token is made again, for no longer than the session but the API's least.
	h.clock = h.clock.Add(26 * time.Minute)
	if renewed := upstream(dev); renewed == got || renewed == "" {
		t.Errorf("with 4 minutes left: %q", renewed)
	}
	if asked, _ = minter.made(); len(asked) != 2 || asked[1] != 600 {
		t.Errorf("tokens made: %v, want a second of 600 s", asked)
	}

	// A token of one's own stays one's own, as does a credential the browser sends.
	if own := upstream(dev, "headlamp-auth-defaultone.0=own-token"); own != "" {
		t.Errorf("with a token of the user's own, Headlamp got %q from the gate", own)
	}

	viewer := h.browser()
	viewer.signIn("viewer@pve")
	if view := upstream(viewer); view != "" {
		t.Errorf("a viewer was signed in with %q", view)
	}
	if asked, _ = minter.made(); len(asked) != 2 {
		t.Errorf("tokens made for a viewer: %v", asked)
	}
}

func TestHeadlampSignsNobodyInWithoutTheIssuer(t *testing.T) {
	t.Parallel()
	one := cluster("default", "one")
	one.UID = "uid-one"
	h := newHarness(t, one, adminKubeconfig("default", "one", "https://10.0.0.20:6443"))
	seen := withHeadlamp(t, h)
	minter := &tokenMinter{now: func() time.Time { return h.clock }}
	workloads := minter.workloads()
	h.server.ConnectWorkload = workloads.connect
	if err := h.server.writeHeadlampKubeconfig(t.Context(), h.kube); err != nil {
		t.Fatal(err)
	}
	dev := h.browser()
	dev.signIn("dev@pve")
	for range 2 {
		*seen = nil
		if rec := dev.headlamp("GET", "/clusters/default.one/version"); rec.Code != http.StatusOK || len(*seen) != 1 || (*seen)[0].authorization != "" {
			t.Fatalf("with the sign-in off: %d, upstream %+v", rec.Code, *seen)
		}
	}
	if len(workloads.connected()) != 0 {
		t.Error("reached the cluster without an issuer's token")
	}

	// Once the controller keeps one, the next look after a while signs the user in.
	if err := h.kube.Create(t.Context(), issuerSecret(h.clock.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	h.clock = h.clock.Add(headlampPrivilegeTTL)
	*seen = nil
	if rec := dev.headlamp("GET", "/clusters/default.one/version"); rec.Code != http.StatusOK || !strings.HasPrefix((*seen)[0].authorization, "Bearer ") {
		t.Fatalf("with the sign-in on: %d, upstream %+v", rec.Code, *seen)
	}
}

// Headlamp's log out of a cluster, as its frontend sends it, signs the session out of the cluster
// until the session opens the cluster from the interface again. Other sessions stay signed in.
func TestHeadlampLogOutSignsTheSessionOut(t *testing.T) {
	t.Parallel()
	one := cluster("default", "one")
	one.UID = "uid-one"
	h := newHarness(t, one, adminKubeconfig("default", "one", "https://10.0.0.20:6443"),
		issuerSecret(time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)))
	seen := withHeadlamp(t, h)
	minter := &tokenMinter{now: func() time.Time { return h.clock }}
	h.server.ConnectWorkload = minter.workloads().connect
	if err := h.server.writeHeadlampKubeconfig(t.Context(), h.kube); err != nil {
		t.Fatal(err)
	}
	upstream := func(b *browser) string {
		t.Helper()
		*seen = nil
		if rec := b.headlamp("GET", "/clusters/default.one/api/v1/namespaces"); rec.Code != http.StatusOK || len(*seen) != 1 {
			t.Fatalf("through the gate: %d %s", rec.Code, rec.Body)
		}
		return (*seen)[0].authorization
	}
	// Headlamp names the cluster in its log out's path in unpadded URL-safe base64.
	logOut := "/encoded-clusters/" + base64.RawURLEncoding.EncodeToString([]byte("default.one")) + "/clear-token"

	dev, other := h.browser(), h.browser()
	dev.signIn("dev@pve")
	other.signIn("dev@pve")
	if upstream(dev) == "" || upstream(other) == "" {
		t.Fatal("not signed in before logging out")
	}
	*seen = nil
	if rec := dev.headlamp("POST", logOut); rec.Code != http.StatusOK || len(*seen) != 1 || (*seen)[0].path != logOut {
		t.Fatalf("Headlamp's log out: %d, upstream %+v", rec.Code, *seen)
	}
	asked, _ := minter.made()
	for range 2 {
		if got := upstream(dev); got != "" {
			t.Fatalf("signed in again with %q after logging out", got)
		}
	}
	if again, _ := minter.made(); len(again) != len(asked) {
		t.Errorf("tokens made for a session signed out: %v", again)
	}
	if upstream(other) == "" {
		t.Error("another session of the same user was signed out too")
	}

	// Opening the cluster from the interface signs the session in again, with a token of its own.
	rec := dev.headlamp("GET", headlampOpenPath+"default.one/pods")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/c/default.one/pods" {
		t.Fatalf("opening the cluster: %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := upstream(dev); !strings.HasPrefix(got, "Bearer header.") {
		t.Errorf("after opening the cluster again Headlamp got %q", got)
	}
	if again, _ := minter.made(); len(again) != len(asked)+1 {
		t.Errorf("tokens made: %v, want one more for the session signed in again", again)
	}

	encoded := func(cluster, route string) string {
		return "/encoded-clusters/" + base64.RawURLEncoding.EncodeToString([]byte(cluster)) + "/" + route
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{encoded("default.two", "clear-token"), http.StatusNotFound},
		{"/encoded-clusters/!!!/clear-token", http.StatusBadRequest},
		{encoded("default.one", "other"), http.StatusForbidden},
		{headlampOpenPath + "default.two", http.StatusNotFound},
	} {
		path, want := tc.path, tc.want
		*seen = nil
		viewer := h.browser()
		viewer.signIn("viewer@pve")
		method := http.MethodPost
		if strings.HasPrefix(path, headlampOpenPath) {
			method = http.MethodGet
		}
		if rec := viewer.headlamp(method, path); rec.Code != want || len(*seen) != 0 {
			t.Errorf("%s %s: %d, upstream %+v; want %d and nothing passed on", method, path, rec.Code, *seen, want)
		}
	}
	*seen = nil
	if rec := dev.headlamp("GET", logOut); rec.Code != http.StatusForbidden || len(*seen) != 0 {
		t.Errorf("a log out by GET: %d, upstream %+v", rec.Code, *seen)
	}
}

func TestCredentialID(t *testing.T) {
	t.Parallel()
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://kubernetes.default.svc","jti":"0f6b2c"}`))
	for token, want := range map[string]string{
		"h." + claims + ".s": "JTI=0f6b2c",
		"h." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"x"}`)) + ".s": "",
		"not-a-jwt":  "",
		"h.%%%%.s":   "",
		"h.e30.s.ex": "",
	} {
		if got := credentialID(token); got != want {
			t.Errorf("credentialID(%q) = %q, want %q", token, got, want)
		}
	}
}

func TestHeadlampSignInsArePruned(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	h := &Headlamp{
		signIns: map[string]*headlampSignIn{
			"expired": {token: "t", expires: now.Add(-time.Second)},
			"held":    {token: "t", expires: now.Add(time.Minute)},
			"waiting": {retry: now.Add(time.Second)},
		},
		signedOut: map[string]time.Time{"ended": now, "on": now.Add(time.Minute)},
	}
	h.pruneHeadlampSignIns(now)
	if _, ok := h.signIns["expired"]; ok || len(h.signIns) != 2 {
		t.Fatalf("kept %v", h.signIns)
	}
	if _, ok := h.signedOut["on"]; !ok || len(h.signedOut) != 1 {
		t.Fatalf("kept the sign-outs %v, want those of sessions still on", h.signedOut)
	}
}
