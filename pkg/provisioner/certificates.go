package provisioner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// certificatesVariable issues certificates for the cluster's ingress and Gateway.
	certificatesVariable = "certificates"

	// CredentialsStoredAnnotation on a Cluster records when the interface last stored or removed a
	// credential the cluster uses, so that the controller takes the change to the workload cluster at
	// once rather than at its next resync.
	CredentialsStoredAnnotation = "kubehenge.dev/credentials-stored-at" //nolint:gosec // an annotation's name, not a credential
	// IngressCADomainAnnotation on a cluster's CA Secret names the domain the CA is constrained to.
	IngressCADomainAnnotation = "kubehenge.dev/ingress-ca-domain"
	// certificatesAnnotation marks a cluster whose workload cluster may hold the issuer and the
	// default certificate, until the controller has taken them out again.
	certificatesAnnotation = "kubehenge.dev/certificates"
	// issuerIdentityAnnotation on the workload cluster's issuer tells which CA or ACME server it
	// stands for, and reissueAnnotation that the certificates of the one before are still to be
	// issued again.
	issuerIdentityAnnotation = "kubehenge.dev/issuer-identity"
	reissueAnnotation        = "kubehenge.dev/reissue"

	// dns01Purpose is the user credential of a DNS provider's API.
	dns01Purpose = "dns01"
	// dns01TokenKey is the key a DNS provider's credential is under, in the user credential and in
	// the Secret cert-manager reads.
	dns01TokenKey = "token"

	// What the controller makes in the workload cluster: the ClusterIssuer every certificate of the
	// add-on comes from, and the default certificate of the same name, its ACME account's key, its
	// certificate's Secret, the copy of the cluster's CA and the DNS provider's credential.
	ingressIssuer    = "kubehenge-ingress"
	ingressACMEKey   = "kubehenge-ingress-acme"
	ingressTLSSecret = "kubehenge-ingress-tls" //nolint:gosec // a Secret's name, not a credential
	ingressCASecret  = "kubehenge-ingress-ca"  //nolint:gosec // a Secret's name, not a credential
	dns01Secret      = "kubehenge-dns01"       //nolint:gosec // a Secret's name, not a credential
	// traefikValuesSecret in kube-system holds, under traefikValuesKey, the values RKE2's and k3s's
	// Traefik take from the cluster's certificates; the classes' HelmChartConfig names it.
	traefikValuesSecret = "kubehenge-traefik" //nolint:gosec // a Secret's name, not a credential
	traefikValuesKey    = "values.yaml"

	certManagerGroup = "cert-manager.io"
	traefikGateway   = "traefik-gateway"
	certificateKind  = "Certificate"
	// webListener is the HTTP listener of Traefik's Gateway.
	webListener = "web"
	// issuerAnnotation asks cert-manager's ingress-shim for an Ingress's certificate.
	issuerAnnotation = certManagerGroup + "/cluster-issuer"

	letsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	letsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

	// ingressCAValidity is long enough that nobody has to hand out a new CA while the cluster lives.
	ingressCAValidity = 10 * 365 * 24 * time.Hour
)

// The issuers a cluster's certificates come from: an ACME server proving names by DNS-01 or by
// HTTP-01, or the cluster's own CA.
const (
	IssuerACMEDNS01  = "acme-dns01"
	IssuerACMEHTTP01 = "acme-http01"
	IssuerCA         = "ca"
)

// IngressCAName is the Secret beside a cluster that holds the CA its certificates come from with
// the issuer ca, so that the CA outlives the add-on and can be handed out without reaching the
// workload cluster.
func IngressCAName(cluster string) string {
	return cluster + "-ingress-ca"
}

// Certificates is a cluster's certificates variable.
type Certificates struct {
	Enabled bool   `json:"enabled"`
	Issuer  string `json:"issuer,omitempty"`
	Domain  string `json:"domain,omitempty"`
	ACME    ACME   `json:"acme,omitzero"`
	DNS01   DNS01  `json:"dns01,omitzero"`
}

