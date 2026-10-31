package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const testOIDC = `[{"issuerURL":"https://id.example.org","clientID":"kubernetes","groupsClaim":"roles","certificateAuthority":"CA PEM"}]`

func TestArgoCDSignsInThroughTheClustersProviderOnly(t *testing.T) {
	t.Parallel()
	provider := &oidcProvider{IssuerURL: "https://id.example.org", ClientID: "kubernetes", GroupsClaim: "roles", CertificateAuthority: "CA PEM"}
	g := &GitOps{Enabled: true, Repository: GitRepository{URL: "https://git.example.org/a/b.git"}, Admins: []string{"platform", "{{ops}}"}, Hostname: "argo.example.org"}
	specs, values := proxySpecs(t, templates.RKE2, CNI{}, &Addons{GitOps: *g, gitOpsFound: gitOpsState{provider: provider, knownHosts: "git.example.org ssh-ed25519 AAAA\n"}})
	spec, v := specs["web-argocd"], values["web-argocd"]
	if spec == nil || spec["chartName"] != "argo-cd" || spec["namespace"] != argoCDNamespace || spec["releaseName"] != argoCD {
		t.Fatalf("Argo CD proxy %v", spec)
	}
	if !strings.Contains(lookup[string](t, spec, "valuesTemplate"), `{{"{{"}}ops}}`) {
		t.Errorf("a group's braces reach the provider's template: %s", spec["valuesTemplate"])
	}
	cm := lookup[map[string]any](t, v, "configs", "cm")
	if lookup[bool](t, cm, "admin.enabled") || lookup[bool](t, cm, "exec.enabled") || cm[urlKey] != "https://argo.example.org" {
		t.Errorf("configuration %v", cm)
	}
	oidc := map[string]any{}
	if err := yaml.Unmarshal([]byte(lookup[string](t, cm, "oidc.config")), &oidc); err != nil {
		t.Fatal(err)
	}
	if oidc["issuer"] != provider.IssuerURL || oidc["clientID"] != "kubernetes" || !lookup[bool](t, oidc, "enablePKCEAuthentication") ||
		oidc["clientSecret"] != nil || oidc["rootCA"] != "CA PEM" {
		t.Errorf("sign-in %v", oidc)
	}
	rbac := lookup[map[string]any](t, v, "configs", "rbac")
	if rbac["policy.default"] != "" || rbac["scopes"] != "[roles]" || rbac["policy.csv"] != "g, platform, role:admin\ng, {{\"{{\"}}ops}}, role:admin\n" {
		t.Errorf("RBAC %v", rbac)
	}
	if lookup[string](t, v, "configs", "ssh", "extraHosts") != "git.example.org ssh-ed25519 AAAA\n" {
		t.Error("the host's keys are not Argo CD's")
	}
	for _, off := range []string{"dex", "notifications"} {
		if lookup[bool](t, v, off, "enabled") {
			t.Errorf("%s on", off)
		}
	}
	if !lookup[bool](t, v, "global", "networkPolicy", "create") || lookup[bool](t, v, "crds", "keep") {
		t.Error("network policies off, or CRDs kept")
	}
	if lookup[string](t, v, "server", "ingress", "hostname") != "argo.example.org" {
		t.Error("no ingress at the host name")
	}
	if lookup[string](t, v, "global", "image", "tag") != lookup[string](t, testCharts.ArgoCD.Values, "global", "image", "tag") {
		t.Error("the pinned image is lost")
	}

	g.SignIn.ClientSecret = clientSecretStored
	_, values = proxySpecs(t, templates.RKE2, CNI{}, &Addons{GitOps: *g, gitOpsFound: gitOpsState{provider: provider, clientSecret: true}})
	oidc = map[string]any{}
	if err := yaml.Unmarshal([]byte(lookup[string](t, values["web-argocd"], "configs", "cm", "oidc.config")), &oidc); err != nil {
		t.Fatal(err)
	}
	if oidc["clientSecret"] != "$kubehenge-oidc-client:clientSecret" || oidc["enablePKCEAuthentication"] != nil {
		t.Errorf("sign-in with a client secret %v", oidc)
	}

	// Without a provider nobody signs in, and nobody has rights.
	_, values = proxySpecs(t, templates.RKE2, CNI{}, &Addons{GitOps: GitOps{Enabled: true}})
	if cm := lookup[map[string]any](t, values["web-argocd"], "configs", "cm"); cm["oidc.config"] != nil || cm["url"] != argoCDLocalURL {
		t.Errorf("without a provider %v", cm)
	}
}

