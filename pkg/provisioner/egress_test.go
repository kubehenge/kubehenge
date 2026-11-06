package provisioner

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// testAuthority is a certificate authority in PEM, for the settings that take one.
func testAuthority(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Example CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func clusterWithRanges() *clusterv1.Cluster {
	c := cluster()
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.244.0.0/16"}
	c.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"10.96.0.0/16"}
	return c
}

func TestEgressResolvesFromTheClusterThenTheManagementSide(t *testing.T) {
	t.Parallel()
	management := Egress{URL: "http://proxy.example.org:3128", NoProxy: []string{"pve.example.org", ".corp.example"}}
	for name, tc := range map[string]struct {
		egress  Egress
		set     EgressProxy
		url     string
		noProxy string
		skipped bool
	}{
		"nothing anywhere":     {url: ""},
		"the management side":  {egress: management, url: "http://proxy.example.org:3128", noProxy: "10.244.0.0/16,10.96.0.0/16,localhost,127.0.0.1,.svc,.cluster.local,pve.example.org,.corp.example"},
		"its own address":      {egress: management, set: EgressProxy{URL: "http://other.example.org:8080", NoProxy: []string{"s3.internal"}}, url: "http://other.example.org:8080", noProxy: "10.244.0.0/16,10.96.0.0/16,localhost,127.0.0.1,.svc,.cluster.local,s3.internal"},
		"more exceptions":      {egress: management, set: EgressProxy{NoProxy: []string{"s3.internal", ".svc"}}, url: "http://proxy.example.org:3128", noProxy: "10.244.0.0/16,10.96.0.0/16,localhost,127.0.0.1,.svc,.cluster.local,pve.example.org,.corp.example,s3.internal"},
		"going direct":         {egress: management, set: EgressProxy{Disabled: true}, url: ""},
		"credentials in a URL": {egress: Egress{URL: "http://user:hunter2@proxy.example.org:3128"}, url: "", skipped: true}, //nolint:gosec // a fake address
	} {
		got, skipped := tc.egress.resolve(clusterWithRanges(), &tc.set)
		if got.url != tc.url || (tc.url != "" && got.exceptions() != tc.noProxy) {
			t.Errorf("%s: %q behind %q, want %q behind %q", name, got.url, got.exceptions(), tc.url, tc.noProxy)
		}
		if (skipped != "") != tc.skipped {
			t.Errorf("%s: skipped %q", name, skipped)
		}
		if strings.Contains(skipped, "hunter2") {
			t.Errorf("%s: the reason shows the password: %q", name, skipped)
		}
	}
}

