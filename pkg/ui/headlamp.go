package ui

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// Headlamp puts one Headlamp server, shared by every cluster, behind the interface's sign-in. It
// runs on its own port, so its pages are another origin than the interface's and cannot read the
// interface's CSRF token. Headlamp has no sign-in of its own and its kubeconfig holds only each
// cluster's address and CA: users sign in to each cluster with their own credentials, which
// Headlamp keeps in cookies scoped to that cluster's path, or, where an administrator lets it and
// they may download the cluster's admin kubeconfig, are signed in by the gate (signInToHeadlamp).
type Headlamp struct {
	// Upstream is the Headlamp server, listening on the loopback interface only.
	Upstream *url.URL
	// Kubeconfig is the file the Headlamp server reads its clusters from and watches.
	Kubeconfig string
	// Port is where browsers reach Headlamp, on the interface's host name.
	Port int
	// InterfacePort is where browsers reach the interface, to sign in.
	InterfacePort int

	mu      sync.Mutex
	written []byte
	seen    map[string]seenEntry
	// endpoints are the clusters' API addresses and CAs, by cluster (headlampKey), which hold for a
	// cluster's life: kept, its kubeconfig Secret is read once rather than every round.
	endpoints map[string]headlampEndpoint
	// uids are the clusters in Headlamp's kubeconfig, by their names there, so that a sign-in to a
	// cluster is not taken for one to another created under its name since.
	uids map[string]types.UID
	// signIns are the sessions' sign-ins to clusters, by session, cluster and its UID.
	signIns map[string]*headlampSignIn
	// signedOut are the sessions that signed out of a cluster in Headlamp, by session and cluster,
	// until the session ends.
	signedOut map[string]time.Time
	// actions is whether an administrator has given the shared Headlamp its actions
	// (headlampActionsKey), as last read.
	actions bool
}

// headlampKey tells clusters apart, one created again under an earlier one's name too.
func headlampKey(c *clusterv1.Cluster) string {
	return string(c.UID) + "/" + c.Namespace + "/" + c.Name
}

type headlampEndpoint struct {
	server string
	ca     []byte
	until  time.Time
}

// headlampEndpointTTL is how long a cluster's address and CA are kept before they are read again.
const headlampEndpointTTL = 10 * time.Minute

// headlampOIDCCookie binds an OIDC sign-in Headlamp starts to the browser that started it, which
// Headlamp does not: it would accept the return of a sign-in begun anywhere and set that person's
// token. Lax, since the identity provider sends the browser back from its own site, which also
// keeps the interface's Strict session cookie from coming with the return.
const headlampOIDCCookie = "__Host-kubehenge-headlamp-oidc"

type seenEntry struct {
	visible bool
	// admin is whether the user may also download the cluster's admin kubeconfig, and so be signed
	// in to it as cluster-admin.
	admin bool
	until time.Time
}

// Headlamp asks Proxmox VE again after this long whether a user still sees a cluster; it fires
// many requests per page, and each would otherwise be a call to Proxmox VE.
const headlampPrivilegeTTL = 30 * time.Second

// headlampContext names a cluster in Headlamp. A namespace holds no dots, so the first one
// separates it from the name.
func headlampContext(namespace, name string) string { return namespace + "." + name }

// headlampDenied are Headlamp's routes the shared server must not offer: the external proxy fetches
// any address from the appliance, the multiplexer and the drain routes reach clusters they name
// themselves, around the per-cluster paths checked here (a cluster's own drain-node is one of
// those), the metrics are the server's own across every cluster, and the rest change the server's
// list of clusters or its credentials.
var headlampDenied = map[string]bool{
	"/externalproxy": true, "/wsMultiplexer": true, "/drain-node": true, "/drain-node-status": true,
	"/metrics": true, "/parseKubeConfig": true, "/cluster": true, "/auth/set-token": true,
}

// headlampOIDCLifetime is how long a sign-in Headlamp starts may take at the identity provider.
const headlampOIDCLifetime = 10 * time.Minute

