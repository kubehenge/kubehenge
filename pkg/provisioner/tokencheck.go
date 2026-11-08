package provisioner

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProxmoxAccessRefusedCondition is true on a Cluster while the Proxmox VE token of its cloud
// controller manager and CSI plugin is refused or lacks a privilege it needs.
const ProxmoxAccessRefusedCondition = "ProxmoxAccessRefused"

// The ways a token fails, the reason of ProxmoxAccessRefusedCondition and TokenState.Problem.
const (
	// TokenRefused is a token Proxmox VE does not accept: expired, disabled, or its user removed.
	TokenRefused = "Refused"
	// TokenCertificate is an API whose certificate no CA the token's holder trusts signed.
	TokenCertificate = "Certificate"
	// TokenUnreachable is an API that did not answer.
	TokenUnreachable = "Unreachable"
	// TokenPrivileges is a token that is accepted but lacks privileges.
	TokenPrivileges = "Privileges"
)

// TokenCheckInterval is how often every token is tested. Each test of a token that only a Job holds
// starts a Job, so it is rarer than the reachability check.
const TokenCheckInterval = 10 * time.Minute

// Need is a privilege a token holds on a path of Proxmox VE's permission tree.
type Need struct {
	Path      string `json:"path"`
	Privilege string `json:"privilege"`
}

// String is the need as the documentation writes it.
func (n Need) String() string { return n.Privilege + " on " + n.Path }

// TokenState is the outcome of testing one token.
type TokenState struct {
	// Name says whose token it is, such as "Controller" or "Cluster team-a/web".
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Problem is one of the Token constants, empty when OK.
	Problem string `json:"problem,omitempty"`
	// Error is what Proxmox VE or the connection answered, for a problem that is not a missing privilege.
	Error string `json:"error,omitempty"`
	// Missing are the privileges the token lacks, each with the path it is needed on.
	Missing []Need `json:"missing,omitempty"`
	// Cluster is "<namespace>/<name>" for a cluster's token.
	Cluster string `json:"cluster,omitempty"`
}

// Message says what is wrong in the words Health, the notification and the Cluster use, empty when OK.
func (s *TokenState) Message() string {
	switch s.Problem {
	case "":
		return ""
	case TokenRefused:
		return fmt.Sprintf("%s token is refused by Proxmox VE (%s). It may have expired, been disabled or lost its user.", s.Name, s.Error)
	case TokenCertificate:
		return fmt.Sprintf("%s token cannot verify the Proxmox VE API's certificate (%s). The API may use a new CA.", s.Name, s.Error)
	case TokenPrivileges:
		needs := make([]string, len(s.Missing))
		for i, n := range s.Missing {
			needs[i] = n.String()
		}
		return fmt.Sprintf("%s token lacks %s.", s.Name, strings.Join(needs, ", "))
	default:
		return fmt.Sprintf("%s token could not be tested: %s.", s.Name, s.Error)
	}
}

// Permissions are the privileges a token holds, by path, as /access/permissions answers.
type Permissions map[string]map[string]int

// TokenProbe tests one token: it asks what the token holds on the paths in Needs.
type TokenProbe struct {
	Name    string
	Cluster string
	Needs   []Need
	// Ask reads the token's effective permissions on each path.
	Ask func(ctx context.Context, paths []string) (Permissions, error)
}

// permissionsPath is Proxmox VE's answer for the effective permissions of the caller on one path
// (`PVE::API2::AccessControl`, `permissions`): a map of the path to its privileges.
const permissionsPath = "/access/permissions?path="

// AskPermissions reads a token's permissions on paths with the token's own client.
func AskPermissions(r proxmoxReader) func(context.Context, []string) (Permissions, error) {
	return func(ctx context.Context, paths []string) (Permissions, error) {
		out := Permissions{}
		for _, p := range paths {
			var got Permissions
			if err := r.Get(ctx, permissionsPath+url.QueryEscape(p), &got); err != nil {
				return nil, fmt.Errorf("reading permissions on %s: %w", p, err)
			}
			out[p] = got[p]
		}
		return out, nil
	}
}

// PermissionsRequest asks a token Job for its token's permissions on paths.
type PermissionsRequest struct {
	PermissionsOf []string `json:"permissionsOf"`
}

