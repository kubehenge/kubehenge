package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const gitopsVariable = "gitops"

// gitOpsView is what the cluster's owner needs to see of its GitOps: the deploy key's public half
// the controller made, to add to the repository, and the fingerprints of the repository host's keys
// the cluster trusts, to compare with the forge's published ones.
type gitOpsView struct {
	DeployKey            string   `json:"deployKey,omitempty"`
	DeployKeyFingerprint string   `json:"deployKeyFingerprint,omitempty"`
	HostKeys             []string `json:"hostKeys,omitempty"`
}

func gitOpsOn(raw json.RawMessage) bool {
	var g struct {
		Enabled bool `json:"enabled"`
	}
	return raw != nil && json.Unmarshal(raw, &g) == nil && g.Enabled
}

// allowGitOps checks the right to point a cluster's GitOps at a repository: the engine applies
// whatever the repository holds with its own cluster-wide rights, so whoever chooses the repository
// holds what the admin kubeconfig gives, and needs what taking it does, Sys.Modify on the pool.
// Switching GitOps off needs no more than any other change.
func (s *Server) allowGitOps(w http.ResponseWriter, r *http.Request, u user, path string, value, old json.RawMessage) bool {
	if !gitOpsOn(value) || (old != nil && sameJSON(old, value)) {
		return true
	}
	return s.require(w, r, u, path, privKubeconfig)
}

// addGitOps adds the fingerprints of the host keys the cluster gives, and the deploy key the
// controller made for it once it has, to its view.
func addGitOps(ctx context.Context, u user, v *clusterView) error {
	raw := v.Variables[gitopsVariable]
	if raw == nil {
		return nil
	}
	var given struct {
		KnownHosts string `json:"knownHosts"`
	}
	if err := json.Unmarshal(raw, &given); err != nil {
		return fmt.Errorf("reading the cluster's GitOps: %w", err)
	}
	g := &gitOpsView{HostKeys: provisioner.HostKeyFingerprints([]byte(given.KnownHosts))}
	secret := &corev1.Secret{}
	err := u.kube.Get(ctx, client.ObjectKey{Namespace: v.Namespace, Name: provisioner.GitOpsSecretName(v.Name)}, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading the cluster's GitOps keys: %w", err)
	}
	if public := strings.TrimSpace(string(secret.Data["identity.pub"])); public != "" {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(public))
		if err != nil {
			return fmt.Errorf("reading the cluster's deploy key: %w", err)
		}
		g.DeployKey, g.DeployKeyFingerprint = public, ssh.FingerprintSHA256(key)
	}
	if g.DeployKey != "" || len(g.HostKeys) > 0 {
		v.GitOps = g
	}
	return nil
}
