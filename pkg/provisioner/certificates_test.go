package provisioner

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func certificatesCluster(raw string) *clusterv1.Cluster {
	return withAddons(storageCluster(), map[string]string{certificatesVariable: raw})
}

func TestDefaultCertificateCoversTheDomainAndOneLabelBeneath(t *testing.T) {
	t.Parallel()
	c := Certificates{Enabled: true, Issuer: IssuerCA, Domain: "lab.example.org"}
	for host, want := range map[string]bool{
		"lab.example.org":            true,
		"grafana.lab.example.org":    true,
		"Grafana.Lab.Example.org.":   true,
		"a.grafana.lab.example.org":  false,
		"example.org":                false,
		"otherlab.example.org":       false,
		"grafana.lab.example.org.uk": false,
	} {
		if got := c.covers(host); got != want {
			t.Errorf("covers(%q) = %v", host, got)
		}
	}
	if (&Certificates{Enabled: true, Issuer: IssuerACMEHTTP01, Domain: "lab.example.org"}).covers("lab.example.org") {
		t.Error("HTTP-01 gives no default certificate")
	}
	if (&Certificates{Issuer: IssuerCA, Domain: "lab.example.org"}).covers("lab.example.org") {
		t.Error("a default certificate while the add-on is off")
	}
}

func TestAddonIngressAsksTheIssuerOnlyForWhatTheDefaultCertificateMisses(t *testing.T) {
	t.Parallel()
	dns := Certificates{Enabled: true, Domain: "lab.example.org"}
	for _, tc := range []struct {
		name        string
		c           Certificates
		host, given string
		secret      string
		annotated   bool
	}{
		{"off", Certificates{}, "grafana.example.org", "", "", false},
		{"its own Secret", dns, "grafana.example.org", "mine", "mine", false},
		{"covered", dns, "grafana.lab.example.org", "", "", false},
		{"not covered", dns, "grafana.example.org", "", "grafana-tls", true},
		{"HTTP-01", Certificates{Enabled: true, Issuer: IssuerACMEHTTP01}, "grafana.example.org", "", "grafana-tls", true},
	} {
		tls, annotations := addonIngress(&tc.c, "grafana", tc.host, tc.given)
		if secret := tls["secretName"]; (tc.secret == "" && secret != nil) || (tc.secret != "" && secret != tc.secret) {
			t.Errorf("%s: secret %v", tc.name, secret)
		}
		if annotated := annotations[issuerAnnotation] == ingressIssuer; annotated != tc.annotated {
			t.Errorf("%s: annotations %v", tc.name, annotations)
		}
	}
	values := headlampValues(Headlamp{Enabled: true, Hostname: "headlamp.example.org"}, &dns)
	if got := lookup[string](t, values, "ingress", "annotations", issuerAnnotation); got != ingressIssuer {
		t.Errorf("Headlamp's Ingress: %v", values["ingress"])
	}
	addons := &Addons{Monitoring: Monitoring{Enabled: true, Hostname: "grafana.example.org"}, Certificates: dns}
	grafana := monitoringValues(templates.Kubeadm, Image{}, addons)
	if got := lookup[string](t, grafana, "grafana", "ingress", "annotations", issuerAnnotation); got != ingressIssuer {
		t.Errorf("Grafana's Ingress: %v", grafana["grafana"])
	}
	signedIn := &gitOpsState{provider: &oidcProvider{IssuerURL: "https://id.example.org"}, clientSecret: true}
	argo, err := argoCDValues(&dns, &GitOps{Enabled: true, Hostname: "argo.example.org"}, signedIn)
	if err != nil {
		t.Fatal(err)
	}
	if lookup[string](t, argo, "server", "ingress", "annotations", issuerAnnotation) != ingressIssuer ||
		lookup[string](t, argo, "server", "ingress", "extraTls", 0, "secretName") != "argocd-tls" {
		t.Errorf("Argo CD's Ingress: %v", argo["server"])
	}
	fluxWeb := fluxOperatorValues(&dns, &GitOps{Enabled: true, Engine: flux, UI: true, Hostname: "flux.example.org"}, signedIn)
	if lookup[string](t, fluxWeb, "web", "ingress", "annotations", issuerAnnotation) != ingressIssuer ||
		lookup[string](t, fluxWeb, "web", "ingress", "tls", 0, "secretName") != "flux-operator-tls" {
		t.Errorf("Flux's Ingress: %v", fluxWeb["web"])
	}
}

