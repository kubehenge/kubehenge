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

// gitOpsView is what the controller made for the cluster's GitOps that its owner needs to see: the
// deploy key's public half, to add to the repository, and the fingerprints of the repository host's
// keys it trusts, to compare with the host's own.
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

// addGitOps adds what the controller made for the cluster's GitOps to its view, once it has.
func addGitOps(ctx context.Context, u user, v *clusterView) error {
	if v.Variables[gitopsVariable] == nil {
		return nil
	}
	secret := &corev1.Secret{}
	err := u.kube.Get(ctx, client.ObjectKey{Namespace: v.Namespace, Name: provisioner.GitOpsSecretName(v.Name)}, secret)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the cluster's GitOps keys: %w", err)
	}
	g := &gitOpsView{HostKeys: provisioner.HostKeyFingerprints(secret.Data["known_hosts"])}
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
