package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// fakeAccounts is Proxmox VE with password users, TOTP for some, and privileges granted on paths
// and inherited below them.
type fakeAccounts struct {
	mu        sync.Mutex
	passwords map[string]string
	totp      map[string]string
	grants    map[string]map[string][]string // user -> path -> privileges
	// acls are paths with ACL entries besides the grants, such as a cluster's pool with its own
	// user's; privilegeCalls counts the questions about privileges.
	acls           []string
	privilegeCalls int
	backupChecks   int
	renewals       int
	tokenChecks    int
	now            func() time.Time
	// members are every pool's guests; deleted records the VMs deleted.
	members []pve.Member
	deleted []int
	// listed, when set, runs after each listing of the templates.
	listed func()
	// released records the prefixes of the HA objects removed.
	released []string
	// loadReads records the VMs whose load was read.
	loadReads []int
	// users are the users made with an administrator token, by ID; usersDeleted counts deletions.
	users        map[string]string
	usersDeleted int
	rebooted     []int
}

func (f *fakeAccounts) CreateAdministratorToken(_ context.Context, t *pve.Ticket, userid, name, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.effective(t.Username, "/").Has("Permissions.Modify", "User.Modify") {
		return "", pve.ErrForbidden
	}
	if f.users == nil {
		f.users = map[string]string{}
	}
	f.users[userid] = userid + "!" + name + "=5ec2e7"
	return f.users[userid], nil
}

func (f *fakeAccounts) DeleteUser(_ context.Context, _ *pve.Ticket, userid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.users, userid)
	f.usersDeleted++
	return nil
}

// TokenUser takes a token whose secret is "secret" for any user with a password, as acting with
// that user's privileges, a token without privilege separation.
func (f *fakeAccounts) TokenUser(_ context.Context, token string) (*pve.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenChecks++
	id, secret, _ := strings.Cut(token, "=")
	username, _, _ := strings.Cut(id, "!")
	if secret != "secret" || f.passwords[username] == "" {
		return nil, pve.ErrAuthentication
	}
	return &pve.Ticket{Username: id, Token: token, Issued: f.now()}, nil
}

func (f *fakeAccounts) SignIn(_ context.Context, username, password string) (*pve.Ticket, *pve.Challenge, error) {
	if f.passwords[username] == "" || f.passwords[username] != password {
		return nil, nil, pve.ErrAuthentication
	}
	if f.totp[username] != "" {
		return nil, &pve.Challenge{Username: username, TOTP: true}, nil
	}
	return &pve.Ticket{Username: username, Ticket: "ticket:" + username, Issued: f.now()}, nil, nil
}

func (f *fakeAccounts) Answer(_ context.Context, c *pve.Challenge, kind, code string) (*pve.Ticket, error) {
	if kind != "totp" || f.totp[c.Username] != code {
		return nil, pve.ErrAuthentication
	}
	return &pve.Ticket{Username: c.Username, Ticket: "ticket:" + c.Username, Issued: f.now()}, nil
}

func (f *fakeAccounts) Renew(_ context.Context, t *pve.Ticket) (*pve.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.passwords[t.Username] == "" {
		return nil, pve.ErrAuthentication
	}
	f.renewals++
	return &pve.Ticket{Username: t.Username, Ticket: t.Ticket, Issued: f.now()}, nil
}

func (f *fakeAccounts) Privileges(_ context.Context, t *pve.Ticket, path string) (pve.Privileges, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.privilegeCalls++
	return f.effective(t.Username, path), nil
}

// AllPrivileges lists, as Proxmox VE does, the top-level paths and every path with an ACL entry,
// anyone's, each where the user holds a privilege.
func (f *fakeAccounts) AllPrivileges(_ context.Context, t *pve.Ticket) (map[string]pve.Privileges, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.privilegeCalls++
	paths := map[string]bool{"/": true, "/access": true, "/access/groups": true, "/nodes": true, "/pool": true, "/sdn": true, "/storage": true, "/vms": true}
	for _, grants := range f.grants {
		for p := range grants {
			paths[p] = true
		}
	}
	for _, p := range f.acls {
		paths[p] = true
	}
	all := map[string]pve.Privileges{}
	for p := range paths {
		if privs := f.effective(t.Username, p); len(privs) > 0 {
			all[p] = privs
		}
	}
	return all, nil
}

func (*fakeAccounts) Realms(context.Context) ([]pve.Realm, error) {
	return []pve.Realm{{Realm: "pve", Type: "pve", Default: true}, {Realm: "sso", Type: "openid"}}, nil
}

// The identity provider of realm sso: code "good" signs in sso-user@sso.
func (*fakeAccounts) OpenIDAuthURL(_ context.Context, realm, redirect string) (string, error) {
	// Proxmox VE's state holds quotes, as here.
	return "https://idp.example/auth?state=" + url.QueryEscape(`"st-`+realm+`"`) + "&redirect_uri=" + url.QueryEscape(redirect), nil
}

func (f *fakeAccounts) OpenIDLogin(_ context.Context, code, state, _ string) (*pve.Ticket, error) {
	if code != "good" || state != `"st-sso"` {
		return nil, pve.ErrAuthentication
	}
	return &pve.Ticket{Username: "sso-user@sso", Ticket: "ticket:sso", Issued: f.now()}, nil
}

func (*fakeAccounts) Storages(context.Context, *pve.Ticket) ([]pve.Storage, error) {
	return []pve.Storage{
		{Storage: "local-lvm", Node: "pve1", Content: []string{"images", "rootdir"}},
		{Storage: "tank", Node: "pve1", Content: []string{"images"}},
		{Storage: "local", Node: "pve1", Content: []string{"iso", "import", "vztmpl"}},
	}, nil
}

func (*fakeAccounts) Nodes(context.Context, *pve.Ticket) ([]pve.Node, error) {
	return []pve.Node{{Node: "pve1", Online: true}, {Node: "pve2", Online: true}}, nil
}

// NodeAffinityRules answers those with Sys.Audit on / with a rule keeping to pve2, one keeping off
// it, and one naming only a node nobody sees.
func (f *fakeAccounts) NodeAffinityRules(_ context.Context, t *pve.Ticket) ([]pve.HARule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.effective(t.Username, "/").Has("Sys.Audit") {
		return nil, pve.ErrForbidden
	}
	return []pve.HARule{
		{Rule: "fast", Nodes: []string{"pve2"}, Strict: true},
		{Rule: "not-pve2", Nodes: []string{"pve2"}, Negative: true},
		{Rule: "gone", Nodes: []string{"pve9"}},
	}, nil
}

func (f *fakeAccounts) ReleaseHA(_ context.Context, t *pve.Ticket, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.effective(t.Username, "/").Has("Sys.Console") {
		return pve.ErrForbidden
	}
	f.released = append(f.released, prefix)
	return nil
}

func (*fakeAccounts) Bridges(_ context.Context, _ *pve.Ticket, node string) ([]pve.Bridge, error) {
	return []pve.Bridge{{Bridge: "vmbr0", Node: node, CIDR: "10.0.0.5/24", Gateway: "10.0.0.1"}, {Bridge: "vmbr1", Node: node}}, nil
}

func (*fakeAccounts) DNS(context.Context, *pve.Ticket, string) ([]string, error) {
	return []string{"192.0.2.53"}, nil
}

// PoolMembers answers as Proxmox VE does, without the tags.
func (f *fakeAccounts) PoolMembers(context.Context, *pve.Ticket, string) ([]pve.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	members := slices.Clone(f.members)
	for i := range members {
		members[i].Tags = ""
	}
	return members, nil
}

// VMLoad answers a load of a quarter for every VM.
func (f *fakeAccounts) VMLoad(_ context.Context, _ *pve.Ticket, _ string, vmid int) ([]pve.LoadPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadReads = append(f.loadReads, vmid)
	return []pve.LoadPoint{{Time: 1}, {Time: 2, CPU: new(0.25)}}, nil
}

func (f *fakeAccounts) Templates(context.Context, *pve.Ticket, string) ([]pve.Member, error) {
	f.mu.Lock()
	var list []pve.Member
	for i := range f.members {
		if m := &f.members[i]; m.Template == 1 {
			list = append(list, *m)
		}
	}
	listed := f.listed
	f.mu.Unlock()
	if listed != nil {
		listed()
	}
	return list, nil
}

