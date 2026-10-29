//go:build e2e

package appliance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestInterfaceKubeconfigEndToEnd makes the interface's kubeconfig from the admin kubeconfig in
// KUBECONFIG, as the appliance does after an apply, and checks what it may do in the management
// cluster: everything the interface needs and nothing more. KUBEHENGE_E2E_API_ENDPOINT is the
// management cluster's API, such as https://192.0.2.10:6443; KUBEHENGE_E2E_NAMESPACE, if set, one of
// the namespaces the module's extra_cluster_namespaces gives the interface.
func TestInterfaceKubeconfigEndToEnd(t *testing.T) {
	t.Parallel()
	adminPath, endpoint := os.Getenv("KUBECONFIG"), os.Getenv("KUBEHENGE_E2E_API_ENDPOINT")
	if adminPath == "" || endpoint == "" {
		t.Fatal("KUBECONFIG and KUBEHENGE_E2E_API_ENDPOINT must be set")
	}
	admin, err := os.ReadFile(adminPath) //nolint:gosec // the test's own setting
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tokenFile := filepath.Join(t.TempDir(), "interface.token")
	raw, err := InterfaceKubeconfig(ctx, admin, endpoint, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BearerToken == "" || cfg.BearerTokenFile != tokenFile || cfg.CertFile != "" || len(cfg.CertData) != 0 {
		t.Fatal("the interface's kubeconfig must read its token from the file and hold nothing else")
	}
	if issued, expires, ok := tokenTimes(cfg.BearerToken); !ok || expires.Sub(issued) > interfaceTokenLifetime {
		t.Fatalf("the interface's token is not one that runs out within %v", interfaceTokenLifetime)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	type access struct {
		verb, group, resource, namespace string
		want                             bool
	}
	// One of the module's extra_cluster_namespaces, when it was given any.
	var extra []access
	if ns := os.Getenv("KUBEHENGE_E2E_NAMESPACE"); ns != "" {
		extra = []access{
			{"create", "cluster.x-k8s.io", "clusters", ns, true},
			{"delete", "cluster.x-k8s.io", "clusters", ns, true},
			{"get", "", "secrets", ns, true},
			{"list", "", "secrets", ns, false},
			{"create", "", "pods", ns, false},
		}
	}
	for _, c := range append([]access{
		{"list", "cluster.x-k8s.io", "clusters", "", true},
		{"list", "cluster.x-k8s.io", "machines", "", true},
		{"list", "cluster.x-k8s.io", "clusterclasses", "kubehenge-system", true},
		{"create", "cluster.x-k8s.io", "clusters", "clusters", true},
		{"delete", "cluster.x-k8s.io", "clusters", "clusters", true},
		{"get", "", "secrets", "clusters", true},
		{"create", "cluster.x-k8s.io", "clusters", "default", false},
		{"delete", "cluster.x-k8s.io", "clusters", "default", false},
		{"get", "", "secrets", "kubehenge-system", false},
		{"get", "", "secrets", "default", false},
		{"list", "", "secrets", "clusters", false},
		{"create", "", "namespaces", "", false},
		{"update", "cluster.x-k8s.io", "clusterclasses", "kubehenge-system", false},
		{"create", "", "pods", "clusters", false},
		{"list", "cluster.x-k8s.io", "machinedeployments", "", true},
		{"list", "cluster.x-k8s.io", "machinehealthchecks", "", true},
		{"get", "controlplane.cluster.x-k8s.io", "kubeadmcontrolplanes", "clusters", true},
		{"get", "controlplane.cluster.x-k8s.io", "rke2controlplanes", "clusters", true},
		{"get", "controlplane.cluster.x-k8s.io", "kthreescontrolplanes", "clusters", true},
		{"list", "addons.cluster.x-k8s.io", "helmreleaseproxies", "", true},
		{"list", "", "events", "clusters", true},
		{"update", "cluster.x-k8s.io", "machinedeployments", "clusters", false},
		{"update", "controlplane.cluster.x-k8s.io", "kubeadmcontrolplanes", "clusters", false},
		{"delete", "cluster.x-k8s.io", "machines", "clusters", false},
		{"get", "", "pods", "clusters", false},
		{"create", "", "events", "clusters", false},
	}, extra...) {
		review, err := kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: c.verb, Group: c.group, Resource: c.resource, Namespace: c.namespace,
			}},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed != c.want {
			t.Errorf("%s %s/%s in %q: allowed %v, want %v", c.verb, c.group, c.resource, c.namespace, review.Status.Allowed, c.want)
		}
	}
	// The interface makes its next token with the one it has, and no other account's.
	for account, want := range map[string]bool{interfaceAccount: true, "kubehenge-controller": false, "default": false} {
		review, err := kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: "create", Resource: "serviceaccounts", Subresource: "token", Name: account, Namespace: interfaceNamespace,
			}},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed != want {
			t.Errorf("a token of %s: allowed %v, want %v", account, review.Status.Allowed, want)
		}
	}
	if _, err := requestInterfaceToken(ctx, kube); err != nil {
		t.Fatalf("the interface renewing its own token: %v", err)
	}
}