func TestTraefikServesTheDefaultCertificate(t *testing.T) {
	t.Parallel()
	for _, c := range []Certificates{{}, {Enabled: true, Issuer: IssuerACMEHTTP01, Domain: "lab.example.org"}} {
		values := traefikValues("192.0.2.50", &c)
		_, websecure := lookup[map[string]any](t, values, "gateway", "listeners")["websecure"]
		if _, store := values["tlsStore"]; store || websecure {
			t.Errorf("%+v: a default certificate %v", c, values)
		}
	}
	certs := Certificates{Enabled: true, Issuer: IssuerCA, Domain: "lab.example.org"}
	values := traefikValues("", &certs)
	if got := lookup[string](t, values, "tlsStore", "default", "defaultCertificate", "secretName"); got != ingressTLSSecret {
		t.Errorf("default certificate %q", got)
	}
	listener := lookup[map[string]any](t, values, "gateway", "listeners", "websecure")
	if listener["protocol"] != "HTTPS" || listener["port"] != 8443 || lookup[string](t, listener, "certificateRefs", 0, "name") != ingressTLSSecret {
		t.Errorf("HTTPS listener %v", listener)
	}
	if lookup[string](t, values, "gateway", "listeners", "web", "namespacePolicy", "from") != "All" {
		t.Error("the HTTP listener went with the HTTPS one")
	}
	if _, ok := lookup[map[string]any](t, values, "providers", "kubernetesGateway")["statusAddress"]; ok {
		t.Error("a Gateway address with no API address")
	}
}

// RKE2's and k3s's Traefik take what follows the certificates from a Secret the cluster's
// ClusterResourceSet keeps in the running cluster, the same values the Traefik Kubehenge installs
// into kubeadm clusters gets, and empty while there is no default certificate.
func TestRKE2AndK3sTraefikTakeTheDefaultCertificateFromTheRunningCluster(t *testing.T) {
	t.Parallel()
	traefikSecretValues := func(class string, certs Certificates) (map[string]any, bool) {
		c := certificatesCluster(`{"enabled": false}`)
		c.Spec.Topology.ClassRef.Name = class
		secrets, err := resourceSetSecrets(c, &corev1.Secret{Name: "proxmox"}, CNI{}, &Addons{Certificates: certs}, nil)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(secrets, func(s *corev1.Secret) bool { return s.Name == traefikConfigName(c) })
		if i < 0 {
			return nil, false
		}
		var applied corev1.Secret
		if err := yaml.Unmarshal(secrets[i].Data[traefikValuesSecret+".yaml"], &applied); err != nil {
			t.Fatal(err)
		}
		if applied.Kind != secretKind || applied.Name != traefikValuesSecret || applied.Namespace != "kube-system" {
			t.Fatalf("%s: applies %s %s/%s", class, applied.Kind, applied.Namespace, applied.Name)
		}
		var values map[string]any
		if err := yaml.Unmarshal([]byte(applied.StringData[traefikValuesKey]), &values); err != nil {
			t.Fatal(err)
		}
		return values, true
	}
	ca := Certificates{Enabled: true, Issuer: IssuerCA, Domain: "lab.example.org"}
	for _, class := range []string{ClassPrefix + "rke2", ClassPrefix + "k3s"} {
		values, found := traefikSecretValues(class, ca)
		if !found {
			t.Fatalf("%s: no values for Traefik", class)
		}
		want, err := yaml.Marshal(traefikCertificateValues(&ca))
		if err != nil {
			t.Fatal(err)
		}
		got, err := yaml.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: Traefik's values\n%s\nwant\n%s", class, got, want)
		}
		for _, certs := range []Certificates{{}, {Enabled: true, Issuer: IssuerACMEHTTP01, Domain: "lab.example.org"}} {
			if values, found = traefikSecretValues(class, certs); !found || len(values) != 0 {
				t.Errorf("%s %+v: Traefik's values %v, found %v", class, certs, values, found)
			}
		}
	}
	if _, found := traefikSecretValues(ClassPrefix+"kubeadm", ca); found {
		t.Error("a kubeadm cluster, whose Traefik Kubehenge installs, got the values of RKE2's")
	}
}

