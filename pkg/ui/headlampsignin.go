package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// headlampSignIn is one session's sign-in to one cluster in the shared Headlamp: a token of the
// cluster's Headlamp account, kept in the interface's memory and set on that session's requests to
// the cluster, so it never reaches the browser.
type headlampSignIn struct {
	mu      sync.Mutex
	token   string
	expires time.Time
	// retry is when a cluster that gave no token is asked again: Headlamp sends many requests a
	// page, and the sign-in may be off.
	retry time.Time
}

const (
	// headlampTokenLifetime is the longest a sign-in's token lasts. A session that ends sooner
	// gets one that ends with it.
	headlampTokenLifetime = time.Hour
	// minTokenLifetime is the shortest token the TokenRequest API makes.
	minTokenLifetime = 10 * time.Minute
	// headlampTokenMargin is what is left of a token when the next request gets a new one, so that
	// none runs out under a request just sent.
	headlampTokenMargin = 5 * time.Minute
)

// headlampCookieName drops from a cluster's name what Headlamp's cookie names leave out.
var headlampCookieName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// maxHeadlampCookieCluster is as much of a cluster's name as Headlamp puts into its cookies' names.
const maxHeadlampCookieCluster = 50

// headlampOpenPath is where the interface's links open a cluster in Headlamp, the choice that
// signs a session in again after it signed out of the cluster in Headlamp. Headlamp has no route
// of its own under it.
const headlampOpenPath = "/kubehenge/open/"

