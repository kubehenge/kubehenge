package pve

import (
	"context"
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
	"testing"
)

// fakeTicketAPI answers /access/ticket like Proxmox VE: alice signs in with a password alone, bob
// also needs TOTP 123456, and any issued ticket renews itself.
func fakeTicketAPI(t *testing.T) *Accounts {
	t.Helper()
	// Proxmox VE escapes only the colons of a ticket's data (PVE::Ticket, uri_escape($data, ':')).
	challenge := "PVE:!tfa!" + strings.ReplaceAll(`{"totp":true,"recovery":[0,2],"webauthn":null}`, ":", "%3A") + ":68000000::c2ln"
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}
	mux.HandleFunc("POST /api2/json/access/ticket", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		user, password, answer := r.PostForm.Get("username"), r.PostForm.Get("password"), r.PostForm.Get("tfa-challenge")
		switch {
		case user == "alice@pve" && (password == "secret" || password == "PVE:alice@pve:1"): //nolint:gosec // test data
			reply(w, map[string]any{"username": user, "ticket": "PVE:alice@pve:1", "CSRFPreventionToken": "csrf"})
		case user == "bob@pve" && answer == "" && password == "secret":
			reply(w, map[string]any{"username": user, "ticket": challenge, "CSRFPreventionToken": "csrf", "NeedTFA": 1})
		case user == "bob@pve" && answer == challenge && password == "totp:123456":
			reply(w, map[string]any{"username": user, "ticket": "PVE:bob@pve:1", "CSRFPreventionToken": "csrf"})
		default:
			http.Error(w, "authentication failure", http.StatusUnauthorized)
		}
	})
	mux.HandleFunc("GET /api2/json/access/permissions", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("PVEAuthCookie"); err != nil || c.Value != "PVE:alice@pve:1" {
			http.Error(w, "no ticket", http.StatusUnauthorized)
			return
		}
		if !r.URL.Query().Has("path") {
			reply(w, map[string]any{"/": map[string]int{"Sys.Audit": 1}, "/pool/kubehenge/default.one": map[string]int{"VM.Audit": 1, "VM.Allocate": 0}})
			return
		}
		path := r.URL.Query().Get("path")
		reply(w, map[string]any{path: map[string]int{"VM.Audit": 1, "VM.Allocate": 0}})
	})
	mux.HandleFunc("GET /api2/json/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{
			{"vmid": 150, "node": "one", "pool": "infra"},
			{"vmid": 151, "node": "one"},
			{"vmid": 152, "node": "one"},
			{"vmid": 153, "node": "one"},
			{"vmid": 154, "node": "two"},
			{"vmid": 160, "node": "one", "type": "qemu", "name": "kubehenge-ubuntu-26.04", "template": 1, "tags": "cloud-image;kubehenge;ubuntu-26.04", "pool": "kubehenge-templates"},
			{"vmid": 161, "node": "one", "type": "qemu", "name": "kubehenge-ubuntu-26.04", "template": 1, "tags": "cloud-image;kubehenge;ubuntu-26.04", "pool": "elsewhere"},
			{"vmid": 162, "node": "one", "type": "qemu", "name": "being-built", "template": 0, "pool": "kubehenge-templates"},
			{"vmid": 163, "node": "one", "type": "lxc", "name": "a-container-template", "template": 1, "pool": "kubehenge-templates"},
		})
	})
	mux.HandleFunc("GET /api2/json/cluster/backup", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{
			{"id": "off", "enabled": 0, "vmid": "151"},
			{"id": "all", "all": 1, "node": "one", "exclude": "152,999"},
			{"id": "pool", "pool": "infra"},
		})
	})
	// As Proxmox VE 9 answers, for alice only; bob lacks Sys.Audit on /.
	mux.HandleFunc("GET /api2/json/cluster/ha/rules", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("PVEAuthCookie"); err != nil || c.Value != "PVE:alice@pve:1" {
			http.Error(w, "Permission check failed (/, Sys.Audit)", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("type") != "node-affinity" {
			t.Errorf("rules asked for with %s", r.URL.RawQuery)
		}
		reply(w, []map[string]any{
			{"rule": "racks", "type": "node-affinity", "affinity": "positive", "comment": "rack A", "nodes": "one:2,two", "resources": "vm:100", "strict": 1, "order": 1},
			{"rule": "off-three", "type": "node-affinity", "affinity": "negative", "nodes": "three", "resources": "vm:101", "order": 2},
			{"rule": "old", "type": "node-affinity", "affinity": "positive", "nodes": "two", "resources": "vm:102", "disable": 1, "order": 3},
		})
	})
	mux.HandleFunc("GET /api2/json/pools", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("poolid") != "kubehenge/clusters.web" {
			// Proxmox VE puts its message in the status line, which net/http cannot write.
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.WriteString(conn, "HTTP/1.1 500 pool '"+r.URL.Query().Get("poolid")+"' does not exist\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		reply(w, []map[string]any{{"poolid": "kubehenge/clusters.web", "members": []map[string]any{
			{"vmid": 170, "node": "two", "type": "qemu", "name": "web-cp-a"},
			{"node": "two", "type": "storage", "storage": "local"},
		}}})
	})
	mux.HandleFunc("GET /api2/json/access/domains", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{{"realm": "pve", "type": "pve", "default": 1}, {"realm": "pam", "type": "pam"}})
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

