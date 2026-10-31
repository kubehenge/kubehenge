package provisioner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"net/url"
	"strings"
	"testing"

	certificatesv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// servingRequest is what a kubelet named node asks for, as it asks: its node's name and addresses.
func servingRequest(t *testing.T, name, node string, edit func(*x509.CertificateRequest, *certificatesv1.CertificateSigningRequestSpec)) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: "system:node:" + node, Organization: []string{"system:nodes"}},
		DNSNames:    []string{node},
		IPAddresses: []net.IP{net.ParseIP("10.0.0.11")},
	}
	spec := certificatesv1.CertificateSigningRequestSpec{
		SignerName: certificatesv1.KubeletServingSignerName,
		Username:   "system:node:" + node,
		Groups:     []string{"system:nodes", "system:authenticated"},
		Usages:     []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageServerAuth},
	}
	if edit != nil {
		edit(tmpl, &spec)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Request == nil {
		spec.Request = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	}
	return &certificatesv1.CertificateSigningRequest{Name: name, Spec: spec}
}

func servingMachines() []clusterv1.Machine {
	return []clusterv1.Machine{
		{
			Name: "web-control-plane-abc",
			Status: clusterv1.MachineStatus{
				NodeRef: clusterv1.MachineNodeReference{Name: "web-control-plane-abc"},
				Addresses: []clusterv1.MachineAddress{
					{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"},
					{Type: clusterv1.MachineHostName, Address: "web-control-plane-abc"},
				},
			},
		},
		// Its node has yet to be seen. Its kubelet asks as soon as it registers.
		{
			Name:   "web-md-0-def",
			Status: clusterv1.MachineStatus{Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.12"}}},
		},
	}
}

func TestServingRefusal(t *testing.T) {
	t.Parallel()
	const cp = "web-control-plane-abc"
	gone := servingMachines()
	now := metav1.Now()
	gone[0].DeletionTimestamp = &now
	for _, tc := range []struct {
		name     string
		node     string
		edit     func(*x509.CertificateRequest, *certificatesv1.CertificateSigningRequestSpec)
		machines []clusterv1.Machine
		refused  string
	}{
		{name: "the node's own name and address", node: cp},
		{name: "a node whose Machine has no node yet", node: "web-md-0-def", edit: func(r *x509.CertificateRequest, _ *certificatesv1.CertificateSigningRequestSpec) {
			r.DNSNames, r.IPAddresses = nil, []net.IP{net.ParseIP("10.0.0.12")}
		}},
		{name: "another node's address", node: cp, refused: "address 10.0.0.12", edit: func(r *x509.CertificateRequest, _ *certificatesv1.CertificateSigningRequestSpec) {
			r.IPAddresses = append(r.IPAddresses, net.ParseIP("10.0.0.12"))
		}},
		{name: "a name that is not the node's", node: cp, refused: "name kubernetes.default", edit: func(r *x509.CertificateRequest, _ *certificatesv1.CertificateSigningRequestSpec) {
			r.DNSNames = append(r.DNSNames, "kubernetes.default")
		}},
		{name: "a URI", node: cp, refused: "more than addresses", edit: func(r *x509.CertificateRequest, _ *certificatesv1.CertificateSigningRequestSpec) {
			r.URIs = []*url.URL{{Scheme: "spiffe", Host: "cluster.local"}}
		}},
		{name: "no address", node: cp, refused: "names no address", edit: func(r *x509.CertificateRequest, _ *certificatesv1.CertificateSigningRequestSpec) {
			r.DNSNames, r.IPAddresses = nil, nil
		}},
		{name: "another node's name in the certificate", node: cp, refused: "someone else", edit: func(r *x509.CertificateRequest, _ *certificatesv1.CertificateSigningRequestSpec) {
			r.Subject.CommonName = "system:node:web-md-0-def"
		}},
		{name: "a group in the certificate", node: cp, refused: "someone else", edit: func(r *x509.CertificateRequest, _ *certificatesv1.CertificateSigningRequestSpec) {
			r.Subject.Organization = []string{"system:masters"}
		}},
		{name: "a user that is not a node", node: cp, refused: "not asked for by a node", edit: func(_ *x509.CertificateRequest, s *certificatesv1.CertificateSigningRequestSpec) {
			s.Username = "admin"
		}},
		{name: "a node outside the nodes group", node: cp, refused: "not asked for by a node", edit: func(_ *x509.CertificateRequest, s *certificatesv1.CertificateSigningRequestSpec) {
			s.Groups = []string{"system:authenticated"}
		}},
		{name: "client authentication", node: cp, refused: "usages", edit: func(_ *x509.CertificateRequest, s *certificatesv1.CertificateSigningRequestSpec) {
			s.Usages = append(s.Usages, certificatesv1.UsageClientAuth)
		}},
		{name: "no server authentication", node: cp, refused: "usages", edit: func(_ *x509.CertificateRequest, s *certificatesv1.CertificateSigningRequestSpec) {
			s.Usages = []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature}
		}},
		{name: "not a certificate request", node: cp, refused: "no certificate request", edit: func(_ *x509.CertificateRequest, s *certificatesv1.CertificateSigningRequestSpec) {
			s.Request = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})
		}},
		{name: "a node of no Machine", node: "elsewhere", refused: "no Machine", edit: func(r *x509.CertificateRequest, _ *certificatesv1.CertificateSigningRequestSpec) {
			r.DNSNames = []string{"elsewhere"}
		}},
		{name: "a Machine being deleted", node: cp, machines: gone, refused: "no Machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			machines := tc.machines
			if machines == nil {
				machines = servingMachines()
			}
			got := servingRefusal(servingRequest(t, "csr", tc.node, tc.edit), machines)
			if tc.refused == "" && got != "" || !strings.Contains(got, tc.refused) {
				t.Errorf("refusal %q, want one with %q", got, tc.refused)
			}
		})
	}
}

func TestApproveServing(t *testing.T) {
	t.Parallel()
	own := servingRequest(t, "own", "web-control-plane-abc", nil)
	foreign := servingRequest(t, "foreign", "web-control-plane-abc", func(r *x509.CertificateRequest, _ *certificatesv1.CertificateSigningRequestSpec) {
		r.IPAddresses = []net.IP{net.ParseIP("192.0.2.1")}
	})
	kubeletClient := servingRequest(t, "client", "web-control-plane-abc", func(_ *x509.CertificateRequest, s *certificatesv1.CertificateSigningRequestSpec) {
		s.SignerName = certificatesv1.KubeAPIServerClientKubeletSignerName
	})
	denied := servingRequest(t, "denied", "web-control-plane-abc", nil)
	denied.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{Type: certificatesv1.CertificateDenied, Status: "True"}}
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).
		WithObjects(own, foreign, kubeletClient, denied).
		WithStatusSubresource(&certificatesv1.CertificateSigningRequest{}).
		Build()

	if err := approveServing(context.Background(), workload, servingMachines()); err != nil {
		t.Fatal(err)
	}
	for name, approved := range map[string]bool{"own": true, "foreign": false, "client": false, "denied": false} {
		got := &certificatesv1.CertificateSigningRequest{}
		if err := workload.Get(context.Background(), clientKey(name), got); err != nil {
			t.Fatal(err)
		}
		is := len(got.Status.Conditions) > 0 && got.Status.Conditions[len(got.Status.Conditions)-1].Type == certificatesv1.CertificateApproved
		if is != approved {
			t.Errorf("%s approved %t, want %t: %v", name, is, approved, got.Status.Conditions)
		}
	}
}

func clientKey(name string) client.ObjectKey { return client.ObjectKey{Name: name} }