func TestEgressReachesEveryAddonThatLeavesTheCluster(t *testing.T) {
	t.Parallel()
	egress, _ := Egress{URL: "http://proxy.example.org:3128", NoProxy: []string{"pve.example.org"}}.resolve(clusterWithRanges(), &EgressProxy{})
	const wantNoProxy = "10.244.0.0/16,10.96.0.0/16,localhost,127.0.0.1,.svc,.cluster.local,pve.example.org"
	addons := &Addons{
		Headlamp:     Headlamp{Enabled: true},
		Rancher:      Rancher{Install: true, Hostname: "rancher.example.org"},
		Certificates: Certificates{Enabled: true, Issuer: IssuerACMEHTTP01, Domain: "lab.example.org"},
		GitOps:       GitOps{Enabled: true, Repository: GitRepository{URL: "https://git.example.org/a/b.git"}},
		viaProxy:     egress,
	}
	_, values := proxySpecs(t, templates.RKE2, CNI{}, addons)

	envOf := func(list any) map[string]string {
		out := map[string]string{}
		items, ok := list.([]any)
		if !ok {
			t.Fatalf("env is %T, not a list", list)
		}
		for _, item := range items {
			e, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("env entry is %T", item)
			}
			name, _ := e["name"].(string)
			value, _ := e["value"].(string)
			out[name] = value
		}
		return out
	}
	for name, env := range map[string]map[string]string{
		"web-headlamp": envOf(values["web-headlamp"]["env"]),
		"web-argocd":   envOf(lookup[any](t, values["web-argocd"], "global", "env")),
	} {
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			if env[k] != "http://proxy.example.org:3128" {
				t.Errorf("%s: %s is %q", name, k, env[k])
			}
		}
		if env["NO_PROXY"] != wantNoProxy || env["no_proxy"] != wantNoProxy {
			t.Errorf("%s: NO_PROXY %q", name, env["NO_PROXY"])
		}
	}
	cm := values["web-cert-manager"]
	if cm["http_proxy"] != "http://proxy.example.org:3128" || cm["https_proxy"] != "http://proxy.example.org:3128" || cm["no_proxy"] != wantNoProxy {
		t.Errorf("cert-manager: %v", cm)
	}
	if values["web-rancher"]["proxy"] != "http://proxy.example.org:3128" || values["web-rancher"]["noProxy"] != wantNoProxy {
		t.Errorf("Rancher: %v %v", values["web-rancher"]["proxy"], values["web-rancher"]["noProxy"])
	}
	// The charts that reach only Proxmox VE and the cluster stay as they were.
	for _, name := range []string{"web-proxmox-ccm", "web-proxmox-csi"} {
		if v, ok := values[name]; !ok || strings.Contains(string(mustMarshal(t, v)), "proxy.example.org") {
			t.Errorf("%s was put behind the proxy, or is missing: %v", name, v)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAddonsGoDirectWithoutAProxy(t *testing.T) {
	t.Parallel()
	addons := &Addons{
		Headlamp:     Headlamp{Enabled: true},
		Certificates: Certificates{Enabled: true, Issuer: IssuerACMEHTTP01, Domain: "lab.example.org"},
		GitOps:       GitOps{Enabled: true, Repository: GitRepository{URL: "https://git.example.org/a/b.git"}},
	}
	specs, values := proxySpecs(t, templates.RKE2, CNI{}, addons)
	for name := range specs {
		raw := lookup[string](t, specs[name], "valuesTemplate")
		if strings.Contains(strings.ToLower(raw), "proxy") && !strings.Contains(name, "proxmox") {
			t.Errorf("%s mentions a proxy without one: %s", name, raw)
		}
	}
	if _, ok := values["web-headlamp"]["env"]; ok {
		t.Error("Headlamp got an environment")
	}
}

func TestVeleroServerAndNodeAgentsGoThroughTheProxy(t *testing.T) {
	t.Parallel()
	c := backupsCluster(`{"enabled":true,"target":"aws"}`)
	addons, err := addonsOf(c)
	if err != nil {
		t.Fatal(err)
	}
	addons.backupTarget = &BackupTarget{Name: "aws", Bucket: "aws-backups", Region: "eu-central-2"}
	addons.viaProxy = egressEnv{url: "http://proxy.example.org:3128", noProxy: []string{".svc", "10.0.0.0/8"}}
	charts := testCharts
	charts.PodSecurityExempt = PodSecurityExempt{Always: []string{"kube-system"}, Backups: []string{VeleroNamespace}}
	proxies, err := helmChartProxies(c, &charts, "rke2", "local-lvm", CNI{}, &addons)
	if err != nil {
		t.Fatal(err)
	}
	_, values, ok := proxyValues(t, proxies, veleroProxy(c))
	if !ok {
		t.Fatal("no Velero proxy")
	}
	for _, path := range [][]any{{"configuration", "extraEnvVars"}, {"nodeAgent", "extraEnvVars"}} {
		list := lookup[[]any](t, values, path...)
		if len(list) != 6 {
			t.Fatalf("%v: %d variables", path, len(list))
		}
		if lookup[string](t, list, 1, "value") != "http://proxy.example.org:3128" || lookup[string](t, list, 4, "value") != ".svc,10.0.0.0/8" {
			t.Errorf("%v: %v", path, list)
		}
	}
	// What Kubehenge sets beside the environment stays.
	if lookup[string](t, values, "configuration", "uploaderType") != "kopia" || len(lookup[[]any](t, values, "nodeAgent", "extraArgs")) != 1 {
		t.Errorf("Velero's settings %v", values)
	}
}

func TestFluxControllersGoThroughTheProxy(t *testing.T) {
	t.Parallel()
	g := &GitOps{Enabled: true, Engine: flux, Repository: GitRepository{URL: "https://git.example.org/a/b.git"}}
	instance := bootstrapObject(g, "2.9.6")
	if err := (egressEnv{}).patchFlux(instance); err != nil {
		t.Fatal(err)
	}
	if n := len(lookup[[]any](t, instance.Object, "spec", "kustomize", "patches")); n != 1 {
		t.Fatalf("a FluxInstance without a proxy has %d patches", n)
	}
	e := egressEnv{url: "http://proxy.example.org:3128", noProxy: []string{".svc", "10.96.0.0/16"}}
	if err := e.patchFlux(instance); err != nil {
		t.Fatal(err)
	}
	patches := lookup[[]any](t, instance.Object, "spec", "kustomize", "patches")
	if len(patches) != 2 || lookup[string](t, patches, 1, "target", "kind") != "Deployment" {
		t.Fatalf("patches %v", patches)
	}
	var patch map[string]any
	if err := yaml.Unmarshal([]byte(lookup[string](t, patches, 1, "patch")), &patch); err != nil {
		t.Fatal(err)
	}
	container := lookup[map[string]any](t, patch, "spec", "template", "spec", "containers", 0)
	env := lookup[[]any](t, container, "env")
	if container["name"] != "manager" || len(env) != 6 || lookup[string](t, env, 0, "name") != "HTTP_PROXY" || lookup[string](t, env, 4, "value") != ".svc,10.96.0.0/16" {
		t.Errorf("the proxy patch %v", patch)
	}
	// The requests patch is still the first.
	if !strings.Contains(lookup[string](t, patches, 0, "patch"), "cpu: 10m") {
		t.Error("the requests patch moved")
	}
}

func TestEgressVariableIsHeldToAddressesWithoutCredentials(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		v  EgressProxy
		ok bool
	}{
		"empty":                {EgressProxy{}, true},
		"an address":           {EgressProxy{URL: "http://proxy.example.org:3128", NoProxy: []string{".corp.example"}}, true},
		"going direct":         {EgressProxy{Disabled: true}, true},
		"credentials":          {EgressProxy{URL: "http://user:secret@proxy.example.org:3128"}, false}, //nolint:gosec // a fake address
		"a user name":          {EgressProxy{URL: "http://user@proxy.example.org"}, false},
		"no scheme":            {EgressProxy{URL: "proxy.example.org:3128"}, false},
		"a socks proxy":        {EgressProxy{URL: "socks5://proxy.example.org:1080"}, false},
		"a path":               {EgressProxy{URL: "http://proxy.example.org/pac"}, false},
		"direct with an URL":   {EgressProxy{URL: "http://proxy.example.org", Disabled: true}, false},
		"an exception list":    {EgressProxy{NoProxy: []string{"a,b"}}, false},
		"an empty exception":   {EgressProxy{NoProxy: []string{""}}, false},
		"too many exceptions":  {EgressProxy{NoProxy: slices.Repeat([]string{"a.example"}, 65)}, false},
		"an exception with ws": {EgressProxy{NoProxy: []string{"a b"}}, false},
	} {
		err := checkEgress(&tc.v)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error shows the password: %v", name, err)
		}
	}
}

