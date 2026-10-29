package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// maxCredentialValue bounds one value of a credential: a token, a key or a password, or a
// provider's key file.
const maxCredentialValue = 16 << 10

// credentialView tells what is known of a stored credential, never its values: the API takes them
// and gives none back.
type credentialView struct {
	Purpose string `json:"purpose"`
	Set     bool   `json:"set"`
	// SetBy and SetAt are the Proxmox VE user who last stored it, and when.
	SetBy string     `json:"setBy,omitempty"`
	SetAt *time.Time `json:"setAt,omitempty"`
	// CanChange tells whether the user may store, replace or remove it.
	CanChange bool `json:"canChange"`
}

// credentialTarget is where one credential lives: a cluster's, or with no cluster, the one that
// serves every cluster.
type credentialTarget struct {
	purpose provisioner.CredentialPurpose
	key     client.ObjectKey
	cluster *clusterv1.Cluster
	// canChange tells whether the user may store, replace or remove it.
	canChange bool
}

func (t *credentialTarget) describe() string {
	if t.cluster == nil {
		return t.purpose.Name + " for every cluster"
	}
	return t.purpose.Name + " of " + t.cluster.Namespace + "/" + t.cluster.Name
}

// clusterCredentialTarget finds the request's cluster and credential purpose, checking privs on the
// cluster's pool; nil when it has answered the request. Seeing a credential needs what seeing the
// cluster does; changing it needs Sys.Modify on the cluster's pool, as its admin kubeconfig does,
// since a credential reaches as far into what the cluster runs.
func (s *Server) clusterCredentialTarget(w http.ResponseWriter, r *http.Request, u user, privs ...string) *credentialTarget {
	c, access, ok := s.cluster(w, r, u, privs...)
	if !ok {
		return nil
	}
	purpose, known := provisioner.CredentialPurposeNamed(r.PathValue("purpose"))
	if !known {
		writeError(w, http.StatusNotFound, "no such credential")
		return nil
	}
	// The cluster's name is the credential's label, which holds at most 63 characters.
	if len(validation.IsValidLabelValue(c.Name)) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "the interface stores credentials only for clusters whose names have at most 63 characters")
		return nil
	}
	return &credentialTarget{
		purpose:   purpose,
		key:       client.ObjectKey{Namespace: c.Namespace, Name: provisioner.CredentialName(c.Name, purpose.Name)},
		cluster:   c,
		canChange: access.held.Has(privKubeconfig),
	}
}

// changedClusterCredential is the cluster credential a request stores, replaces or removes.
func (s *Server) changedClusterCredential(w http.ResponseWriter, r *http.Request, u user) *credentialTarget {
	t := s.clusterCredentialTarget(w, r, u, privSee, privKubeconfig)
	if t != nil && !t.cluster.DeletionTimestamp.IsZero() {
		writeError(w, http.StatusConflict, "the cluster is being deleted")
		return nil
	}
	return t
}

// applianceCredentialTarget is the request's credential for every cluster, which only an
// administrator sees and changes, as the appliance's other settings; nil when it has answered the
// request.
func (s *Server) applianceCredentialTarget(w http.ResponseWriter, r *http.Request, u user) *credentialTarget {
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return nil
	}
	purpose, known := provisioner.CredentialPurposeNamed(r.PathValue("purpose"))
	if !known || !purpose.Appliance {
		writeError(w, http.StatusNotFound, "no such credential")
		return nil
	}
	return &credentialTarget{
		purpose:   purpose,
		key:       client.ObjectKey{Namespace: u.namespaces[0], Name: provisioner.CredentialName("", purpose.Name)},
		canChange: true,
	}
}

// Naming privSee has s.cluster refuse a cluster outside the namespaces the interface manages,
// whose Secrets it may not read.
func (s *Server) clusterCredential(w http.ResponseWriter, r *http.Request, u user) {
	if t := s.clusterCredentialTarget(w, r, u, privSee); t != nil {
		s.showCredential(w, r, u, t)
	}
}

func (s *Server) storeClusterCredential(w http.ResponseWriter, r *http.Request, u user) {
	if t := s.changedClusterCredential(w, r, u); t != nil {
		s.storeCredential(w, r, u, t)
	}
}

func (s *Server) removeClusterCredential(w http.ResponseWriter, r *http.Request, u user) {
	if t := s.changedClusterCredential(w, r, u); t != nil {
		s.removeCredential(w, r, u, t)
	}
}

func (s *Server) applianceCredential(w http.ResponseWriter, r *http.Request, u user) {
	if t := s.applianceCredentialTarget(w, r, u); t != nil {
		s.showCredential(w, r, u, t)
	}
}

func (s *Server) storeApplianceCredential(w http.ResponseWriter, r *http.Request, u user) {
	if t := s.applianceCredentialTarget(w, r, u); t != nil {
		s.storeCredential(w, r, u, t)
	}
}

func (s *Server) removeApplianceCredential(w http.ResponseWriter, r *http.Request, u user) {
	if t := s.applianceCredentialTarget(w, r, u); t != nil {
		s.removeCredential(w, r, u, t)
	}
}

var errNotCredential = errors.New("a Secret of the credential's name exists that the interface did not store; an administrator of the management cluster has to remove it")