func (f *fakeAccounts) DeleteVM(_ context.Context, _ *pve.Ticket, _ string, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, vmid)
	return nil
}

// RebootVM reboots for those with VM.PowerMgmt on the cluster pools, as Proxmox VE checks it on
// the VM.
func (f *fakeAccounts) RebootVM(_ context.Context, t *pve.Ticket, _ string, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.effective(t.Username, "/pool/kubehenge").Has("VM.PowerMgmt") {
		return pve.ErrForbidden
	}
	f.rebooted = append(f.rebooted, vmid)
	return nil
}

// BackupCovers knows guest 150 as backed up.
func (f *fakeAccounts) BackupCovers(_ context.Context, _ *pve.Ticket, vmid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backupChecks++
	return vmid == 150, nil
}

// Zone knows one SDN VNet, tenants, in zone evpn; everything else is a plain bridge.
func (*fakeAccounts) Zone(_ context.Context, _ *pve.Ticket, bridge string) (string, error) {
	if bridge == "tenants" {
		return "evpn", nil
	}
	return "localnetwork", nil
}

func (f *fakeAccounts) effective(username, aclPath string) pve.Privileges {
	username, _, _ = strings.Cut(username, "!")
	privs := pve.Privileges{}
	for p, granted := range f.grants[username] {
		if aclPath == p || strings.HasPrefix(aclPath, p+"/") || p == "/" {
			for _, g := range granted {
				privs[g] = true
			}
		}
	}
	return privs
}

type harness struct {
	t        *testing.T
	server   *Server
	handler  http.Handler
	accounts *fakeAccounts
	kube     client.Client
	clock    time.Time
}

func newHarness(t *testing.T, objects ...client.Object) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	// The kinds the interface reads only as unstructured objects: the add-on releases, and the
	// control plane of whichever provider, which it finds through the RESTMapper.
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{rke2ControlPlane.GroupVersion()})
	for _, gvk := range []schema.GroupVersionKind{helmReleaseProxyList.GroupVersion().WithKind("HelmReleaseProxy"), rke2ControlPlane} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	h := &harness{t: t, clock: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	h.accounts = &fakeAccounts{
		passwords: map[string]string{"admin@pve": "secret", "viewer@pve": "secret", "dev@pve": "secret", "otp@pve": "secret"},
		totp:      map[string]string{"otp@pve": "123456"},
		grants: map[string]map[string][]string{
			"admin@pve": {"/": {
				"VM.Audit", "VM.Allocate", "VM.Console", "VM.PowerMgmt", "Sys.Modify", "Datastore.AllocateSpace", "SDN.Use",
				"Permissions.Modify", "User.Modify", "Group.Allocate", "Realm.AllocateUser", "Pool.Allocate", "Datastore.AllocateTemplate", "Sys.Audit",
			}},
			"viewer@pve": {"/pool/kubehenge/default.one": {"VM.Audit"}},
			"dev@pve": {
				"/pool/kubehenge":                  {"VM.Audit", "VM.Allocate"},
				"/storage/local-lvm":               {"Datastore.AllocateSpace"},
				"/sdn/zones/localnetwork/vmbr0":    {"SDN.Use"},
				"/sdn/zones/localnetwork/vmbr1/10": {"SDN.Use"},
				"/sdn/zones/localnetwork/tenants":  {"SDN.Use"},
				"/pool/kubehenge/default.one":      {"Sys.Modify"},
				"/pool/kubehenge-management":       {"VM.Audit"},
			},
			"otp@pve": {"/": {"VM.Audit"}},
		},
		now: func() time.Time { return h.clock },
	}
	objects = append(objects, rke2Class())
	h.kube = fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objects...).Build()
	h.server = &Server{
		Accounts:       h.accounts,
		Kube:           h.kube,
		WorkloadPool:   "kubehenge",
		ClassNamespace: "kubehenge-system",
		Namespace:      "default",
		SessionMaxAge:  8 * time.Hour,
		Assets: fstest.MapFS{
			"index.html":       {Data: []byte("<!doctype html><title>Kubehenge</title>")},
			"assets/app-1.js":  {Data: []byte("console.log(1)")},
			"assets/style.css": {Data: []byte("body{}")},
		},
	}
	h.handler = h.server.Handler()
	h.server.clock = func() time.Time { return h.clock }
	h.server.sessions.now = func() time.Time { return h.clock }
	h.server.limiter.now = func() time.Time { return h.clock }
	return h
}

func rke2Class() *clusterv1.ClusterClass {
	variableSchema := func(s *clusterv1.JSONSchemaProps) clusterv1.VariableSchema {
		return clusterv1.VariableSchema{OpenAPIV3Schema: *s}
	}
	required := true
	return &clusterv1.ClusterClass{
		Namespace: "kubehenge-system", Name: "kubehenge-rke2", Annotations: map[string]string{
			"kubehenge.dev/default-pods": "10.42.0.0/16", "kubehenge.dev/default-services": "10.43.0.0/16",
		},
		Spec: clusterv1.ClusterClassSpec{Variables: []clusterv1.ClusterClassVariable{
			{Name: "controlPlaneEndpoint", Required: &required, Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "string", Format: "ipv4"})},
			{Name: "network", Required: &required, Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "object"})},
			{Name: "storage", Required: &required, Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "string"})},
			{Name: "headlamp", Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "object"})},
			{Name: "rancher", Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "object"})},
			{Name: "csi", Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "object"})},
			{Name: "ccm", Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "object"})},
			{Name: "certificates", Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "object"})},
			{Name: "gitops", Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "object"})},
			{Name: "nodeOS", Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "object", Default: &apiextensionsv1.JSON{
				Raw: []byte(`{"family":"almalinux","release":"10"}`),
			}})},
			{Name: "oidc", Schema: variableSchema(&clusterv1.JSONSchemaProps{Type: "array", Default: &apiextensionsv1.JSON{
				Raw: []byte(`[{"issuerURL":"https://sso.example","clientID":"kubernetes"},{"issuerURL":"https://lab.example","clientID":"lab"}]`),
			}})},
		}},
	}
}

func cluster(namespace, name string) *clusterv1.Cluster {
	cp, workers := int32(1), int32(2)
	return &clusterv1.Cluster{
		Namespace: namespace, Name: name,
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef:     clusterv1.ClusterClassRef{Name: "kubehenge-rke2", Namespace: "kubehenge-system"},
			Version:      "v1.36.5+rke2r1",
			ControlPlane: clusterv1.ControlPlaneTopology{Replicas: &cp},
			Workers:      clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: &workers}}},
			Variables: []clusterv1.ClusterVariable{
				{Name: "storage", Value: apiextensionsv1.JSON{Raw: []byte(`"local-lvm"`)}},
			},
		}},
	}
}

// browser is one signed-in browser: it keeps the session cookie and the CSRF token.
type browser struct {
	h      *harness
	cookie string
	csrf   string
	// address is where the browser connects from; unset, 192.0.2.10.
	address string
	// generation, when set, is sent with each request as the cluster's generation a change was
	// made against.
	generation string
	// token, when set, is sent with each request as a Proxmox VE API token, as a script would.
	token string
}

func (h *harness) browser() *browser { return &browser{h: h} }

func (b *browser) do(method, path, body string) *httptest.ResponseRecorder {
	b.h.t.Helper()
	req := httptest.NewRequestWithContext(b.h.t.Context(), method, "https://kubehenge.example"+path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:50000"
	if b.address != "" {
		req.RemoteAddr = b.address + ":50000"
	}
	if b.generation != "" {
		req.Header.Set(generationHeader, b.generation)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.cookie != "" {
		req.Header.Set("Cookie", sessionCookie+"="+b.cookie)
	}
	if b.csrf != "" {
		req.Header.Set(csrfHeader, b.csrf)
	}
	if b.token != "" {
		req.Header.Set("Authorization", tokenPrefix+b.token)
	}
	rec := httptest.NewRecorder()
	b.h.handler.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
				b.h.t.Fatalf("session cookie %+v is not Secure, HttpOnly, SameSite=Strict for /", c)
			}
			b.cookie = c.Value
		}
	}
	var view sessionView
	if strings.HasPrefix(path, "/api/session") && json.Unmarshal(rec.Body.Bytes(), &view) == nil && view.CSRF != "" {
		b.csrf = view.CSRF
	}
	return rec
}