func TestFluxWebInterfaceOnlyWhenAskedForAndSignedIn(t *testing.T) {
	t.Parallel()
	provider := &oidcProvider{IssuerURL: "https://id.example.org", ClientID: "kubernetes", CertificateAuthority: "CA PEM"}
	g := GitOps{Enabled: true, Engine: flux, Hostname: "flux.example.org"}
	for _, tc := range []struct {
		name  string
		ui    bool
		state gitOpsState
		want  bool
	}{
		{"not asked for", false, gitOpsState{provider: provider, clientSecret: true}, false},
		{"no provider", true, gitOpsState{clientSecret: true}, false},
		{"no client secret", true, gitOpsState{provider: provider}, false},
		{"asked for and signed in", true, gitOpsState{provider: provider, clientSecret: true}, true},
	} {
		g.UI = tc.ui
		specs, values := proxySpecs(t, templates.RKE2, CNI{}, &Addons{GitOps: g, gitOpsFound: tc.state})
		if spec := specs["web-flux-operator"]; spec == nil || spec["namespace"] != fluxNamespace || spec["chartName"] != fluxOperator {
			t.Fatalf("%s: Flux Operator proxy %v", tc.name, spec)
		}
		v := values["web-flux-operator"]
		if got := lookup[bool](t, v, "web", "enabled"); got != tc.want {
			t.Errorf("%s: web interface %v", tc.name, got)
		}
		if !lookup[bool](t, v, "installCRDs") {
			t.Errorf("%s: no CRDs", tc.name)
		}
		if tc.want && (lookup[string](t, v, "web", "configSecretName") != fluxWebSecret ||
			lookup[string](t, v, "web", "ingress", "hosts", 0, "host") != "flux.example.org" ||
			lookup[string](t, v, "extraEnvs", 0, "value") != "/etc/kubehenge/oidc-ca" ||
			!lookup[bool](t, v, "extraVolumes", 0, "secret", "optional")) {
			t.Errorf("%s: web interface %v", tc.name, v)
		}
	}
}

func TestFluxWebActsAsTheUserTheAPIServerKnows(t *testing.T) {
	t.Parallel()
	none := ""
	for _, tc := range []struct {
		provider     oidcProvider
		user, groups string
	}{
		{
			oidcProvider{IssuerURL: "https://id.example.org", ClientID: "k"},
			`has(claims.email) ? "oidc:" + claims["email"] : ''`, `has(claims.groups) ? claims["groups"].map(g, "oidc:" + g) : []`,
		},
		{
			oidcProvider{IssuerURL: "https://id.example.org", ClientID: "k", UsernameClaim: "sub", UsernamePrefix: &none, GroupsClaim: "roles", GroupsPrefix: &none},
			`has(claims.sub) ? "" + claims["sub"] : ''`, `has(claims.roles) ? claims["roles"].map(g, "" + g) : []`,
		},
	} {
		config, err := fluxWebConfig(&GitOps{Hostname: "flux.example.org"}, &tc.provider, "s3cret")
		if err != nil {
			t.Fatal(err)
		}
		doc := map[string]any{}
		if err := yaml.Unmarshal(config, &doc); err != nil {
			t.Fatal(err)
		}
		oauth := lookup[map[string]any](t, doc, "spec", "authentication", "oauth2")
		if doc["kind"] != "Config" || lookup[string](t, doc, "spec", "baseURL") != "https://flux.example.org" ||
			oauth["clientSecret"] != "s3cret" || oauth["issuerURL"] != "https://id.example.org" || oauth["provider"] != "OIDC" {
			t.Errorf("configuration %v", doc)
		}
		if got := lookup[string](t, oauth, "impersonation", "username"); got != tc.user {
			t.Errorf("user %s, want %s", got, tc.user)
		}
		if got := lookup[string](t, oauth, "impersonation", "groups"); got != tc.groups {
			t.Errorf("groups %s, want %s", got, tc.groups)
		}
	}
}