func TestCertManagerComesWithCertificatesAndWaitsForTheGatewayAPI(t *testing.T) {
	t.Parallel()
	c := certificatesCluster(`{"enabled": true, "domain": "lab.example.org", "dns01": {"provider": "cloudflare", "nameservers": ["1.1.1.1:53", "9.9.9.9:53"]}}`)
	addons, err := addonsOf(c)
	if err != nil {
		t.Fatal(err)
	}
	proxies, err := helmChartProxies(c, &testCharts, templates.RKE2, "local-lvm", CNI{}, &addons)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(proxies, func(p *unstructured.Unstructured) bool { return p.GetName() == certManagerProxy(c) })
	if i < 0 {
		t.Fatal("no cert-manager for certificates")
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(lookup[string](t, proxies[i].Object, "spec", "valuesTemplate")), &values); err != nil {
		t.Fatal(err)
	}
	if !lookup[bool](t, values, "config", "gatewayAPI", "enabled") ||
		lookup[string](t, values, "ingressShim", "defaultIssuerName") != ingressIssuer ||
		values["dns01RecursiveNameservers"] != "1.1.1.1:53,9.9.9.9:53" || !lookup[bool](t, values, "dns01RecursiveNameserversOnly") {
		t.Errorf("cert-manager's values %v", values)
	}
	plain := certManagerValues(&Certificates{})
	if _, ok := plain["ingressShim"]; ok || !lookup[bool](t, plain, "config", "gatewayAPI", "enabled") {
		t.Errorf("cert-manager's values for monitoring %v", plain)
	}

	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: certManagerProxy(c)}, hcp); err == nil {
		t.Error("cert-manager installed before the Gateway API is there")
	}
}

func TestClusterIssuerOfEveryKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		c    Certificates
		path []any
		want any
	}{
		{"CA", Certificates{Issuer: IssuerCA}, []any{"spec", "ca", "secretName"}, ingressCASecret},
		{"Let's Encrypt", Certificates{DNS01: DNS01{Provider: "cloudflare"}}, []any{"spec", "acme", "server"}, letsEncrypt},
		{"staging", Certificates{ACME: ACME{Staging: true}, DNS01: DNS01{Provider: "cloudflare"}}, []any{"spec", "acme", "server"}, letsEncryptStaging},
		{"own server", Certificates{ACME: ACME{Server: "https://acme.test/dir", CABundle: "PEM"}, DNS01: DNS01{Provider: "cloudflare"}}, []any{"spec", "acme", "caBundle"}, base64.StdEncoding.EncodeToString([]byte("PEM"))},
		{"HTTP-01", Certificates{Issuer: IssuerACMEHTTP01}, []any{"spec", "acme", "solvers", 0, "http01", "gatewayHTTPRoute", "parentRefs", 0, "sectionName"}, "web"},
		{"Cloudflare", Certificates{DNS01: DNS01{Provider: "cloudflare"}}, []any{"spec", "acme", "solvers", 0, "dns01", "cloudflare", "apiTokenSecretRef", "name"}, dns01Secret},
		{"DigitalOcean", Certificates{DNS01: DNS01{Provider: "digitalocean", ExistingSecret: "mine"}}, []any{"spec", "acme", "solvers", 0, "dns01", "digitalocean", "tokenSecretRef", "name"}, "mine"},
		{"Route 53", Certificates{DNS01: DNS01{Provider: "route53", Route53: Route53{Region: "eu-central-2", AccessKeyID: "AKIA"}}}, []any{"spec", "acme", "solvers", 0, "dns01", "route53", "accessKeyID"}, "AKIA"},
		{"Azure", Certificates{DNS01: DNS01{Provider: "azuredns", AzureDNS: AzureDNS{TenantID: "t", ClientID: "c"}}}, []any{"spec", "acme", "solvers", 0, "dns01", "azureDNS", "clientSecretSecretRef", "key"}, dns01TokenKey},
		{"Google", Certificates{DNS01: DNS01{Provider: "clouddns", CloudDNS: CloudDNS{Project: "p"}}}, []any{"spec", "acme", "solvers", 0, "dns01", "cloudDNS", "project"}, "p"},
		{"RFC 2136", Certificates{DNS01: DNS01{Provider: "rfc2136", RFC2136: RFC2136{Nameserver: "ns.test:53", TSIGKeyName: "k"}}}, []any{"spec", "acme", "solvers", 0, "dns01", "rfc2136", "tsigAlgorithm"}, "HMACSHA256"},
		{"ACME-DNS", Certificates{DNS01: DNS01{Provider: "acmedns", ACMEDNS: ACMEDNS{Host: "https://auth.test"}}}, []any{"spec", "acme", "solvers", 0, "dns01", "acmeDNS", "accountSecretRef", "key"}, dns01TokenKey},
		{"Akamai", Certificates{DNS01: DNS01{Provider: "akamai", ExistingSecret: "edge", Akamai: Akamai{ServiceConsumerDomain: "d"}}}, []any{"spec", "acme", "solvers", 0, "dns01", "akamai", "accessTokenSecretRef", "key"}, "accessToken"},
	} {
		issuer, err := clusterIssuer(&tc.c)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := lookup[any](t, issuer.Object, tc.path...); got != tc.want {
			t.Errorf("%s: %v, not %v", tc.name, got, tc.want)
		}
	}
	if _, err := clusterIssuer(&Certificates{DNS01: DNS01{Provider: "akamai"}}); err == nil {
		t.Error("Akamai without an existing Secret")
	}
}

func TestIngressCASignsOnlyItsDomain(t *testing.T) {
	t.Parallel()
	certPEM, _, err := newIngressCA("web", "lab.example.org", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.PermittedDNSDomainsCritical || !slices.Equal(ca.PermittedDNSDomains, []string{"lab.example.org"}) || len(ca.ExcludedIPRanges) != 2 {
		t.Errorf("CA constraints: %+v", ca)
	}
	if ca.NotAfter.Before(time.Now().Add(9 * 365 * 24 * time.Hour)) {
		t.Errorf("CA runs out %v", ca.NotAfter)
	}
}

// certWorkload stands in for a workload cluster, holding what is applied to it and deleted from it.
type certWorkload struct {
	client.WithWatch

	mu        sync.Mutex
	installed bool
	applied   map[string]map[string]any
	deleted   []string
}

func newCertWorkload(t *testing.T, objs ...client.Object) *certWorkload {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	w := &certWorkload{applied: map[string]map[string]any{}}
	key := func(o client.Object, kind string) string { return kind + "/" + o.GetNamespace() + "/" + o.GetName() }
	w.WithWatch = interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			kind := obj.GetObjectKind().GroupVersionKind().Kind
			if kind != clusterIssuerKind {
				return c.Get(ctx, k, obj, opts...)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			stored, ok := w.applied[kind+"//"+k.Name]
			if !ok {
				return apierrors.NewNotFound(schema.GroupResource{Group: certManagerGroup, Resource: "clusterissuers"}, k.Name)
			}
			obj.SetName(k.Name)
			obj.SetAnnotations((&unstructured.Unstructured{Object: stored}).GetAnnotations())
			return nil
		},
		Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			u, ok := obj.(interface{ UnstructuredContent() map[string]any })
			if !ok {
				return fmt.Errorf("applied %T", obj)
			}
			o := &unstructured.Unstructured{Object: u.UnstructuredContent()}
			w.mu.Lock()
			defer w.mu.Unlock()
			if !w.installed {
				return &meta.NoKindMatchError{GroupKind: o.GroupVersionKind().GroupKind()}
			}
			// As the API server and the client do: an apply carrying managedFields is refused, and
			// the answer, which carries them, is written back into the object applied.
			if o.GetManagedFields() != nil {
				return apierrors.NewBadRequest("metadata.managedFields must be nil")
			}
			w.applied[key(o, o.GetKind())] = o.DeepCopy().Object
			o.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: fieldOwner, Operation: metav1.ManagedFieldsOperationApply}})
			return nil
		},
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			k := key(obj, obj.GetObjectKind().GroupVersionKind().Kind)
			w.deleted = append(w.deleted, k)
			delete(w.applied, k)
			return nil
		},
	})
	return w
}

