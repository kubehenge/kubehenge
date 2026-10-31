package provisioner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // OpenSSH hashes known_hosts names with HMAC-SHA1; no other hash matches them
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The keys of a cluster's GitOps Secret beside it, which Flux's pull Secret shares: the deploy key's
// two halves and the URL it was made for; and of Flux's pull Secret, the repository host's SSH keys.
const (
	identityKey      = "identity"
	identityPubKey   = "identity.pub"
	knownHostsKey    = "known_hosts"
	urlKey           = "url"
	usernameKey      = "username"
	passwordKey      = "password"
	gitOpsPurpose    = "gitops"
	clientPurpose    = "oidc-client"
	deployKeyComment = "kubehenge"
)

// GitOpsSecretName is the Secret beside a cluster that holds its deploy key and its repository
// host's SSH keys; the interface shows their public halves.
func GitOpsSecretName(cluster string) string { return cluster + "-gitops" }

// gitOpsState reads what the cluster's GitOps needs before its chart's values are written, and
// makes what is missing of it: a deploy key for the repository. The keys of its host are the ones
// the cluster gives.
func (r *Reconciler) gitOpsState(ctx context.Context, cluster *clusterv1.Cluster, g *GitOps) (gitOpsState, error) {
	var state gitOpsState
	if !g.Enabled {
		return state, nil
	}
	var err error
	if state.provider, err = signInProvider(cluster); err != nil {
		return state, err
	}
	switch g.clientSecret() {
	case clientSecretStored:
		_, state.clientSecret, err = r.credential(ctx, cluster, clientPurpose)
		if err != nil {
			return state, err
		}
	case clientSecretExisting:
		state.clientSecret = true
	}
	keys, err := r.ensureGitOpsKeys(ctx, cluster, g)
	if err != nil {
		return state, err
	}
	state.keys, state.knownHosts = keys, g.KnownHosts
	return state, nil
}

// ensureGitOpsKeys keeps the cluster's GitOps Secret: a deploy key while the cluster reads its
// repository with one, made again for a new repository, since a forge takes a deploy key for one
// repository only. It stays when GitOps is switched off, so that switching it on again needs no
// new deploy key.
func (r *Reconciler) ensureGitOpsKeys(ctx context.Context, cluster *clusterv1.Cluster, g *GitOps) (map[string][]byte, error) {
	s := &corev1.Secret{Namespace: cluster.Namespace, Name: GitOpsSecretName(cluster.Name)}
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(s), s)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading the cluster's GitOps keys: %w", err)
	}
	data := maps.Clone(s.Data)
	if data == nil {
		data = map[string][]byte{}
	}
	if g.auth() == gitAuthKey && (len(data[identityKey]) == 0 || string(data[urlKey]) != g.Repository.URL) {
		private, public, err := newDeployKey(deployKeyComment + " " + cluster.Namespace + "/" + cluster.Name)
		if err != nil {
			return nil, err
		}
		data[identityKey], data[identityPubKey], data[urlKey] = private, public, []byte(g.Repository.URL)
	}
	if maps.EqualFunc(data, s.Data, bytes.Equal) {
		return data, nil
	}
	desired := maps.Clone(data)
	return data, applyOwned(ctx, r.Client, cluster, s, func(got *corev1.Secret) {
		got.Type, got.Data = corev1.SecretTypeOpaque, desired
	})
}

// newDeployKey makes an Ed25519 key pair: the private half in OpenSSH's format, as Flux and Argo
// CD read it, and the public half as a forge takes it, with comment naming the cluster.
func newDeployKey(comment string) ([]byte, []byte, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("making a deploy key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(private, comment)
	if err != nil {
		return nil, nil, fmt.Errorf("writing the deploy key: %w", err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return nil, nil, fmt.Errorf("writing the deploy key's public half: %w", err)
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic))) + " " + comment + "\n"
	return pem.EncodeToMemory(block), []byte(authorized), nil
}

// checkKnownHosts holds the host keys a cluster gives for its repository to known_hosts lines
// that parse, at least one of them for the repository's host, hashed or not, as the engines look
// it up. Kubehenge never reads a host's keys from the network: whoever could answer for the
// host on that first connection would be trusted from then on.
func checkKnownHosts(knownHosts, repositoryURL string) error {
	address, err := sshAddress(repositoryURL)
	if err != nil {
		return err
	}
	host := knownhosts.Normalize(address)
	matched := false
	rest := []byte(knownHosts)
	for len(bytes.TrimSpace(rest)) > 0 {
		marker, hosts, _, _, next, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			return fmt.Errorf("the host keys are not known_hosts lines: %w", err)
		}
		if marker != "revoked" && knownHostsLineMatches(hosts, host) {
			matched = true
		}
		rest = next
	}
	if !matched {
		return fmt.Errorf("no host key is for %s, the repository's host", host)
	}
	return nil
}

// knownHostsLineMatches tells whether a known_hosts line's patterns name host, as OpenSSH reads
// them: one of them does, and none of those negated with ! does.
func knownHostsLineMatches(patterns []string, host string) bool {
	matched := false
	for _, p := range patterns {
		if negated, ok := strings.CutPrefix(p, "!"); ok {
			if knownHostMatches(negated, host) {
				return false
			}
			continue
		}
		matched = matched || knownHostMatches(p, host)
	}
	return matched
}

// knownHostMatches tells whether a known_hosts host pattern names host, which is in the form
// knownhosts.Normalize gives: plainly, with OpenSSH's wildcards, or hashed as its HashKnownHosts
// writes it.
func knownHostMatches(pattern, host string) bool {
	salt, sum, ok := strings.Cut(strings.TrimPrefix(pattern, "|1|"), "|")
	if !ok || !strings.HasPrefix(pattern, "|1|") {
		if !strings.ContainsAny(pattern, "*?") {
			return pattern == host
		}
		wild := strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern))
		matched, err := regexp.MatchString("^"+wild+"$", host)
		return err == nil && matched
	}
	rawSalt, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		return false
	}
	rawSum, err := base64.StdEncoding.DecodeString(sum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha1.New, rawSalt)
	_, _ = mac.Write([]byte(host))
	return hmac.Equal(mac.Sum(nil), rawSum)
}

// HostKeyFingerprints are the SHA256 fingerprints of the keys in known_hosts lines, as ssh-keygen
// shows them, by key type.
func HostKeyFingerprints(knownHosts []byte) []string {
	var prints []string
	rest := knownHosts
	for len(rest) > 0 {
		_, _, key, _, next, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			break
		}
		prints = append(prints, key.Type()+" "+ssh.FingerprintSHA256(key))
		rest = next
	}
	return prints
}

// credential reads the cluster's user credential of purpose, and whether it is there with every
// value the purpose holds.
func (r *Reconciler) credential(ctx context.Context, cluster *clusterv1.Cluster, purpose string) (map[string][]byte, bool, error) {
	p, ok := CredentialPurposeNamed(purpose)
	if !ok {
		return nil, false, fmt.Errorf("no credential purpose %q", purpose)
	}
	s := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: CredentialName(cluster.Name, purpose)}, s)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading the cluster's %s credential: %w", purpose, err)
	}
	// The interface writes only Secrets of this type and label, so that a Secret of another kind
	// under the same name is never taken for a credential.
	if s.Type != CredentialType || s.Labels[CredentialLabel] != purpose {
		return nil, false, nil
	}
	for _, k := range p.Keys {
		if len(s.Data[k]) == 0 {
			return nil, false, nil
		}
	}
	return s.Data, true, nil
}
