package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// certificatesVariable issues certificates for a cluster's ingress and Gateway.
const certificatesVariable = "certificates"

// sharesDNS01 reports whether a certificates value takes the DNS-01 credential stored for every
// cluster; a value that cannot be read takes nothing.
func sharesDNS01(raw json.RawMessage) bool {
	if raw == nil || string(raw) == jsonNull {
		return false
	}
	var c provisioner.Certificates
	return json.Unmarshal(raw, &c) == nil && c.SharesDNS01Credential()
}

// checkSharedDNS01 checks a certificates value that switches the credential stored for every
// cluster on, from current, which held, the user's privileges on path, have to allow; it answers
// the request when they do not. That credential can write the zone's records for every cluster that
// uses it, so handing it to one more cluster needs what trusting another identity provider does.
func checkSharedDNS01(w http.ResponseWriter, u user, path string, held map[string]bool, raw, current json.RawMessage) bool {
	if !sharesDNS01(raw) || sharesDNS01(current) || held[privKubeconfig] {
		return true
	}
	writeError(w, http.StatusForbidden, fmt.Sprintf(
		"%s needs %s on %s to give the cluster the DNS-01 credential stored for every cluster, which can write the zone's records for every cluster that uses it",
		u.ticket.Username, privKubeconfig, path))
	return false
}

// ingressCA hands out the certificate of the CA a cluster's certificates come from with the issuer
// ca, for its users to trust; never its key. Naming privSee keeps to the interface's namespace,
// whose Secrets alone it may read.
func (s *Server) ingressCA(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privSee)
	if !ok {
		return
	}
	secret := &corev1.Secret{}
	err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: provisioner.IngressCAName(c.Name)}, secret)
	if apierrors.IsNotFound(err) || err == nil && len(secret.Data[corev1.TLSCertKey]) == 0 {
		writeError(w, http.StatusNotFound, "the cluster has no CA yet")
		return
	}
	if err != nil {
		s.kubeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", c.Name+"-ca.crt"))
	_, _ = w.Write(secret.Data[corev1.TLSCertKey])
}

// stampCredentialsStored marks the clusters given as using a credential that just changed, so that
// the controller takes it to them at once. The credential itself is stored by then: a cluster left
// unmarked gets it at the controller's next resync, so a failure is only logged.
func (s *Server) stampCredentialsStored(r *http.Request, u user, clusters ...*clusterv1.Cluster) {
	at := time.Now().UTC().Format(time.RFC3339)
	for _, c := range clusters {
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{provisioner.CredentialsStoredAnnotation: at}}})
		if err == nil {
			err = u.kube.Patch(r.Context(), c, client.RawPatch(types.MergePatchType, patch))
		}
		if err != nil {
			s.Log.Warn("could not mark a cluster as using a changed credential; it gets it at the controller's next resync",
				"cluster", c.Namespace+"/"+c.Name, "error", err)
		}
	}
}

// sharingDNS01 lists the clusters of the interface's namespace that take the DNS-01 credential
// stored for every cluster.
func (s *Server) sharingDNS01(r *http.Request, u user) []*clusterv1.Cluster {
	var list clusterv1.ClusterList
	if err := u.kube.List(r.Context(), &list, client.InNamespace(s.Namespace)); err != nil {
		s.Log.Warn("could not list the clusters using the DNS-01 credential for every cluster; they get it at the controller's next resync", "error", err)
		return nil
	}
	var sharing []*clusterv1.Cluster
	for i := range list.Items {
		c := &list.Items[i]
		if !provisioner.Owns(c, s.ClassNamespace) {
			continue
		}
		if certs, err := provisioner.CertificatesOf(c); err == nil && certs.SharesDNS01Credential() {
			sharing = append(sharing, c)
		}
	}
	return sharing
}