func (w *certWorkload) has(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.applied[key]
	return ok
}

func TestCertificatesFromTheClustersCA(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := certificatesCluster(`{"enabled": true, "issuer": "ca", "domain": "lab.example.org"}`)
	r, kube := newReconciler(t, &fakeCloud{}, c)
	workload := newCertWorkload(t)
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	certs, err := CertificatesOf(c)
	if err != nil {
		t.Fatal(err)
	}

	waiting, err := r.reconcileCertificates(ctx, c, &certs)
	if err != nil || !waiting {
		t.Fatalf("before cert-manager: waiting %v, %v", waiting, err)
	}
	ca := &corev1.Secret{}
	if err = kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: IngressCAName("web")}, ca); err != nil {
		t.Fatal(err)
	}
	if ca.Type != corev1.SecretTypeTLS || ca.Annotations[IngressCADomainAnnotation] != "lab.example.org" || len(ca.OwnerReferences) != 1 {
		t.Errorf("the cluster's CA %+v", ca.ObjectMeta)
	}
	if _, marked := c.Annotations[certificatesAnnotation]; !marked {
		t.Error("the cluster is not marked as holding certificates' objects")
	}

	workload.installed = true
	if waiting, err = r.reconcileCertificates(ctx, c, &certs); err != nil || waiting {
		t.Fatalf("waiting %v, %v", waiting, err)
	}
	copied := workload.applied["Secret/cert-manager/"+ingressCASecret]
	if copied == nil || lookup[string](t, copied, "data", corev1.TLSCertKey) != base64.StdEncoding.EncodeToString(ca.Data[corev1.TLSCertKey]) {
		t.Errorf("the CA's copy %v", copied)
	}
	certificate := workload.applied["Certificate/kube-system/"+ingressIssuer]
	if certificate == nil || lookup[string](t, certificate, "spec", "dnsNames", 1) != "*.lab.example.org" {
		t.Errorf("the default certificate %v", certificate)
	}
	if !workload.has("ClusterIssuer//" + ingressIssuer) {
		t.Error("no issuer")
	}

	// The same CA while the domain stays, a new one for a new domain.
	before := string(ca.Data[corev1.TLSCertKey])
	if _, err = r.ingressCA(ctx, c, "lab.example.org"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ingressCA(ctx, c, "other.example.org"); err != nil {
		t.Fatal(err)
	}
	if err = kube.Get(ctx, client.ObjectKeyFromObject(ca), ca); err != nil {
		t.Fatal(err)
	}
	if string(ca.Data[corev1.TLSCertKey]) == before || ca.Annotations[IngressCADomainAnnotation] != "other.example.org" {
		t.Error("a new domain kept the old CA")
	}

	// Switched off: everything goes from the workload cluster, the CA stays beside the cluster.
	if waiting, err = r.reconcileCertificates(ctx, c, &Certificates{}); err != nil || waiting {
		t.Fatalf("switching off: waiting %v, %v", waiting, err)
	}
	if len(workload.applied) != 0 {
		t.Errorf("left in the workload cluster: %v", workload.applied)
	}
	if _, marked := c.Annotations[certificatesAnnotation]; marked {
		t.Error("still marked once everything is gone")
	}
	if err = kube.Get(ctx, client.ObjectKeyFromObject(ca), ca); err != nil {
		t.Errorf("the CA went with the add-on: %v", err)
	}
	workload.deleted = nil
	if _, err = r.reconcileCertificates(ctx, c, &Certificates{}); err != nil || len(workload.deleted) != 0 {
		t.Errorf("a cluster without certificates reached again: %v, %v", workload.deleted, err)
	}
}

