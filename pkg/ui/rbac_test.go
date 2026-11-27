package ui

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// apiAccess is one request the interface makes to the management cluster with its own identity.
type apiAccess struct{ verb, group, resource, namespace, name string }

func (a apiAccess) String() string {
	what := a.group + "/" + a.resource
	if a.name != "" {
		what += "/" + a.name
	}
	if a.namespace != "" {
		what += " in " + a.namespace
	}
	return a.verb + " " + what
}

// accessLog records the requests a client makes.
type accessLog struct {
	mu   sync.Mutex
	seen map[apiAccess]bool
}

func (l *accessLog) note(c client.Client, verb string, obj runtime.Object, namespace, name string) {
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		panic(err)
	}
	gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
	resource, _ := meta.UnsafeGuessKindToResource(gvk)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[apiAccess{verb, gvk.Group, resource.Resource, namespace, name}] = true
}

// watching wraps kube so that every request it makes is noted. A list names the object it lists
// and a create no name, as the API server's authorisation sees them.
func (l *accessLog) watching(kube client.WithWatch) client.WithWatch {
	return interceptor.NewClient(kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			l.note(c, "get", obj, key.Namespace, key.Name)
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var o client.ListOptions
			o.ApplyOptions(opts)
			l.note(c, "list", list, o.Namespace, "")
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			l.note(c, "create", obj, obj.GetNamespace(), "")
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			l.note(c, "update", obj, obj.GetNamespace(), obj.GetName())
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			l.note(c, "patch", obj, obj.GetNamespace(), obj.GetName())
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			l.note(c, "delete", obj, obj.GetNamespace(), obj.GetName())
			return c.Delete(ctx, obj, opts...)
		},
	})
}

// interfaceRights is what the chart's RBAC gives the interface's service account.
type interfaceRights struct {
	clusterRules []rbacv1.PolicyRule
	namespaced   map[string][]rbacv1.PolicyRule
}

// renderInterfaceRights reads the Roles and ClusterRoles that the chart's RoleBindings and
// ClusterRoleBindings give the interface's service account.
func renderInterfaceRights(t *testing.T) *interfaceRights {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", "../../charts/kubehenge", "--namespace", "kubehenge-system",
		"--values", "../provisioner/testdata/integration-values.yaml", "--set", "interface.offerHARules=true", "--set", "templateRefresh.enabled=true",
		"--show-only", "templates/interface.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	roles := map[string][]rbacv1.PolicyRule{}
	clusterRoles := map[string][]rbacv1.PolicyRule{}
	type binding struct{ namespace, role, kind string }
	var bindings []binding
	subject := func(subjects []rbacv1.Subject) bool {
		return slices.ContainsFunc(subjects, func(s rbacv1.Subject) bool {
			return s.Kind == rbacv1.ServiceAccountKind && s.Name == "kubehenge-interface" && s.Namespace == "kubehenge-system"
		})
	}
	for doc := range strings.SplitSeq(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n---\n") {
		var head metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatal(err)
		}
		switch head.Kind {
		case "Role":
			var r rbacv1.Role
			if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
				t.Fatal(err)
			}
			roles[r.Namespace+"/"+r.Name] = r.Rules
		case "ClusterRole":
			var r rbacv1.ClusterRole
			if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
				t.Fatal(err)
			}
			clusterRoles[r.Name] = r.Rules
		case "RoleBinding":
			var b rbacv1.RoleBinding
			if err := yaml.Unmarshal([]byte(doc), &b); err != nil {
				t.Fatal(err)
			}
			if subject(b.Subjects) {
				bindings = append(bindings, binding{b.Namespace, b.RoleRef.Name, "Role"})
			}
		case "ClusterRoleBinding":
			var b rbacv1.ClusterRoleBinding
			if err := yaml.Unmarshal([]byte(doc), &b); err != nil {
				t.Fatal(err)
			}
			if subject(b.Subjects) {
				bindings = append(bindings, binding{"", b.RoleRef.Name, "ClusterRole"})
			}
		}
	}
	rights := &interfaceRights{namespaced: map[string][]rbacv1.PolicyRule{}}
	for _, b := range bindings {
		switch b.kind {
		case "ClusterRole":
			rights.clusterRules = append(rights.clusterRules, clusterRoles[b.role]...)
		default:
			rights.namespaced[b.namespace] = append(rights.namespaced[b.namespace], roles[b.namespace+"/"+b.role]...)
		}
	}
	if len(rights.clusterRules) == 0 || len(rights.namespaced) == 0 {
		t.Fatalf("the chart gives the interface's service account %d cluster rules and rules in %d namespaces", len(rights.clusterRules), len(rights.namespaced))
	}
	return rights
}