func (b *browser) signIn(username string) {
	b.h.t.Helper()
	if rec := b.do("POST", "/api/session", `{"username":"`+username+`","password":"secret"}`); rec.Code != http.StatusOK {
		b.h.t.Fatalf("sign-in as %s: %d %s", username, rec.Code, rec.Body)
	}
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
}

func TestSignInAndOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	rec := b.do("GET", "/api/session", "")
	expect(t, rec, http.StatusUnauthorized)
	if rec.Header().Get(versionHeader) != "" {
		t.Fatal("the version is told to someone not signed in")
	}
	b.signIn("admin@pve")
	rec = b.do("GET", "/api/session", "")
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"user":"admin@pve"`) {
		t.Fatalf("session = %s", rec.Body)
	}
	if rec.Header().Get(versionHeader) == "" {
		t.Fatal("no version for a signed-in user")
	}
	old := b.cookie
	expect(t, b.do("DELETE", "/api/session", ""), http.StatusNoContent)
	b.cookie = old
	expect(t, b.do("GET", "/api/session", ""), http.StatusUnauthorized)
}

func TestSignInWithRealm(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	expect(t, b.do("POST", "/api/session", `{"username":"admin","realm":"pve","password":"secret"}`), http.StatusOK)
}

func TestFailedSignInsAreLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	for range 5 {
		expect(t, b.do("POST", "/api/session", `{"username":"admin@pve","password":"wrong"}`), http.StatusUnauthorized)
	}
	expect(t, b.do("POST", "/api/session", `{"username":"admin@pve","password":"secret"}`), http.StatusTooManyRequests)
	h.clock = h.clock.Add(16 * time.Minute)
	expect(t, b.do("POST", "/api/session", `{"username":"admin@pve","password":"secret"}`), http.StatusOK)
}

// Failed sign-ins hold back their address, not the user everywhere: nobody can lock an
// administrator out from elsewhere.
func TestFailedSignInsHoldBackTheirAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	attacker := h.browser()
	attacker.address = "198.51.100.7"
	for range failuresAllowed {
		expect(t, attacker.do("POST", "/api/session", `{"username":"admin@pve","password":"wrong"}`), http.StatusUnauthorized)
	}
	expect(t, attacker.do("POST", "/api/session", `{"username":"admin@pve","password":"secret"}`), http.StatusTooManyRequests)
	h.browser().signIn("admin@pve")

	// Other users from the same address are held back only once the address has used up its own.
	for i := failuresAllowed; i < addressFailuresAllowed; i++ {
		expect(t, attacker.do("POST", "/api/session", fmt.Sprintf(`{"username":"user%d@pve","password":"wrong"}`, i)), http.StatusUnauthorized)
	}
	expect(t, attacker.do("POST", "/api/session", `{"username":"dev@pve","password":"secret"}`), http.StatusTooManyRequests)
	h.browser().signIn("dev@pve")
}

// Guesses still being checked count as failures, so parallel requests get no more than the limit.
func TestLimiterCountsGuessesInFlight(t *testing.T) {
	t.Parallel()
	l := newLimiter(time.Minute)
	c := count{key: "user", allowed: 3}
	pending := make([]func(bool), 0, c.allowed)
	for range c.allowed {
		done, ok := l.reserve(c)
		if !ok {
			t.Fatal("a guess within the limit was refused")
		}
		pending = append(pending, done)
	}
	if _, ok := l.reserve(c); ok {
		t.Fatal("a guess beyond those in flight was let through")
	}
	pending[0](false)
	pending[0](true)
	done, ok := l.reserve(c)
	if !ok {
		t.Fatal("a right guess did not give its count back")
	}
	pending[1](true)
	pending[2](true)
	done(true)
	if _, ok := l.reserve(c); ok {
		t.Fatal("three failures did not hold the key back")
	}
	if len(l.pending) != 0 {
		t.Fatalf("pending left behind: %v", l.pending)
	}
}

// slowRenewals holds every ticket renewal until released, as a Proxmox VE that is slow to answer.
type slowRenewals struct {
	*fakeAccounts

	started chan struct{}
	release chan struct{}
}

func (f *slowRenewals) Renew(ctx context.Context, t *pve.Ticket) (*pve.Ticket, error) {
	f.started <- struct{}{}
	<-f.release
	return f.fakeAccounts.Renew(ctx, t)
}

// A renewal waits for Proxmox VE without a lock: other users' requests, the same session's and the
// pruning go on meanwhile, and the session's ticket is renewed once.
func TestSlowRenewalHoldsUpNobodyElse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	slow := &slowRenewals{fakeAccounts: h.accounts, started: make(chan struct{}, 1), release: make(chan struct{})}
	h.server.sessions.accounts = slow
	a := h.browser()
	a.signIn("admin@pve")
	h.clock = h.clock.Add(renewAfter + time.Minute)
	b := h.browser()
	b.signIn("dev@pve")

	session := func(cookie string) *http.Request {
		req := httptest.NewRequestWithContext(t.Context(), "GET", "https://kubehenge.example/api/session", nil)
		req.Header.Set("Cookie", sessionCookie+"="+cookie)
		return req
	}
	renewing := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, session(a.cookie))
		renewing <- rec.Code
	}()
	<-slow.started

	beside := make(chan error)
	go func() {
		for _, cookie := range []string{b.cookie, a.cookie} {
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, session(cookie))
			if rec.Code != http.StatusOK {
				beside <- fmt.Errorf("a request beside the renewal: %d %s", rec.Code, rec.Body)
				return
			}
		}
		h.server.sessions.prune()
		beside <- nil
	}()
	select {
	case err := <-beside:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("requests and pruning waited for another session's renewal")
	}
	close(slow.release)
	if code := <-renewing; code != http.StatusOK {
		t.Fatalf("the renewing request: %d", code)
	}
	h.accounts.mu.Lock()
	defer h.accounts.mu.Unlock()
	if h.accounts.renewals != 1 {
		t.Fatalf("%d renewals, want 1", h.accounts.renewals)
	}
}

func TestSignInNeedsJSON(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	req := httptest.NewRequestWithContext(t.Context(), "POST", "https://kubehenge.example/api/session", strings.NewReader("username=admin@pve&password=secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	expect(t, rec, http.StatusUnsupportedMediaType)
}

func TestSecondFactor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	rec := b.do("POST", "/api/session", `{"username":"otp@pve","password":"secret"}`)
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"secondFactor":{"totp":true`) {
		t.Fatalf("no challenge: %s", rec.Body)
	}
	pending := b.cookie
	expect(t, b.do("GET", "/api/clusters", ""), http.StatusUnauthorized)

	csrf := b.csrf
	b.csrf = "wrong"
	expect(t, b.do("POST", "/api/session/second-factor", `{"kind":"totp","code":"123456"}`), http.StatusUnauthorized)
	b.csrf = csrf
	expect(t, b.do("POST", "/api/session/second-factor", `{"kind":"totp","code":"000000"}`), http.StatusUnauthorized)
	expect(t, b.do("POST", "/api/session/second-factor", `{"kind":"totp","code":"123456"}`), http.StatusOK)
	if b.cookie == pending {
		t.Fatal("the session ID was not replaced after the second factor")
	}
	expect(t, b.do("GET", "/api/clusters", ""), http.StatusOK)

	b.cookie = pending
	expect(t, b.do("GET", "/api/clusters", ""), http.StatusUnauthorized)
}

func TestChallengeExpires(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	expect(t, b.do("POST", "/api/session", `{"username":"otp@pve","password":"secret"}`), http.StatusOK)
	h.clock = h.clock.Add(challengeLifetime)
	expect(t, b.do("POST", "/api/session/second-factor", `{"kind":"totp","code":"123456"}`), http.StatusUnauthorized)
}

func TestSessionsRenewAndEnd(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("admin@pve")
	for range 7 {
		h.clock = h.clock.Add(time.Hour + time.Minute)
		expect(t, b.do("GET", "/api/clusters", ""), http.StatusOK)
	}
	if h.accounts.renewals != 7 {
		t.Fatalf("renewed %d times, want 7", h.accounts.renewals)
	}
	h.clock = h.clock.Add(time.Hour)
	expect(t, b.do("GET", "/api/clusters", ""), http.StatusUnauthorized)
}

func TestTicketExpiresWithoutUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("admin@pve")
	h.clock = h.clock.Add(pve.TicketLifetime)
	expect(t, b.do("GET", "/api/clusters", ""), http.StatusUnauthorized)
}

func TestChangesNeedTheCSRFToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("admin@pve")
	b.csrf = ""
	expect(t, b.do("DELETE", "/api/clusters/default/one", ""), http.StatusForbidden)
	expect(t, b.do("DELETE", "/api/session", ""), http.StatusForbidden)
}

func TestListShowsOnlyVisibleClusters(t *testing.T) {
	t.Parallel()
	foreign := cluster("default", "foreign")
	foreign.Spec.Topology.ClassRef.Name = "someone-elses"
	h := newHarness(t, cluster("team-a", "three"), cluster("default", "one"), cluster("default", "two"), foreign)

	b := h.browser()
	b.signIn("viewer@pve")
	var views []clusterView
	rec := b.do("GET", "/api/clusters", "")
	expect(t, rec, http.StatusOK)
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "one" || views[0].Allowed.Change || views[0].Allowed.Kubeconfig {
		t.Fatalf("viewer sees %+v", views)
	}
	if views[0].Distribution != "rke2" || views[0].Pool != "/pool/kubehenge/default.one" || len(views[0].WorkerPools) != 1 {
		t.Fatalf("view = %+v", views[0])
	}
	expect(t, b.do("GET", "/api/clusters/default/two", ""), http.StatusNotFound)
	expect(t, b.do("GET", "/api/clusters/default/one", ""), http.StatusOK)
	expect(t, b.do("DELETE", "/api/clusters/default/one", ""), http.StatusForbidden)
	expect(t, b.do("GET", "/api/clusters/default/one/kubeconfig", ""), http.StatusForbidden)

	a := h.browser()
	a.signIn("admin@pve")
	rec = a.do("GET", "/api/clusters", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil || len(views) != 3 || views[2].Namespace != "team-a" ||
		views[2].Pool != "/pool/kubehenge/team-a.three" || !views[0].Allowed.Change || views[2].Allowed.Change || views[2].Allowed.Kubeconfig {
		t.Fatalf("admin sees %s", rec.Body)
	}
	// The interface changes only the clusters in its own namespace, whatever the user may do.
	expect(t, a.do("DELETE", "/api/clusters/team-a/three", ""), http.StatusForbidden)
	expect(t, a.do("GET", "/api/clusters/team-a/three", ""), http.StatusOK)
	expect(t, a.do("GET", "/api/clusters/default/foreign", ""), http.StatusNotFound)
}

func TestListAsksAboutEveryClusterAtOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("team-a", "three"), cluster("default", "one"), cluster("default", "two"), cluster("default", "new"))
	// Every pool but the new one has its cluster's user's ACL entry by now.
	h.accounts.acls = []string{"/pool/kubehenge/default.two", "/pool/kubehenge/team-a.three"}
	list := func(b *browser) ([]string, int) {
		before := h.accounts.privilegeCalls
		rec := b.do("GET", "/api/clusters", "")
		expect(t, rec, http.StatusOK)
		var views []clusterView
		if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(views))
		for _, v := range views {
			names = append(names, v.Name)
		}
		return names, h.accounts.privilegeCalls - before
	}
	v := h.browser()
	v.signIn("viewer@pve")
	if names, calls := list(v); !slices.Equal(names, []string{"one"}) || calls != 1 {
		t.Fatalf("viewer sees %v after %d questions, want one after one", names, calls)
	}
	// Holding privileges on / , the administrator may inherit some on the pool without an ACL entry,
	// which only a question about that pool tells.
	a := h.browser()
	a.signIn("admin@pve")
	if names, calls := list(a); !slices.Equal(names, []string{"new", "one", "two", "three"}) || calls != 2 {
		t.Fatalf("admin sees %v after %d questions, want all four after two", names, calls)
	}
}

const createBody = `{"name":"web","class":"kubehenge-rke2","version":"v1.36.5+rke2r1","controlPlaneReplicas":3,"workerReplicas":2,
"variables":{"controlPlaneEndpoint":"10.0.0.20","storage":"local-lvm","network":{"bridge":"vmbr0","addresses":["10.0.0.21-10.0.0.29"],"prefix":24,"gateway":"10.0.0.1","dnsServers":["10.0.0.1"]}}}`

func TestCreate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("POST", "/api/clusters", createBody)
	expect(t, rec, http.StatusCreated)

	c := &clusterv1.Cluster{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	top := c.Spec.Topology
	if top.ClassRef.Namespace != "kubehenge-system" || *top.ControlPlane.Replicas != 3 || len(top.Workers.MachineDeployments) != 1 ||
		top.Workers.MachineDeployments[0].Name != "md-0" || *top.Workers.MachineDeployments[0].Replicas != 2 || len(top.Variables) != 3 {
		t.Fatalf("topology = %+v", top)
	}
	if c.Annotations[createdBy] != "dev@pve" {
		t.Fatalf("annotations = %v", c.Annotations)
	}
	expect(t, b.do("POST", "/api/clusters", createBody), http.StatusConflict)

	tagged := strings.Replace(strings.Replace(createBody, `"web"`, `"tagged"`, 1), `"bridge":"vmbr0"`, `"bridge":"vmbr1","vlan":10`, 1)
	expect(t, b.do("POST", "/api/clusters", tagged), http.StatusCreated)
}

func TestCreateNeedsTheTemplate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	class := &clusterv1.ClusterClass{}
	key := client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}
	record := func(found string) {
		t.Helper()
		if err := h.kube.Get(t.Context(), key, class); err != nil {
			t.Fatal(err)
		}
		class.Annotations[provisioner.NodeOSTemplatesAnnotation] = found
		if err := h.kube.Update(t.Context(), class); err != nil {
			t.Fatal(err)
		}
	}
	b := h.browser()
	b.signIn("dev@pve")
	record("rocky-10")
	var classes []classView
	if err := json.Unmarshal(b.do("GET", "/api/classes", "").Body.Bytes(), &classes); err != nil {
		t.Fatal(err)
	}
	if len(classes) != 1 || !strings.Contains(classes[0].NoTemplate, "no RKE2 VM template on AlmaLinux 10") {
		t.Fatalf("classes = %+v", classes)
	}
	choices := map[string]string{}
	for _, o := range classes[0].NodeOSes {
		choices[o.String()] = o.NoTemplate
	}
	if len(choices) != len(templates.NodeOSes()) || choices["rocky-10"] != "" || choices["almalinux-10"] == "" {
		t.Fatalf("node OSes = %+v", classes[0].NodeOSes)
	}
	rec := b.do("POST", "/api/clusters", createBody)
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "builds it in Administration") {
		t.Fatalf("refusal = %s", rec.Body)
	}
	onRocky := strings.Replace(createBody, `"storage":`, `"nodeOS":{"family":"rocky","release":"10"},"storage":`, 1)
	expect(t, b.do("POST", "/api/clusters", onRocky), http.StatusCreated)
	for _, refused := range []string{`{"family":"leap","release":"16.1"}`, `{"family":"plan9","release":"4"}`, `"rocky-10"`} {
		body := strings.Replace(strings.Replace(createBody, `"web"`, `"other"`, 1), `"storage":`, `"nodeOS":`+refused+`,"storage":`, 1)
		expect(t, b.do("POST", "/api/clusters", body), http.StatusUnprocessableEntity)
	}
	record("almalinux-10,rocky-10")
	expect(t, b.do("POST", "/api/clusters", strings.Replace(createBody, `"web"`, `"second"`, 1)), http.StatusCreated)
}