func TestSignInWithPassword(t *testing.T) {
	t.Parallel()
	a, ctx := fakeTicketAPI(t), context.Background()
	ticket, challenge, err := a.SignIn(ctx, "alice@pve", "secret")
	if err != nil || challenge != nil || ticket.Ticket != "PVE:alice@pve:1" || ticket.CSRF != "csrf" || ticket.Username != "alice@pve" {
		t.Fatalf("SignIn = %+v, %+v, %v", ticket, challenge, err)
	}
	renewed, err := a.Renew(ctx, ticket)
	if err != nil || renewed.Ticket != "PVE:alice@pve:1" || renewed.Issued.Before(ticket.Issued) {
		t.Fatalf("Renew = %+v, %v", renewed, err)
	}
	if _, _, err := a.SignIn(ctx, "alice@pve", "wrong"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong password: %v, want ErrAuthentication", err)
	}
}

func TestSignInWithSecondFactor(t *testing.T) {
	t.Parallel()
	a, ctx := fakeTicketAPI(t), context.Background()
	ticket, challenge, err := a.SignIn(ctx, "bob@pve", "secret")
	if err != nil || ticket != nil {
		t.Fatalf("SignIn = %+v, %v; want a challenge", ticket, err)
	}
	if !challenge.TOTP || challenge.Yubico || challenge.WebAuthn || !reflect.DeepEqual(challenge.Recovery, []int{0, 2}) {
		t.Fatalf("challenge = %+v", challenge)
	}
	if _, err = a.Answer(ctx, challenge, "totp", "000000"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong code: %v, want ErrAuthentication", err)
	}
	if _, err = a.Answer(ctx, challenge, "webauthn", "{}"); err == nil {
		t.Fatal("a WebAuthn answer was relayed")
	}
	ticket, err = a.Answer(ctx, challenge, "totp", "123456")
	if err != nil || ticket.Ticket != "PVE:bob@pve:1" {
		t.Fatalf("Answer = %+v, %v", ticket, err)
	}
}

func TestParseChallengeRejectsAFullTicket(t *testing.T) {
	t.Parallel()
	if _, err := parseChallenge("PVE:alice@pve:68000000::c2ln"); err == nil {
		t.Fatal("a ticket without a challenge parsed")
	}
}

func TestPrivileges(t *testing.T) {
	t.Parallel()
	a, ctx := fakeTicketAPI(t), context.Background()
	ticket, _, err := a.SignIn(ctx, "alice@pve", "secret")
	if err != nil {
		t.Fatal(err)
	}
	privs, err := a.Privileges(ctx, ticket, "/pool/kubehenge/default.one")
	if err != nil {
		t.Fatal(err)
	}
	if !privs.Has("VM.Audit") || privs.Has("VM.Allocate") || privs.Has("VM.Audit", "Sys.Modify") {
		t.Fatalf("privileges = %v", privs)
	}
	if _, err = a.Privileges(ctx, &Ticket{Username: "alice@pve", Ticket: "forged"}, "/"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("forged ticket: %v, want ErrAuthentication", err)
	}
	all, err := a.AllPrivileges(ctx, ticket)
	if err != nil || len(all) != 2 || !all["/"].Has("Sys.Audit") || !all["/pool/kubehenge/default.one"].Has("VM.Audit") || all["/pool/kubehenge/default.one"].Has("VM.Allocate") {
		t.Fatalf("all privileges = %v, %v", all, err)
	}
}

