package provisioner

import (
	"cmp"
	"fmt"
	"maps"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"
)

// GitOps is a cluster's gitops variable: Argo CD or Flux, syncing the cluster from one path of a
// Git repository.
type GitOps struct {
	Enabled    bool          `json:"enabled"`
	Engine     string        `json:"engine,omitempty"`
	Repository GitRepository `json:"repository"`
	// Auth is how the engine reads the repository: none for a public one, a deploy key Kubehenge's
	// controller makes, a token stored through the interface, or a Secret of the cluster's owner.
	Auth           string `json:"auth,omitempty"`
	ExistingSecret string `json:"existingSecret,omitempty"`
	// KnownHosts are the repository host's SSH keys; unset, the controller reads them from the host
	// once and keeps them.
	KnownHosts string `json:"knownHosts,omitempty"`
	// Hostname serves the engine's web interface through the cluster's ingress controller.
	Hostname  string `json:"hostname,omitempty"`
	TLSSecret string `json:"tlsSecret,omitempty"`
	// Admins are the identity provider's groups Argo CD makes administrators.
	Admins []string     `json:"admins,omitempty"`
	SignIn GitOpsSignIn `json:"signIn"`
	// UI switches Flux's web interface on, which signs in with a client secret.
	UI bool `json:"ui,omitempty"`
}

// GitRepository is what a cluster syncs from.
type GitRepository struct {
	URL      string `json:"url"`
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path,omitempty"`
}

// GitOpsSignIn is how the engine's web interface signs people in through the cluster's first
// identity provider.
type GitOpsSignIn struct {
	ClientID string `json:"clientID,omitempty"`
	// ClientSecret is none for a public client with PKCE, or where the client's secret comes from:
	// a credential stored through the interface, or a Secret of the cluster's owner.
	ClientSecret   string `json:"clientSecret,omitempty"`
	ExistingSecret string `json:"existingSecret,omitempty"`
}

