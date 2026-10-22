package provisioner

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The keys of a cluster's GitOps Secret beside it, which Flux's pull Secret shares: the deploy key's
// two halves, the repository host's SSH keys, the URL the deploy key was made for and the address
// the host's keys were read from.
const (
	identityKey      = "identity"
	identityPubKey   = "identity.pub"
	knownHostsKey    = "known_hosts"
	urlKey           = "url"
	hostKeysFromKey  = "known_hosts.from"
	usernameKey      = "username"
	passwordKey      = "password"
	gitOpsPurpose    = "gitops"
	clientPurpose    = "oidc-client"
	hostKeysTimeout  = 10 * time.Second
	deployKeyComment = "kubehenge"
)

// GitOpsSecretName is the Secret beside a cluster that holds its deploy key and its repository
// host's SSH keys; the interface shows their public halves.
func GitOpsSecretName(cluster string) string { return cluster + "-gitops" }

// gitOpsState reads what the cluster's GitOps needs before its chart's values are written, and
// makes what is missing of it: a deploy key for the repository, and the keys of its host, read
// from the host the first time and trusted from then on, unless the cluster names them itself.
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
	state.keys, state.knownHosts = keys, cmp.Or(g.KnownHosts, string(keys[knownHostsKey]))
	return state, nil
}

// ensureGitOpsKeys keeps the cluster's GitOps Secret: a deploy key while the cluster reads its
// repository with one, made again for a new repository, since a forge takes a deploy key for one
// repository only, and the host's keys while the repository is reached over SSH. Both stay when
// GitOps is switched off, so that switching it on again needs no new deploy key.
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
	if g.sshURL() && g.KnownHosts == "" {
		address, err := sshAddress(g.Repository.URL)
		if err != nil {
			return nil, err
		}
		if len(data[knownHostsKey]) == 0 || string(data[hostKeysFromKey]) != address {
			r.readHostKeys(ctx, cluster, address, data)
		}
	}
	if maps.EqualFunc(data, s.Data, bytes.Equal) {
		return data, nil
	}
	desired := maps.Clone(data)
	return data, applyOwned(ctx, r.Client, cluster, s, func(got *corev1.Secret) {
		got.Type, got.Data = corev1.SecretTypeOpaque, desired
	})
}

// readHostKeys reads the keys of the repository's host into data, or reports on the cluster that it
// could not; the engine then reaches no repository over SSH until it can.
func (r *Reconciler) readHostKeys(ctx context.Context, cluster *clusterv1.Cluster, address string, data map[string][]byte) {
	read := scanHostKeys
	if r.HostKeys != nil {
		read = r.HostKeys
	}
	hosts, err := read(ctx, address)
	if err != nil {
		ctrl.LoggerFrom(ctx).Info("could not read the repository host's SSH keys", "address", address, "error", err.Error())
		if r.Events != nil {
			r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, "GitOpsHostKeys", "ReadHostKeys",
				"could not read the SSH keys of %s, to trust them, nor can GitOps reach the repository until they are read or set as gitops.knownHosts: %v", address, err)
		}
		return
	}
	data[knownHostsKey], data[hostKeysFromKey] = []byte(hosts), []byte(address)
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

// hostKeyAlgorithms are asked for one by one, as a server shows one key per connection: every key
// it has lands in known_hosts, so that the engine trusts whichever it is shown.
var hostKeyAlgorithms = []string{
	ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512,
}

var errHostKeyRead = errors.New("host key read")

// scanHostKeys reads an SSH server's host keys as known_hosts lines.
func scanHostKeys(ctx context.Context, address string) (string, error) {
	var lines []string
	var last error
	for _, algorithm := range hostKeyAlgorithms {
		line, err := hostKey(ctx, address, algorithm)
		var dial *net.OpError
		if errors.As(err, &dial) && dial.Op == "dial" {
			return "", fmt.Errorf("reaching %s: %w", address, err)
		}
		if err != nil {
			last = err
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("reading the SSH host keys of %s: %w", address, last)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// hostKey reads the server's key of one algorithm, as a known_hosts line, and hangs up before
// signing in.
func hostKey(ctx context.Context, address, algorithm string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hostKeysTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return "", fmt.Errorf("dialling: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err = conn.SetDeadline(deadline); err != nil {
			return "", fmt.Errorf("setting the connection's deadline: %w", err)
		}
	}
	var key ssh.PublicKey
	config := &ssh.ClientConfig{
		User:              "git",
		HostKeyAlgorithms: []string{algorithm},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errHostKeyRead
		},
	}
	signedIn, _, _, err := ssh.NewClientConn(conn, address, config)
	if signedIn != nil {
		_ = signedIn.Close()
	}
	if key == nil {
		return "", fmt.Errorf("the SSH handshake: %w", err)
	}
	return knownhosts.Line([]string{address}, key), nil
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