func ruleAllows(r *rbacv1.PolicyRule, a *apiAccess) bool {
	has := func(list []string, s string) bool {
		return slices.Contains(list, s) || slices.Contains(list, rbacv1.ResourceAll)
	}
	return has(r.Verbs, a.verb) && has(r.APIGroups, a.group) && has(r.Resources, a.resource) &&
		(len(r.ResourceNames) == 0 || a.name != "" && slices.Contains(r.ResourceNames, a.name))
}

func (r *interfaceRights) allow(a *apiAccess) bool {
	rules := slices.Clone(r.clusterRules)
	if a.namespace != "" {
		rules = append(rules, r.namespaced[a.namespace]...)
	}
	return slices.ContainsFunc(rules, func(p rbacv1.PolicyRule) bool { return ruleAllows(&p, a) })
}

// Every request the interface's pages make with the interface's own identity must be one the
// chart's Roles and ClusterRoles allow. The harness's fake client answers whatever it is asked,
// so a missing rule shows only here, as a "forbidden" on a real appliance.
func TestInterfaceRequestsAreAllowedByTheChartsRBAC(t *testing.T) {
	t.Parallel()
	rights := renderInterfaceRights(t)
	one := cluster("clusters", "one")
	// The chart makes the pins' ConfigMap, so the interface never creates it.
	h := newHarness(t, one, &corev1.ConfigMap{Namespace: "kubehenge-system", Name: provisioner.ReleasePinsName})
	h.server.Namespace = "clusters"
	srv := releaseServer(t)
	h.server.PinChecks = &provisioner.ReleaseChecksums{HTTP: srv.Client(), GitHub: srv.URL, Raw: srv.URL}
	log := &accessLog{seen: map[apiAccess]bool{}}
	kube, ok := h.kube.(client.WithWatch)
	if !ok {
		t.Fatal("the harness's client cannot watch")
	}
	h.server.setKube(log.watching(kube))

	a := h.browser()
	a.signIn("admin@pve")
	pinned := strings.ReplaceAll(pinnedK3s, "+", "%2B")
	requests := []struct{ method, path, body string }{
		{"GET", "/api/clusters", ""},
		{"GET", "/api/classes", ""},
		{"GET", "/api/fleet/health", ""},
		{"GET", "/api/clusters/clusters/one", ""},
		{"GET", "/api/clusters/clusters/one/spec", ""},
		{"GET", "/api/clusters/clusters/one/health", ""},
		{"GET", "/api/clusters/clusters/one/activity", ""},
		{"GET", "/api/clusters/clusters/one/events", ""},
		{"GET", "/api/clusters/clusters/one/kubeconfig", ""},
		{"GET", "/api/clusters/clusters/one/support-bundle", ""},
		{"GET", "/api/proxmox/ha-rules", ""},
		{"GET", "/api/setup/health", ""},
		{"GET", "/api/appliance/reboot-ready", ""},
		{"GET", "/api/template-builds", ""},
		{"GET", "/api/release-pins", ""},
		{"POST", "/api/release-pins/check", `{"version":"` + pinnedK3s + `"}`},
	}
	for _, r := range requests {
		a.do(r.method, r.path, r.body)
	}
	// A pin is added and removed with what the check showed.
	var shown releasePinView
	if rec := a.do("POST", "/api/release-pins/check", `{"version":"`+pinnedK3s+`"}`); rec.Code == http.StatusOK {
		if err := yaml.Unmarshal(rec.Body.Bytes(), &shown); err != nil {
			t.Fatal(err)
		}
	}
	expect(t, a.do("POST", "/api/release-pins", fmt.Sprintf(`{"version":%q,"file":%q,"script":%q}`, pinnedK3s, shown.File, shown.Script)), http.StatusCreated)
	expect(t, a.do("DELETE", "/api/release-pins/"+pinned, ""), http.StatusNoContent)

	// What the requests above must have asked, so that a page whose request stops reaching the
	// management cluster fails here rather than going unchecked.
	for _, want := range []apiAccess{
		{"get", "", "configmaps", "kubehenge-system", provisioner.ReleasePinsName},
		{"update", "", "configmaps", "kubehenge-system", provisioner.ReleasePinsName},
		{"list", "cluster.x-k8s.io", "clusters", "", ""},
		{"get", "", "configmaps", "kubehenge-system", provisioner.EtcdHealthName},
		{"get", "", "configmaps", "kubehenge-system", provisioner.UnownedVMsName},
		{"list", "", "pods", "kube-system", ""},
		{"list", "", "configmaps", "clusters", ""},
		{"get", "", "configmaps", "clusters", provisioner.SpecSecretsConfigMap("one")},
	} {
		if !log.seen[want] {
			t.Errorf("no request made: %s", want)
		}
	}
	var denied []string
	for a := range log.seen {
		if !rights.allow(&a) {
			denied = append(denied, a.String())
		}
	}
	slices.Sort(denied)
	for _, d := range denied {
		t.Errorf("the chart's RBAC does not allow the interface to %s", d)
	}
}
