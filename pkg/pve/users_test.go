package pve

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeUsersAPI is Proxmox VE's user, role, ACL and token API, recording every call as "METHOD path
// form". acl is what it lists. failToken has it refuse to make tokens.
type fakeUsersAPI struct {
	mu        sync.Mutex
	users     map[string]bool
	acl       []map[string]any
	changes   []string
	failToken bool
}

func (f *fakeUsersAPI) accounts(t *testing.T) *Accounts {
	t.Helper()
	reply := func(w http.ResponseWriter, data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/access/", func(w http.ResponseWriter, r *http.Request) {
		token := strings.HasPrefix(r.Header.Get("Authorization"), "PVEAPIToken=")
		if r.Method != http.MethodGet && !token && r.Header.Get("Csrfpreventiontoken") != "csrf" {
			http.Error(w, "no CSRF token", http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		f.changes = append(f.changes, strings.TrimSpace(r.Method+" "+path+" "+r.PostForm.Encode()))
		if data, ok := f.listOrAccept(r.Method, path, token); ok {
			reply(w, data)
			return
		}
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/access/users/"):
			user := strings.TrimPrefix(path, "/access/users/")
			if !f.users[user] {
				// Proxmox VE puts its message in the status line, which net/http cannot write.
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = io.WriteString(conn, "HTTP/1.1 500 no such user ('"+user+"')\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
				return
			}
			delete(f.users, user)
			reply(w, nil)
		case r.Method == http.MethodPost && path == "/access/users":
			f.users[r.PostForm.Get("userid")] = true
			reply(w, nil)
		case r.Method == http.MethodPut && path == "/access/acl":
			reply(w, nil)
		case r.Method == http.MethodPost && strings.Contains(path, "/token/") && !f.failToken:
			user, name, _ := strings.Cut(strings.TrimPrefix(path, "/access/users/"), "/token/")
			reply(w, map[string]any{"full-tokenid": user + "!" + name, "value": "5ec2e7", "info": map[string]any{"privsep": 0}})
		default:
			http.Error(w, "refused", http.StatusInternalServerError)
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAccounts(Config{URL: srv.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// listOrAccept answers the calls that only list or are accepted as they are: a token deleting
// itself, the roles with one of the upgrader's there from before, the ACL, role and group changes.
func (f *fakeUsersAPI) listOrAccept(method, path string, token bool) (any, bool) {
	switch {
	case method == http.MethodDelete && token && strings.Contains(path, "/token/"):
		return nil, true
	case method == http.MethodGet && path == "/access/roles":
		return []map[string]any{{"roleid": "KubehengeUpgrader"}, {"roleid": "Administrator"}}, true
	case method == http.MethodGet && path == "/access/acl":
		return f.acl, true
	case method == http.MethodPut && strings.HasPrefix(path, "/access/roles/"),
		method == http.MethodPost && path == "/access/roles",
		method == http.MethodPut && strings.HasPrefix(path, "/access/users/"):
		return nil, true
	}
	return nil, false
}

var alice = &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1", CSRF: "csrf"}

var testUpgrader = &Upgrader{
	User: "upgrader@pve", Token: "apply", Comment: "for upgrades", Group: "kubehenge-tokens",
	Roles:  map[string][]string{"KubehengeUpgrader": {"VM.Audit", "VM.PowerMgmt"}, "KubehengeUpgraderAudit": {"Sys.Audit"}},
	Grants: []Grant{{Path: "/", Role: "KubehengeUpgraderAudit", Propagate: true}, {Path: "/pool/kubehenge-management", Role: "KubehengeUpgrader", Propagate: true}},
}

func TestCreateUpgrader(t *testing.T) {
	t.Parallel()
	f := &fakeUsersAPI{users: map[string]bool{}}
	a := f.accounts(t)
	got, err := a.CreateUpgrader(t.Context(), alice, testUpgrader)
	if err != nil {
		t.Fatal(err)
	}
	if got != "upgrader@pve!apply=5ec2e7" {
		t.Fatalf("token = %q", got)
	}
	want := []string{
		"DELETE /access/users/upgrader@pve",
		"POST /access/users comment=for+upgrades&groups=kubehenge-tokens&userid=upgrader%40pve",
		"GET /access/roles",
		"PUT /access/roles/KubehengeUpgrader privs=VM.Audit%2CVM.PowerMgmt",
		"POST /access/roles privs=Sys.Audit&roleid=KubehengeUpgraderAudit",
		"PUT /access/users/upgrader@pve groups=kubehenge-tokens",
		"GET /access/acl",
		"PUT /access/acl path=%2F&propagate=1&roles=KubehengeUpgraderAudit&users=upgrader%40pve",
		"PUT /access/acl path=%2Fpool%2Fkubehenge-management&propagate=1&roles=KubehengeUpgrader&users=upgrader%40pve",
		"POST /access/users/upgrader@pve/token/apply comment=for+upgrades&privsep=0",
	}
	if !reflect.DeepEqual(f.changes, want) {
		t.Fatalf("changes:\n%s\nwant:\n%s", strings.Join(f.changes, "\n"), strings.Join(want, "\n"))
	}

	// Made again, the user left from before goes first, with its token.
	f.changes = nil
	if _, err := a.CreateUpgrader(t.Context(), alice, testUpgrader); err != nil {
		t.Fatal(err)
	}
	if len(f.changes) != len(want) || !f.users["upgrader@pve"] {
		t.Fatalf("changes = %v", f.changes)
	}

	if err := a.DeleteUser(t.Context(), alice, "upgrader@pve"); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteUser(t.Context(), alice, "upgrader@pve"); err != nil {
		t.Fatalf("deleting a user that is gone: %v", err)
	}
}

// An upgrader made with Administrator on / holds the upgrader's grants alone afterwards, losing
// Administrator last. A grant it holds already is left as it is.
func TestGrantUpgraderTakesAdministratorAway(t *testing.T) {
	t.Parallel()
	f := &fakeUsersAPI{users: map[string]bool{"upgrader@pve": true}, acl: []map[string]any{
		{"path": "/", "roleid": "Administrator", "type": "user", "ugid": "upgrader@pve", "propagate": 1},
		{"path": "/", "roleid": "Administrator", "type": "user", "ugid": "root@pam", "propagate": 1},
		{"path": "/storage/old", "roleid": "KubehengeUpgrader", "type": "user", "ugid": "upgrader@pve", "propagate": 0},
		{"path": "/", "roleid": "KubehengeUpgraderAudit", "type": "user", "ugid": "upgrader@pve", "propagate": 1},
	}}
	a := f.accounts(t)
	if err := a.GrantUpgrader(t.Context(), alice, testUpgrader); err != nil {
		t.Fatal(err)
	}
	var acl []string
	for _, c := range f.changes {
		if strings.HasPrefix(c, "PUT /access/acl") {
			acl = append(acl, c)
		}
	}
	want := []string{
		"PUT /access/acl path=%2Fpool%2Fkubehenge-management&propagate=1&roles=KubehengeUpgrader&users=upgrader%40pve",
		"PUT /access/acl delete=1&path=%2Fstorage%2Fold&roles=KubehengeUpgrader&users=upgrader%40pve",
		"PUT /access/acl delete=1&path=%2F&roles=Administrator&users=upgrader%40pve",
	}
	if !reflect.DeepEqual(acl, want) {
		t.Fatalf("ACL changes:\n%s\nwant:\n%s", strings.Join(acl, "\n"), strings.Join(want, "\n"))
	}
}

// A token revokes itself, signed in as itself.
func TestRevokeToken(t *testing.T) {
	t.Parallel()
	f := &fakeUsersAPI{users: map[string]bool{}}
	a := f.accounts(t)
	if err := a.RevokeToken(t.Context(), "upgrader@pve!apply=5ec2e7"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"DELETE /access/users/upgrader@pve/token/apply"}; !reflect.DeepEqual(f.changes, want) {
		t.Fatalf("changes = %v", f.changes)
	}
}

// A user creates its own token with the ticket its password gets. A node that refuses that ticket
// at once runs a clock apart from the node that issued it.
func TestCreateOwnToken(t *testing.T) {
	t.Parallel()
	var refuse atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api2/json/access/ticket", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("username") != "web@pve" || r.FormValue("password") != "pa55word" {
			http.Error(w, "authentication failure", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"username": "web@pve", "ticket": "PVE:web@pve:1", "CSRFPreventionToken": "csrf"}})
	})
	mux.HandleFunc("POST /api2/json/access/users/web@pve/token/cluster", func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() || r.Header.Get("Cookie") != "PVEAuthCookie=PVE:web@pve:1" || r.Header.Get("Csrfpreventiontoken") != "csrf" || r.FormValue("privsep") != "0" {
			http.Error(w, "authentication failure", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"value": "5ec2e7"}})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAccounts(Config{URL: srv.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.CreateOwnToken(t.Context(), "web@pve", "pa55word", "cluster", "c"); err != nil || got != "5ec2e7" {
		t.Fatalf("CreateOwnToken = %q, %v", got, err)
	}
	refuse.Store(true)
	if _, err := a.CreateOwnToken(t.Context(), "web@pve", "pa55word", "cluster", "c"); !errors.Is(err, ErrNodesDisagree) {
		t.Errorf("a token refused right after the sign-in: %v, want ErrNodesDisagree", err)
	}
	if _, err := a.CreateOwnToken(t.Context(), "web@pve", "wrong", "cluster", "c"); !errors.Is(err, ErrAuthentication) || errors.Is(err, ErrNodesDisagree) {
		t.Errorf("a refused sign-in: %v", err)
	}
}

// A user whose token Proxmox VE refused is not left behind without one.
func TestCreateUpgraderLeavesNoUserWhenItFails(t *testing.T) {
	t.Parallel()
	f := &fakeUsersAPI{users: map[string]bool{}, failToken: true}
	a := f.accounts(t)
	if _, err := a.CreateUpgrader(t.Context(), alice, testUpgrader); err == nil {
		t.Fatal("a token made where Proxmox VE refused it")
	}
	if f.users["upgrader@pve"] {
		t.Fatal("the user stayed without its token")
	}
}
