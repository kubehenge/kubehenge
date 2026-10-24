package provisioner

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// ServingCertApprover approves the serving certificates the kubelets of kubeadm and Talos clusters
// ask the cluster CA for. Kubernetes approves none by itself: a kubelet may name any address, and only
// what made the machine knows which are its own. RKE2 and k3s sign their kubelets' certificates
// themselves.
type ServingCertApprover struct {
	Client         client.Client
	ClassNamespace string
	// Remotes reaches the workload clusters.
	Remotes *Remotes
}

// SetupWithManager registers the approver for Clusters.
func (a *ServingCertApprover) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("kubelet-serving-certificates").
		Complete(a)
	if err != nil {
		return fmt.Errorf("setting up the kubelet serving certificate approver: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler. Requests are looked for again every waitInterval, as
// the workload cluster is not watched: until its certificate comes, a kubelet serves nothing, and
// the API server reaches it for logs, exec and Cluster API's etcd checks.
func (a *ServingCertApprover) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := a.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	d := distributionOf(cluster)
	if !Owns(cluster, a.ClassNamespace) || d != templates.Kubeadm && d != templates.Talos || !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	workload, found, err := a.Remotes.get(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	machines := &clusterv1.MachineList{}
	err = a.Client.List(ctx, machines, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing the cluster's machines: %w", err)
	}
	if err := approveServing(ctx, workload.client, machines.Items); err != nil {
		a.Remotes.forget(cluster)
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: waitInterval}, nil
}

// approveServing approves every pending kubelet serving certificate that names only its node and
// the node's Machine's addresses. One that names more stays pending, for an administrator to see.
func approveServing(ctx context.Context, workload client.Client, machines []clusterv1.Machine) error {
	csrs := &certificatesv1.CertificateSigningRequestList{}
	if err := workload.List(ctx, csrs); err != nil {
		return fmt.Errorf("listing certificate signing requests: %w", err)
	}
	for i := range csrs.Items {
		csr := &csrs.Items[i]
		if csr.Spec.SignerName != certificatesv1.KubeletServingSignerName || len(csr.Status.Conditions) > 0 {
			continue
		}
		if why := servingRefusal(csr, machines); why != "" {
			ctrl.LoggerFrom(ctx).Info("not approving a kubelet's serving certificate", "request", csr.Name, "user", csr.Spec.Username, "reason", why)
			continue
		}
		csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
			Type:    certificatesv1.CertificateApproved,
			Status:  corev1.ConditionTrue,
			Reason:  "KubehengeApproved",
			Message: "names only the node and its Machine's addresses",
		})
		if err := workload.SubResource("approval").Update(ctx, csr); err != nil {
			return fmt.Errorf("approving %s: %w", csr.Name, err)
		}
	}
	return nil
}

// servingUsages are those a kubelet asks for, of which server authentication must be one.
var servingUsages = []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageServerAuth}

// servingRefusal says why a kubelet serving certificate request is not approved, or "".
func servingRefusal(csr *certificatesv1.CertificateSigningRequest, machines []clusterv1.Machine) string {
	node, req, why := servingRequestOf(csr)
	if why != "" {
		return why
	}
	return addressRefusal(node, req, machines)
}

// servingRequestOf reads the node a request is for and its certificate request, or says why it
// is not one a kubelet makes for itself.
func servingRequestOf(csr *certificatesv1.CertificateSigningRequest) (string, *x509.CertificateRequest, string) {
	node, ok := strings.CutPrefix(csr.Spec.Username, "system:node:")
	if !ok || node == "" || !slices.Contains(csr.Spec.Groups, "system:nodes") {
		return "", nil, "not asked for by a node"
	}
	if !slices.Contains(csr.Spec.Usages, certificatesv1.UsageServerAuth) ||
		slices.ContainsFunc(csr.Spec.Usages, func(u certificatesv1.KeyUsage) bool { return !slices.Contains(servingUsages, u) }) {
		return "", nil, fmt.Sprintf("usages %v", csr.Spec.Usages)
	}
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "", nil, "no certificate request"
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err == nil {
		err = req.CheckSignature()
	}
	if err != nil {
		return "", nil, err.Error()
	}
	if req.Subject.CommonName != csr.Spec.Username || !slices.Equal(req.Subject.Organization, []string{"system:nodes"}) {
		return "", nil, "the certificate names someone else than the node: " + req.Subject.String()
	}
	if len(req.EmailAddresses) > 0 || len(req.URIs) > 0 || len(req.DNSNames)+len(req.IPAddresses) == 0 {
		return "", nil, "the certificate names no address, or more than addresses"
	}
	return node, req, ""
}

// addressRefusal says which of a certificate's names and addresses are not those of the node's
// Machine, or "".
func addressRefusal(node string, req *x509.CertificateRequest, machines []clusterv1.Machine) string {
	i := slices.IndexFunc(machines, func(m clusterv1.Machine) bool {
		return m.DeletionTimestamp.IsZero() && (m.Status.NodeRef.Name == node || m.Status.NodeRef.Name == "" && m.Name == node)
	})
	if i < 0 {
		return "no Machine of the cluster is node " + node
	}
	names, ips := []string{node}, []netip.Addr{}
	for _, a := range machines[i].Status.Addresses {
		switch a.Type {
		case clusterv1.MachineHostName, clusterv1.MachineInternalDNS, clusterv1.MachineExternalDNS:
			names = append(names, a.Address)
		case clusterv1.MachineInternalIP, clusterv1.MachineExternalIP:
			if ip, err := netip.ParseAddr(a.Address); err == nil {
				ips = append(ips, ip.Unmap())
			}
		}
	}
	for _, name := range req.DNSNames {
		if !slices.Contains(names, name) {
			return "name " + name + " is not the Machine's"
		}
	}
	for _, raw := range req.IPAddresses {
		ip, ok := netip.AddrFromSlice(raw)
		if !ok || !slices.Contains(ips, ip.Unmap()) {
			return "address " + raw.String() + " is not the Machine's"
		}
	}
	return ""
}
