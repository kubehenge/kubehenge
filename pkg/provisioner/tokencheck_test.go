package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// pveTokens is a Proxmox VE API that answers /access/permissions for the tokens it knows, each with
// the privileges it holds by path, and refuses any other token.
type pveTokens struct {
	mu     sync.Mutex
	grants map[string]map[string][]string
}

func (p *pveTokens) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "PVEAPIToken=")
	p.mu.Lock()
	grants, known := p.grants[token]
	p.mu.Unlock()
	if !known {
		http.Error(w, "authentication failure", http.StatusUnauthorized)
		return
	}
	path := r.URL.Query().Get("path")
	// Each privilege maps to whether it propagates. 0, as for a grant that does not propagate, is
	// held all the same.
	held := map[string]int{}
	for _, priv := range grants[path] {
		held[priv] = 0
	}
	answer := struct {
		Data map[string]map[string]int `json:"data"`
	}{map[string]map[string]int{path: held}}
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *pveTokens) set(token string, grants map[string][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grants[token] = grants
}

func objectMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}

func grantsFor(needs []Need) map[string][]string {
	g := map[string][]string{}
	for _, n := range needs {
		g[n.Path] = append(g[n.Path], n.Privilege)
	}
	return g
}

// jobRunner runs a request as the token Job does: with the Job's own token and serve function.
type jobRunner struct {
	c     *proxmox.Client
	serve func(context.Context, *proxmox.Client, []byte) (string, error)
}

func (j jobRunner) Run(ctx context.Context, request any, _ time.Duration) (string, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	return j.serve(ctx, j.c, raw)
}

const (
	testPool     = "kubehenge"
	controllerTk = "controller@pve!controller=c"
	haTk         = "ha@pve!ha=h"
	restoreTk    = "restore@pve!etcd-restore=r"
	capmoxTk     = "capmox@pve!capmox=m"
	builderTk    = "builder@pve!refresh=b"
	clusterTk    = "cluster@pve!web=w"
)

type tokenFixture struct {
	t      *testing.T
	pve    *pveTokens
	srv    *httptest.Server
	kube   client.Client
	probes *TokenProbes
	reach  *ProxmoxReach
}

// testPlaces are the places of an install with one clusters' storage, a template builder on pve1
// and the other defaults.
func testPlaces() Places {
	return Places{
		WorkloadPool: testPool, TemplatePool: "kubehenge-templates", ClusterGroup: "kubehenge-clusters",
		Storages: []string{"local-lvm"}, BootLogs: true,
		Builder: BuilderPlaces{Node: "pve1", Storage: "local-lvm", Import: "local", ISO: "local"},
	}
}

func clientFor(t *testing.T, srv *httptest.Server, token string) *proxmox.Client {
	t.Helper()
	id, secret, _ := strings.Cut(token, "=")
	return proxmox.NewClient(srv.URL+"/api2/json", proxmox.WithAPIToken(id, secret))
}

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	pve := &pveTokens{grants: map[string]map[string][]string{}}
	srv := httptest.NewServer(pve)
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	names, err := NamesFor(testPool, "team-a", "web")
	if err != nil {
		t.Fatal(err)
	}
	web := cluster()
	id, value, _ := strings.Cut(clusterTk, "=")
	creds := credentialsSecret(web, Credentials{TokenID: id, TokenSecret: value}, true)
	provider := &corev1.Secret{ObjectMeta: objectMeta("proxmox-infrastructure-system", "capmox-variables"), Data: map[string][]byte{"PROXMOX_TOKEN": []byte("capmox@pve!capmox")}}
	provider.Data["PROXMOX_"+"SECRET"] = []byte("m")
	builder := &corev1.Secret{ObjectMeta: objectMeta("kubehenge-system", "kubehenge-template-builder"), Data: map[string][]byte{"token": []byte(builderTk)}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(creds, provider, builder, web).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	places := testPlaces()
	pve.set(controllerTk, grantsFor(ControllerNeeds(&places)))
	pve.set(haTk, grantsFor(HANeeds()))
	pve.set(restoreTk, grantsFor(RestoreNeeds(&places)))
	pve.set(capmoxTk, grantsFor(CapmoxNeeds(&places)))
	pve.set(builderTk, grantsFor(TemplateBuilderNeeds(&places)))
	pve.set(clusterTk, grantsFor(ClusterNeeds(names.Pool, &places)))
	probes := &TokenProbes{
		Reader: kube, Client: kube, ClassNamespace: "kubehenge-system", Places: places,
		Controller: clientFor(t, srv, controllerTk),
		HA:         jobRunner{clientFor(t, srv, haTk), ServeHARequest},
		Restore:    jobRunner{clientFor(t, srv, restoreTk), ServeGuestRequest},
		Secrets: []SecretProbe{
			{Name: "Cluster API provider", Namespace: "proxmox-infrastructure-system", From: "capmox-variables", Needs: CapmoxNeeds(&places)},
			{Name: "Template builder", Namespace: "kubehenge-system", From: "kubehenge-template-builder", Needs: TemplateBuilderNeeds(&places)},
		},
		Connect: func(token string) (*proxmox.Client, error) { return clientFor(t, srv, token), nil },
	}
	f := &tokenFixture{t: t, pve: pve, srv: srv, kube: kube, probes: probes}
	f.reach = &ProxmoxReach{Reader: kube, Client: kube, Namespace: "kubehenge-system", Proxmox: probes.Controller, Interval: time.Minute, Tokens: probes}
	return f
}