func TestCertificatesCopyTheDNS01Credential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	credential := func(name, token string) *corev1.Secret {
		return &corev1.Secret{
			Namespace: "team-a", Name: name, Type: CredentialType,
			Labels: map[string]string{CredentialLabel: dns01Purpose},
			Data:   map[string][]byte{dns01TokenKey: []byte(token)},
		}
	}
	c := certificatesCluster(`{"enabled": true, "domain": "lab.example.org", "dns01": {"provider": "cloudflare"}}`)
	r, _ := newReconciler(t, &fakeCloud{}, c, credential("web.dns01", "own"), credential("kubehenge-dns01", "shared"))
	workload := newCertWorkload(t)
	workload.installed = true
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}

	token := func() string {
		s := workload.applied["Secret/cert-manager/"+dns01Secret]
		if s == nil {
			return ""
		}
		raw, err := base64.StdEncoding.DecodeString(lookup[string](t, s, "data", dns01TokenKey))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, tc := range []struct {
		name  string
		dns01 DNS01
		want  string
	}{
		{"the cluster's own", DNS01{Provider: "cloudflare"}, "own"},
		{"the shared one", DNS01{Provider: "cloudflare", SharedCredential: true}, "shared"},
		{"an existing Secret", DNS01{Provider: "cloudflare", ExistingSecret: "mine"}, ""},
	} {
		certs := Certificates{Enabled: true, Domain: "lab.example.org", DNS01: tc.dns01}
		if _, err := r.reconcileCertificates(ctx, c, &certs); err != nil {
			t.Fatal(err)
		}
		if got := token(); got != tc.want {
			t.Errorf("%s: token %q", tc.name, got)
		}
	}

	// An issuer that gives no default certificate takes it out.
	if _, err := r.reconcileCertificates(ctx, c, &Certificates{Enabled: true, Issuer: IssuerACMEHTTP01}); err != nil {
		t.Fatal(err)
	}
	if workload.has("Certificate/kube-system/"+ingressIssuer) || workload.has("Secret/cert-manager/"+dns01Secret) ||
		!slices.Contains(workload.deleted, "Secret/kube-system/"+ingressTLSSecret) || slices.Contains(workload.deleted, "Secret/cert-manager/"+ingressACMEKey) {
		t.Errorf("after switching to HTTP-01: %v, deleted %v", workload.applied, workload.deleted)
	}
}