func TestBootstrapObjects(t *testing.T) {
	t.Parallel()
	g := &GitOps{Enabled: true, Engine: flux, Auth: gitAuthKey, Repository: GitRepository{URL: "ssh://git@git.example.org/a/b.git", Revision: "release", Path: "/clusters/web/"}}
	fi := bootstrapObject(g, "2.9.6")
	if fi.GroupVersionKind() != fluxInstanceGVK || fi.GetName() != "flux" || fi.GetNamespace() != fluxNamespace {
		t.Fatalf("FluxInstance %v", fi.Object)
	}
	syncSpec := lookup[map[string]any](t, fi.Object, "spec", "sync")
	if syncSpec["ref"] != "refs/heads/release" || syncSpec["path"] != "clusters/web" || syncSpec["pullSecret"] != fluxPullSecret || syncSpec[urlKey] != g.Repository.URL {
		t.Errorf("sync %v", syncSpec)
	}
	distribution := lookup[map[string]any](t, fi.Object, "spec", "distribution")
	if _, ok := distribution["artifact"]; ok || distribution["version"] != "2.9.6" { //nolint:misspell // the FluxInstance's field
		t.Errorf("distribution %v", distribution)
	}
	if lookup[string](t, fi.Object, "spec", "kustomize", "patches", 0, "target", "kind") != "Deployment" {
		t.Errorf("Flux's requests patch targets %v", lookup[any](t, fi.Object, "spec", "kustomize", "patches", 0, "target"))
	}
	var patch map[string]any
	if err := yaml.Unmarshal([]byte(lookup[string](t, fi.Object, "spec", "kustomize", "patches", 0, "patch")), &patch); err != nil {
		t.Fatal(err)
	}
	container := lookup[map[string]any](t, patch, "spec", "template", "spec", "containers", 0)
	if container["name"] != "manager" || lookup[string](t, container, "resources", "requests", "cpu") != cpu10m {
		t.Errorf("Flux's requests patch %v", patch)
	}
	if cpu := lookup[string](t, fluxOperatorValues(&Certificates{}, g, &gitOpsState{}), "resources", requestsKey, cpuKey); cpu != cpu10m {
		t.Errorf("the Flux Operator requests %s of CPU", cpu)
	}

	g.Engine, g.Auth, g.Repository.Revision, g.Repository.Path = argoCD, gitAuthNone, "refs/tags/v1", ""
	app := bootstrapObject(g, "2.9.6")
	if app.GroupVersionKind() != applicationGVK || len(app.GetFinalizers()) != 0 {
		t.Fatalf("Application %v", app.Object)
	}
	source := lookup[map[string]any](t, app.Object, "spec", "source")
	if source["targetRevision"] != "v1" || source["path"] != "." {
		t.Errorf("source %v", source)
	}
	if !lookup[bool](t, app.Object, "spec", "syncPolicy", "automated", "prune") {
		t.Error("removed manifests stay")
	}
	g.Engine, g.Auth = flux, gitAuthNone
	if _, ok := lookup[map[string]any](t, bootstrapObject(g, "2.9.6").Object, "spec", "sync")["pullSecret"]; ok {
		t.Error("a public repository read with a Secret")
	}
	g.Auth, g.ExistingSecret = gitAuthExisting, "mine"
	if lookup[string](t, bootstrapObject(g, "2.9.6").Object, "spec", "sync", "pullSecret") != "mine" {
		t.Error("the owner's Secret is not read")
	}
}

func TestDeployKeyIsAnOpenSSHKeyPair(t *testing.T) {
	t.Parallel()
	private, public, err := newDeployKey("kubehenge team-a/web")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	key, comment, _, _, err := ssh.ParseAuthorizedKey(public)
	if err != nil {
		t.Fatal(err)
	}
	if key.Type() != ssh.KeyAlgoED25519 || !bytes.Equal(key.Marshal(), signer.PublicKey().Marshal()) || comment != "kubehenge team-a/web" {
		t.Errorf("public half %q of a %s key", public, signer.PublicKey().Type())
	}
}