func (f *tokenFixture) states() map[string]TokenState {
	f.t.Helper()
	out := map[string]TokenState{}
	for _, s := range CheckTokens(f.t.Context(), f.probes.Probes(f.t.Context())) {
		out[s.Name] = s
	}
	return out
}

func TestEveryTokenKubehengeRelaysOnIsTested(t *testing.T) {
	t.Parallel()
	f := newTokenFixture(t)
	got := f.states()
	for _, name := range []string{"Controller", "Proxmox VE HA", "etcd restore", "Cluster API provider", "Template builder", "Cluster team-a/web"} {
		if s, ok := got[name]; !ok || !s.OK {
			t.Errorf("%s: %+v", name, s)
		}
	}
	if len(got) != 6 {
		t.Errorf("tested %d tokens: %v", len(got), got)
	}
}

func TestATokenWithoutAPrivilegeNamesItAndItsPath(t *testing.T) {
	t.Parallel()
	web, err := NamesFor(testPool, "team-a", "web")
	if err != nil {
		t.Fatal(err)
	}
	places := testPlaces()
	for name, tc := range map[string]struct {
		token, grantee string
		needs          []Need
		drop           Need
	}{
		"controller":                    {controllerTk, "Controller", ControllerNeeds(&places), Need{"/access/realm/pve", "Realm.AllocateUser"}},
		"controller on the group":       {controllerTk, "Controller", ControllerNeeds(&places), Need{"/access/groups/kubehenge-clusters", "User.Modify"}},
		"controller on a storage":       {controllerTk, "Controller", ControllerNeeds(&places), Need{"/storage/local-lvm", "Datastore.AllocateSpace"}},
		"controller on the templates":   {controllerTk, "Controller", ControllerNeeds(&places), Need{"/pool/kubehenge-templates", "VM.Audit"}},
		"HA":                            {haTk, "Proxmox VE HA", HANeeds(), Need{"/", "Sys.Console"}},
		"restore":                       {restoreTk, "etcd restore", RestoreNeeds(&places), Need{"/pool/" + testPool, "VM.GuestAgent.Unrestricted"}},
		"capmox":                        {capmoxTk, "Cluster API provider", CapmoxNeeds(&places), Need{"/pool/" + testPool, "VM.PowerMgmt"}},
		"template builder":              {builderTk, "Template builder", TemplateBuilderNeeds(&places), Need{"/", "Sys.Audit"}},
		"template builder on its pool":  {builderTk, "Template builder", TemplateBuilderNeeds(&places), Need{"/pool/kubehenge-templates", "VM.PowerMgmt"}},
		"template builder on its node":  {builderTk, "Template builder", TemplateBuilderNeeds(&places), Need{"/nodes/pve1", "Sys.AccessNetwork"}},
		"template builder on a storage": {builderTk, "Template builder", TemplateBuilderNeeds(&places), Need{"/storage/local", "Datastore.AllocateTemplate"}},
		"cluster":                       {clusterTk, "Cluster team-a/web", ClusterNeeds(web.Pool, &places), Need{"/pool/" + web.Pool, "VM.Config.Disk"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newTokenFixture(t)
			g := grantsFor(tc.needs)
			g[tc.drop.Path] = slices.DeleteFunc(g[tc.drop.Path], func(p string) bool { return p == tc.drop.Privilege })
			f.pve.set(tc.token, g)
			s := f.states()[tc.grantee]
			if s.OK || s.Problem != TokenPrivileges || len(s.Missing) != 1 || s.Missing[0] != tc.drop {
				t.Fatalf("state %+v, want only %v missing", s, tc.drop)
			}
			if msg := s.Message(); !strings.Contains(msg, tc.drop.Privilege+" on "+tc.drop.Path) {
				t.Errorf("message %q names neither the privilege nor its path", msg)
			}
			for other, st := range f.states() {
				if other != tc.grantee && !st.OK {
					t.Errorf("%s fails too: %+v", other, st)
				}
			}
		})
	}
}