// signInToHeadlamp sets the session's token for the cluster on a request Headlamp passes on to it,
// where the user may download the cluster's admin kubeconfig and has not signed in to the cluster
// in Headlamp with credentials of their own, which stay theirs to choose, nor signed out of it.
func (s *Server) signInToHeadlamp(r *http.Request, u user) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/clusters/")
	if !ok {
		return
	}
	cluster, _, ok := strings.Cut(rest, "/")
	if !ok || r.Header.Get("Authorization") != "" || ownHeadlampToken(r, cluster) {
		return
	}
	token, err := s.headlampToken(r.Context(), u, cluster)
	if err != nil {
		s.Log.WarnContext(r.Context(), "headlamp sign-in", "user", u.ticket.Username, "cluster", cluster, "error", err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
}

// ownHeadlampToken tells whether the browser holds a token of its own for the cluster in Headlamp,
// in the first of the cookies Headlamp keeps it in.
func ownHeadlampToken(r *http.Request, cluster string) bool {
	name := headlampCookieName.ReplaceAllString(cluster, "")
	if len(name) > maxHeadlampCookieCluster {
		name = name[:maxHeadlampCookieCluster]
	}
	c, err := r.Cookie("headlamp-auth-" + name + ".0")
	return err == nil && c.Value != ""
}

// signedOutKey names a session's choice to be signed out of a cluster in Headlamp.
func signedOutKey(u user, cluster string) string { return u.session.id + "\x00" + cluster }

// headlampToken is the session's token for the cluster Headlamp names, made when there is none or
// it runs out soon. Empty where the user may not have one, signed out of the cluster, or the
// cluster gives none. A token that could not be made again comes with the error while the one
// before still holds.
func (s *Server) headlampToken(ctx context.Context, u user, cluster string) (string, error) {
	access, err := s.headlampAccess(ctx, u, cluster)
	if err != nil || !access.admin {
		return "", err
	}
	h := s.Headlamp
	h.mu.Lock()
	uid, known := h.uids[cluster]
	_, signedOut := h.signedOut[signedOutKey(u, cluster)]
	if !known || signedOut {
		h.mu.Unlock()
		return "", nil
	}
	key := signedOutKey(u, cluster) + "\x00" + string(uid)
	in := h.signIns[key]
	if in == nil {
		if h.signIns == nil {
			h.signIns = map[string]*headlampSignIn{}
		}
		in = &headlampSignIn{}
		h.signIns[key] = in
	}
	h.mu.Unlock()

	// One token per session and cluster, whatever the number of Headlamp's requests waiting on it.
	in.mu.Lock()
	defer in.mu.Unlock()
	now := s.now()
	kept := ""
	if now.Before(in.expires) {
		kept = in.token
	}
	if (kept != "" && in.expires.Sub(now) > headlampTokenMargin) || now.Before(in.retry) {
		return kept, nil
	}
	in.retry = now.Add(headlampPrivilegeTTL)
	c := &clusterv1.Cluster{UID: uid}
	c.Namespace, c.Name, _ = strings.Cut(cluster, ".")
	token, expires, err := s.mintHeadlampToken(ctx, u, c, now)
	switch {
	case err != nil:
		return kept, err
	case token == "":
		in.token, in.expires = "", time.Time{}
		return "", nil
	}
	in.token, in.expires = token, expires
	return token, nil
}

// mintHeadlampToken has the cluster make a token of its Headlamp account for the session, with the
// issuer's token Kubehenge's controller keeps beside the cluster. None where that is not there,
// as when the sign-in is off.
func (s *Server) mintHeadlampToken(ctx context.Context, u user, c *clusterv1.Cluster, now time.Time) (string, time.Time, error) {
	kube := s.currentKube()
	if kube == nil {
		return "", time.Time{}, nil
	}
	issuer, _, err := s.reach(ctx, kube, c, provisioner.IdentityHeadlampIssuer)
	if err != nil || issuer == nil {
		return "", time.Time{}, err
	}
	lifetime := max(min(headlampTokenLifetime, u.session.created.Add(s.sessions.maxAge).Sub(now)), minTokenLifetime)
	seconds := int64(lifetime / time.Second)
	made, err := issuer.Kube.CoreV1().ServiceAccounts(metav1.NamespaceSystem).CreateToken(ctx, provisioner.HeadlampAccount,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("making a token of %s: %w", provisioner.HeadlampAccount, err)
	}
	if made.Status.Token == "" {
		return "", time.Time{}, errors.New("the cluster made no token")
	}
	expires := made.Status.ExpirationTimestamp.Time
	// The workload cluster's audit log records the account, with the token's ID beside it. This is
	// where the ID is tied to the Proxmox VE user.
	s.Log.InfoContext(ctx, "headlamp sign-in", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name,
		"credentialID", credentialID(made.Status.Token), "expires", expires.UTC().Format(time.RFC3339))
	return made.Status.Token, expires, nil
}

// credentialID is a ServiceAccount token's ID as the API server's audit log records it
// (authentication.kubernetes.io/credential-id), or empty for a token that carries none.
func credentialID(token string) string {
	_, rest, ok := strings.Cut(token, ".")
	payload, signature, ok2 := strings.Cut(rest, ".")
	if !ok || !ok2 || strings.Contains(signature, ".") {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims struct {
		JTI string `json:"jti"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.JTI == "" {
		return ""
	}
	return "JTI=" + claims.JTI
}

// headlampEncoded tells whether the shared Headlamp server may answer a request on its
// /encoded-clusters/ routes, those its log out takes, and answers it when not. Headlamp's log out
// of a cluster clears the cluster's token cookie there, and, for a cluster signed in to through an
// identity provider, ends that sign-in at the provider. Its clearing the cookie also signs the
// session out of the cluster, which would otherwise be signed straight in again.
func (s *Server) headlampEncoded(w http.ResponseWriter, r *http.Request, u user, tail string) bool {
	encoded, route, _ := strings.Cut(tail, "/")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 {
		http.Error(w, "malformed cluster", http.StatusBadRequest)
		return false
	}
	cluster := string(raw)
	post := r.Method == http.MethodPost
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	offered := (post && (route == "clear-token" || route == "oidc-logout")) || (read && route == "oidc-logout/redirect")
	if !offered {
		http.Error(w, "not offered here", http.StatusForbidden)
		return false
	}
	if !s.headlampSees(w, r, u, cluster) {
		return false
	}
	if route == "clear-token" {
		s.signOutOfHeadlamp(r.Context(), u, cluster)
	}
	return true
}

// signOutOfHeadlamp forgets the session's token for the cluster, which reached nobody else, and
// signs the session in to the cluster no more until it opens the cluster from the interface again.
func (s *Server) signOutOfHeadlamp(ctx context.Context, u user, cluster string) {
	h := s.Headlamp
	key := signedOutKey(u, cluster)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.signedOut == nil {
		h.signedOut = map[string]time.Time{}
	}
	h.signedOut[key] = u.session.created.Add(s.sessions.maxAge)
	for k := range h.signIns {
		if strings.HasPrefix(k, key+"\x00") {
			delete(h.signIns, k)
		}
	}
	s.Log.InfoContext(ctx, "headlamp sign-out", "user", u.ticket.Username, "cluster", cluster)
}

// openInHeadlamp answers the interface's link to a cluster in Headlamp: it lets the gate sign the
// session in to the cluster again, should it have signed out, and sends the browser on to the
// cluster's page there. The session's cookie is SameSite=Strict, so only a link of the same site
// gets here signed in.
func (s *Server) openInHeadlamp(w http.ResponseWriter, r *http.Request, u user) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cluster, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, headlampOpenPath), "/")
	if !s.headlampSees(w, r, u, cluster) {
		return
	}
	h := s.Headlamp
	h.mu.Lock()
	delete(h.signedOut, signedOutKey(u, cluster))
	h.mu.Unlock()
	to := "/c/" + cluster
	if rest != "" {
		to += "/" + rest
	}
	http.Redirect(w, r, (&url.URL{Path: to}).String(), http.StatusSeeOther)
}

// pruneHeadlampSignIns forgets the sign-ins whose token ran out and that are not being made, and
// the sign-outs of sessions that ended.
func (h *Headlamp) pruneHeadlampSignIns(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, in := range h.signIns {
		if !in.mu.TryLock() {
			continue
		}
		if !now.Before(in.expires) && !now.Before(in.retry) {
			delete(h.signIns, key)
		}
		in.mu.Unlock()
	}
	for key, until := range h.signedOut {
		if !now.Before(until) {
			delete(h.signedOut, key)
		}
	}
}