// stored reads a credential's Secret, telling whether there is one. A Secret of its name that is no
// credential is an error, as the interface may not touch it.
func stored(ctx context.Context, kube client.Client, t *credentialTarget) (*corev1.Secret, bool, error) {
	secret := &corev1.Secret{}
	if err := kube.Get(ctx, t.key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading the credential %s: %w", t.key, err)
	}
	if secret.Type != provisioner.CredentialType || secret.Labels[provisioner.CredentialLabel] != t.purpose.Name {
		return nil, false, errNotCredential
	}
	return secret, true, nil
}

// viewOf describes a credential's Secret, nil while none is stored.
func viewOf(t *credentialTarget, secret *corev1.Secret) credentialView {
	v := credentialView{Purpose: t.purpose.Name, CanChange: t.canChange}
	if secret == nil {
		return v
	}
	v.Set = true
	v.SetBy = secret.Annotations[provisioner.CredentialSetBy]
	if at, err := time.Parse(time.RFC3339, secret.Annotations[provisioner.CredentialSetAt]); err == nil {
		v.SetAt = &at
	}
	return v
}

func (s *Server) showCredential(w http.ResponseWriter, r *http.Request, u user, t *credentialTarget) {
	secret, _, err := stored(r.Context(), u.kube, t)
	if err != nil {
		s.credentialError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewOf(t, secret))
}

// credentialValues checks that a request gives every value of the purpose, and nothing else.
func credentialValues(purpose provisioner.CredentialPurpose, values map[string]string) (map[string][]byte, string) {
	for k := range values {
		if !slices.Contains(purpose.Keys, k) {
			return nil, fmt.Sprintf("a %s credential holds %s, not %s", purpose.Name, strings.Join(purpose.Keys, " and "), k)
		}
	}
	data := make(map[string][]byte, len(purpose.Keys))
	for _, k := range purpose.Keys {
		v := values[k]
		switch {
		case v == "":
			return nil, "a value for " + k + " is required"
		case len(v) > maxCredentialValue:
			return nil, fmt.Sprintf("the value for %s is longer than %d bytes", k, maxCredentialValue)
		case !utf8.ValidString(v):
			return nil, "the value for " + k + " is no valid text"
		}
		data[k] = []byte(v)
	}
	return data, ""
}

// newCredential is a credential's Secret before its first values.
func newCredential(t *credentialTarget) *corev1.Secret {
	secret := &corev1.Secret{Namespace: t.key.Namespace, Name: t.key.Name, Type: provisioner.CredentialType}
	if t.cluster != nil {
		// Deleting the cluster deletes its credentials with it.
		secret.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: t.cluster.Name, UID: t.cluster.UID,
		}}
	}
	return secret
}

// storeCredential stores a credential, or replaces the one stored, as a whole.
func (s *Server) storeCredential(w http.ResponseWriter, r *http.Request, u user, t *credentialTarget) {
	var req struct {
		Values map[string]string `json:"values"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	data, problem := credentialValues(t.purpose, req.Values)
	if problem != "" {
		writeError(w, http.StatusUnprocessableEntity, problem)
		return
	}
	ctx := r.Context()
	secret, set, err := stored(ctx, u.kube, t)
	if err != nil {
		s.credentialError(w, r, err)
		return
	}
	if !set {
		secret = newCredential(t)
	}
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[provisioner.CredentialLabel] = t.purpose.Name
	if t.cluster != nil {
		secret.Labels[provisioner.CredentialClusterLabel] = t.cluster.Name
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[provisioner.CredentialSetBy] = u.ticket.Username
	secret.Annotations[provisioner.CredentialSetAt] = time.Now().UTC().Format(time.RFC3339)
	secret.Data = data
	if set {
		err = u.kube.Update(ctx, secret)
	} else {
		err = u.kube.Create(ctx, secret)
	}
	if err != nil {
		s.credentialError(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "stored credential", "user", u.ticket.Username, "credential", t.describe())
	s.credentialChanged(r, u, t)
	writeJSON(w, http.StatusOK, viewOf(t, secret))
}

func (s *Server) removeCredential(w http.ResponseWriter, r *http.Request, u user, t *credentialTarget) {
	ctx := r.Context()
	secret, set, err := stored(ctx, u.kube, t)
	if err == nil && set {
		err = u.kube.Delete(ctx, secret, client.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		s.credentialError(w, r, err)
		return
	}
	if set {
		s.Log.InfoContext(r.Context(), "removed credential", "user", u.ticket.Username, "credential", t.describe())
		s.credentialChanged(r, u, t)
	}
	w.WriteHeader(http.StatusNoContent)
}

// credentialChanged marks the clusters that use a credential just stored or removed: its own
// cluster, or for the DNS-01 credential of every cluster, those that chose it.
func (s *Server) credentialChanged(r *http.Request, u user, t *credentialTarget) {
	switch {
	case t.cluster != nil:
		s.stampCredentialsStored(r, u, t.cluster)
	case t.purpose.Name == "dns01":
		s.stampCredentialsStored(r, u, s.sharingDNS01(r, u)...)
	}
}

func (s *Server) credentialError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errNotCredential):
		writeError(w, http.StatusConflict, err.Error())
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		writeError(w, http.StatusConflict, "the credential changed meanwhile; try again")
	default:
		s.kubeError(w, r, err)
	}
}