func TestKnownHostsHaveToNameTheRepositorysHost(t *testing.T) {
	t.Parallel()
	const key = " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"
	const repository, onPort = "ssh://git@git.example.org/a/b.git", "ssh://git@git.example.org:2222/a/b.git"
	for _, tc := range []struct {
		name, knownHosts, url string
		ok                    bool
	}{
		{"plain", "git.example.org" + key, repository, true},
		{"one of several names", "other.example.org,git.example.org,192.0.2.1" + key, repository, true},
		{"port", "[git.example.org]:2222" + key, onPort, true},
		{"port 22 written out", "[git.example.org]:22" + key, repository, false},
		{"another port", "git.example.org" + key, onPort, false},
		{"wildcard", "*.example.org" + key, repository, true},
		{"single-character wildcard", "git.example.or?" + key, repository, true},
		{"wildcard of another domain", "*.example.com" + key, repository, false},
		{"negated", "*.example.org,!git.example.org" + key, repository, false},
		{"negated another", "*.example.org,!www.example.org" + key, repository, true},
		{"hashed", "|1|T0JMWCz6nvC/AFNDT+MLe/Gm8nE=|p46nBNVmER9Hjw+IZX/Ob8vUpKs=" + key, repository, true},
		{"hashed with its port", "|1|COROazIpRGWVbO0xWrXifrPaZ7w=|CbdyrYIXekfLjUyyIr82IOuIrdI=" + key, onPort, true},
		{"hashed for another port", "|1|T0JMWCz6nvC/AFNDT+MLe/Gm8nE=|p46nBNVmER9Hjw+IZX/Ob8vUpKs=" + key, onPort, false},
		{"comments and blank lines", "# the forge\n\ngit.example.org" + key, repository, true},
		{"certificate authority", "@cert-authority *.example.org" + key, repository, true},
		{"revoked only", "@revoked git.example.org" + key, repository, false},
		{"another host", "git.example.com" + key, repository, false},
		{"no key", "git.example.org\n", repository, false},
		{"not known_hosts", "SHA256:abc\n", repository, false},
	} {
		err := checkKnownHosts(tc.knownHosts, tc.url)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestValidatorAsksForTheRepositoryHostsKeys(t *testing.T) {
	t.Parallel()
	const otherHost = `"knownHosts":"git.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"`
	for _, tc := range [][2]string{
		{`{"enabled":true,"auth":"deployKey","repository":` + testRepository + `}`, "knownHosts: Required value"},
		{`{"enabled":true,"auth":"existingSecret","existingSecret":"s","repository":` + testRepository + `}`, "knownHosts: Required value"},
		{`{"enabled":true,"engine":"flux","auth":"existingSecret","existingSecret":"s","repository":` + testRepository + `}`, ""},
		{`{"enabled":true,"auth":"deployKey",` + testKnownHosts + `,"repository":` + testRepository + `}`, ""},
		{`{"enabled":true,"auth":"deployKey",` + otherHost + `,"repository":` + testRepository + `}`, "no host key is for git.example.org"},
		{`{"enabled":true,"auth":"token","repository":{"url":"https://git.example.org/a/b.git"}}`, ""},
		{`{"enabled":false,"repository":` + testRepository + `}`, ""},
	} {
		gitops, want := tc[0], tc[1]
		errs := validateGitOps(withAddons(storageCluster(), map[string]string{gitopsVariable: gitops}))
		if got := errs.ToAggregate(); (want == "") != (got == nil) || (got != nil && !strings.Contains(got.Error(), want)) {
			t.Errorf("%s: %v, want %q", gitops, got, want)
		}
	}
}

func TestRevisionsAndPaths(t *testing.T) {
	t.Parallel()
	for in, want := range map[string][2]string{
		"":                   {"refs/heads/main", "main"},
		"release":            {"refs/heads/release", "release"},
		"refs/heads/feature": {"refs/heads/feature", "feature"},
		"refs/tags/v1.2":     {"refs/tags/v1.2", "v1.2"},
	} {
		if gitRef(in) != want[0] || targetRevision(in) != want[1] {
			t.Errorf("%q: %s, %s", in, gitRef(in), targetRevision(in))
		}
	}
	for in, want := range map[string]string{"": ".", ".": ".", "/apps/": "apps", "clusters/web": "clusters/web"} {
		if got := gitPath(in); got != want {
			t.Errorf("path %q: %s", in, got)
		}
	}
	for in, want := range map[string]string{"ssh://git@git.example.org/a.git": "git.example.org:22", "ssh://git@git.example.org:2222/a.git": "git.example.org:2222"} {
		if got, err := sshAddress(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	if _, err := sshAddress("https://git.example.org/a.git"); err == nil {
		t.Error("an https:// URL has an SSH address")
	}
}

// gitOpsWorkload is a workload cluster that keeps what is applied to it and the objects of the
// engines' CRDs, which exist once installed. FluxInstances wait for the operator to finish deleting.
type gitOpsWorkload struct {
	client.WithWatch

	mu        sync.Mutex
	installed bool
	objects   map[string]*unstructured.Unstructured
}

func objectKey(kind, namespace, name string) string { return kind + "/" + namespace + "/" + name }

func newGitOpsWorkload(t *testing.T) *gitOpsWorkload {
	t.Helper()
	w := &gitOpsWorkload{objects: map[string]*unstructured.Unstructured{}}
	gvkOf := func(obj runtime.Object) schema.GroupVersionKind {
		if _, ok := obj.(*corev1.Secret); ok {
			return corev1.SchemeGroupVersion.WithKind(secretKind)
		}
		return obj.GetObjectKind().GroupVersionKind()
	}
	w.WithWatch = interceptor.NewClient(fake.NewClientBuilder().WithScheme(remoteScheme).Build(), interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			u, ok := obj.(interface{ UnstructuredContent() map[string]any })
			if !ok {
				return fmt.Errorf("applied %T", obj)
			}
			o := &unstructured.Unstructured{Object: u.UnstructuredContent()}
			w.mu.Lock()
			defer w.mu.Unlock()
			if !w.installed {
				if o.GetKind() == secretKind {
					return apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, o.GetNamespace())
				}
				return &meta.NoKindMatchError{GroupKind: o.GroupVersionKind().GroupKind()}
			}
			w.objects[objectKey(o.GetKind(), o.GetNamespace(), o.GetName())] = o.DeepCopy()
			return nil
		},
		Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			gvk := gvkOf(obj)
			if !w.installed && gvk.Kind != secretKind {
				return &meta.NoKindMatchError{GroupKind: gvk.GroupKind()}
			}
			o, ok := w.objects[objectKey(gvk.Kind, key.Namespace, key.Name)]
			if !ok {
				return apierrors.NewNotFound(schema.GroupResource{Resource: gvk.Kind}, key.Name)
			}
			return runtime.DefaultUnstructuredConverter.FromUnstructured(o.DeepCopy().Object, obj)
		},
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			ul, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return fmt.Errorf("listed %T", list)
			}
			kind := strings.TrimSuffix(ul.GetKind(), "List")
			if !w.installed {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: ul.GroupVersionKind().Group, Kind: kind}}
			}
			for _, o := range w.objects {
				if o.GetKind() == kind {
					ul.Items = append(ul.Items, *o.DeepCopy())
				}
			}
			return nil
		},
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			o, ok := w.objects[objectKey(gvkOf(obj).Kind, obj.GetNamespace(), obj.GetName())]
			if !ok {
				return apierrors.NewNotFound(schema.GroupResource{}, obj.GetName())
			}
			o.SetFinalizers(obj.GetFinalizers())
			return nil
		},
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			key := objectKey(gvkOf(obj).Kind, obj.GetNamespace(), obj.GetName())
			o, ok := w.objects[key]
			if !ok {
				return apierrors.NewNotFound(schema.GroupResource{}, obj.GetName())
			}
			if len(o.GetFinalizers()) > 0 || o.GetKind() == fluxInstanceGVK.Kind {
				now := metav1.Now()
				o.SetDeletionTimestamp(&now)
				return nil
			}
			delete(w.objects, key)
			return nil
		},
	})
	return w
}