// ACME is how a cluster's certificates are asked of an ACME server.
type ACME struct {
	Email    string `json:"email,omitempty"`
	Server   string `json:"server,omitempty"`
	Staging  bool   `json:"staging,omitempty"`
	CABundle string `json:"caBundle,omitempty"`
}

// DNS01 is how ACME's DNS-01 challenge is answered: the DNS provider, where its credential is, and
// what the provider needs besides.
type DNS01 struct {
	Provider         string   `json:"provider,omitempty"`
	ExistingSecret   string   `json:"existingSecret,omitempty"`
	SharedCredential bool     `json:"sharedCredential,omitempty"`
	Nameservers      []string `json:"nameservers,omitempty"`
	Route53          Route53  `json:"route53,omitzero"`
	AzureDNS         AzureDNS `json:"azureDNS,omitzero"`
	CloudDNS         CloudDNS `json:"cloudDNS,omitzero"`
	RFC2136          RFC2136  `json:"rfc2136,omitzero"`
	ACMEDNS          ACMEDNS  `json:"acmeDNS,omitzero"`
	Akamai           Akamai   `json:"akamai,omitzero"`
}

// Route53 is Amazon Route 53's settings; the secret access key is the credential.
type Route53 struct {
	Region       string `json:"region,omitempty"`
	AccessKeyID  string `json:"accessKeyID,omitempty"`
	HostedZoneID string `json:"hostedZoneID,omitempty"`
}

// AzureDNS is Azure DNS's settings; the service principal's client secret is the credential.
type AzureDNS struct {
	SubscriptionID    string `json:"subscriptionID,omitempty"`
	ResourceGroupName string `json:"resourceGroupName,omitempty"`
	TenantID          string `json:"tenantID,omitempty"`
	ClientID          string `json:"clientID,omitempty"`
	HostedZoneName    string `json:"hostedZoneName,omitempty"`
	Environment       string `json:"environment,omitempty"`
}

// CloudDNS is Google Cloud DNS's settings; the service account's JSON key is the credential.
type CloudDNS struct {
	Project        string `json:"project,omitempty"`
	HostedZoneName string `json:"hostedZoneName,omitempty"`
}

// RFC2136 is a DNS server's that takes dynamic updates; the TSIG secret is the credential.
type RFC2136 struct {
	Nameserver    string `json:"nameserver,omitempty"`
	TSIGKeyName   string `json:"tsigKeyName,omitempty"`
	TSIGAlgorithm string `json:"tsigAlgorithm,omitempty"`
}

// ACMEDNS is an ACME-DNS server's; the account it registered is the credential.
type ACMEDNS struct {
	Host string `json:"host,omitempty"`
}

// Akamai is Akamai Edge DNS's; its three secrets are in an existing Secret.
type Akamai struct {
	ServiceConsumerDomain string `json:"serviceConsumerDomain,omitempty"`
}

// SharesDNS01Credential reports whether the cluster's certificates take the DNS-01 credential stored
// for every cluster.
func (c *Certificates) SharesDNS01Credential() bool {
	return c.on() && c.issuer() == IssuerACMEDNS01 && c.DNS01.ExistingSecret == "" && c.DNS01.SharedCredential
}

// on reports whether the cluster issues certificates.
func (c *Certificates) on() bool { return c.Enabled }

// issuer is the cluster's issuer, the variable's default when it names none.
func (c *Certificates) issuer() string {
	if c.Issuer == "" {
		return IssuerACMEDNS01
	}
	return c.Issuer
}

// defaultCertificate reports whether the cluster has a certificate for its domain and every name
// one label beneath it, which Traefik serves wherever no other certificate is asked for. HTTP-01
// cannot prove a wildcard, so only DNS-01 and the cluster's CA give one.
func (c *Certificates) defaultCertificate() bool {
	return c.on() && c.Domain != "" && c.issuer() != IssuerACMEHTTP01
}

// covers reports whether the default certificate is valid for host.
func (c *Certificates) covers(host string) bool {
	if !c.defaultCertificate() {
		return false
	}
	host, domain := strings.ToLower(strings.TrimSuffix(host, ".")), strings.ToLower(c.Domain)
	label, under := strings.CutSuffix(host, "."+domain)
	return host == domain || (under && label != "" && !strings.Contains(label, "."))
}

