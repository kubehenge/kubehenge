package ui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	ownDNS01    = `{"enabled":true,"domain":"apps.example","acme":{"email":"ops@example.org"},"dns01":{"provider":"cloudflare"}}`
	sharedDNS01 = `{"enabled":true,"domain":"apps.example","acme":{"email":"ops@example.org"},"dns01":{"provider":"cloudflare","sharedCredential":true}}`
)

func withCertificates(c *clusterv1.Cluster, value string) *clusterv1.Cluster {
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: certificatesVariable, Value: apiextensionsv1.JSON{Raw: []byte(value)},
	})
	return c
}

func stampOf(t *testing.T, h *harness, name string) string {
	t.Helper()
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, c); err != nil {
		t.Fatal(err)
	}
	return c.Annotations[provisioner.CredentialsStoredAnnotation]
}

func TestIngressCA(t *testing.T) {
	t.Parallel()
	const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	ca := &corev1.Secret{
		Namespace: "default", Name: provisioner.IngressCAName("one"), Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: []byte(pem), corev1.TLSPrivateKeyKey: []byte("the CA's key")},
	}
	unsigned := &corev1.Secret{Namespace: "default", Name: provisioner.IngressCAName("two"), Data: map[string][]byte{}}
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"), cluster("default", "three"), cluster("team-a", "elsewhere"), ca, unsigned)
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	rec := viewer.do("GET", "/api/clusters/default/one/certificates/ca", "")
	expect(t, rec, http.StatusOK)
	if rec.Body.String() != pem || rec.Header().Get("Content-Type") != "application/x-pem-file" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="one-ca.crt"` {
		t.Fatalf("CA = %v %q", rec.Header(), rec.Body)
	}
	expect(t, viewer.do("GET", "/api/clusters/default/two/certificates/ca", ""), http.StatusNotFound)

	admin := h.browser()
	admin.signIn("admin@pve")
	for _, name := range []string{"two", "three"} {
		rec = admin.do("GET", "/api/clusters/default/"+name+"/certificates/ca", "")
		expect(t, rec, http.StatusNotFound)
		if !strings.Contains(rec.Body.String(), "no CA yet") {
			t.Fatalf("%s: %s", name, rec.Body)
		}
	}
	expect(t, admin.do("GET", "/api/clusters/team-a/elsewhere/certificates/ca", ""), http.StatusForbidden)
}

func TestSharedDNS01NeedsTheKubeconfigRight(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"))
	create := func(name, certificates string) string {
		return strings.Replace(strings.Replace(createBody, `"web"`, `"`+name+`"`, 1), `"storage":`, `"certificates":`+certificates+`,"storage":`, 1)
	}
	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("POST", "/api/clusters", create("a", ownDNS01)), http.StatusCreated)
	// Another issuer, or a Secret of the cluster's own, takes nothing stored for every cluster.
	expect(t, dev.do("POST", "/api/clusters", create("b", strings.Replace(sharedDNS01, `"enabled":true`, `"enabled":true,"issuer":"ca"`, 1))), http.StatusCreated)
	expect(t, dev.do("POST", "/api/clusters", create("c", strings.Replace(sharedDNS01, `"provider"`, `"existingSecret":"dns","provider"`, 1))), http.StatusCreated)
	rec := dev.do("POST", "/api/clusters", create("d", sharedDNS01))
	expect(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "Sys.Modify on /pool/kubehenge") {
		t.Fatalf("refusal = %s", rec.Body)
	}
	admin := h.browser()
	admin.signIn("admin@pve")
	expect(t, admin.do("POST", "/api/clusters", create("e", sharedDNS01)), http.StatusCreated)

	// dev holds Sys.Modify on one's pool, not on two's.
	change := `{"variables":{"certificates":` + sharedDNS01 + `}}`
	expect(t, dev.do("PATCH", "/api/clusters/default/one", change), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", change), http.StatusForbidden)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"certificates":`+ownDNS01+`}}`), http.StatusOK)

	// Keeping what an administrator gave the cluster, or giving it up, needs nothing more. Taking it
	// back once given up does.
	expect(t, admin.do("PATCH", "/api/clusters/default/two", change), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"certificates":`+strings.Replace(sharedDNS01, "apps.example", "web.example", 1)+`}}`), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"certificates":`+strings.Replace(sharedDNS01, `"enabled":true`, `"enabled":false`, 1)+`}}`), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", change), http.StatusForbidden)
}

// Storing or removing a credential marks the clusters that use it, so the controller takes it to
// them at once.
func TestCredentialChangesMarkTheirClusters(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		withCertificates(cluster("default", "one"), ownDNS01),
		withCertificates(cluster("default", "two"), sharedDNS01),
		withCertificates(cluster("default", "three"), ownDNS01))
	dev := h.browser()
	dev.signIn("dev@pve")
	before := time.Now().UTC().Add(-time.Second)
	expect(t, dev.do("PUT", "/api/clusters/default/one/credentials/dns01", `{"values":{"token":"zone-token"}}`), http.StatusOK)
	at, err := time.Parse(time.RFC3339, stampOf(t, h, "one"))
	if err != nil || at.Before(before) {
		t.Fatalf("one's mark: %v %v", at, err)
	}
	if stampOf(t, h, "two") != "" || stampOf(t, h, "three") != "" {
		t.Fatal("storing one's credential marked another cluster")
	}

	admin := h.browser()
	admin.signIn("admin@pve")
	expect(t, admin.do("PUT", "/api/credentials/dns01", `{"values":{"token":"every-zone"}}`), http.StatusOK)
	if stampOf(t, h, "two") == "" || stampOf(t, h, "three") != "" {
		t.Fatalf("after storing the shared credential: two %q, three %q", stampOf(t, h, "two"), stampOf(t, h, "three"))
	}
	expect(t, admin.do("DELETE", "/api/clusters/default/three/credentials/dns01", ""), http.StatusNoContent)
	if stampOf(t, h, "three") != "" {
		t.Fatal("removing a credential never stored marked the cluster")
	}
}
