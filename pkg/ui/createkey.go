package ui

import (
	"context"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// storeKeyAhead stores the access key a create request carries, before the cluster exists, so that
// the cluster never runs without it. The Secret has no owner until ownKeyAhead gives it one, and
// the controller deletes one whose cluster never came. It returns nil when the request has no key.
// It answers the request and returns false when it cannot store the key.
func (s *Server) storeKeyAhead(w http.ResponseWriter, r *http.Request, u user, req *createRequest) (*corev1.Secret, bool) {
	if len(req.BackupKey) == 0 || r.URL.Query().Get("dryRun") == metav1.DryRunAll {
		return nil, true
	}
	purpose, _ := provisioner.CredentialPurposeNamed("backup")
	data, problem := credentialValues(purpose, req.BackupKey)
	if problem != "" {
		writeError(w, http.StatusUnprocessableEntity, "backup key: "+problem)
		return nil, false
	}
	ctx := r.Context()
	// A key stored under the name of a cluster that exists would replace that cluster's own.
	exists := &clusterv1.Cluster{}
	switch err := u.kube.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, exists); {
	case err == nil:
		writeError(w, http.StatusConflict, "cluster "+req.Namespace+"/"+req.Name+" already exists")
		return nil, false
	case !apierrors.IsNotFound(err):
		s.kubeError(w, r, err)
		return nil, false
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: req.Namespace, Name: provisioner.CredentialName(req.Name, purpose.Name),
			Labels: map[string]string{
				provisioner.CredentialLabel: purpose.Name, provisioner.CredentialClusterLabel: req.Name,
			},
			Annotations: map[string]string{
				provisioner.CredentialSetBy: u.ticket.Username,
				provisioner.CredentialSetAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
		Type: provisioner.CredentialType,
		Data: data,
	}
	err := u.kube.Create(ctx, secret)
	if apierrors.IsAlreadyExists(err) {
		// Only one left without an owner, by a create that failed, may be replaced.
		old := &corev1.Secret{}
		if err = u.kube.Get(ctx, client.ObjectKeyFromObject(secret), old); err == nil {
			if len(old.OwnerReferences) > 0 {
				writeError(w, http.StatusConflict, "another cluster's credential has the name "+secret.Name)
				return nil, false
			}
			secret.ResourceVersion = old.ResourceVersion
			err = u.kube.Update(ctx, secret)
		}
	}
	if err != nil {
		s.kubeError(w, r, err)
		return nil, false
	}
	return secret, true
}

// dropKeyAhead deletes a key stored ahead of a cluster that was not made.
func (s *Server) dropKeyAhead(r *http.Request, u user, key *corev1.Secret) {
	if key == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if err := u.kube.Delete(ctx, key); err != nil && !apierrors.IsNotFound(err) {
		s.Log.ErrorContext(ctx, "deleting a key stored ahead of a cluster not made", "secret", client.ObjectKeyFromObject(key), "error", err)
	}
}

// ownKeyAhead makes the cluster the owner of the key stored ahead of it, so that it goes with the
// cluster. If this fails, the controller does it.
func (s *Server) ownKeyAhead(r *http.Request, u user, key *corev1.Secret, c *clusterv1.Cluster) {
	if key == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	key.OwnerReferences = []metav1.OwnerReference{provisioner.CredentialOwner(c)}
	if err := u.kube.Update(ctx, key); err != nil {
		s.Log.ErrorContext(ctx, "making a cluster the owner of its key", "secret", client.ObjectKeyFromObject(key), "error", err)
	}
}