func TestAnotherIssuerIssuesEveryCertificateAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	issued := func(namespace, name, issuer string) *corev1.Secret {
		return &corev1.Secret{Namespace: namespace, Name: name, Annotations: map[string]string{
			"cert-manager.io/issuer-name": issuer, "cert-manager.io/issuer-kind": clusterIssuerKind,
		}}
	}
	c := certificatesCluster(`{"enabled": true, "issuer": "ca", "domain": "lab.example.org"}`)
	r, _ := newReconciler(t, &fakeCloud{}, c)
	workload := newCertWorkload(t,
		issued("kube-system", ingressTLSSecret, ingressIssuer), issued("headlamp", "headlamp-tls", ingressIssuer), issued("shop", "web-tls", "theirs"))
	workload.installed = true
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	ca := Certificates{Enabled: true, Issuer: IssuerCA, Domain: "lab.example.org"}
	for range 2 {
		if _, err := r.reconcileCertificates(ctx, c, &ca); err != nil {
			t.Fatal(err)
		}
	}
	if slices.Contains(workload.deleted, "Secret/headlamp/headlamp-tls") {
		t.Fatal("certificates issued again by the same issuer")
	}
	dns := Certificates{Enabled: true, Domain: "lab.example.org", DNS01: DNS01{Provider: "cloudflare", ExistingSecret: "mine"}}
	if _, err := r.reconcileCertificates(ctx, c, &dns); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Secret/kube-system/" + ingressTLSSecret, "Secret/headlamp/headlamp-tls"} {
		if !slices.Contains(workload.deleted, want) {
			t.Errorf("%s not issued again: deleted %v", want, workload.deleted)
		}
	}
	if slices.Contains(workload.deleted, "Secret/shop/web-tls") {
		t.Error("a certificate of another issuer issued again")
	}
	annotations := (&unstructured.Unstructured{Object: workload.applied["ClusterIssuer//"+ingressIssuer]}).GetAnnotations()
	if _, pending := annotations[reissueAnnotation]; pending || !strings.HasPrefix(annotations[issuerIdentityAnnotation], "acme/") {
		t.Errorf("the issuer's annotations %v", annotations)
	}
}

func TestValidatorHoldsAddonHostsToTheClustersCA(t *testing.T) {
	t.Parallel()
	for raw, wantErr := range map[string]bool{
		`{"enabled": true, "hostname": "headlamp.lab.example.org"}`:                  false,
		`{"enabled": true, "hostname": "a.b.lab.example.org"}`:                       false,
		`{"enabled": true, "hostname": "headlamp.example.org"}`:                      true,
		`{"enabled": true, "hostname": "headlamp.example.org", "tlsSecret": "mine"}`: false,
	} {
		c := withAddons(certificatesCluster(`{"enabled": true, "issuer": "ca", "domain": "lab.example.org"}`), map[string]string{headlampAddon: raw})
		errs := validateCertificates(c)
		if (len(errs) > 0) != wantErr {
			t.Errorf("%s: %v", raw, errs)
		}
		if wantErr && !strings.Contains(errs.ToAggregate().Error(), "lab.example.org") {
			t.Errorf("%s: %v", raw, errs)
		}
	}
	gitops := withAddons(certificatesCluster(`{"enabled": true, "issuer": "ca", "domain": "lab.example.org"}`),
		map[string]string{gitopsVariable: `{"enabled": true, "hostname": "argo.example.org"}`})
	if errs := validateCertificates(gitops); len(errs) == 0 {
		t.Error("Argo CD's host name is not held to the domain")
	}
	acme := withAddons(certificatesCluster(`{"enabled": true, "domain": "lab.example.org", "dns01": {"provider": "cloudflare"}}`),
		map[string]string{headlampAddon: `{"enabled": true, "hostname": "headlamp.example.org"}`})
	if errs := validateCertificates(acme); len(errs) > 0 {
		t.Errorf("ACME held to the domain: %v", errs)
	}
}

func TestCertificatesVariableIsParsed(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: certificatesVariable, Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled": true, "dns01": {"provider": "rfc2136", "sharedCredential": true}}`)},
	})
	certs, err := CertificatesOf(c)
	if err != nil {
		t.Fatal(err)
	}
	if certs.issuer() != IssuerACMEDNS01 || !certs.SharesDNS01Credential() || certs.DNS01.Provider != "rfc2136" {
		t.Errorf("%+v", certs)
	}
	if _, err := addonsOf(withAddons(storageCluster(), map[string]string{certificatesVariable: `"on"`})); err == nil {
		t.Error("a certificates variable that is no object")
	}
}