// HeadlampHandler serves the Headlamp port.
func (s *Server) HeadlampHandler() http.Handler {
	s.init()
	proxy := &httputil.ReverseProxy{
		Rewrite:        s.Headlamp.rewrite,
		ModifyResponse: s.modifyHeadlampResponse,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.Log.Error("headlamp", "error", err)
			http.Error(w, "Headlamp is not running", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Strict-Transport-Security", hsts)
		if !headlampSameOrigin(r) {
			http.Error(w, "refused: the request did not come from Headlamp's own pages", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/oidc-callback" {
			if headlampCallbackBound(w, r) {
				proxy.ServeHTTP(w, r)
			}
			return
		}
		sess := s.sessions.lookup(r)
		var u user
		if sess != nil {
			u = user{session: sess, ticket: s.sessions.signedIn(r.Context(), sess)}
		}
		if u.ticket == nil {
			http.Redirect(w, r, s.interfaceURL(r), http.StatusSeeOther) //nolint:gosec // to the configured URL or the host the browser asked for, checked as a host name
			return
		}
		if s.headlampOffers(w, r, u) {
			s.signInToHeadlamp(r, u)
			proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), headlampUserKey{}, u)))
		}
	})
}

// headlampSameOrigin tells whether a request that may change something comes from Headlamp's own
// pages. The gate signs requests in as the user, so a forged one acts with the user's token.
// SameSite=Strict keeps the session cookie from other sites' requests, but not from another origin
// of the same site, such as a sibling host name or the interface's own port, and a WebSocket is not
// held to CORS: a page there could open a pod's exec. Headlamp's frontend cannot send the
// interface's CSRF token, so the origin the browser reports stands for it. Reads change nothing and
// pass, links from the interface among them; a WebSocket opens with a GET and is checked.
func headlampSameOrigin(r *http.Request) bool {
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.Header.Get("Upgrade") == "" {
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return strings.EqualFold(origin, "https://"+r.Host)
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

func (h *Headlamp) rewrite(pr *httputil.ProxyRequest) {
	// Headlamp answers only to a loopback Host with its own port or none, and the upstream is the
	// socket that starts it, on another port; so the Host goes without one. It sets its cookies
	// Secure only when told the browser came over HTTPS. Its OIDC callback URL is configured, not
	// taken from here.
	pr.SetURL(h.Upstream)
	pr.Out.Host = h.Upstream.Hostname()
	if strings.Contains(pr.Out.Host, ":") {
		pr.Out.Host = "[" + pr.Out.Host + "]"
	}
	pr.SetXForwarded()
	// With dynamic clusters, which the appliance leaves off, Headlamp would connect to the clusters a
	// KUBECONFIG header names, kept under the user an X-HEADLAMP-USER-ID header claims; the browser
	// chooses neither, whatever Headlamp's flags.
	pr.Out.Header.Del("Kubeconfig")
	pr.Out.Header.Del("X-Headlamp-User-Id")
	pr.Out.Header.Del("Cookie")
	for _, c := range pr.In.Cookies() {
		if !strings.HasPrefix(c.Name, "__Host-kubehenge-") {
			pr.Out.AddCookie(c)
		}
	}
	if pr.In.URL.Path == "/config" || pr.In.URL.Path == "/plugins" {
		pr.Out.Header.Del("Accept-Encoding")
	}
}

func (s *Server) modifyHeadlampResponse(resp *http.Response) error {
	switch resp.Request.URL.Path {
	case "/config":
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		u, ok := resp.Request.Context().Value(headlampUserKey{}).(user)
		if !ok {
			return errors.New("headlamp's config was asked for without a signed-in user")
		}
		return s.filterHeadlampConfig(resp, u)
	case "/plugins":
		return s.filterHeadlampPlugins(resp)
	case "/oidc":
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || loc.Query().Get("state") == "" {
			return errors.New("headlamp started an OIDC sign-in without a state")
		}
		resp.Header.Add("Set-Cookie", (&http.Cookie{
			Name: headlampOIDCCookie, Value: stateDigest(loc.Query().Get("state")), Path: "/",
			MaxAge: int(headlampOIDCLifetime.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		}).String())
		return nil
	case "/oidc-callback":
		return navigateOn(resp)
	default:
		return nil
	}
}

// headlampCallbackBound tells whether the return from the identity provider comes to the browser
// that started the sign-in, and answers it when not. The return carries no session cookie; the
// binding cookie set when a signed-in user started the sign-in stands for it.
func headlampCallbackBound(w http.ResponseWriter, r *http.Request) bool {
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie(headlampOIDCCookie)
	http.SetCookie(w, &http.Cookie{Name: headlampOIDCCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !read || err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(stateDigest(state))) != 1 {
		http.Error(w, "this sign-in was not started in this browser; start it again", http.StatusForbidden)
		return false
	}
	return true
}

// headlampOffers tells whether the shared Headlamp server may answer the signed-in user's request,
// and answers it when not.
func (s *Server) headlampOffers(w http.ResponseWriter, r *http.Request, u user) bool {
	p := r.URL.Path
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	// Headlamp's router cleans paths itself; anything it would read differently from this check is
	// refused rather than interpreted.
	if p != path.Clean(p) && p != path.Clean(p)+"/" || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		http.Error(w, "malformed path", http.StatusBadRequest)
		return false
	}
	switch {
	case strings.HasPrefix(p, "/clusters/"):
		name, rest, _ := strings.Cut(strings.TrimPrefix(p, "/clusters/"), "/")
		// Headlamp offers port forwards in its desktop app alone: a forward listens on the server's
		// loopback, which no browser reaches, for everyone signed in to the cluster.
		if rest == "portforward" || strings.HasPrefix(rest, "portforward/") || rest == "helm" {
			http.Error(w, "not offered here", http.StatusForbidden)
			return false
		}
		if route, ok := strings.CutPrefix(rest, "helm/"); ok {
			return s.headlampHelm(w, r, u, name, route)
		}
		return s.headlampSees(w, r, u, name)
	case strings.HasPrefix(p, "/encoded-clusters/"):
		return s.headlampEncoded(w, r, u, strings.TrimPrefix(p, "/encoded-clusters/"))
	case strings.HasPrefix(p, headlampOpenPath):
		s.openInHeadlamp(w, r, u)
		return false
	case p == "/oidc":
		if !read {
			http.Error(w, "no such cluster", http.StatusNotFound)
			return false
		}
		return s.headlampSees(w, r, u, r.URL.Query().Get("cluster"))
	case headlampDenied[p] || strings.HasPrefix(p, "/cluster/") || strings.HasPrefix(p, "/user-plugins/"),
		strings.HasPrefix(p, "/"+staticPluginsPrefix) && !s.Headlamp.actionsOffered():
		http.Error(w, "not offered here", http.StatusForbidden)
		return false
	case !read:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	default:
		return true
	}
}

// headlampSees tells whether the user sees the cluster Headlamp names, and answers the request
// when not.
func (s *Server) headlampSees(w http.ResponseWriter, r *http.Request, u user, cluster string) bool {
	visible, err := s.headlampVisible(r.Context(), u, cluster)
	if err != nil {
		s.proxmoxError(w, err)
		return false
	}
	if !visible {
		http.Error(w, "no such cluster", http.StatusNotFound)
		return false
	}
	return true
}

type headlampUserKey struct{}

// interfaceURL is where a browser without a session signs in: the interface on the host name the
// browser used, which names nothing the browser did not already reach.
func (s *Server) interfaceURL(r *http.Request) string {
	if s.PublicURL != "" {
		return strings.TrimSuffix(s.PublicURL, "/") + "/"
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		host = ip.String()
		if ip.To4() == nil {
			host = "[" + host + "]"
		}
	} else if !hostName.MatchString(host) {
		host = "localhost"
	}
	if p := s.Headlamp.InterfacePort; p != 0 && p != 443 {
		host += ":" + strconv.Itoa(p)
	}
	return (&url.URL{Scheme: "https", Host: host, Path: "/"}).String()
}

var hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// headlampVisible tells whether the user sees the cluster Headlamp names.
func (s *Server) headlampVisible(ctx context.Context, u user, cluster string) (bool, error) {
	e, err := s.headlampAccess(ctx, u, cluster)
	return e.visible, err
}

// headlampAccess is what the user may do with the cluster Headlamp names.
func (s *Server) headlampAccess(ctx context.Context, u user, cluster string) (seenEntry, error) {
	pool, ok := s.headlampPool(cluster)
	if !ok {
		return seenEntry{}, nil
	}
	h := s.Headlamp
	key := u.ticket.Username + "\x00" + pool
	now := s.now()
	h.mu.Lock()
	e, cached := h.seen[key]
	h.mu.Unlock()
	if cached && now.Before(e.until) {
		return e, nil
	}
	privs, err := s.Accounts.Privileges(ctx, u.ticket, pool)
	if err != nil {
		return seenEntry{}, fmt.Errorf("the privileges on %s: %w", pool, err)
	}
	visible := privs.Has(privSee)
	e = seenEntry{visible: visible, admin: visible && privs.Has(privKubeconfig), until: now.Add(headlampPrivilegeTTL)}
	h.mu.Lock()
	if h.seen == nil {
		h.seen = map[string]seenEntry{}
	}
	h.seen[key] = e
	h.mu.Unlock()
	return e, nil
}

// headlampPool is the pool of the cluster Headlamp names; not ok for a name no cluster of the
// interface can have, which no user sees.
func (s *Server) headlampPool(cluster string) (string, bool) {
	namespace, name, ok := strings.Cut(cluster, ".")
	if !ok {
		return "", false
	}
	pool, err := s.poolPath(namespace, name)
	return pool, err == nil
}

// maxHeadlampConfig bounds Headlamp's config, which lists one entry per cluster.
const maxHeadlampConfig = 8 << 20

// filterHeadlampConfig leaves in Headlamp's list of clusters only those the user sees.
func (s *Server) filterHeadlampConfig(resp *http.Response, u user) error {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxHeadlampConfig))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("reading headlamp's config: %w", err)
	}
	var config map[string]json.RawMessage
	if err = json.Unmarshal(raw, &config); err != nil {
		return fmt.Errorf("headlamp's config: %w", err)
	}
	var clusters []map[string]any
	if c, ok := config["clusters"]; ok && string(c) != jsonNull {
		if err = json.Unmarshal(c, &clusters); err != nil {
			return fmt.Errorf("headlamp's clusters: %w", err)
		}
	}
	kept, err := s.headlampVisibleOf(resp.Request.Context(), u, clusters)
	if err != nil {
		return err
	}
	if config["clusters"], err = json.Marshal(kept); err != nil {
		return fmt.Errorf("headlamp's clusters: %w", err)
	}
	out, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("headlamp's config: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Set("Cache-Control", "no-store")
	return nil
}

// headlampVisibleOf are the entries of Headlamp's list of clusters the user sees.
func (s *Server) headlampVisibleOf(ctx context.Context, u user, clusters []map[string]any) ([]map[string]any, error) {
	kept := []map[string]any{}
	for _, c := range clusters {
		name, ok := c["name"].(string)
		if !ok {
			continue
		}
		visible, err := s.headlampVisible(ctx, u, name)
		if err != nil {
			return nil, err
		}
		if visible {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// syncHeadlamp keeps Headlamp's kubeconfig to the clusters there are, until ctx ends.
func (s *Server) syncHeadlamp(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		kube := s.currentKube()
		if err := s.writeHeadlampKubeconfig(ctx, kube); err != nil {
			s.Log.Error("headlamp's kubeconfig", "error", err)
		}
		if err := s.readHeadlampActions(ctx, kube); err != nil {
			s.Log.Error("headlamp's actions", "error", err)
		}
		s.pruneHeadlampSeen()
		s.Headlamp.pruneHeadlampSignIns(s.now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) pruneHeadlampSeen() {
	h := s.Headlamp
	now := s.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, e := range h.seen {
		if now.After(e.until) {
			delete(h.seen, k)
		}
	}
}

// navigateOn turns Headlamp's redirect at the end of an OIDC sign-in into a page that navigates on
// by itself. A redirect would continue the identity provider's cross-site navigation, on which
// browsers send no SameSite=Strict cookie, and the next page would find the user signed out.
func navigateOn(resp *http.Response) error {
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return nil
	}
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return fmt.Errorf("headlamp's redirect: %w", err)
	}
	// Only somewhere on this origin, as a path.
	target := (&url.URL{Path: to.Path, RawQuery: to.RawQuery}).String()
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		target = "/"
	}
	page := []byte(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0; url=` +
		html.EscapeString(target) + `"><title>Signing in</title><a href="` + html.EscapeString(target) + `">Continue</a>`)
	_ = resp.Body.Close()
	resp.StatusCode, resp.Status = http.StatusOK, "200 OK"
	resp.Header.Del("Location")
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")
	resp.Header.Set("Cache-Control", "no-store")
	resp.Header.Set("Content-Length", strconv.Itoa(len(page)))
	resp.ContentLength = int64(len(page))
	resp.Body = io.NopCloser(bytes.NewReader(page))
	return nil
}

// writeHeadlampKubeconfig writes every cluster of the namespaces the interface manages with its
// API address and CA, taken from the cluster's admin kubeconfig. Its credentials are left behind:
// Headlamp would use them for every visitor who has not signed in to the cluster. Without a
// management cluster the file lists none, so Headlamp still starts.
func (s *Server) writeHeadlampKubeconfig(ctx context.Context, kube client.Client) error {
	clusters, err := s.managedClusters(ctx, kube)
	if err != nil {
		return err
	}
	out := clientcmdapi.NewConfig()
	out.AuthInfos["user"] = clientcmdapi.NewAuthInfo()
	var classes map[string]*clusterv1.ClusterClass
	now := time.Now()
	uids := map[string]types.UID{}
	for i := range clusters {
		c := &clusters[i]
		if !provisioner.Owns(c, s.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
			continue
		}
		var endpoint headlampEndpoint
		var ok bool
		endpoint, ok, err = s.headlampEndpoint(ctx, kube, c, now)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		name := headlampContext(c.Namespace, c.Name)
		cluster := clientcmdapi.NewCluster()
		cluster.Server, cluster.CertificateAuthorityData = endpoint.server, endpoint.ca
		out.Clusters[name] = cluster
		kctx := clientcmdapi.NewContext()
		kctx.Cluster, kctx.AuthInfo = name, "user"
		if classes == nil {
			if classes, err = s.classesByName(ctx, kube); err != nil {
				return err
			}
		}
		if auth := headlampAuth(firstOIDC(c, classes[c.Spec.Topology.ClassRef.Name])); auth != nil {
			out.AuthInfos[name] = auth
			kctx.AuthInfo = name
		}
		out.Contexts[name] = kctx
		uids[name] = c.UID
	}
	raw, err := clientcmd.Write(*out)
	if err != nil {
		return fmt.Errorf("headlamp's kubeconfig: %w", err)
	}
	s.Headlamp.mu.Lock()
	s.Headlamp.uids = uids
	s.Headlamp.mu.Unlock()
	return s.Headlamp.write(raw, now)
}

// managedClusters lists the clusters of the namespaces the interface manages, none without a
// management cluster.
func (s *Server) managedClusters(ctx context.Context, kube client.Client) ([]clusterv1.Cluster, error) {
	if kube == nil {
		return nil, nil
	}
	managed, err := s.namespaces(ctx, kube)
	if err != nil {
		return nil, err
	}
	var list clusterv1.ClusterList
	if err := kube.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing the clusters: %w", err)
	}
	return slices.DeleteFunc(list.Items, func(c clusterv1.Cluster) bool { return !managed.has(c.Namespace) }), nil
}

// classesByName lists every cluster's class at once. A class's default identity provider may
// change with an upgrade of Kubehenge, so it is read each round.
func (s *Server) classesByName(ctx context.Context, kube client.Client) (map[string]*clusterv1.ClusterClass, error) {
	var all clusterv1.ClusterClassList
	if err := kube.List(ctx, &all, client.InNamespace(s.ClassNamespace)); err != nil {
		return nil, fmt.Errorf("listing the cluster classes: %w", err)
	}
	classes := make(map[string]*clusterv1.ClusterClass, len(all.Items))
	for i := range all.Items {
		classes[all.Items[i].Name] = &all.Items[i]
	}
	return classes, nil
}

// headlampAuth is the identity provider's sign-in Headlamp offers beside the token, as a public
// client with PKCE, or nil without one. The auth-provider stanza holds no secret and no token.
func headlampAuth(p *appliance.OIDCProvider) *clientcmdapi.AuthInfo {
	if p == nil {
		return nil
	}
	scopes := p.Scopes
	if len(scopes) == 0 {
		scopes = []string{"email", "profile"}
	}
	cfg := map[string]string{"idp-issuer-url": p.IssuerURL, "client-id": p.ClientID, "scope": strings.Join(scopes, ",")}
	if p.CertificateAuthority != "" {
		cfg["idp-certificate-authority-data"] = base64.StdEncoding.EncodeToString([]byte(p.CertificateAuthority))
	}
	auth := clientcmdapi.NewAuthInfo()
	auth.AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "oidc", Config: cfg}
	return auth
}

// write replaces Headlamp's kubeconfig when it changed, and forgets the endpoints that ran out.
func (h *Headlamp) write(raw []byte, now time.Time) error {
	h.mu.Lock()
	for key, e := range h.endpoints {
		if now.After(e.until) {
			delete(h.endpoints, key)
		}
	}
	unchanged := bytes.Equal(raw, h.written)
	h.mu.Unlock()
	if unchanged {
		return nil
	}
	if err := writePublicFile(h.Kubeconfig, raw); err != nil {
		return err
	}
	h.mu.Lock()
	h.written = raw
	h.mu.Unlock()
	return nil
}

// headlampEndpoint is a cluster's API address and CA from the admin kubeconfig Cluster API keeps
// for it; not ok until that exists.
func (s *Server) headlampEndpoint(ctx context.Context, kube client.Client, c *clusterv1.Cluster, now time.Time) (headlampEndpoint, bool, error) {
	h := s.Headlamp
	h.mu.Lock()
	e, ok := h.endpoints[headlampKey(c)]
	h.mu.Unlock()
	if ok && now.Before(e.until) {
		return e, true, nil
	}
	secret := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name + "-kubeconfig"}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return headlampEndpoint{}, false, nil
		}
		return headlampEndpoint{}, false, fmt.Errorf("cluster %s/%s's kubeconfig: %w", c.Namespace, c.Name, err)
	}
	admin, err := clientcmd.Load(secret.Data["value"])
	if err != nil {
		s.Log.Warn("unreadable kubeconfig", "cluster", c.Namespace+"/"+c.Name, "error", err)
		return headlampEndpoint{}, false, nil
	}
	current := admin.Contexts[admin.CurrentContext]
	if current == nil || admin.Clusters[current.Cluster] == nil {
		return headlampEndpoint{}, false, nil
	}
	from := admin.Clusters[current.Cluster]
	e = headlampEndpoint{server: from.Server, ca: from.CertificateAuthorityData, until: now.Add(headlampEndpointTTL)}
	h.mu.Lock()
	if h.endpoints == nil {
		h.endpoints = map[string]headlampEndpoint{}
	}
	h.endpoints[headlampKey(c)] = e
	h.mu.Unlock()
	return e, true, nil
}