func TestMissingKubeadmTemplate(t *testing.T) {
	t.Parallel()
	class := func(annotations map[string]string) *clusterv1.ClusterClass {
		return &clusterv1.ClusterClass{Annotations: annotations}
	}
	versions := provisioner.KubeadmVersionsAnnotation
	for _, c := range []struct {
		name, version, want string
		class               *clusterv1.ClusterClass
	}{
		{"not looked at yet", "v1.36.5", "", class(nil)},
		{"none", "", "no kubeadm VM template on AlmaLinux 10 yet", class(map[string]string{versions: "{}"})},
		{"another node OS's only", "", "no kubeadm VM template on AlmaLinux 10 yet", class(map[string]string{versions: `{"rocky-10":["v1.37.1"]}`})},
		{"the version's", "v1.36.5", "", class(map[string]string{versions: `{"almalinux-10":["v1.37.1","v1.36.5"]}`})},
		{
			"another version's", "v1.35.2", "no kubeadm v1.35.2 VM template on AlmaLinux 10",
			class(map[string]string{versions: `{"almalinux-10":["v1.37.1","v1.36.5"]}`}),
		},
		{"the class only", "", "", class(map[string]string{versions: `{"almalinux-10":["v1.37.1"]}`})},
	} {
		if got := missingTemplate(c.class, "kubeadm", c.version, templates.DefaultNodeOS); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: missingTemplate = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCreateChecksProxmoxPrivileges(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ user, body string }{
		"storage":  {"dev@pve", strings.Replace(createBody, `"local-lvm"`, `"tank"`, 1)},
		"bridge":   {"dev@pve", strings.Replace(createBody, `"vmbr0"`, `"vmbr2"`, 1)},
		"vlan":     {"dev@pve", strings.Replace(createBody, `"bridge":"vmbr0"`, `"bridge":"vmbr1","vlan":30`, 1)},
		"untagged": {"dev@pve", strings.Replace(createBody, `"bridge":"vmbr0"`, `"bridge":"vmbr1"`, 1)},
		// The grant on localnetwork does not cover a VNet of that name in another zone.
		"vnet":    {"dev@pve", strings.Replace(createBody, `"bridge":"vmbr0"`, `"bridge":"tenants"`, 1)},
		"rancher": {"dev@pve", strings.Replace(createBody, `"storage":`, `"rancher":{"install":true,"hostname":"rancher.example"},"storage":`, 1)},
		"csi":     {"dev@pve", strings.Replace(createBody, `"storage":`, `"csi":{"datastoreAllocate":true},"storage":`, 1)},
		"viewer":  {"viewer@pve", createBody},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			b := h.browser()
			b.signIn(c.user)
			expect(t, b.do("POST", "/api/clusters", c.body), http.StatusForbidden)
			var list clusterv1.ClusterList
			if err := h.kube.List(t.Context(), &list); err != nil || len(list.Items) != 0 {
				t.Fatalf("clusters created: %d, %v", len(list.Items), err)
			}
		})
	}
}

// The interface passes the ranges asked for as they are and leaves the others to the controller's
// admission, which chooses and checks them for kubectl's clusters alike (provisioner's
// ClusterDefaulter, and the integration tier through a real API server).
func TestCreateLeavesRangesToTheController(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("admin@pve")
	ranges := func(name string) ([]string, []string) {
		t.Helper()
		c := &clusterv1.Cluster{}
		if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, c); err != nil {
			t.Fatal(err)
		}
		return c.Spec.ClusterNetwork.Pods.CIDRBlocks, c.Spec.ClusterNetwork.Services.CIDRBlocks
	}
	named := func(name, extra string) string {
		body := strings.Replace(createBody, `"web"`, `"`+name+`"`, 1)
		return strings.Replace(body, `"workerReplicas":2,`, `"workerReplicas":2,`+extra, 1)
	}
	expect(t, b.do("POST", "/api/clusters", named("plain", "")), http.StatusCreated)
	if pods, services := ranges("plain"); pods != nil || services != nil {
		t.Errorf("ranges the controller chooses: %v %v", pods, services)
	}
	expect(t, b.do("POST", "/api/clusters", named("asked", `"pods":["172.30.0.0/16"],"services":["172.31.0.0/16"],`)), http.StatusCreated)
	if pods, services := ranges("asked"); !slices.Equal(pods, []string{"172.30.0.0/16"}) || !slices.Equal(services, []string{"172.31.0.0/16"}) {
		t.Errorf("the ranges asked for: %v %v", pods, services)
	}
}

func TestOIDCNeedsTheKubeconfigRight(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"))
	withOIDC := func(name, oidc string) string {
		return strings.Replace(strings.Replace(createBody, `"web"`, `"`+name+`"`, 1), `"storage":`, `"oidc":`+oidc+`,"storage":`, 1)
	}
	own := `[{"issuerURL":"https://mine.example","clientID":"k8s","groupsPrefix":""}]`
	dev := h.browser()
	dev.signIn("dev@pve")
	// The class's providers, some of them or none let nobody new in.
	expect(t, dev.do("POST", "/api/clusters", withOIDC("a", `[{"clientID":"lab","issuerURL":"https://lab.example"}]`)), http.StatusCreated)
	expect(t, dev.do("POST", "/api/clusters", withOIDC("b", `[]`)), http.StatusCreated)
	rec := dev.do("POST", "/api/clusters", withOIDC("c", own))
	expect(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "Sys.Modify on /pool/kubehenge") {
		t.Fatalf("refusal = %s", rec.Body)
	}
	admin := h.browser()
	admin.signIn("admin@pve")
	expect(t, admin.do("POST", "/api/clusters", withOIDC("d", own)), http.StatusCreated)
	for name, oidc := range map[string]string{
		"http":   `[{"issuerURL":"http://mine.example","clientID":"k8s"}]`,
		"system": `[{"issuerURL":"https://mine.example","clientID":"k8s","usernamePrefix":"system:"}]`,
		"field":  `[{"issuerURL":"https://mine.example","clientID":"k8s","audience":"x"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fresh := newHarness(t).browser()
			fresh.signIn("admin@pve")
			expect(t, fresh.do("POST", "/api/clusters", withOIDC("e", oidc)), http.StatusUnprocessableEntity)
		})
	}

	// dev holds Sys.Modify on one's pool, not on two's.
	change := `{"variables":{"oidc":` + own + `}}`
	expect(t, dev.do("PATCH", "/api/clusters/default/one", change), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", change), http.StatusForbidden)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"oidc":[]}}`), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"oidc":null}}`), http.StatusOK)

	// Dropping one of a cluster's own providers lets nobody new in, so it needs no Sys.Modify;
	// bringing it back once dropped does.
	both := `[{"issuerURL":"https://mine.example","clientID":"k8s","groupsPrefix":""},{"clientID":"lab","issuerURL":"https://lab.example"}]`
	expect(t, admin.do("PATCH", "/api/clusters/default/two", `{"variables":{"oidc":`+both+`}}`), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"oidc":`+own+`}}`), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"oidc":[]}}`), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"oidc":`+own+`}}`), http.StatusForbidden)
}