func TestValidatorRefusesAnEgressProxyWithCredentials(t *testing.T) {
	t.Parallel()
	good := withAddons(cluster(), map[string]string{egressVariable: `{"url":"http://proxy.example.org:3128"}`})
	if errs := validateEgress(good); len(errs) != 0 {
		t.Errorf("a plain address: %v", errs)
	}
	bad := withAddons(cluster(), map[string]string{egressVariable: `{"url":"http://u:p@proxy.example.org:3128"}`}) //nolint:gosec // a fake address
	errs := validateEgress(bad)
	if len(errs) != 1 || !strings.Contains(errs[0].Detail, "credentials") {
		t.Errorf("an address with credentials: %v", errs)
	}
	if errs = validateEgress(cluster()); len(errs) != 0 {
		t.Errorf("no variable: %v", errs)
	}
	if errs = validateEgress(withAddons(cluster(), map[string]string{egressVariable: `{"url": 3}`})); len(errs) != 1 {
		t.Errorf("a variable of the wrong type: %v", errs)
	}
}

func TestChartsComeFromTheirRepositoriesMirror(t *testing.T) {
	t.Parallel()
	mirrors, err := ParseChartMirrors([]string{
		"ghcr.io=oci://registry.example.org/ghcr", "helm.cilium.io=https://charts.example.org/cilium",
	})
	if err != nil {
		t.Fatal(err)
	}
	charts := testCharts
	charts.Mirrors = mirrors
	proxies, err := helmChartProxies(cluster(), &charts, templates.Kubeadm, "local-lvm", CNI{Name: cniCilium, Pods: "10.244.0.0/16"}, &Addons{Headlamp: Headlamp{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	repos := map[string]string{}
	for _, p := range proxies {
		repos[p.GetName()] = lookup[string](t, p.Object, "spec", "repoURL")
	}
	for name, want := range map[string]string{
		"web-proxmox-ccm": "oci://registry.example.org/ghcr/sergelogvinov/charts",
		"web-proxmox-csi": "oci://registry.example.org/ghcr/sergelogvinov/charts",
		"web-cni":         "https://charts.example.org/cilium",
		"web-headlamp":    "https://kubernetes-sigs.github.io/headlamp",
		"web-traefik":     "https://traefik.github.io/charts",
	} {
		if repos[name] != want {
			t.Errorf("%s comes from %q, want %q", name, repos[name], want)
		}
	}
}

func TestChartMirrorsAreHeldToTheirCharts(t *testing.T) {
	t.Parallel()
	charts := []Chart{testCharts.CCM, testCharts.Cilium}
	for name, tc := range map[string]struct {
		pairs   []string
		parse   bool
		problem string
	}{
		"both kinds":            {pairs: []string{"ghcr.io=oci://r.example.org/g", "helm.cilium.io=https://c.example.org"}},
		"a port":                {pairs: []string{"ghcr.io=oci://r.example.org:5000/g"}},
		"no mirrors":            {},
		"a host no chart uses":  {pairs: []string{"quay.io=oci://r.example.org/q"}, problem: "no chart comes from quay.io"},
		"an HTTP mirror of OCI": {pairs: []string{"ghcr.io=https://r.example.org/g"}, problem: "oci://"},
		"an OCI mirror of HTTP": {pairs: []string{"helm.cilium.io=oci://r.example.org/c"}, problem: "http(s)://"},
		"no address":            {pairs: []string{"ghcr.io"}, parse: true},
		"a trailing slash":      {pairs: []string{"ghcr.io=oci://r.example.org/g/"}, parse: true},
		"credentials":           {pairs: []string{"ghcr.io=oci://u:p@r.example.org/g"}, parse: true},
		"another scheme":        {pairs: []string{"ghcr.io=ftp://r.example.org/g"}, parse: true},
		"twice":                 {pairs: []string{"ghcr.io=oci://a.example.org", "ghcr.io=oci://b.example.org"}, parse: true},
	} {
		mirrors, err := ParseChartMirrors(tc.pairs)
		if (err != nil) != tc.parse {
			t.Errorf("%s: parsing gave %v", name, err)
			continue
		}
		if err != nil {
			continue
		}
		err = mirrors.Check(charts)
		if tc.problem == "" && err != nil || tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)) {
			t.Errorf("%s: checking gave %v, want %q", name, err, tc.problem)
		}
	}
	mirrors := ChartMirrors{"charts.jetstack.io": "https://charts.example.org/jetstack"}
	if got := mirrors.Repository("https://charts.jetstack.io"); got != "https://charts.example.org/jetstack" {
		t.Errorf("a repository without a path: %s", got)
	}
	if got := mirrors.Repository("https://example.org/charts"); got != "https://example.org/charts" {
		t.Errorf("a repository without a mirror: %s", got)
	}
}

func TestChartsInValuesListsEveryPinnedChart(t *testing.T) {
	t.Parallel()
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	charts, err := ChartsInValues([]byte("addons:\n  headlamp:\n    repository: https://kubernetes-sigs.github.io/headlamp\n    chart: headlamp\n    version: 0.46.0@sha256:" + digest + "\n  rancher:\n    kubernetes: 34-36\n  imageOnly:\n    image: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://kubernetes-sigs.github.io/headlamp/headlamp:0.46.0@sha256:" + digest
	if len(charts) != 1 || charts["headlamp"].String() != want {
		t.Errorf("charts %v", charts)
	}
	if _, err = ChartsInValues([]byte("addons: {}\n")); err == nil {
		t.Error("values with no chart")
	}
	if _, err = ChartsInValues([]byte("addons:\n  x:\n    repository: https://r.example.org\n    chart: x\n    version: 1.0.0\n")); err == nil {
		t.Error("a chart without a digest")
	}
}

func TestArgoCDTrustsTheAuthorityOfTheRepositoryForItsHost(t *testing.T) {
	t.Parallel()
	ca := testAuthority(t)
	g := &GitOps{Enabled: true, Repository: GitRepository{URL: "https://git.example.org:8443/a/b.git", CABundle: ca}}
	_, values := proxySpecs(t, templates.RKE2, CNI{}, &Addons{GitOps: *g})
	got := lookup[string](t, values["web-argocd"], "configs", "tls", "certificates", "git.example.org")
	if got != ca {
		t.Errorf("argocd-tls-certs-cm gets %q", got)
	}
	g.Repository.CABundle = ""
	_, values = proxySpecs(t, templates.RKE2, CNI{}, &Addons{GitOps: *g})
	if _, ok := lookup[map[string]any](t, values["web-argocd"], "configs")["tls"]; ok {
		t.Error("a repository without an authority gets certificates")
	}
}

func TestFluxReadsTheAuthorityOfTheRepositoryFromItsPullSecret(t *testing.T) {
	t.Parallel()
	ca := testAuthority(t)
	r := &Reconciler{}
	for name, tc := range map[string]struct {
		auth string
		want []string
	}{
		"public":     {gitAuthNone, []string{"ca.crt"}},
		"with token": {gitAuthToken, nil},
	} {
		g := &GitOps{Enabled: true, Engine: flux, Auth: tc.auth, Repository: GitRepository{URL: "https://git.example.org/a/b.git", CABundle: ca}}
		if g.pullSecret() != fluxPullSecret {
			t.Errorf("%s: Flux reads %q", name, g.pullSecret())
		}
		if tc.auth == gitAuthToken {
			continue
		}
		creds, missing, err := r.repositoryCredentials(t.Context(), cluster(), g, &gitOpsState{})
		if err != nil || missing != "" {
			t.Fatalf("%s: %v %q", name, err, missing)
		}
		if string(creds["ca.crt"]) != ca || len(creds) != len(tc.want) {
			t.Errorf("%s: credentials %v", name, creds)
		}
		secret := repositorySecret(g, creds)
		if secret.Name != fluxPullSecret || secret.Namespace != fluxNamespace || string(secret.Data["ca.crt"]) != ca {
			t.Errorf("%s: pull Secret %v", name, secret)
		}
	}
	// Without an authority a public repository needs no Secret, as before.
	g := &GitOps{Enabled: true, Engine: flux, Repository: GitRepository{URL: "https://git.example.org/a/b.git"}}
	if g.pullSecret() != "" {
		t.Errorf("a public repository is read with %q", g.pullSecret())
	}
	creds, _, _ := r.repositoryCredentials(t.Context(), cluster(), g, &gitOpsState{})
	if creds != nil {
		t.Errorf("credentials %v for a public repository", creds)
	}
	// Argo CD's repository Secret never carries it: Argo CD reads its own ConfigMap.
	a := &GitOps{Enabled: true, Auth: gitAuthNone, Repository: GitRepository{URL: "https://git.example.org/a/b.git", CABundle: ca}}
	creds, _, _ = r.repositoryCredentials(t.Context(), cluster(), a, &gitOpsState{})
	if creds != nil {
		t.Errorf("Argo CD got credentials %v", creds)
	}
}

func TestRepositoryAuthorityIsHeldToCertificatesForHTTPS(t *testing.T) {
	t.Parallel()
	ca := testAuthority(t)
	const url = "https://git.example.org/a/b.git"
	for name, tc := range map[string]struct {
		g       GitOps
		problem string
	}{
		"argo":               {GitOps{Repository: GitRepository{URL: url, CABundle: ca}}, ""},
		"flux":               {GitOps{Engine: flux, Auth: gitAuthToken, Repository: GitRepository{URL: url, CABundle: ca}}, ""},
		"argo with a secret": {GitOps{Auth: gitAuthExisting, ExistingSecret: "mine", Repository: GitRepository{URL: url, CABundle: ca}}, ""},
		"flux with a secret": {GitOps{Engine: flux, Auth: gitAuthExisting, ExistingSecret: "mine", Repository: GitRepository{URL: url, CABundle: ca}}, "that Secret's ca.crt"},
		"ssh":                {GitOps{Auth: gitAuthKey, Repository: GitRepository{URL: "ssh://git@git.example.org/a/b.git", CABundle: ca}}, "https://"},
		"not a certificate":  {GitOps{Repository: GitRepository{URL: url, CABundle: "hello"}}, "no PEM certificate"},
		"a private key":      {GitOps{Repository: GitRepository{URL: url, CABundle: ca + "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"}}, "private key"},
		"too large":          {GitOps{Repository: GitRepository{URL: url, CABundle: ca + strings.Repeat("x", maxRepositoryCA)}}, "at most"},
	} {
		tc.g.Enabled = true
		err := tc.g.checkRepositoryCA()
		if tc.problem == "" && err != nil || tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)) {
			t.Errorf("%s: %v, want %q", name, err, tc.problem)
		}
	}
	// The server-side check runs on the cluster's variable.
	c := withAddons(cluster(), map[string]string{gitopsVariable: `{"enabled":true,"repository":{"url":"` + url + `","caBundle":"hello"}}`})
	errs := validateGitOps(c)
	if len(errs) != 1 || !strings.Contains(errs[0].Field, "caBundle") {
		t.Errorf("a repository authority that is no certificate: %v", errs)
	}
}