func (w *gitOpsWorkload) install() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.installed = true
}

func (w *gitOpsWorkload) object(kind, namespace, name string) *unstructured.Unstructured {
	w.mu.Lock()
	defer w.mu.Unlock()
	if o, ok := w.objects[objectKey(kind, namespace, name)]; ok {
		return o.DeepCopy()
	}
	return nil
}

func (w *gitOpsWorkload) put(o *unstructured.Unstructured) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.objects[objectKey(o.GetKind(), o.GetNamespace(), o.GetName())] = o
}

// finishDeleting stands for the Flux Operator, which lets its FluxInstance go once Flux is uninstalled.
func (w *gitOpsWorkload) finishDeleting() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, o := range w.objects {
		if o.GetDeletionTimestamp() != nil {
			delete(w.objects, key)
		}
	}
}

func secretData(t *testing.T, o *unstructured.Unstructured) map[string][]byte {
	t.Helper()
	if o == nil {
		t.Fatal("no Secret")
	}
	s := &corev1.Secret{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, s); err != nil {
		t.Fatal(err)
	}
	return s.Data
}

// gitOpsHarness reconciles one cluster against a fake workload cluster.
type gitOpsHarness struct {
	t        *testing.T
	c        *clusterv1.Cluster
	r        *Reconciler
	kube     client.Client
	workload *gitOpsWorkload
}