// copiesDNS01Credential reports whether the controller copies a user credential into the workload
// cluster for the DNS provider, rather than cert-manager reading a Secret already there.
func (c *Certificates) copiesDNS01Credential() bool {
	return c.on() && c.issuer() == IssuerACMEDNS01 && c.DNS01.ExistingSecret == ""
}

// CertificatesOf reads a cluster's certificates variable, the zero value if it has none.
func CertificatesOf(cluster *clusterv1.Cluster) (Certificates, error) {
	c, _, err := optionalVariable[Certificates](cluster, certificatesVariable)
	return c, err
}

// certManagerValues are cert-manager's, for whichever add-ons need it. It serves the Gateway API, so
// that an HTTP-01 challenge is answered through Traefik's Gateway and a Gateway's listeners can ask
// for certificates; the controller holds the chart back until the Gateway API is there, since
// cert-manager looks for it only as it starts. With the add-on on, an Ingress that asks for a
// certificate without naming an issuer gets one from the cluster's, and DNS-01's records are looked
// up where the cluster says, for a domain whose public answers differ from those inside.
func certManagerValues(c *Certificates) map[string]any {
	values := map[string]any{
		"crds": map[string]any{"enabled": true},
		// A Secret goes with its Certificate, so that nothing an add-on asked for outlives it.
		"enableCertificateOwnerRef": true,
		configKey:                   map[string]any{"gatewayAPI": map[string]any{"enabled": true}},
	}
	if !c.on() {
		return values
	}
	values["ingressShim"] = map[string]any{
		"defaultIssuerName": ingressIssuer, "defaultIssuerKind": clusterIssuerKind, "defaultIssuerGroup": certManagerGroup,
	}
	if c.issuer() == IssuerACMEDNS01 && len(c.DNS01.Nameservers) > 0 {
		values["dns01RecursiveNameservers"] = strings.Join(c.DNS01.Nameservers, ",")
		values["dns01RecursiveNameserversOnly"] = true
	}
	return values
}

// addonIngress is the TLS and the annotations of an add-on's Ingress for hostname. A certificate
// the cluster names is used as it is; one the default certificate covers is left to Traefik, which
// serves that one where an Ingress names none; any other is asked of the cluster's issuer.
func addonIngress(c *Certificates, release, hostname, secret string) (map[string]any, map[string]any) {
	if secret != "" || !c.on() || c.covers(hostname) {
		return ingressTLS(hostname, secret), nil
	}
	return ingressTLS(hostname, release+"-tls"), map[string]any{issuerAnnotation: ingressIssuer}
}

// traefikCertificateValues give Traefik the default certificate, for routes and Ingresses that name
// none, and its Gateway an HTTPS listener with it, which takes routes from every namespace as its
// HTTP listener does. Traefik serves its own self-signed certificate until cert-manager has issued
// the default one.
func traefikCertificateValues(c *Certificates) map[string]any {
	if !c.defaultCertificate() {
		return nil
	}
	const websecure = 8443
	return map[string]any{
		"tlsStore": map[string]any{defaultKey: map[string]any{"defaultCertificate": map[string]any{"secretName": ingressTLSSecret}}},
		"gateway": map[string]any{"listeners": map[string]any{"websecure": map[string]any{
			"port":            websecure,
			protocolKey:       "HTTPS",
			"mode":            "Terminate",
			"certificateRefs": []any{map[string]any{"kind": secretKind, "name": ingressTLSSecret}},
			"namespacePolicy": map[string]any{"from": "All"},
		}}},
	}
}