func TestCreateValidates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("admin@pve")
	for name, body := range map[string]string{
		"name":          strings.Replace(createBody, `"web"`, `"Web_1"`, 1),
		"long name":     strings.Replace(createBody, `"web"`, `"`+strings.Repeat("a", 41)+`"`, 1),
		"class":         strings.Replace(createBody, `"kubehenge-rke2"`, `"kubehenge-k3s"`, 1),
		"foreign class": strings.Replace(createBody, `"kubehenge-rke2"`, `"other"`, 1),
		"version":       strings.Replace(createBody, `"v1.36.5+rke2r1"`, `"v1.36.5+k3s1"`, 1),
		"even":          strings.Replace(createBody, `"controlPlaneReplicas":3`, `"controlPlaneReplicas":2`, 1),
		"workers":       strings.Replace(createBody, `"workerReplicas":2`, `"workerReplicas":-1`, 1),
		"variable":      strings.Replace(createBody, `"storage":`, `"cni":"cilium","storage":`, 1),
		"namespace":     strings.Replace(createBody, `"name"`, `"namespace":"kube-system","name"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fresh := newHarness(t).browser()
			fresh.signIn("admin@pve")
			expect(t, fresh.do("POST", "/api/clusters", body), http.StatusUnprocessableEntity)
		})
	}
	expect(t, b.do("POST", "/api/clusters", strings.Replace(createBody, `"name"`, `"labels":{"team":"a"},"name"`, 1)), http.StatusBadRequest)
}

func TestDecodeProblemsNameNoGoTypes(t *testing.T) {
	t.Parallel()
	var req createRequest
	for body, want := range map[string]string{
		`{"workerReplicas":1.5}`: "workerReplicas: wrong type of value (number 1.5)",
		`{"name":`:               "the body is not valid JSON",
		`[]`:                     "the body: wrong type of value (array)",
		`{"labels":"x"}`:         `unknown field "labels"`,
	} {
		dec := json.NewDecoder(strings.NewReader(body))
		dec.DisallowUnknownFields()
		if got := decodeProblem(dec.Decode(&req)); got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}

func TestCreateKeepsToWhatClustersMayUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	class := &clusterv1.ClusterClass{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	for i, v := range class.Spec.Variables {
		switch v.Name {
		case "storage":
			class.Spec.Variables[i].Schema.OpenAPIV3Schema.Enum = []apiextensionsv1.JSON{{Raw: []byte(`"local-lvm"`)}}
		case "network":
			class.Spec.Variables[i].Schema.OpenAPIV3Schema.Properties = map[string]clusterv1.JSONSchemaProps{
				"bridge": {Type: "string", Enum: []apiextensionsv1.JSON{{Raw: []byte(`"vmbr0"`)}}},
			}
		}
	}
	if err := h.kube.Update(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("admin@pve")
	rec := b.do("POST", "/api/clusters", strings.Replace(createBody, `"local-lvm"`, `"tank"`, 1))
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "storage: choose local-lvm") {
		t.Fatalf("refusal = %s", rec.Body)
	}
	expect(t, b.do("POST", "/api/clusters", strings.Replace(createBody, `"vmbr0"`, `"vmbr1"`, 1)), http.StatusUnprocessableEntity)
	expect(t, b.do("POST", "/api/clusters", createBody), http.StatusCreated)
}

func TestHARulesOfferPlaces(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	admin := h.browser()
	admin.signIn("admin@pve")
	rec := admin.do("GET", "/api/proxmox/ha-rules", "")
	expect(t, rec, http.StatusOK)
	var view haRulesView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Readable || len(view.Rules) != 2 || view.Rules[0].Rule != "fast" || !slices.Equal(view.Rules[0].Allowed, []string{"pve2"}) ||
		view.Rules[1].Rule != "not-pve2" || !slices.Equal(view.Rules[1].Allowed, []string{"pve1"}) {
		t.Fatalf("rules = %+v", view)
	}

	dev := h.browser()
	dev.signIn("dev@pve")
	rec = dev.do("GET", "/api/proxmox/ha-rules", "")
	expect(t, rec, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"readable":false,"rules":[]}` {
		t.Fatalf("without Sys.Audit on / = %s", got)
	}
}

func offeredRules(data string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{Data: map[string]string{provisioner.HARulesKey: data}}
	cm.Namespace, cm.Name = "kubehenge-system", provisioner.HARulesName
	return cm
}

func TestHARulesOfferedToThoseWhoMayPlaceAClusterWithoutSysAudit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, offeredRules(`[{"rule":"fast","nodes":["pve2"]},{"rule":"gone","nodes":["pve9"]}]`))
	h.accounts.passwords["owner@pve"] = "secret"
	h.accounts.grants["owner@pve"] = map[string][]string{"/pool/kubehenge/default.one": {"VM.Audit", "VM.Allocate"}}
	h.accounts.passwords["auditor@pve"] = "secret"
	h.accounts.grants["auditor@pve"] = map[string][]string{"/": {"VM.Audit"}}
	rules := func(user string) string {
		t.Helper()
		b := h.browser()
		b.signIn(user)
		rec := b.do("GET", "/api/proxmox/ha-rules", "")
		expect(t, rec, http.StatusOK)
		return strings.TrimSpace(rec.Body.String())
	}

	// Only the name and the nodes come through, and only nodes the user sees.
	offered := `{"readable":true,"offered":true,"rules":[{"rule":"fast","nodes":["pve2"],"allowed":["pve2"]}]}`
	for _, user := range []string{"dev@pve", "owner@pve"} {
		if got := rules(user); got != offered {
			t.Errorf("%s, who may place a cluster, got %s", user, got)
		}
	}
	for _, user := range []string{"viewer@pve", "auditor@pve"} {
		if got := rules(user); got != `{"readable":false,"rules":[]}` {
			t.Errorf("%s, who may place no cluster, got %s", user, got)
		}
	}
	// Whoever may read the rules reads them in full, from Proxmox VE.
	var view haRulesView
	if err := json.Unmarshal([]byte(rules("admin@pve")), &view); err != nil {
		t.Fatal(err)
	}
	if view.Offered || len(view.Rules) != 2 || !view.Rules[0].Strict || !view.Rules[1].Negative {
		t.Fatalf("with Sys.Audit on / = %+v", view)
	}
}

func TestHARulesNotOfferedWhenUnreadable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, offeredRules(`not json`))
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/proxmox/ha-rules", "")
	expect(t, rec, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"readable":false,"rules":[]}` {
		t.Fatalf("a record that cannot be read = %s", got)
	}
}

func TestCreateKeepsToTheNodesAsked(t *testing.T) {
	t.Parallel()
	placed := func(name, nodes string) string {
		return strings.Replace(strings.Replace(createBody, `"web"`, `"`+name+`"`, 1), `"storage":`, `"allowedNodes":`+nodes+`,"storage":`, 1)
	}
	placing := func(t *testing.T) *harness {
		t.Helper()
		h := newHarness(t)
		class := &clusterv1.ClusterClass{}
		if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
			t.Fatal(err)
		}
		class.Spec.Variables = append(class.Spec.Variables, clusterv1.ClusterClassVariable{
			Name: "allowedNodes", Schema: clusterv1.VariableSchema{OpenAPIV3Schema: clusterv1.JSONSchemaProps{Type: "array"}},
		})
		if err := h.kube.Update(t.Context(), class); err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := placing(t)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("POST", "/api/clusters", placed("two", `["pve2","pve1"]`)), http.StatusCreated)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "two"}, c); err != nil {
		t.Fatal(err)
	}
	if got := string(variableValue(c, "allowedNodes")); got != `["pve2","pve1"]` {
		t.Fatalf("allowedNodes = %s", got)
	}
	for name, nodes := range map[string]string{
		"unseen": `["pve9"]`,
		"twice":  `["pve1","pve1"]`,
		"none":   `[]`,
		"null":   `null`,
		"string": `"pve1"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fresh := placing(t).browser()
			fresh.signIn("dev@pve")
			expect(t, fresh.do("POST", "/api/clusters", placed("refused", nodes)), http.StatusUnprocessableEntity)
		})
	}
}

func TestChangeMovesWhereNewVMsGo(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	setVariable(c, "allowedNodes", json.RawMessage(`["pve9"]`))
	h := newHarness(t, c)
	b := h.browser()
	b.signIn("dev@pve")
	change := func(nodes string) *httptest.ResponseRecorder {
		return b.do("PATCH", "/api/clusters/default/one", `{"variables":{"allowedNodes":`+nodes+`}}`)
	}
	// A node the cluster has stays, whether the user sees it or not.
	expect(t, change(`["pve9","pve1"]`), http.StatusOK)
	got := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if v := string(variableValue(got, "allowedNodes")); v != `["pve9","pve1"]` {
		t.Fatalf("allowedNodes = %s", v)
	}
	for _, refused := range []string{`["pve8"]`, `["pve1","pve1"]`, `[]`, `"pve1"`} {
		expect(t, change(refused), http.StatusUnprocessableEntity)
	}
	expect(t, change(`null`), http.StatusOK)
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if v := variableValue(got, "allowedNodes"); v != nil {
		t.Fatalf("allowedNodes after null = %s", v)
	}
	expect(t, change(`["pve9"]`), http.StatusUnprocessableEntity)
}

