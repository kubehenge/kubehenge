package ui

import (
	"crypto/rand"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// createTimeout bounds a create request's calls to Kubernetes and Proxmox VE, the key stored ahead
// of the cluster included. A key left behind by a request cut short is the controller's to clear,
// or the next create's of that name to replace.
const createTimeout = 30 * time.Second

// storeKeyAhead stores the access key a create request carries, before the cluster exists, so that
// the cluster never runs without it. Storing it takes the right the credential endpoint asks for,
// on the cluster's pool. The Secret and the Cluster c carry the request's mark
// (provisioner.CreateRequestKey), so that the controller adopts the Secret only into this request's
// Cluster, or deletes it once that Cluster never came. A Secret of that name that exists is left as
// it is and the request refused, unless an earlier create left it behind (replaceLeftover). It
// returns nil when the request has no key.
// It answers the request and returns false when it cannot store the key.
func (s *Server) storeKeyAhead(w http.ResponseWriter, r *http.Request, u user, req *createRequest, pool string, c *clusterv1.Cluster) (*corev1.Secret, bool) {
	if len(req.BackupKey) == 0 {
		return nil, true
	}
	if !s.require(w, r, u, pool, privKubeconfig) {
		return nil, false
	}
	purpose, _ := provisioner.CredentialPurposeNamed("backup")
	data, problem := credentialValues(purpose, req.BackupKey)
	if problem != "" {
		writeError(w, http.StatusUnprocessableEntity, "backup key: "+problem)
		return nil, false
	}
	if r.URL.Query().Get("dryRun") == metav1.DryRunAll {
		return nil, true
	}
	ctx := r.Context()
	// A key stored under the name of a cluster that exists would stand beside that cluster's own.
	exists := &clusterv1.Cluster{}
	switch err := u.kube.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, exists); {
	case err == nil:
		writeError(w, http.StatusConflict, "cluster "+req.Namespace+"/"+req.Name+" already exists")
		return nil, false
	case !apierrors.IsNotFound(err):
		s.kubeError(w, r, err)
		return nil, false
	}
	request := rand.Text()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: req.Namespace, Name: provisioner.CredentialName(req.Name, purpose.Name),
			Labels: map[string]string{
				provisioner.CredentialLabel: purpose.Name, provisioner.CredentialClusterLabel: req.Name,
				provisioner.CreateRequestKey: request,
			},
			Annotations: map[string]string{
				provisioner.CredentialSetBy: u.ticket.Username,
				provisioner.CredentialSetAt: s.now().UTC().Format(time.RFC3339),
			},
		},
		Type: provisioner.CredentialType,
		Data: data,
	}
	if err := u.kube.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			s.kubeError(w, r, err)
			return nil, false
		}
		if !s.replaceLeftover(w, r, u, req, secret) {
			return nil, false
		}
	}
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[provisioner.CreateRequestKey] = request
	return secret, true
}

// replaceLeftover stores secret in place of the Secret of its name, when that one is a key an
// earlier create left behind: it carries a create's mark and no owner, its request has ended, and
// no Cluster of the name exists. A request runs for createTimeout at most, so a younger key may
// belong to a create still under way, whose Cluster would then go without its key. Any other Secret
// of the name is left as it is and the request refused. It answers the request and returns false
// when it refuses.
func (s *Server) replaceLeftover(w http.ResponseWriter, r *http.Request, u user, req *createRequest, secret *corev1.Secret) bool {
	ctx := r.Context()
	exists := "a credential named " + secret.Name + " exists. Create the cluster without a key to use it, or remove it first"
	old := &corev1.Secret{}
	if err := u.kube.Get(ctx, client.ObjectKeyFromObject(secret), old); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusConflict, exists)
			return false
		}
		s.kubeError(w, r, err)
		return false
	}
	if old.Labels[provisioner.CreateRequestKey] == "" || len(old.OwnerReferences) > 0 {
		writeError(w, http.StatusConflict, exists)
		return false
	}
	if set, err := time.Parse(time.RFC3339, old.Annotations[provisioner.CredentialSetAt]); err != nil || s.now().Sub(set) < createTimeout {
		writeError(w, http.StatusConflict, "another create of cluster "+req.Namespace+"/"+req.Name+" is under way. Try again in a minute")
		return false
	}
	// The leftover's own request may have made its Cluster after this request looked for one.
	switch err := u.kube.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, &clusterv1.Cluster{}); {
	case err == nil:
		writeError(w, http.StatusConflict, "cluster "+req.Namespace+"/"+req.Name+" already exists")
		return false
	case !apierrors.IsNotFound(err):
		s.kubeError(w, r, err)
		return false
	}
	if err := u.kube.Delete(ctx, old, client.Preconditions{UID: &old.UID, ResourceVersion: &old.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			writeError(w, http.StatusConflict, exists)
			return false
		}
		s.kubeError(w, r, err)
		return false
	}
	if err := u.kube.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, exists)
			return false
		}
		s.kubeError(w, r, err)
		return false
	}
	return true
}

// dropKeyAhead deletes the key this request stored ahead of a cluster that was not made. The UID
// precondition keeps it off a Secret of the same name made since.
func (s *Server) dropKeyAhead(r *http.Request, u user, key *corev1.Secret) {
	if key == nil {
		return
	}
	ctx := r.Context()
	if err := u.kube.Delete(ctx, key, client.Preconditions{UID: &key.UID}); err != nil && !apierrors.IsNotFound(err) {
		s.Log.ErrorContext(ctx, "deleting a key stored ahead of a cluster not made", "secret", client.ObjectKeyFromObject(key), "error", err)
	}
}

// ownKeyAhead makes the cluster the owner of the key stored ahead of it, so that it goes with the
// cluster. The key's resource version keeps it off a Secret changed since. If this fails, the
// controller does it.
func (s *Server) ownKeyAhead(r *http.Request, u user, key *corev1.Secret, c *clusterv1.Cluster) {
	if key == nil {
		return
	}
	ctx := r.Context()
	key.OwnerReferences = []metav1.OwnerReference{provisioner.CredentialOwner(c)}
	if err := u.kube.Update(ctx, key); err != nil {
		s.Log.ErrorContext(ctx, "making a cluster the owner of its key", "secret", client.ObjectKeyFromObject(key), "error", err)
	}
}