const (
	gitopsVariable = "gitops"
	// The engines a cluster chooses from, by their names in its gitops variable, which name their
	// memory figures too.
	argoCD = "argocd"
	flux   = "flux"
	// argoCDNamespace and fluxNamespace are where each engine runs, and its release's name.
	argoCDNamespace = "argocd"
	fluxNamespace   = "flux-system"
	fluxOperator    = "flux-operator"

	// The ways an engine reads the repository, by their names in the variable.
	gitAuthNone     = "none"
	gitAuthKey      = "deployKey"
	gitAuthToken    = "token"
	gitAuthExisting = "existingSecret"
	// The sources of a sign-in client's secret.
	clientSecretStored   = "stored"
	clientSecretExisting = "existingSecret"

	// The Secrets the controller writes into the workload cluster: the repository's credentials as
	// each engine reads them, the sign-in client's secret for Argo CD, and the configuration of
	// Flux's web interface, which holds its client secret.
	argoCDRepositorySecret = "kubehenge-repository"  //nolint:gosec // a Secret's name, not a credential
	fluxPullSecret         = "kubehenge-gitops"      //nolint:gosec // a Secret's name, not a credential
	argoCDClientSecret     = "kubehenge-oidc-client" //nolint:gosec // a Secret's name, not a credential
	fluxWebSecret          = "kubehenge-flux-web"    //nolint:gosec // a Secret's name, not a credential
	clientSecretKey        = "clientSecret"

	// argoCDLocalURL is where Argo CD's interface is reached without a host name, through
	// kubectl -n argocd port-forward svc/argocd-server 8080:80.
	argoCDLocalURL = "http://localhost:8080"
	// bootstrapApplication is the Argo CD Application that syncs the repository.
	bootstrapApplication = "kubehenge"
	// fluxInstanceName is the only name the Flux Operator takes for its FluxInstance.
	fluxInstanceName = "flux"
	argoCDGroup      = "argoproj.io"
	// fluxSolverPolicy lets cert-manager's HTTP-01 solvers in Flux's namespace be reached.
	fluxSolverPolicy = "kubehenge-acme-http01-solver"
	// fluxRequestsPatch has every Flux controller request 10m of CPU, as Headlamp and Traefik do, and
	// the Flux Operator itself (fluxOperatorValues). The size profile requests 100m each, which with
	// the operator's is half a vCPU, more than a 2-vCPU RKE2 worker has left beside RKE2's own pods
	// and the other add-ons, so one of Flux's pods would stay Pending. Flux mostly waits on its
	// next sync, and the profile's limits still let it use more when it works.
	fluxRequestsPatch = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: all
spec:
  template:
    spec:
      containers:
        - name: manager
          resources:
            requests:
              cpu: 10m
`
)

var (
	applicationGVK    = schema.GroupVersionKind{Group: argoCDGroup, Version: v1alpha1, Kind: "Application"}
	applicationSetGVK = schema.GroupVersionKind{Group: argoCDGroup, Version: v1alpha1, Kind: "ApplicationSet"}
	appProjectGVK     = schema.GroupVersionKind{Group: argoCDGroup, Version: v1alpha1, Kind: "AppProject"}
	fluxInstanceGVK   = schema.GroupVersionKind{Group: "fluxcd.controlplane.io", Version: "v1", Kind: "FluxInstance"}
)

// engine is the engine chosen, Argo CD unless it is Flux.
func (g *GitOps) engine() string { return cmp.Or(g.Engine, argoCD) }

// on reports whether the cluster runs the engine named.
func (g *GitOps) on(engine string) bool { return g.Enabled && g.engine() == engine }

func (g *GitOps) auth() string { return cmp.Or(g.Auth, gitAuthNone) }

func (g *GitOps) clientSecret() string { return cmp.Or(g.SignIn.ClientSecret, gitAuthNone) }

// sshURL reports whether the repository is reached over SSH, which needs its host's keys.
func (g *GitOps) sshURL() bool { return strings.HasPrefix(g.Repository.URL, "ssh://") }

// namespace is the chosen engine's.
func (g *GitOps) namespace() string {
	if g.engine() == flux {
		return fluxNamespace
	}
	return argoCDNamespace
}

// gitOpsProxy names the proxy of an engine's chart.
func gitOpsProxy(cluster *clusterv1.Cluster, engine string) string {
	if engine == flux {
		return cluster.Name + "-" + fluxOperator
	}
	return cluster.Name + "-" + argoCD
}

// sshAddress is the host and port an ssh:// URL reaches, port 22 unless it names another.
func sshAddress(repository string) (string, error) {
	u, err := url.Parse(repository)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" {
		return "", fmt.Errorf("%q is no ssh:// URL with a host", repository)
	}
	return u.Hostname() + ":" + cmp.Or(u.Port(), "22"), nil
}

// gitRef is the revision as Flux takes it, a full ref: a branch unless it names a ref itself.
func gitRef(revision string) string {
	revision = cmp.Or(revision, "main")
	if strings.HasPrefix(revision, "refs/") {
		return revision
	}
	return "refs/heads/" + revision
}

// targetRevision is the revision as Argo CD takes it, a branch or tag by its short name.
func targetRevision(revision string) string {
	revision = cmp.Or(revision, "main")
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if short, ok := strings.CutPrefix(revision, prefix); ok {
			return short
		}
	}
	return revision
}

// gitPath is the repository's directory the cluster syncs, its root unless one is named.
func gitPath(path string) string { return cmp.Or(strings.Trim(path, "/"), ".") }

// oidcProvider is one entry of a cluster's oidc variable, with the defaults its schema gives.
type oidcProvider struct {
	IssuerURL            string   `json:"issuerURL"`
	ClientID             string   `json:"clientID"`
	UsernameClaim        string   `json:"usernameClaim,omitempty"`
	UsernamePrefix       *string  `json:"usernamePrefix,omitempty"`
	GroupsClaim          string   `json:"groupsClaim,omitempty"`
	GroupsPrefix         *string  `json:"groupsPrefix,omitempty"`
	Scopes               []string `json:"scopes,omitempty"`
	CertificateAuthority string   `json:"certificateAuthority,omitempty"`
}

func (p *oidcProvider) usernameClaim() string { return cmp.Or(p.UsernameClaim, "email") }
func (p *oidcProvider) groupsClaim() string   { return cmp.Or(p.GroupsClaim, "groups") }

// prefix is a provider's user name or groups prefix, oidc: unless set, empty included.
func prefix(p *string) string {
	if p == nil {
		return "oidc:"
	}
	return *p
}

// signInProvider is the cluster's first identity provider, which the engines' interfaces sign in
// with, or nil.
func signInProvider(cluster *clusterv1.Cluster) (*oidcProvider, error) {
	providers, _, err := optionalVariable[[]oidcProvider](cluster, "oidc")
	if err != nil || len(providers) == 0 {
		return nil, err
	}
	return &providers[0], nil
}

// gitOpsState is what the controller found for a cluster's GitOps before its charts' values are
// written: the cluster's GitOps keys, the repository host's SSH keys, the identity provider, and
// whether there is a secret of the sign-in client to use.
type gitOpsState struct {
	keys         map[string][]byte
	knownHosts   string
	provider     *oidcProvider
	clientSecret bool
}

// fluxWebOn reports whether Flux's web interface is served: asked for, with a host name, an
// identity provider and a client secret to sign in with.
func (g *GitOps) fluxWebOn(state *gitOpsState) bool {
	return g.on(flux) && g.UI && g.Hostname != "" && state.provider != nil && state.clientSecret
}

// gitOpsCharts are the chart of the cluster's engine: Argo CD, or the Flux Operator, which installs
// Flux itself from its FluxInstance.
func gitOpsCharts(charts *Charts, certs *Certificates, g *GitOps, state *gitOpsState) ([]chartProxy, error) {
	if g.engine() == flux {
		return []chartProxy{{fluxOperator, fluxOperator, fluxNamespace, charts.FluxOperator, fluxOperatorValues(certs, g, state)}}, nil
	}
	values, err := argoCDValues(certs, g, state)
	if err != nil {
		return nil, err
	}
	return []chartProxy{{argoCD, argoCD, argoCDNamespace, charts.ArgoCD, values}}, nil
}

// argoCDValues run Argo CD without its local admin, Dex, notifications or exec: people sign in
// through the cluster's first identity provider, as a public client with PKCE unless the client has
// a secret, and only the admins' groups get any rights. Its server serves plain HTTP behind the
// cluster's ingress controller, which terminates TLS. Its CRDs go with it, once the controller has
// taken every Application's finalizers away. Network policies keep Redis and the repository server
// to Argo CD's own components.
func argoCDValues(certs *Certificates, g *GitOps, state *gitOpsState) (map[string]any, error) {
	address := argoCDLocalURL
	if g.Hostname != "" {
		address = "https://" + g.Hostname
	}
	cm := map[string]any{"admin.enabled": false, "exec.enabled": false, urlKey: address}
	rbac := map[string]any{"policy.default": ""}
	if p := state.provider; p != nil {
		oidc := map[string]any{
			"name":            "Single sign-on",
			"issuer":          p.IssuerURL,
			"clientID":        cmp.Or(g.SignIn.ClientID, p.ClientID),
			"requestedScopes": append([]string{"openid"}, scopesOf(p)...),
		}
		if p.CertificateAuthority != "" {
			oidc["rootCA"] = p.CertificateAuthority
		}
		if g.clientSecret() != gitAuthNone && state.clientSecret {
			oidc["clientSecret"] = "$" + g.clientSecretName() + ":" + clientSecretKey
		} else {
			oidc["enablePKCEAuthentication"] = true
		}
		config, err := yaml.Marshal(oidc)
		if err != nil {
			return nil, fmt.Errorf("writing Argo CD's sign-in configuration: %w", err)
		}
		cm["oidc.config"] = string(config)
		rbac["scopes"] = "[" + p.groupsClaim() + "]"
		var csv strings.Builder
		for _, group := range g.Admins {
			fmt.Fprintf(&csv, "g, %s, role:admin\n", group)
		}
		rbac["policy.csv"] = csv.String()
	}
	values := map[string]any{
		"crds":          map[string]any{"keep": false},
		"global":        map[string]any{"networkPolicy": map[string]any{createKey: true}},
		"dex":           map[string]any{"enabled": false},
		"notifications": map[string]any{"enabled": false},
		"configs": map[string]any{
			"cm":     cm,
			"params": map[string]any{"server.insecure": true},
			"rbac":   rbac,
			"ssh":    map[string]any{"extraHosts": state.knownHosts},
		},
	}
	if g.Hostname != "" {
		tls, annotations := addonIngress(certs, argoCD, g.Hostname, g.TLSSecret)
		ingress := map[string]any{"enabled": true, "hostname": g.Hostname, "extraTls": []any{tls}}
		if annotations != nil {
			ingress["annotations"] = annotations
		}
		values["server"] = map[string]any{ingressKey: ingress}
	}
	return values, nil
}

// clientSecretName is the Secret in the engine's namespace that holds the sign-in client's secret.
func (g *GitOps) clientSecretName() string {
	if g.clientSecret() == clientSecretExisting {
		return g.SignIn.ExistingSecret
	}
	return argoCDClientSecret
}

// scopesOf are the scopes a provider's sign-ins ask for besides openid, its schema's default unless
// it sets its own.
func scopesOf(p *oidcProvider) []string {
	if p.Scopes == nil {
		return []string{"email", "profile"}
	}
	return p.Scopes
}

// fluxOperatorValues run the Flux Operator, which installs Flux from the manifests inside its own
// image, pinned by digest. Its web interface is off unless the cluster asks for it, as it signs in
// with a client secret; switched on, it is served at the host name with the configuration the
// controller writes, which the operator reads from the API as it changes, and verifies the
// provider's certificate with the provider's CA where one is set, beside the image's own.
func fluxOperatorValues(certs *Certificates, g *GitOps, state *gitOpsState) map[string]any {
	web := map[string]any{"enabled": false}
	values := map[string]any{
		installCRDsKey: true, "web": web,
		// The chart's limits and memory request stay (fluxRequestsPatch).
		"resources": map[string]any{requestsKey: map[string]any{cpuKey: cpu10m}},
	}
	if !g.fluxWebOn(state) {
		return values
	}
	web["enabled"] = true
	web["configSecretName"] = fluxWebSecret
	tls, annotations := addonIngress(certs, fluxOperator, g.Hostname, g.TLSSecret)
	ingress := map[string]any{
		"enabled": true,
		"hosts":   []any{map[string]any{"host": g.Hostname, "paths": []any{map[string]any{"path": "/", "pathType": "Prefix"}}}},
		"tls":     []any{tls},
	}
	if annotations != nil {
		ingress["annotations"] = annotations
	}
	web[ingressKey] = ingress
	if state.provider != nil && state.provider.CertificateAuthority != "" {
		const dir = "/etc/kubehenge/oidc-ca"
		// Optional, so that the operator, which installs Flux, never waits for the web interface's
		// configuration to start.
		values["extraVolumes"] = []any{map[string]any{
			"name": "oidc-ca",
			"secret": map[string]any{
				"secretName": fluxWebSecret, "optional": true,
				"items": []any{map[string]any{"key": proxmoxCAKey, "path": proxmoxCAKey}},
			},
		}}
		values["extraVolumeMounts"] = []any{map[string]any{"name": "oidc-ca", "mountPath": dir, "readOnly": true}}
		values["extraEnvs"] = []any{map[string]any{"name": "SSL_CERT_DIR", valueKey: dir}}
	}
	return values
}

// fluxWebConfig is the configuration of Flux's web interface: signed in through the identity
// provider, and acting in the cluster as the user the API server would take the provider's ID token
// for, with the same claims and prefixes, so that the cluster's RBAC bindings apply to it alike.
func fluxWebConfig(g *GitOps, p *oidcProvider, secret string) ([]byte, error) {
	user := fmt.Sprintf("has(claims.%[1]s) ? %[2]q + claims[%[1]q] : ''", p.usernameClaim(), prefix(p.UsernamePrefix))
	groups := fmt.Sprintf("has(claims.%[1]s) ? claims[%[1]q].map(g, %[2]q + g) : []", p.groupsClaim(), prefix(p.GroupsPrefix))
	config, err := yaml.Marshal(map[string]any{
		apiVersionKey: "web.fluxcd.controlplane.io/v1",
		"kind":        "Config",
		"spec": map[string]any{
			"baseURL": "https://" + g.Hostname,
			"authentication": map[string]any{
				"type": "OAuth2",
				"oauth2": map[string]any{
					"provider":      "OIDC",
					"issuerURL":     p.IssuerURL,
					"clientID":      cmp.Or(g.SignIn.ClientID, p.ClientID),
					clientSecretKey: secret,
					"scopes":        append([]string{"openid"}, scopesOf(p)...),
					"impersonation": map[string]any{"username": user, "groups": groups},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("writing Flux's web configuration: %w", err)
	}
	return config, nil
}

// bootstrapObject is what has the engine sync the repository: an Argo CD Application without the
// resources finalizer, so that removing it leaves what it deployed running, or the FluxInstance,
// from which the Flux Operator installs the pinned Flux and syncs the repository with it.
func bootstrapObject(g *GitOps, fluxVersion string) *unstructured.Unstructured {
	if g.engine() == flux {
		sync := map[string]any{
			"kind": "GitRepository",
			urlKey: g.Repository.URL,
			"ref":  gitRef(g.Repository.Revision),
			"path": gitPath(g.Repository.Path),
		}
		if name := g.pullSecret(); name != "" {
			sync["pullSecret"] = name
		}
		return &unstructured.Unstructured{Object: map[string]any{
			apiVersionKey: fluxInstanceGVK.GroupVersion().String(),
			"kind":        fluxInstanceGVK.Kind,
			"metadata":    map[string]any{"name": fluxInstanceName, "namespace": fluxNamespace},
			"spec": map[string]any{
				// No artefact: the operator installs the manifests inside its own image, whose digest
				// pins them and every Flux image they name, rather than polling a moving tag.
				"distribution": map[string]any{"version": fluxVersion, "registry": "ghcr.io/fluxcd"},
				"components":   []any{"source-controller", "kustomize-controller", "helm-controller", "notification-controller"},
				"cluster":      map[string]any{"type": "kubernetes", "size": "small", "networkPolicy": true, "multitenant": false, "domain": "cluster.local"},
				"kustomize":    map[string]any{"patches": []any{map[string]any{"target": map[string]any{"kind": "Deployment"}, "patch": fluxRequestsPatch}}},
				"sync":         sync,
			},
		}}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		apiVersionKey: applicationGVK.GroupVersion().String(),
		"kind":        applicationGVK.Kind,
		"metadata":    map[string]any{"name": bootstrapApplication, "namespace": argoCDNamespace},
		"spec": map[string]any{
			"project": "default",
			"source": map[string]any{
				"repoURL":        g.Repository.URL,
				"targetRevision": targetRevision(g.Repository.Revision),
				"path":           gitPath(g.Repository.Path),
			},
			"destination": map[string]any{"server": "https://kubernetes.default.svc"},
			"syncPolicy": map[string]any{
				"automated": map[string]any{"prune": true, "selfHeal": true},
				"retry":     map[string]any{"limit": int64(-1)},
			},
		},
	}}
}

// solverPolicy admits cert-manager's HTTP-01 solvers in Flux's namespace on their port from
// anywhere. Flux's own network policies take connections there only from the namespace itself, so
// that the solver answering for the web interface's certificate would never hear the ingress
// controller, through which the ACME server and cert-manager's own check reach it.
func solverPolicy() *unstructured.Unstructured {
	const solverPort = 8089
	return &unstructured.Unstructured{Object: map[string]any{
		apiVersionKey: "networking.k8s.io/v1",
		"kind":        "NetworkPolicy",
		"metadata":    map[string]any{"name": fluxSolverPolicy, "namespace": fluxNamespace},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{"acme.cert-manager.io/http01-solver": "true"}},
			"policyTypes": []any{"Ingress"},
			"ingress":     []any{map[string]any{"ports": []any{map[string]any{"protocol": "TCP", "port": int64(solverPort)}}}},
		},
	}}
}

// pullSecret is the Secret Flux reads the repository with, or "" for a public one.
func (g *GitOps) pullSecret() string {
	switch g.auth() {
	case gitAuthKey, gitAuthToken:
		return fluxPullSecret
	case gitAuthExisting:
		return g.ExistingSecret
	}
	return ""
}

// repositorySecret is the repository's credentials as the engine reads them: Flux's pull Secret
// with the deploy key and the host's keys, or a token's user name and password; Argo CD's
// repository Secret, which it matches to the Application by the URL.
func repositorySecret(g *GitOps, creds map[string][]byte) *corev1.Secret {
	if g.engine() == flux {
		return opaqueSecret(fluxNamespace, fluxPullSecret, nil, maps.Clone(creds))
	}
	data := map[string][]byte{"type": []byte("git"), urlKey: []byte(g.Repository.URL)}
	if key, ok := creds[identityKey]; ok {
		data["sshPrivateKey"] = key
	}
	for _, k := range []string{usernameKey, passwordKey} {
		if v, ok := creds[k]; ok {
			data[k] = v
		}
	}
	return opaqueSecret(argoCDNamespace, argoCDRepositorySecret, map[string]string{"argocd.argoproj.io/secret-type": "repository"}, data)
}

// argoCDClientSecretObject holds the sign-in client's secret where Argo CD's configuration names it;
// Argo CD reads only Secrets labelled as its own.
func argoCDClientSecretObject(clientSecret []byte) *corev1.Secret {
	return opaqueSecret(argoCDNamespace, argoCDClientSecret, map[string]string{"app.kubernetes.io/part-of": argoCD},
		map[string][]byte{clientSecretKey: clientSecret})
}

// fluxWebSecretObject holds the configuration of Flux's web interface and the provider's CA.
func fluxWebSecretObject(config []byte, ca string) *corev1.Secret {
	data := map[string][]byte{"config.yaml": config}
	if ca != "" {
		data[proxmoxCAKey] = []byte(ca)
	}
	return opaqueSecret(fluxNamespace, fluxWebSecret, nil, data)
}

func opaqueSecret(namespace, name string, labels map[string]string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		APIVersion: "v1", Kind: secretKind,
		Namespace: namespace, Name: name, Labels: labels,
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}