func newGitOpsHarness(t *testing.T, gitops string) *gitOpsHarness {
	t.Helper()
	c := withAddons(storageCluster(), map[string]string{gitopsVariable: gitops})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	h := &gitOpsHarness{t: t, c: c, r: r, kube: kube, workload: newGitOpsWorkload(t)}
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: h.workload, until: time.Now().Add(time.Hour)}}}
	return h
}

func (h *gitOpsHarness) run() ctrl.Result {
	h.t.Helper()
	result, err := h.r.Reconcile(h.t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.c)})
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

// proxy reports whether the cluster has the HelmChartProxy named.
func (h *gitOpsHarness) proxy(name string) bool {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	return h.kube.Get(h.t.Context(), client.ObjectKey{Namespace: "team-a", Name: name}, p) == nil
}

func (h *gitOpsHarness) setGitOps(value string) {
	h.t.Helper()
	current := &clusterv1.Cluster{}
	if err := h.kube.Get(h.t.Context(), client.ObjectKeyFromObject(h.c), current); err != nil {
		h.t.Fatal(err)
	}
	for i := range current.Spec.Topology.Variables {
		if current.Spec.Topology.Variables[i].Name == gitopsVariable {
			current.Spec.Topology.Variables[i].Value.Raw = []byte(value)
		}
	}
	if err := h.kube.Update(h.t.Context(), current); err != nil {
		h.t.Fatal(err)
	}
}

// keys is the cluster's GitOps Secret beside it.
func (h *gitOpsHarness) keys() *corev1.Secret {
	h.t.Helper()
	keys := &corev1.Secret{}
	if err := h.kube.Get(h.t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web-gitops"}, keys); err != nil {
		h.t.Fatal(err)
	}
	return keys
}

const (
	testRepository = `{"url":"ssh://git@git.example.org/a/b.git","revision":"main","path":"clusters/web"}`
	testKnownHosts = `"knownHosts":"git.example.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"`
)

func TestReconcileBootstrapsFluxThenSwitchesToArgoCD(t *testing.T) {
	t.Parallel()
	h := newGitOpsHarness(t, `{"enabled":true,"engine":"flux","auth":"deployKey",`+testKnownHosts+`,"repository":`+testRepository+`}`)
	keys := checkFluxBootstrap(t, h)
	checkSwitchToArgoCD(t, h, keys)
	checkGitOpsSwitchedOff(t, h)
}

func checkFluxBootstrap(t *testing.T, h *gitOpsHarness) *corev1.Secret {
	t.Helper()
	// Before the operator's CRDs exist, the sync waits.
	if result := h.run(); result.RequeueAfter != waitInterval {
		t.Errorf("does not wait for Flux's CRDs: %v", result)
	}
	if !h.proxy("web-flux-operator") || h.proxy("web-argocd") {
		t.Fatal("the Flux Operator is not the engine installed")
	}
	keys := h.keys()
	if len(keys.Data[identityKey]) == 0 || !strings.HasSuffix(string(keys.Data[identityPubKey]), " kubehenge team-a/web\n") || len(keys.OwnerReferences) != 1 {
		t.Errorf("GitOps keys %v", keys)
	}

	h.workload.install()
	if result := h.run(); result.RequeueAfter != resync {
		t.Errorf("waits with Flux installed: %v", result)
	}
	fi := h.workload.object(fluxInstanceGVK.Kind, fluxNamespace, fluxInstanceName)
	if fi == nil || lookup[string](t, fi.Object, "spec", "sync", "pullSecret") != fluxPullSecret {
		t.Fatalf("FluxInstance %v", fi)
	}
	pull := secretData(t, h.workload.object(secretKind, fluxNamespace, fluxPullSecret))
	if !bytes.Equal(pull[identityKey], keys.Data[identityKey]) || !strings.HasPrefix(string(pull[knownHostsKey]), "git.example.org ssh-ed25519 ") {
		t.Errorf("pull Secret %v", pull)
	}
	policy := h.workload.object("NetworkPolicy", fluxNamespace, fluxSolverPolicy)
	if policy == nil || lookup[string](t, policy.Object, "spec", "podSelector", "matchLabels", "acme.cert-manager.io/http01-solver") != "true" {
		t.Errorf("HTTP-01 solvers in Flux's namespace are not let through: %v", policy)
	}
	return keys
}