// validateCertificates holds the add-ons' host names to the cluster's CA, which signs nothing
// outside its domain.
func validateCertificates(cluster *clusterv1.Cluster) field.ErrorList {
	variables := field.NewPath("spec", "topology", "variables")
	c, err := CertificatesOf(cluster)
	if err != nil {
		return field.ErrorList{field.Invalid(variables.Key(certificatesVariable), certificatesVariable, err.Error())}
	}
	if !c.on() || c.issuer() != IssuerCA {
		return nil
	}
	addons, err := addonsOf(cluster)
	if err != nil {
		return field.ErrorList{field.Invalid(variables, "add-ons", err.Error())}
	}
	var errs field.ErrorList
	for _, h := range []struct{ variable, hostname, secret string }{
		{headlampAddon, addons.Headlamp.Hostname, addons.Headlamp.TLSSecret},
		{monitoringAddon, addons.Monitoring.Hostname, addons.Monitoring.TLSSecret},
		{gitopsVariable, addons.GitOps.Hostname, addons.GitOps.TLSSecret},
	} {
		if h.hostname != "" && h.secret == "" && !inDomain(h.hostname, c.Domain) {
			errs = append(errs, field.Invalid(variables.Key(h.variable).Child("hostname"), h.hostname,
				"the cluster's CA signs only names in "+c.Domain+". Choose one there or set tlsSecret"))
		}
	}
	return errs
}

// inDomain reports whether host is domain or beneath it.
func inDomain(host, domain string) bool {
	host, domain = strings.ToLower(strings.TrimSuffix(host, ".")), strings.ToLower(domain)
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// reconcileCertificates puts the cluster's issuer and default certificate into the workload
// cluster, with what they need from the management cluster, and takes them out again once the
// add-on is off. It reports whether it waits for the cluster's API or for cert-manager.
func (r *Reconciler) reconcileCertificates(ctx context.Context, cluster *clusterv1.Cluster, c *Certificates) (bool, error) {
	_, marked := cluster.Annotations[certificatesAnnotation]
	if !c.on() && !marked {
		return false, nil
	}
	if c.on() && !marked {
		// Marked first, so that whatever is applied from here on is taken out again.
		if err := r.markCertificates(ctx, cluster); err != nil {
			return false, err
		}
	}
	var ca *corev1.Secret
	if c.on() && c.issuer() == IssuerCA {
		var err error
		if ca, err = r.ingressCA(ctx, cluster, c.Domain); err != nil {
			return false, err
		}
	}
	var credential map[string][]byte
	if c.copiesDNS01Credential() {
		var err error
		if credential, _, err = r.dns01Credential(ctx, cluster, c); err != nil {
			return false, err
		}
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		// Without a kubeconfig, nothing was ever applied to take out.
		return c.on(), err
	}
	if !c.on() {
		if err = removeCertificates(ctx, workload.client); err != nil {
			r.Remotes.forget(cluster)
			return false, err
		}
		return false, r.unmarkCertificates(ctx, cluster)
	}
	objects, err := certificateObjects(c, ca, credential)
	if err != nil {
		return false, fmt.Errorf("cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	waiting, err := applyIssued(ctx, workload.client, objects, issuerIdentity(c, ca))
	if err != nil {
		r.Remotes.forget(cluster)
	}
	return waiting, err
}

// applyIssued applies the add-on's objects. When the issuer is another than before, every
// certificate it issued is issued again: cert-manager would keep a certificate whose names and
// issuer's name stay as they are, though another CA signed it. The issuer carries the change until
// that is done, so that an error on the way is picked up again. What a setting switched off leaves
// behind goes: the CA's copy, a credential no longer copied, the default certificate of an issuer
// that gives none.
func applyIssued(ctx context.Context, workload client.Client, objects []*unstructured.Unstructured, identity string) (bool, error) {
	i := slices.IndexFunc(objects, func(o *unstructured.Unstructured) bool { return o.GetKind() == clusterIssuerKind })
	issuer := objects[i]
	reissue, err := reissueNeeded(ctx, workload, identity)
	if err != nil {
		return false, err
	}
	annotations := map[string]string{issuerIdentityAnnotation: identity}
	if reissue {
		annotations[reissueAnnotation] = strconv.FormatBool(true)
	}
	issuer.SetAnnotations(annotations)
	waiting, err := applyCertificates(ctx, workload, objects)
	if err != nil || waiting {
		return waiting, err
	}
	if reissue {
		if err := removeIssued(ctx, workload); err != nil {
			return false, err
		}
		issuer.SetAnnotations(map[string]string{issuerIdentityAnnotation: identity})
		if _, err := applyCertificates(ctx, workload, []*unstructured.Unstructured{issuer}); err != nil {
			return false, err
		}
		ctrl.LoggerFrom(ctx).Info("issuing the cluster's certificates again from its new issuer")
	}
	return false, removeUnwanted(ctx, workload, objects)
}

// issuerIdentity tells issuers apart that sign differently: the cluster's CA by its certificate,
// an ACME issuer by its server.
func issuerIdentity(c *Certificates, ca *corev1.Secret) string {
	if c.issuer() == IssuerCA && ca != nil {
		sum := sha256.Sum256(ca.Data[corev1.TLSCertKey])
		return IssuerCA + "/" + hex.EncodeToString(sum[:8])
	}
	return "acme/" + acmeServer(c.ACME)
}

// reissueNeeded reports whether the issuer in the workload cluster is another than identity, or
// still has certificates to issue again; not while there is none yet.
func reissueNeeded(ctx context.Context, workload client.Client, identity string) (bool, error) {
	issuer := &metav1.PartialObjectMetadata{}
	issuer.SetGroupVersionKind(schema.GroupVersionKind{Group: certManagerGroup, Version: "v1", Kind: clusterIssuerKind})
	err := workload.Get(ctx, client.ObjectKey{Name: ingressIssuer}, issuer)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the cluster's issuer: %w", err)
	}
	was, known := issuer.Annotations[issuerIdentityAnnotation]
	_, pending := issuer.Annotations[reissueAnnotation]
	return pending || (known && was != identity), nil
}

// removeIssued deletes the Secret of every certificate the cluster's issuer issued, in any
// namespace, so that cert-manager issues each again.
func removeIssued(ctx context.Context, workload client.Client) error {
	secrets := &metav1.PartialObjectMetadataList{}
	secrets.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := workload.List(ctx, secrets); err != nil {
		return fmt.Errorf("listing the certificates' Secrets: %w", err)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Annotations[certManagerGroup+"/issuer-name"] != ingressIssuer || s.Annotations[certManagerGroup+"/issuer-kind"] != clusterIssuerKind {
			continue
		}
		s.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(secretKind))
		if err := workload.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing the certificate Secret %s/%s: %w", s.Namespace, s.Name, err)
		}
	}
	return nil
}