func TestBackupCovers(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	tk := &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"}
	// 150 by its pool, 151 only by a disabled job but by "all", 152 excluded from "all", 153 by "all",
	// 154 on a node the "all" job leaves out.
	for vmid, want := range map[int]bool{150: true, 151: true, 152: false, 153: true, 154: false} {
		got, err := a.BackupCovers(context.Background(), tk, vmid)
		if err != nil || got != want {
			t.Errorf("BackupCovers(%d) = %v, %v; want %v", vmid, got, err, want)
		}
	}
}

func TestTemplatesOfAPoolWithTheirTags(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	got, err := a.Templates(context.Background(), &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"}, "kubehenge-templates")
	want := []Member{{VMID: 160, Node: "one", Type: "qemu", Name: "kubehenge-ubuntu-26.04", Template: 1, Tags: "cloud-image;kubehenge;ubuntu-26.04"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Templates = %+v, %v; want %+v", got, err, want)
	}
}

func TestMembersOfANestedPool(t *testing.T) {
	t.Parallel()
	a, ctx := fakeTicketAPI(t), context.Background()
	tk := &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"}
	got, err := a.PoolMembers(ctx, tk, "kubehenge/clusters.web")
	want := []Member{{VMID: 170, Node: "two", Type: "qemu", Name: "web-cp-a"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("PoolMembers = %+v, %v; want %+v", got, err, want)
	}
	if got, err := a.PoolMembers(ctx, tk, "kubehenge/clusters.gone"); err != nil || got != nil {
		t.Fatalf("missing pool: %+v, %v; want none", got, err)
	}
}

func TestNodeAffinityRules(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	rules, err := a.NodeAffinityRules(context.Background(), &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []HARule{
		{Rule: "racks", Comment: "rack A", Nodes: []string{"one", "two"}, Strict: true},
		{Rule: "off-three", Nodes: []string{"three"}, Negative: true},
	}
	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("rules = %+v, want %+v", rules, want)
	}
	if _, err = a.NodeAffinityRules(context.Background(), &Ticket{Username: "bob@pve", Ticket: "PVE:bob@pve:1"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("without Sys.Audit: %v, want ErrForbidden", err)
	}
}

func TestReleaseHARemovesOnlyThePrefixed(t *testing.T) {
	t.Parallel()
	var removed []string
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}
	mux.HandleFunc("GET /api2/json/cluster/ha/rules", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{
			{"rule": "kubehenge-1-apart", "comment": "Kubehenge cluster kubehenge/a.web"},
			{"rule": "racks", "comment": "rack A"},
			{"rule": "bare"},
		})
	})
	mux.HandleFunc("GET /api2/json/cluster/ha/resources", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{
			{"sid": "vm:101", "comment": "Kubehenge cluster kubehenge/a.web"},
			{"sid": "vm:102", "comment": "Kubehenge cluster kubehenge-other/a.web"},
			{"sid": "ct:103"},
		})
	})
	mux.HandleFunc("DELETE /api2/json/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Csrfpreventiontoken") != "csrf" {
			http.Error(w, "no CSRF token", http.StatusUnauthorized)
			return
		}
		removed = append(removed, strings.TrimPrefix(r.URL.Path, "/api2/json/cluster/ha/"))
		reply(w, nil)
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
	if err := a.ReleaseHA(t.Context(), &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1", CSRF: "csrf"}, "Kubehenge cluster kubehenge/"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"rules/kubehenge-1-apart", "resources/vm:101"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
}

func TestRealms(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	realms, err := a.Realms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Realm{{Realm: "pam", Type: "pam"}, {Realm: "pve", Type: "pve", Default: true}}
	if !reflect.DeepEqual(realms, want) {
		t.Fatalf("realms = %+v, want %+v", realms, want)
	}
}