// firstOIDC is the first identity provider the cluster trusts, the one sign-in tools use, or nil:
// from its own oidc variable, or, left unset, from its class's default, which its API server then
// trusts. class may be nil.
func firstOIDC(c *clusterv1.Cluster, class *clusterv1.ClusterClass) *appliance.OIDCProvider {
	var raw []byte
	for _, v := range c.Spec.Topology.Variables {
		if v.Name == oidcVariable {
			raw = v.Value.Raw
		}
	}
	if raw == nil && class != nil {
		if d := classDefault(class, oidcVariable); d != nil {
			raw = d.Raw
		}
	}
	// The value was checked when it was set; here it is only read.
	var providers []appliance.OIDCProvider
	if raw == nil || json.Unmarshal(raw, &providers) != nil || len(providers) == 0 {
		return nil
	}
	return &providers[0]
}

// publicFile is the mode of a file another user, such as Headlamp's, reads.
const publicFile os.FileMode = 0o644

// writePublicFile replaces a file another user reads, by rename so a reader never sees half of it.
func writePublicFile(name string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return fmt.Errorf("replacing %s: %w", name, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(data)
	if err = errors.Join(err, tmp.Chmod(publicFile), tmp.Close()); err != nil {
		return fmt.Errorf("replacing %s: %w", name, err)
	}
	if err = os.Rename(tmp.Name(), name); err != nil {
		return fmt.Errorf("replacing %s: %w", name, err)
	}
	return nil
}