func (r *Reconciler) markCertificates(ctx context.Context, cluster *clusterv1.Cluster) error {
	patch := client.MergeFrom(cluster.DeepCopy())
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[certificatesAnnotation] = strconv.FormatBool(true)
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("marking the cluster as holding certificates' objects: %w", err)
	}
	return nil
}

func (r *Reconciler) unmarkCertificates(ctx context.Context, cluster *clusterv1.Cluster) error {
	patch := client.MergeFrom(cluster.DeepCopy())
	delete(cluster.Annotations, certificatesAnnotation)
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("marking the cluster as holding no certificates' objects: %w", err)
	}
	return nil
}

// ingressCA is the cluster's CA for domain, made the first time the cluster needs it and again
// when its domain changes. It is owned by the Cluster, so it stays while the add-on is switched off
// and on, and goes with the cluster.
func (r *Reconciler) ingressCA(ctx context.Context, cluster *clusterv1.Cluster, domain string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: IngressCAName(cluster.Name)}, s)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading the cluster's CA: %w", err)
	}
	if err == nil && s.Annotations[IngressCADomainAnnotation] == domain {
		if _, invalid := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey]); invalid == nil {
			return s, nil
		}
	}
	certPEM, keyPEM, err := newIngressCA(cluster.Name, domain, time.Now())
	if err != nil {
		return nil, err
	}
	s = &corev1.Secret{Namespace: cluster.Namespace, Name: IngressCAName(cluster.Name)}
	mutate := func(got *corev1.Secret) {
		got.Type = corev1.SecretTypeTLS
		got.Data = map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations[IngressCADomainAnnotation] = domain
	}
	if err := applyOwned(ctx, r.Client, cluster, s, mutate); err != nil {
		return nil, err
	}
	ctrl.LoggerFrom(ctx).Info("made the cluster's CA", "domain", domain)
	return s, nil
}

