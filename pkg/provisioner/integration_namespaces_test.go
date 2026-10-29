//go:build integration

package provisioner

import (
	"context"
	"encoding/json"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// interfaceAccount is the identity the interface acts with in the management cluster.
func interfaceAccount(cfg *rest.Config) *rest.Config {
	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + classNamespace + ":kubehenge-interface",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + classNamespace, "system:authenticated"},
	}
	return as
}

// TestIntegrationInterfaceMoreNamespaces gives the interface the namespaces of
// testdata/more-namespaces-values.yaml beside its own: in each it may create, change and delete
// clusters, read their kubeconfigs and store credentials, held to them by the same policy, and in
// no other.
func TestIntegrationInterfaceMoreNamespaces(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	cfg, admin := startAPIConfig(t, "testdata/more-namespaces-values.yaml")
	ctx := context.Background()
	as := interfaceAccount(cfg)
	ui, err := client.New(as, client.Options{Scheme: admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	reviews, err := kubernetes.NewForConfig(as)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{Name: "team-c"}); err != nil {
		t.Fatal(err)
	}

	listed := &corev1.ConfigMap{}
	if err := ui.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: "kubehenge-interface"}, listed); err != nil {
		t.Fatalf("the interface reading where it manages clusters: %v", err)
	}
	var namespaces []string
	if err := json.Unmarshal([]byte(listed.Data["namespaces"]), &namespaces); err != nil ||
		!slices.Equal(namespaces, []string{interfaceNamespace, "team-a", "team-b"}) {
		t.Fatalf("namespaces = %q (%v)", listed.Data["namespaces"], err)
	}
	other := &corev1.ConfigMap{}
	if err := ui.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: "kubehenge-release-checksums"}, other); !apierrors.IsForbidden(err) {
		t.Errorf("the interface reading another ConfigMap of Kubehenge's: %v, want forbidden", err)
	}

	allowed := func(verb, group, resource, namespace string) bool {
		t.Helper()
		review, err := reviews.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: verb, Group: group, Resource: resource, Namespace: namespace,
			}},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return review.Status.Allowed
	}
	for _, ns := range []string{interfaceNamespace, "team-a", "team-b", "team-c", "default", classNamespace} {
		want := ns == interfaceNamespace || ns == "team-a" || ns == "team-b"
		for _, verb := range []string{"create", "update", "patch", "delete"} {
			if got := allowed(verb, "cluster.x-k8s.io", "clusters", ns); got != want {
				t.Errorf("%s clusters in %s: %v, want %v", verb, ns, got, want)
			}
		}
		if got := allowed("get", "", "secrets", ns); got != want {
			t.Errorf("get secrets in %s: %v, want %v", ns, got, want)
		}
		if allowed("list", "", "secrets", ns) {
			t.Errorf("list secrets in %s allowed", ns)
		}
	}

	waitForCredentialPolicy(t, ui, admin, "team-a")
	for _, ns := range []string{"team-a", "team-b"} {
		kubeconfig := &corev1.Secret{Namespace: ns, Name: "one-kubeconfig", Data: map[string][]byte{"value": []byte("apiVersion: v1\n")}}
		if err := admin.Create(ctx, kubeconfig); err != nil {
			t.Fatal(err)
		}
		if err := ui.Get(ctx, client.ObjectKeyFromObject(kubeconfig), &corev1.Secret{}); err != nil {
			t.Errorf("reading %s's kubeconfig: %v", ns, err)
		}
		c := credential("one", "backup", map[string][]byte{"accessKeyID": []byte("a"), "secretAccessKey": []byte("b")})
		c.Namespace = ns
		if err := ui.Create(ctx, c); err != nil {
			t.Errorf("storing a credential in %s: %v", ns, err)
		} else if err := ui.Delete(ctx, c); err != nil {
			t.Errorf("removing a credential in %s: %v", ns, err)
		}
		unlabelled := &corev1.Secret{Namespace: ns, Name: "one.backup", Type: CredentialType}
		if err := ui.Create(ctx, unlabelled); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "not an unlabelled Secret") {
			t.Errorf("an unlabelled Secret in %s: %v, want refused by the policy", ns, err)
		}
		if err := ui.Delete(ctx, kubeconfig); !apierrors.IsInvalid(err) {
			t.Errorf("deleting %s's kubeconfig: %v, want refused by the policy", ns, err)
		}
	}
	elsewhere := credential("one", "backup", nil)
	elsewhere.Namespace = "team-c"
	if err := ui.Create(ctx, elsewhere); !apierrors.IsForbidden(err) {
		t.Errorf("a credential in team-c: %v, want forbidden", err)
	}
}