func TestARefusedTokenIsSaidToBeRefused(t *testing.T) {
	t.Parallel()
	f := newTokenFixture(t)
	f.pve.mu.Lock()
	delete(f.pve.grants, capmoxTk)
	delete(f.pve.grants, haTk)
	f.pve.mu.Unlock()
	got := f.states()
	for _, name := range []string{"Cluster API provider", "Proxmox VE HA"} {
		if s := got[name]; s.OK || s.Problem != TokenRefused || !strings.Contains(s.Message(), "refused") {
			t.Errorf("%s: %+v", name, s)
		}
	}
}

func TestACertificateFromAnotherCAIsSaidToBeUntrusted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	// The client trusts no CA the server's certificate chains to, as one holding the old CA.
	c := proxmox.NewClient(srv.URL+"/api2/json", proxmox.WithAPIToken("controller@pve!controller", "c"))
	places := testPlaces()
	s := TokenProbe{Name: "Controller", Needs: ControllerNeeds(&places), Ask: AskPermissions(c)}.Check(t.Context())
	if s.OK || s.Problem != TokenCertificate || !strings.Contains(s.Message(), "certificate") {
		t.Fatalf("state %+v", s)
	}
}

func TestAnAPIThatDoesNotAnswerIsUnreachableNotRefused(t *testing.T) {
	t.Parallel()
	f := newTokenFixture(t)
	f.srv.Close()
	if s := f.states()["Controller"]; s.OK || s.Problem != TokenUnreachable {
		t.Fatalf("state %+v", s)
	}
}

// Addresses and ports are part of the error text, so digits in them never decide the class.
func TestTokenErrorsAreClassifiedByMeaningNotDigits(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err  error
		want string
	}{
		{errors.New(`reading permissions on /: Get "https://127.0.0.1:40139/api2/json/access/permissions": dial tcp 127.0.0.1:40139: connect: connection refused`), TokenUnreachable},
		{errors.New(`dial tcp 10.0.4.1:4010: i/o timeout`), TokenUnreachable},
		{fmt.Errorf("reading permissions on /: %w", proxmox.ErrNotAuthorized), TokenRefused},
		{errors.New("asking a token Job: not authorized to access endpoint"), TokenRefused},
		{errors.New("asking a token Job: authentication failure"), TokenRefused},
		{errors.New(`Get "https://pve:8006/": x509: certificate signed by unknown authority`), TokenCertificate},
	} {
		if got := classifyTokenError(c.err); got != c.want {
			t.Errorf("%q is %s, want %s", c.err, got, c.want)
		}
	}
}

func TestTheTokensOutcomeIsRecordedAndClearsOnRecovery(t *testing.T) {
	t.Parallel()
	f := newTokenFixture(t)
	names, _ := NamesFor(testPool, "team-a", "web")
	places := testPlaces()
	ctx := t.Context()
	noon := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	f.reach.Now = func() time.Time { return noon }
	// A privilege of the controller and of the cluster's token are removed.
	f.pve.set(controllerTk, grantsFor(ControllerNeeds(&places)[:1]))
	f.pve.set(clusterTk, grantsFor(ClusterNeeds(names.Pool, &places)[:1]))
	f.reach.testTokens(ctx, noon)
	if err := f.reach.publish(ctx); err != nil {
		t.Fatal(err)
	}
	recorded := func() (map[string]TokenState, *clusterv1.Cluster) {
		cm := &corev1.ConfigMap{}
		if err := f.kube.Get(ctx, client.ObjectKey{Namespace: "kubehenge-system", Name: ProxmoxReachName}, cm); err != nil {
			t.Fatal(err)
		}
		r, err := ParseReachability(cm)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]TokenState{}
		for _, s := range r.Tokens {
			out[s.Name] = s
		}
		c := &clusterv1.Cluster{}
		if err := f.kube.Get(ctx, webKey, c); err != nil {
			t.Fatal(err)
		}
		return out, c
	}
	got, web := recorded()
	if got["Controller"].OK || got["Cluster team-a/web"].OK || !got["Template builder"].OK {
		t.Fatalf("recorded %+v", got)
	}
	cond := meta.FindStatusCondition(web.Status.Conditions, ProxmoxAccessRefusedCondition)
	if cond == nil || cond.Reason != TokenPrivileges || !strings.Contains(cond.Message, "VM.Config.Disk") {
		t.Fatalf("the cluster's condition is %+v", cond)
	}
	f.pve.set(controllerTk, grantsFor(ControllerNeeds(&places)))
	f.pve.set(clusterTk, grantsFor(ClusterNeeds(names.Pool, &places)))
	f.reach.testTokens(ctx, noon.Add(TokenCheckInterval))
	if err := f.reach.publish(ctx); err != nil {
		t.Fatal(err)
	}
	got, web = recorded()
	for name, s := range got {
		if !s.OK {
			t.Errorf("%s still fails: %+v", name, s)
		}
	}
	if meta.FindStatusCondition(web.Status.Conditions, ProxmoxAccessRefusedCondition) != nil {
		t.Error("the cluster's condition stays after its token works")
	}
}