// newIngressCA makes a CA that signs names in domain and nothing else: no other name and no
// address, so that a client trusting it trusts nothing beyond the cluster's own names.
func newIngressCA(cluster, domain string, now time.Time) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("making the cluster CA's key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "Kubehenge CA of cluster " + cluster + ", " + domain},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(ingressCAValidity),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{domain},
		ExcludedIPRanges:            []*net.IPNet{{IP: net.IPv4zero, Mask: net.IPMask(net.IPv4zero.To4())}, {IP: net.IPv6zero, Mask: net.IPMask(net.IPv6zero)}},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("making the cluster's CA: %w", err)
	}
	return encodePair(der, key)
}

// dns01Credential reads the DNS provider's credential the cluster takes: the one stored for every
// cluster when it shares that, else its own; and whether anyone has stored it.
func (r *Reconciler) dns01Credential(ctx context.Context, cluster *clusterv1.Cluster, c *Certificates) (map[string][]byte, bool, error) {
	name := CredentialName(cluster.Name, dns01Purpose)
	if c.SharesDNS01Credential() {
		name = CredentialName("", dns01Purpose)
	}
	s := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, s)
	if apierrors.IsNotFound(err) {
		r.noDNS01Credential(ctx, cluster, name)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading the DNS-01 credential %s: %w", name, err)
	}
	if s.Type != CredentialType || s.Labels[CredentialLabel] != dns01Purpose || len(s.Data[dns01TokenKey]) == 0 {
		r.noDNS01Credential(ctx, cluster, name)
		return nil, false, nil
	}
	return map[string][]byte{dns01TokenKey: s.Data[dns01TokenKey]}, true, nil
}

func (r *Reconciler) noDNS01Credential(ctx context.Context, cluster *clusterv1.Cluster, name string) {
	ctrl.LoggerFrom(ctx).Info("no DNS-01 credential stored, so no certificate can be issued", "credential", name)
	if r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, "NoDNS01Credential", "IssueCertificates",
			"no DNS-01 credential is stored for the cluster (%s): store one, or name an existing Secret", name)
	}
}

// certificateObjects are what the cluster's certificates need in the workload cluster: the CA's or
// the credential's Secret when there is one, the issuer, and the default certificate when the issuer
// gives one, in the order they are applied.
func certificateObjects(c *Certificates, ca *corev1.Secret, credential map[string][]byte) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured
	if ca != nil {
		objects = append(objects, secretObject(ingressCASecret, corev1.SecretTypeTLS, map[string][]byte{
			corev1.TLSCertKey: ca.Data[corev1.TLSCertKey], corev1.TLSPrivateKeyKey: ca.Data[corev1.TLSPrivateKeyKey],
		}))
	}
	if credential != nil {
		objects = append(objects, secretObject(dns01Secret, corev1.SecretTypeOpaque, credential))
	}
	issuer, err := clusterIssuer(c)
	if err != nil {
		return nil, err
	}
	objects = append(objects, issuer)
	if c.defaultCertificate() {
		objects = append(objects, defaultCertificate(c.Domain))
	}
	return objects, nil
}

func secretObject(name string, t corev1.SecretType, data map[string][]byte) *unstructured.Unstructured {
	encoded := make(map[string]any, len(data))
	for k, v := range data {
		encoded[k] = base64.StdEncoding.EncodeToString(v)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		apiVersionKey: "v1", "kind": secretKind,
		"metadata": map[string]any{"name": name, "namespace": certManagerAddon},
		"type":     string(t),
		"data":     encoded,
	}}
}

// clusterIssuer is the ClusterIssuer the cluster's certificates come from.
func clusterIssuer(c *Certificates) (*unstructured.Unstructured, error) {
	var spec map[string]any
	if c.issuer() == IssuerCA {
		spec = map[string]any{"ca": map[string]any{"secretName": ingressCASecret}}
	} else {
		solver, err := acmeSolver(c)
		if err != nil {
			return nil, err
		}
		acme := map[string]any{
			"server":              acmeServer(c.ACME),
			"privateKeySecretRef": map[string]any{"name": ingressACMEKey},
			"solvers":             []any{solver},
		}
		if c.ACME.Email != "" {
			acme["email"] = c.ACME.Email
		}
		if c.ACME.CABundle != "" {
			acme["caBundle"] = base64.StdEncoding.EncodeToString([]byte(c.ACME.CABundle))
		}
		spec = map[string]any{"acme": acme}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		apiVersionKey: certManagerAPI, "kind": clusterIssuerKind,
		"metadata": map[string]any{"name": ingressIssuer},
		"spec":     spec,
	}}, nil
}