// waitForCredentialPolicy waits until the API server holds the interface to the credentials policy
// in namespace, which it takes up from its own watch a moment after the chart is applied.
func waitForCredentialPolicy(t *testing.T, ui, admin client.Client, namespace string) {
	t.Helper()
	ctx := context.Background()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		probe := &corev1.Secret{Namespace: namespace, Name: "probe"}
		err := ui.Create(ctx, probe)
		if apierrors.IsInvalid(err) {
			return
		}
		if err == nil {
			if deleted := admin.Delete(ctx, probe); deleted != nil {
				t.Fatal(deleted)
			}
		} else {
			t.Fatalf("a Secret of its own making: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the API server let the interface write any Secret for 30 seconds")
		}
	}
}

// TestIntegrationChartInterfaceNamespaces renders the chart without more namespaces, with some,
// and with those it refuses.
func TestIntegrationChartInterfaceNamespaces(t *testing.T) {
	t.Parallel()
	roles := func(values ...string) (map[string]bool, []string, map[string]bool) {
		t.Helper()
		bound, made := map[string]bool{}, map[string]bool{}
		var listed []string
		for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, append([]string{"testdata/integration-values.yaml"}, values...)...) {
			switch {
			case o.GetKind() == "RoleBinding" && o.GetName() == "kubehenge-interface":
				bound[o.GetNamespace()] = true
			case o.GetKind() == "Namespace":
				made[o.GetName()] = o.GetAnnotations()["helm.sh/resource-policy"] == "keep"
			case o.GetKind() == "ConfigMap" && o.GetName() == "kubehenge-interface":
				data, _, err := unstructured.NestedString(o.Object, "data", "namespaces")
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(data), &listed); err != nil {
					t.Fatal(err)
				}
			}
		}
		return bound, listed, made
	}

	bound, listed, made := roles()
	if !maps.Equal(bound, map[string]bool{classNamespace: true, interfaceNamespace: true}) ||
		!slices.Equal(listed, []string{interfaceNamespace}) || !maps.Equal(made, map[string]bool{interfaceNamespace: false, autoscalerNamespace: false}) {
		t.Errorf("without more namespaces: bound in %v, listed %v, namespaces %v", bound, listed, made)
	}
	bound, listed, made = roles("testdata/more-namespaces-values.yaml")
	if !maps.Equal(bound, map[string]bool{classNamespace: true, interfaceNamespace: true, "team-a": true, "team-b": true}) ||
		!slices.Equal(listed, []string{interfaceNamespace, "team-a", "team-b"}) ||
		!maps.Equal(made, map[string]bool{interfaceNamespace: false, autoscalerNamespace: false, "team-a": true, "team-b": true}) {
		t.Errorf("with team-a and team-b: bound in %v, listed %v, namespaces %v", bound, listed, made)
	}

	for _, refused := range []string{interfaceNamespace, classNamespace, "default", "kube-system", "kube-public", "capi-system", "cert-manager"} {
		helm := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", "../../charts/kubehenge", "--namespace", classNamespace, //nolint:gosec // the test's own chart and values
			"-f", "testdata/integration-values.yaml", "--set", "interface.extraNamespaces={team-a,"+refused+"}")
		out, err := helm.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "interface.extraNamespaces: "+refused) {
			t.Errorf("%s: %v\n%s", refused, err, lastLines(string(out), 3))
		}
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