// checkSwitchToArgoCD puts Argo CD in Flux's place: it starts syncing once Flux has let go.
func checkSwitchToArgoCD(t *testing.T, h *gitOpsHarness, keys *corev1.Secret) {
	t.Helper()
	h.setGitOps(`{"enabled":true,"engine":"argocd","auth":"deployKey","repository":` + testRepository + `}`)
	if result := h.run(); result.RequeueAfter != waitInterval {
		t.Errorf("does not wait for Flux to go: %v", result)
	}
	if !h.proxy("web-argocd") || !h.proxy("web-flux-operator") {
		t.Error("Flux removed before the operator let go, or Argo CD not installed beside it")
	}
	if h.workload.object(applicationGVK.Kind, argoCDNamespace, bootstrapApplication) != nil {
		t.Error("Argo CD syncs while Flux still does")
	}
	h.workload.finishDeleting()
	h.run()
	if h.proxy("web-flux-operator") {
		t.Error("the Flux Operator stays once Flux is gone")
	}
	if h.workload.object("NetworkPolicy", fluxNamespace, fluxSolverPolicy) != nil {
		t.Error("the solvers' NetworkPolicy outlives Flux")
	}
	if h.workload.object(secretKind, fluxNamespace, fluxPullSecret) != nil {
		t.Error("Flux's pull Secret stays")
	}
	app := h.workload.object(applicationGVK.Kind, argoCDNamespace, bootstrapApplication)
	if app == nil || lookup[string](t, app.Object, "spec", "source", "path") != "clusters/web" {
		t.Fatalf("Application %v", app)
	}
	repoSecret := secretData(t, h.workload.object(secretKind, argoCDNamespace, argoCDRepositorySecret))
	if !bytes.Equal(repoSecret["sshPrivateKey"], keys.Data[identityKey]) || string(repoSecret[urlKey]) != "ssh://git@git.example.org/a/b.git" {
		t.Errorf("repository Secret %v", repoSecret)
	}
	if !bytes.Equal(h.keys().Data[identityKey], repoSecret["sshPrivateKey"]) {
		t.Error("the deploy key changed with the engine")
	}
}

// checkGitOpsSwitchedOff switches GitOps off: Applications go without their finalizers, so what
// they deployed stays.
func checkGitOpsSwitchedOff(t *testing.T, h *gitOpsHarness) {
	t.Helper()
	deployed := &unstructured.Unstructured{}
	deployed.SetGroupVersionKind(applicationGVK)
	deployed.SetNamespace(argoCDNamespace)
	deployed.SetName("apps")
	deployed.SetFinalizers([]string{"resources-finalizer.argocd.argoproj.io"})
	h.workload.put(deployed)
	h.setGitOps(`{"enabled":false,"engine":"argocd","auth":"deployKey","repository":` + testRepository + `}`)
	if result := h.run(); result.RequeueAfter != waitInterval || !h.proxy("web-argocd") {
		t.Errorf("Argo CD removed before its Applications: %v", result)
	}
	if h.workload.object(applicationGVK.Kind, argoCDNamespace, "apps") != nil {
		t.Error("an Application with its finalizer stays")
	}
	h.run()
	if h.proxy("web-argocd") {
		t.Error("Argo CD stays switched off")
	}
	if h.workload.object(secretKind, argoCDNamespace, argoCDRepositorySecret) != nil {
		t.Error("the repository Secret stays")
	}
	h.keys()
}