// acmeServer is the ACME directory the cluster's certificates are asked of.
func acmeServer(a ACME) string {
	switch {
	case a.Server != "":
		return a.Server
	case a.Staging:
		return letsEncryptStaging
	}
	return letsEncrypt
}

// acmeSolver answers the ACME server's challenge: HTTP-01 through Traefik's Gateway, whose HTTP
// listener takes the solver's route in any namespace, or DNS-01 through the provider's API.
func acmeSolver(c *Certificates) (map[string]any, error) {
	if c.issuer() == IssuerACMEHTTP01 {
		return map[string]any{"http01": map[string]any{"gatewayHTTPRoute": map[string]any{"parentRefs": []any{map[string]any{
			"kind": "Gateway", "name": traefikGateway, "namespace": metav1.NamespaceSystem, "sectionName": webListener,
		}}}}}, nil
	}
	d := c.DNS01
	secret := d.ExistingSecret
	if secret == "" {
		secret = dns01Secret
	}
	ref := func(key string) map[string]any { return map[string]any{"name": secret, "key": key} }
	token := ref(dns01TokenKey)
	optional := func(m map[string]any, key, value string) map[string]any {
		if value != "" {
			m[key] = value
		}
		return m
	}
	var provider map[string]any
	switch d.Provider {
	case "cloudflare":
		provider = map[string]any{"cloudflare": map[string]any{"apiTokenSecretRef": token}}
	case "digitalocean":
		provider = map[string]any{"digitalocean": map[string]any{"tokenSecretRef": token}}
	case "route53":
		provider = map[string]any{"route53": optional(map[string]any{
			regionKey: d.Route53.Region, "accessKeyID": d.Route53.AccessKeyID, "secretAccessKeySecretRef": token,
		}, "hostedZoneID", d.Route53.HostedZoneID)}
	case "azuredns":
		a := d.AzureDNS
		provider = map[string]any{"azureDNS": optional(optional(map[string]any{
			"subscriptionID": a.SubscriptionID, "resourceGroupName": a.ResourceGroupName,
			"tenantID": a.TenantID, "clientID": a.ClientID, "clientSecretSecretRef": token,
		}, "hostedZoneName", a.HostedZoneName), "environment", a.Environment)}
	case "clouddns":
		provider = map[string]any{"cloudDNS": optional(map[string]any{
			"project": d.CloudDNS.Project, "serviceAccountSecretRef": token,
		}, "hostedZoneName", d.CloudDNS.HostedZoneName)}
	case "rfc2136":
		algorithm := d.RFC2136.TSIGAlgorithm
		if algorithm == "" {
			algorithm = "HMACSHA256"
		}
		provider = map[string]any{"rfc2136": map[string]any{
			"nameserver": d.RFC2136.Nameserver, "tsigKeyName": d.RFC2136.TSIGKeyName,
			"tsigAlgorithm": algorithm, "tsigSecretSecretRef": token,
		}}
	case "acmedns":
		provider = map[string]any{"acmeDNS": map[string]any{"host": d.ACMEDNS.Host, "accountSecretRef": token}}
	case "akamai":
		if d.ExistingSecret == "" {
			return nil, errors.New("only an existing Secret holds Akamai's three secrets (certificates.dns01.existingSecret)")
		}
		provider = map[string]any{"akamai": map[string]any{
			"serviceConsumerDomain": d.Akamai.ServiceConsumerDomain,
			"clientTokenSecretRef":  ref("clientToken"),
			"clientSecretSecretRef": ref("clientSecret"),
			"accessTokenSecretRef":  ref("accessToken"),
		}}
	default:
		return nil, fmt.Errorf("no DNS-01 provider %q", d.Provider)
	}
	return map[string]any{"dns01": provider}, nil
}