func TestChangeOptsOutOfProxmoxHA(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	h := newHarness(t, c)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"proxmoxHA":false}}`), http.StatusOK)
	got := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if v := string(variableValue(got, "proxmoxHA")); v != "false" {
		t.Fatalf("proxmoxHA = %s", v)
	}
}

// Datastore.Allocate on a cluster's storage takes the user's own, there.
func TestDatastoreAllocateNeedsTheUsersOwn(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	h := newHarness(t, c)
	b := h.browser()
	b.signIn("dev@pve")
	switchOn := func() *httptest.ResponseRecorder {
		return b.do("PATCH", "/api/clusters/default/one", `{"variables":{"csi":{"datastoreAllocate":true}}}`)
	}
	expect(t, switchOn(), http.StatusForbidden)
	h.accounts.mu.Lock()
	h.accounts.grants["dev@pve"]["/storage/local-lvm"] = []string{"Datastore.AllocateSpace", "Datastore.Allocate"}
	h.accounts.mu.Unlock()
	expect(t, switchOn(), http.StatusOK)
	got := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if v := string(variableValue(got, "csi")); v != `{"datastoreAllocate":true}` {
		t.Fatalf("csi = %s", v)
	}

	rec := b.do("GET", "/api/proxmox/storages", "")
	var storages []storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &storages); err != nil || !storages[0].Allocatable || storages[1].Allocatable || storages[2].Allocatable {
		t.Fatalf("storages = %s", rec.Body)
	}
	created := strings.Replace(createBody, `"storage":`, `"csi":{"datastoreAllocate":true},"storage":`, 1)
	expect(t, b.do("POST", "/api/clusters", created), http.StatusCreated)
	noStorage := strings.Replace(created, `"storage":"local-lvm",`, ``, 1)
	expect(t, b.do("POST", "/api/clusters", strings.Replace(noStorage, `"web"`, `"bare"`, 1)), http.StatusUnprocessableEntity)
}

// Sys.Audit on / for a cluster's token reads only the Proxmox VE cluster's configuration, so the HA
// group labels take no right of the user's own: Kubehenge's administrator decides on them.
func TestHAGroupLabelsTakeNoRightOfTheUsersOwn(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	h := newHarness(t, c)
	b := h.browser()
	b.signIn("dev@pve")
	for _, on := range []bool{false, true} {
		expect(t, b.do("PATCH", "/api/clusters/default/one", fmt.Sprintf(`{"variables":{"ccm":{"haGroups":%t}}}`, on)), http.StatusOK)
		got := &clusterv1.Cluster{}
		if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
			t.Fatal(err)
		}
		if v, want := string(variableValue(got, "ccm")), fmt.Sprintf(`{"haGroups":%t}`, on); v != want {
			t.Fatalf("ccm = %s, want %s", v, want)
		}
	}
	created := strings.Replace(createBody, `"storage":`, `"ccm":{"haGroups":false},"storage":`, 1)
	expect(t, b.do("POST", "/api/clusters", created), http.StatusCreated)
}

// Volume snapshots copy only the cluster's own volumes onto its own storage, so switching them on
// takes no right of the user's beyond changing the cluster.
func TestVolumeSnapshotsTakeNoRightOfTheUsersOwn(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	h := newHarness(t, c)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"csi":{"volumeSnapshots":true}}}`), http.StatusOK)
	got := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if v := string(variableValue(got, "csi")); v != `{"volumeSnapshots":true}` {
		t.Fatalf("csi = %s", v)
	}
	if session := b.do("GET", "/api/session", "").Body.String(); strings.Contains(session, "Permissions") {
		t.Errorf("session = %s", session)
	}
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"csi":{"volumeSnapshots":false}}}`), http.StatusOK)
}

func TestClusterShowsWhereItsVMsRun(t *testing.T) {
	t.Parallel()
	machine := func(name, infra string, controlPlane bool) *clusterv1.Machine {
		m := &clusterv1.Machine{}
		m.Namespace, m.Name = "default", name
		m.Labels = map[string]string{clusterv1.ClusterNameLabel: "one"}
		if controlPlane {
			m.Labels[clusterv1.MachineControlPlaneLabel] = ""
		}
		m.Spec.ClusterName = "one"
		m.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{Kind: "ProxmoxMachine", APIGroup: "infrastructure.cluster.x-k8s.io", Name: infra}
		return m
	}
	cp, worker := machine("one-cp-a", "one-cp-a-x1", true), machine("one-md-a", "one-md-a-y2", false)
	cp.Status.Conditions = []metav1.Condition{{Type: clusterv1.MachineReadyCondition, Status: metav1.ConditionTrue, Message: "left over"}}
	waiting := "* InfrastructureReady:\n  * VirtualMachineProvisioned: VM template not found"
	worker.Status.Conditions = []metav1.Condition{{Type: clusterv1.MachineReadyCondition, Status: metav1.ConditionFalse, Message: waiting}}
	h := newHarness(t, cluster("default", "one"), cp, worker)
	// The provider names a VM after its infrastructure machine, not after the Machine.
	h.accounts.members = []pve.Member{{VMID: 201, Node: "pve2", Type: "qemu", Name: "one-cp-a-x1"}, {VMID: 202, Node: "pve1", Type: "qemu", Name: "one-cp-a"}}
	h.server.ProxmoxURL = "https://pve.example.org:8006"
	view := func(username string) clusterView {
		b := h.browser()
		b.signIn(username)
		rec := b.do("GET", "/api/clusters/default/one", "")
		expect(t, rec, http.StatusOK)
		var v clusterView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	admin := view("admin@pve")
	if len(admin.Machines) != 2 || admin.Machines[0].ProxmoxNode != "pve2" || admin.Machines[1].ProxmoxNode != "" {
		t.Fatalf("machines = %+v", admin.Machines)
	}
	// The console opens in Proxmox VE's own web interface, for a VM it named.
	if c := admin.Machines[0].Console; c != "https://pve.example.org:8006/?cmd=&console=kvm&node=pve2&novnc=1&resize=off&vmid=201&vmname=one-cp-a-x1" || admin.Machines[1].Console != "" {
		t.Fatalf("consoles = %q, %q", c, admin.Machines[1].Console)
	}
	// Without VM.Console there is no console to offer.
	if viewer := view("viewer@pve"); len(viewer.Machines) != 2 || viewer.Machines[0].Console != "" {
		t.Fatalf("viewer's machines = %+v", viewer.Machines)
	}
	machines := admin.Machines
	// What keeps a machine waiting is shown, and only while it waits.
	if !machines[0].Ready || machines[0].Waiting != "" || machines[1].Waiting != waiting {
		t.Fatalf("waiting = %q, %q", machines[0].Waiting, machines[1].Waiting)
	}
}

func TestChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"version":"v1.37.1+rke2r1","workerPools":{"md-0":4},"variables":{"headlamp":{"enabled":true}}}`), http.StatusOK)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	if c.Spec.Topology.Version != "v1.37.1+rke2r1" || *c.Spec.Topology.Workers.MachineDeployments[0].Replicas != 4 || len(c.Spec.Topology.Variables) != 2 {
		t.Fatalf("topology = %+v", c.Spec.Topology)
	}
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"headlamp":null}}`), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"version":"v1.39.0+rke2r1"}`), http.StatusUnprocessableEntity)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"version":"v1.36.5+rke2r1"}`), http.StatusUnprocessableEntity)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"version":"v1.37.1+rke2r2"}`), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"storage":"tank"}}`), http.StatusUnprocessableEntity)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"workerPools":{"md-9":1}}`), http.StatusUnprocessableEntity)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"controlPlaneReplicas":4}`), http.StatusUnprocessableEntity)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"rancher":{"install":true,"hostname":"r.example"}}}`), http.StatusForbidden)
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil || len(c.Spec.Topology.Variables) != 1 {
		t.Fatalf("variables = %+v, %v", c.Spec.Topology.Variables, err)
	}
}

// A change made against a cluster someone has changed since is refused, its plan too, rather than
// undoing the other change unseen; a change status alone made since goes through.
func TestChangeAgainstAnOlderGeneration(t *testing.T) {
	t.Parallel()
	one := cluster("default", "one")
	one.Generation = 3
	h := newHarness(t, one)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one", "")
	if !strings.Contains(rec.Body.String(), `"generation":3`) {
		t.Fatalf("view = %s", rec.Body)
	}

	read := func() *clusterv1.Cluster {
		t.Helper()
		c := &clusterv1.Cluster{}
		if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	// The fake client leaves the generation to its writers; the API server counts it.
	c := read()
	c.Generation = 4
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "headlamp", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)}})
	if err := h.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	b.generation = "3"
	for _, req := range [][2]string{{"POST", "/api/clusters/default/one/plan"}, {"PATCH", "/api/clusters/default/one"}} {
		rec = b.do(req[0], req[1], `{"variables":{"headlamp":{"enabled":false}}}`)
		expect(t, rec, http.StatusPreconditionFailed)
		if !strings.Contains(rec.Body.String(), "the cluster changed meanwhile") {
			t.Fatalf("%s: %s", req[0], rec.Body)
		}
	}
	if raw := variableValue(read(), "headlamp"); string(raw) != `{"enabled":true}` {
		t.Fatalf("the other change was undone: headlamp = %s", raw)
	}
	b.generation = "three"
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"headlamp":{"enabled":false}}}`), http.StatusBadRequest)

	// Metadata and status leave the generation as it is.
	c = read()
	c.Annotations = map[string]string{"example.org/touched": "yes"}
	if err := h.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	b.generation = "4"
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"headlamp":{"enabled":false}}}`), http.StatusOK)
}