func TestReconcileWaitsForTheTokenItIsToReadWith(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := withAddons(storageCluster(), map[string]string{
		gitopsVariable: `{"enabled":true,"engine":"flux","auth":"token","repository":{"url":"https://git.example.org/a/b.git"}}`,
	})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	workload := newGitOpsWorkload(t)
	workload.install()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
	if err != nil || result.RequeueAfter != waitInterval {
		t.Fatalf("does not wait for the token: %v %v", result, err)
	}
	if workload.object(fluxInstanceGVK.Kind, fluxNamespace, fluxInstanceName) != nil {
		t.Error("syncs without its token")
	}
	// A Secret of another kind under the credential's name is no credential.
	stray := &corev1.Secret{Namespace: "team-a", Name: CredentialName("web", gitOpsPurpose), Data: map[string][]byte{usernameKey: []byte("u"), passwordKey: []byte("p")}}
	if err := kube.Create(ctx, stray); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	if workload.object(fluxInstanceGVK.Kind, fluxNamespace, fluxInstanceName) != nil {
		t.Error("read a Secret the interface never wrote")
	}
	stray.Type, stray.Labels = CredentialType, map[string]string{CredentialLabel: gitOpsPurpose, CredentialClusterLabel: "web"}
	if err := kube.Delete(ctx, stray); err != nil {
		t.Fatal(err)
	}
	stray.ResourceVersion = ""
	if err := kube.Create(ctx, stray); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	pull := secretData(t, workload.object(secretKind, fluxNamespace, fluxPullSecret))
	if string(pull[usernameKey]) != "u" || string(pull[passwordKey]) != "p" || len(pull) != 2 {
		t.Errorf("pull Secret %v", pull)
	}
}

func TestFluxWebConfigurationIsWrittenWithTheStoredClientSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := withAddons(storageCluster(), map[string]string{
		"oidc": testOIDC,
		gitopsVariable: `{"enabled":true,"engine":"flux","repository":{"url":"https://git.example.org/a/b.git"},` +
			`"hostname":"flux.example.org","ui":true,"signIn":{"clientSecret":"stored"}}`,
	})
	secret := &corev1.Secret{
		Namespace: "team-a", Name: CredentialName("web", clientPurpose), Type: CredentialType,
		Labels: map[string]string{CredentialLabel: clientPurpose, CredentialClusterLabel: "web"},
		Data:   map[string][]byte{clientSecretKey: []byte("s3cret")},
	}
	r, kube := newReconciler(t, &fakeCloud{}, c, secret)
	workload := newGitOpsWorkload(t)
	workload.install()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	web := secretData(t, workload.object(secretKind, fluxNamespace, fluxWebSecret))
	if !strings.Contains(string(web["config.yaml"]), "clientSecret: s3cret") || string(web[proxmoxCAKey]) != "CA PEM" {
		t.Errorf("web configuration %s", web)
	}
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "web-flux-operator"}, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lookup[string](t, p.Object, "spec", "valuesTemplate"), "configSecretName: "+fluxWebSecret) {
		t.Error("the web interface is off")
	}
}

func TestGitOpsWebInterfacesNeedAProvider(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, gitops, oidc string
		refused            bool
	}{
		{"Argo CD without a provider", `{"enabled":true,"repository":{"url":"https://g.example.org/a"}}`, "", false},
		{"Argo CD administrators without a provider", `{"enabled":true,"admins":["ops"],"repository":{"url":"https://g.example.org/a"}}`, "", true},
		{"Flux's interface without a provider", `{"enabled":true,"engine":"flux","ui":true,"repository":{"url":"https://g.example.org/a"}}`, "", true},
		{"Flux's interface with one", `{"enabled":true,"engine":"flux","ui":true,"repository":{"url":"https://g.example.org/a"}}`, testOIDC, false},
		{"switched off", `{"enabled":false,"engine":"flux","ui":true}`, "", false},
	} {
		vars := map[string]string{gitopsVariable: tc.gitops}
		if tc.oidc != "" {
			vars["oidc"] = tc.oidc
		}
		if errs := validateGitOps(withAddons(cluster(), vars)); (len(errs) > 0) != tc.refused {
			t.Errorf("%s: %v", tc.name, errs)
		}
	}
}

func TestGitOpsEnginesCountTheirMemory(t *testing.T) {
	t.Parallel()
	for engine, want := range map[string]string{argoCD: argoCD, flux: flux} {
		on, err := addonsOn(withAddons(cluster(), map[string]string{gitopsVariable: `{"enabled":true,"engine":"` + engine + `"}`}))
		if err != nil || len(on) != 2 || on[1] != want {
			t.Errorf("%s: %v %v", engine, on, err)
		}
	}
}