// defaultCertificate is the certificate for domain and every name one label beneath it, in Traefik's
// namespace, where its default TLS store and its Gateway read it.
func defaultCertificate(domain string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		apiVersionKey: certManagerAPI, "kind": certificateKind,
		"metadata": map[string]any{"name": ingressIssuer, "namespace": metav1.NamespaceSystem},
		"spec": map[string]any{
			"secretName": ingressTLSSecret,
			"dnsNames":   []any{domain, "*." + domain},
			"issuerRef":  map[string]any{"name": ingressIssuer, "kind": clusterIssuerKind, "group": certManagerGroup},
			// int64, the only integer an unstructured object can be copied with.
			"privateKey": map[string]any{"algorithm": "ECDSA", "size": int64(keyBits), "rotationPolicy": "Always"},
		},
	}}
}

// applyCertificates applies the objects in order, and reports whether it waits for cert-manager:
// its CRDs, its namespace, or its webhook, which checks the issuer and the certificate.
func applyCertificates(ctx context.Context, workload client.Client, objects []*unstructured.Unstructured) (bool, error) {
	for _, o := range objects {
		// A copy, since the client writes the server's answer back into what it applies, and an
		// object carrying the managedFields that answer holds cannot be applied again.
		err := workload.Apply(ctx, client.ApplyConfigurationFromUnstructured(o.DeepCopy()), client.FieldOwner(fieldOwner), client.ForceOwnership)
		if meta.IsNoMatchError(err) || apierrors.IsInternalError(err) || apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("applying %s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return false, nil
}

// certificateKinds are everything certificateObjects can make, as far as deleting it goes; the
// issued certificate's Secret and the ACME account's key go too, which cert-manager leaves.
func certificateKinds() []*unstructured.Unstructured {
	object := func(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{apiVersionKey: apiVersion, "kind": kind}}
		u.SetNamespace(namespace)
		u.SetName(name)
		return u
	}
	return []*unstructured.Unstructured{
		object(certManagerAPI, certificateKind, metav1.NamespaceSystem, ingressIssuer),
		object("v1", secretKind, metav1.NamespaceSystem, ingressTLSSecret),
		object(certManagerAPI, clusterIssuerKind, "", ingressIssuer),
		object("v1", secretKind, certManagerAddon, ingressACMEKey),
		object("v1", secretKind, certManagerAddon, ingressCASecret),
		object("v1", secretKind, certManagerAddon, dns01Secret),
	}
}

// removeCertificates deletes every object of the add-on from the workload cluster.
func removeCertificates(ctx context.Context, workload client.Client) error {
	return deleteCertificateObjects(ctx, workload, certificateKinds())
}

// removeUnwanted deletes the add-on's objects that the cluster's settings no longer make. The ACME
// account's key stays while an ACME issuer is there, and the issued Secret with its certificate.
func removeUnwanted(ctx context.Context, workload client.Client, wanted []*unstructured.Unstructured) error {
	kept := map[string]bool{}
	for _, o := range wanted {
		kept[o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName()] = true
		switch {
		case o.GetKind() == certificateKind:
			kept[secretKind+"/"+metav1.NamespaceSystem+"/"+ingressTLSSecret] = true
		case o.GetKind() == clusterIssuerKind && isACME(o):
			kept[secretKind+"/"+certManagerAddon+"/"+ingressACMEKey] = true
		}
	}
	var unwanted []*unstructured.Unstructured
	for _, o := range certificateKinds() {
		if !kept[o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName()] {
			unwanted = append(unwanted, o)
		}
	}
	return deleteCertificateObjects(ctx, workload, unwanted)
}

func isACME(issuer *unstructured.Unstructured) bool {
	_, found, err := unstructured.NestedFieldNoCopy(issuer.Object, "spec", "acme")
	return err == nil && found
}

func deleteCertificateObjects(ctx context.Context, workload client.Client, objects []*unstructured.Unstructured) error {
	for _, o := range objects {
		err := workload.Delete(ctx, o)
		if err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return fmt.Errorf("removing %s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}