// AskThroughJob reads a permissions by a token Job, which alone holds the token.
func AskThroughJob(jobs TokenRunner) func(context.Context, []string) (Permissions, error) {
	return func(ctx context.Context, paths []string) (Permissions, error) {
		out, err := jobs.Run(ctx, PermissionsRequest{PermissionsOf: paths}, tokenJobProbeTimeout)
		if err != nil {
			return nil, fmt.Errorf("asking a token Job: %w", err)
		}
		var got Permissions
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			return nil, fmt.Errorf("the token Job's answer: %w", err)
		}
		return got, nil
	}
}

const tokenJobProbeTimeout = time.Minute

// servePermissions answers a PermissionsRequest with the Job's token. It reports false for any
// other request.
func servePermissions(ctx context.Context, c *proxmox.Client, raw []byte) (string, bool, error) {
	var req PermissionsRequest
	if err := json.Unmarshal(raw, &req); err != nil || len(req.PermissionsOf) == 0 {
		return "", false, nil //nolint:nilerr // not a permissions request, which the caller reads as its own kind
	}
	got, err := AskPermissions(c)(ctx, req.PermissionsOf)
	if err != nil {
		return "", true, err
	}
	out, err := json.Marshal(got)
	if err != nil {
		return "", true, fmt.Errorf("encoding the permissions: %w", err)
	}
	return string(out), true, nil
}

// Check tests the token.
func (p TokenProbe) Check(ctx context.Context) TokenState {
	state := TokenState{Name: p.Name, Cluster: p.Cluster}
	paths := make([]string, 0, len(p.Needs))
	for _, n := range p.Needs {
		if !slices.Contains(paths, n.Path) {
			paths = append(paths, n.Path)
		}
	}
	got, err := p.Ask(ctx, paths)
	if err != nil {
		state.Problem, state.Error = classifyTokenError(err), err.Error()
		return state
	}
	for _, n := range p.Needs {
		// Proxmox VE maps each privilege held to whether it propagates, so a 0 is held too.
		if _, ok := got[n.Path][n.Privilege]; !ok {
			state.Missing = append(state.Missing, n)
		}
	}
	if len(state.Missing) > 0 {
		state.Problem = TokenPrivileges
		return state
	}
	state.OK = true
	return state
}

// classifyTokenError tells a refused token from an untrusted certificate and from an API that did
// not answer. Errors that came through a Job carry only text.
func classifyTokenError(err error) string {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	text := err.Error()
	switch {
	case errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &host) || strings.Contains(text, "x509:"):
		return TokenCertificate
	case errors.Is(err, proxmox.ErrNotAuthorized) || strings.Contains(text, "401") || strings.Contains(text, "authentication failure"):
		return TokenRefused
	}
	return TokenUnreachable
}

// CheckTokens tests every token of probes.
func CheckTokens(ctx context.Context, probes []TokenProbe) []TokenState {
	states := make([]TokenState, 0, len(probes))
	for _, p := range probes {
		states = append(states, p.Check(ctx))
	}
	return states
}

// SecretToken reads a token a Secret holds: the key "token" in the form user@realm!name=secret, or
// the keys PROXMOX_TOKEN and PROXMOX_SECRET Cluster API's provider is given.
func SecretToken(s *corev1.Secret) (string, error) {
	if t := string(s.Data["token"]); t != "" {
		return t, nil
	}
	id, secret := string(s.Data["PROXMOX_TOKEN"]), string(s.Data["PROXMOX_SECRET"])
	if id == "" || secret == "" {
		return "", fmt.Errorf("secret %s/%s holds neither token nor PROXMOX_TOKEN with PROXMOX_SECRET", s.Namespace, s.Name)
	}
	return id + "=" + secret, nil
}

// markClusterAccess sets ProxmoxAccessRefusedCondition on the cluster a state is of, and takes it
// away once the token works.
func markClusterAccess(ctx context.Context, c client.Client, live client.Reader, cluster *clusterv1.Cluster, s *TokenState) error {
	var cond *metav1.Condition
	switch {
	case !s.OK:
		cond = &metav1.Condition{Status: metav1.ConditionTrue, Reason: s.Problem, Message: s.Message()}
	case meta.FindStatusCondition(cluster.Status.Conditions, ProxmoxAccessRefusedCondition) == nil:
		return nil
	}
	if _, err := markCluster(ctx, c, live, client.ObjectKeyFromObject(cluster), ProxmoxAccessRefusedCondition, cond); err != nil {
		return fmt.Errorf("marking %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return nil
}