func TestSessionTellsWhoMayCreate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for user, want := range map[string]string{
		"admin@pve":  `"canCreate":true,"canTrust":true`,
		"dev@pve":    `"canCreate":true,"canTrust":false`,
		"viewer@pve": `"canCreate":false,"canTrust":false`,
	} {
		b := h.browser()
		b.signIn(user)
		if got := b.do("GET", "/api/session", "").Body.String(); !strings.Contains(got, want) {
			t.Errorf("%s: session = %s", user, got)
		}
	}
}

func TestOnlyOneClusterRunsRancher(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"))
	b := h.browser()
	b.signIn("admin@pve")
	on := `{"variables":{"rancher":{"install":true,"hostname":"r.example"}}}`
	expect(t, b.do("PATCH", "/api/clusters/default/one", on), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/default/one", on), http.StatusOK)
	rec := b.do("PATCH", "/api/clusters/default/two", on)
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "Rancher: cluster default/one already runs it") {
		t.Fatalf("refusal = %s", rec.Body)
	}
	rec = b.do("POST", "/api/clusters", strings.Replace(createBody, `"storage":`, `"rancher":{"install":true,"hostname":"rancher.example"},"storage":`, 1))
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "default/one already runs it") {
		t.Fatalf("refusal = %s", rec.Body)
	}

	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"rancher":null}}`), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/default/two", on), http.StatusOK)
}

func TestDeleteAndKubeconfig(t *testing.T) {
	t.Parallel()
	secret := &corev1.Secret{
		Namespace: "default", Name: "one-kubeconfig",
		Data: map[string][]byte{"value": []byte("apiVersion: v1\nkind: Config\n")},
	}
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"), secret)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/kubeconfig", "")
	expect(t, rec, http.StatusOK)
	if rec.Body.String() != "apiVersion: v1\nkind: Config\n" || !strings.Contains(rec.Header().Get("Content-Disposition"), `filename="one.kubeconfig"`) {
		t.Fatalf("kubeconfig: %v %s", rec.Header(), rec.Body)
	}
	expect(t, b.do("GET", "/api/clusters/default/two/kubeconfig", ""), http.StatusForbidden)
	// A deletion names the cluster it is for, as the dialog has its user type it.
	expect(t, b.do("DELETE", "/api/clusters/default/one", ""), http.StatusUnsupportedMediaType)
	expect(t, b.do("DELETE", "/api/clusters/default/one", `{}`), http.StatusUnprocessableEntity)
	expect(t, b.do("DELETE", "/api/clusters/default/one", `{"confirm":"two"}`), http.StatusUnprocessableEntity)
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "one"}, &clusterv1.Cluster{}); err != nil {
		t.Fatalf("an unconfirmed deletion: %v", err)
	}
	expect(t, b.do("DELETE", "/api/clusters/default/one", `{"confirm":"one"}`), http.StatusAccepted)
	err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "one"}, &clusterv1.Cluster{})
	if err == nil {
		t.Fatal("the cluster is still there")
	}
}

func TestProxmoxResources(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/proxmox/storages", "")
	expect(t, rec, http.StatusOK)
	var storages []storageView
	// Every storage comes with its content, for the setup's image and ISO storages; only one that
	// takes VM disks is usable for a cluster's.
	if err := json.Unmarshal(rec.Body.Bytes(), &storages); err != nil || len(storages) != 3 || !storages[0].Usable || storages[1].Usable ||
		storages[2].Storage.Storage != "local" || storages[2].Usable {
		t.Fatalf("storages = %s", rec.Body)
	}
	rec = b.do("GET", "/api/proxmox/nodes/pve1/bridges", "")
	var bridges []bridgeView
	if err := json.Unmarshal(rec.Body.Bytes(), &bridges); err != nil || len(bridges) != 2 || !bridges[0].Usable || bridges[1].Usable {
		t.Fatalf("bridges = %s", rec.Body)
	}
	rec = b.do("GET", "/api/proxmox/nodes/pve1/dns", "")
	expect(t, rec, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"servers":["192.0.2.53"]}` {
		t.Fatalf("dns = %s", got)
	}
}

func TestClasses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	b.signIn("viewer@pve")
	rec := b.do("GET", "/api/classes", "")
	expect(t, rec, http.StatusOK)
	var classes []classView
	if err := json.Unmarshal(rec.Body.Bytes(), &classes); err != nil || len(classes) != 1 {
		t.Fatalf("classes = %s", rec.Body)
	}
	if c := classes[0]; c.Distribution != "rke2" || len(c.Variables) != 11 || !c.Variables[0].Required || c.Variables[0].Schema.Format != "ipv4" ||
		c.Annotations["kubehenge.dev/default-pods"] != "10.42.0.0/16" || len(c.NodeOSes) != len(templates.NodeOSes()) {
		t.Fatalf("class = %s", rec.Body)
	}
	// Leap waits for its release; what the interface may offer, the page is told.
	for _, o := range classes[0].NodeOSes {
		if (o.Unreleased != "") != (o.Family == "leap") || o.Default != (o.NodeOS == templates.DefaultNodeOS) || o.MAC != templates.SELinux {
			t.Errorf("node OS %+v", o)
		}
	}
}

func TestAssets(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	b := h.browser()
	for path, want := range map[string]string{"/": "<!doctype html>", "/clusters/default/one": "<!doctype html>", "/assets/app-1.js": "console.log"} {
		rec := b.do("GET", path, "")
		expect(t, rec, http.StatusOK)
		if !strings.HasPrefix(rec.Body.String(), want) {
			t.Fatalf("%s: %s", path, rec.Body)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") {
			t.Fatalf("%s: CSP %q", path, csp)
		}
		if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
			t.Fatalf("%s: Strict-Transport-Security %q", path, got)
		}
	}
	if cc := b.do("GET", "/assets/app-1.js", "").Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset Cache-Control %q", cc)
	}
	expect(t, b.do("GET", "/assets/missing.js", ""), http.StatusNotFound)
	expect(t, b.do("GET", "/api/nothing", ""), http.StatusNotFound)
}

func TestAssetsPrecompressedAndValidated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.Assets = fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html>")},
		"assets/app-1.js":    {Data: []byte("console.log(1)")},
		"assets/app-1.js.br": {Data: []byte("brotli")},
		"assets/app-1.js.gz": {Data: []byte("gzip")},
	}
	h.handler = h.server.Handler()
	get := func(path string, header ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), "GET", path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}
	for accept, want := range map[string]string{
		"gzip, deflate, br": "brotli",
		"gzip, br;q=0":      "gzip",
		"br;q=0, gzip;q=0":  "console.log(1)",
		"":                  "console.log(1)",
	} {
		rec := get("/assets/app-1.js", "Accept-Encoding", accept)
		coding := map[string]string{"brotli": "br", "gzip": "gzip"}[want]
		if rec.Code != http.StatusOK || rec.Body.String() != want || rec.Header().Get("Content-Encoding") != coding ||
			!strings.Contains(rec.Header().Get("Content-Type"), "javascript") || rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: %d %q, headers %v", accept, rec.Code, rec.Body, rec.Header())
		}
	}
	// The page itself is checked on every load, and answered without a body while unchanged.
	first := get("/")
	tag := first.Header().Get("ETag")
	if tag == "" || first.Header().Get("Content-Length") != "15" {
		t.Fatalf("index.html headers %v", first.Header())
	}
	if again := get("/clusters", "If-None-Match", tag); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Fatalf("unchanged page: %d %q", again.Code, again.Body)
	}
}
