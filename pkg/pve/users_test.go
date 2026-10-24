package pve

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeUsersAPI is Proxmox VE's user, ACL and token API with one user known, old@pve, recording
// every change as "METHOD path form". failToken has it refuse to make tokens.
type fakeUsersAPI struct {
	mu        sync.Mutex
	users     map[string]bool
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
		if r.Header.Get("Csrfpreventiontoken") != "csrf" {
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
				_, _ = io.WriteString(conn, "HTTP/1.1 500 delete user failed: user '"+user+"' does not exist\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
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

var alice = &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1", CSRF: "csrf"}

func TestCreateAdministratorToken(t *testing.T) {
	t.Parallel()
	f := &fakeUsersAPI{users: map[string]bool{}}
	a := f.accounts(t)
	got, err := a.CreateAdministratorToken(t.Context(), alice, "upgrader@pve", "apply", "for upgrades")
	if err != nil {
		t.Fatal(err)
	}
	if got != "upgrader@pve!apply=5ec2e7" {
		t.Fatalf("token = %q", got)
	}
	want := []string{
		"DELETE /access/users/upgrader@pve",
		"POST /access/users comment=for+upgrades&userid=upgrader%40pve",
		"PUT /access/acl path=%2F&propagate=1&roles=Administrator&users=upgrader%40pve",
		"POST /access/users/upgrader@pve/token/apply comment=for+upgrades&privsep=0",
	}
	if !reflect.DeepEqual(f.changes, want) {
		t.Fatalf("changes:\n%s\nwant:\n%s", strings.Join(f.changes, "\n"), strings.Join(want, "\n"))
	}

	// Made again, the user left from before goes first, with its token.
	f.changes = nil
	if _, err := a.CreateAdministratorToken(t.Context(), alice, "upgrader@pve", "apply", "for upgrades"); err != nil {
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

// A user whose token Proxmox VE refused is not left behind without one.
func TestCreateAdministratorTokenLeavesNoUserWhenItFails(t *testing.T) {
	t.Parallel()
	f := &fakeUsersAPI{users: map[string]bool{}, failToken: true}
	a := f.accounts(t)
	if _, err := a.CreateAdministratorToken(t.Context(), alice, "upgrader@pve", "apply", "for upgrades"); err == nil {
		t.Fatal("a token made where Proxmox VE refused it")
	}
	if f.users["upgrader@pve"] {
		t.Fatal("the user stayed without its token")
	}
}
